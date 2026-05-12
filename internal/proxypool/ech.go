package proxypool

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	log "github.com/sirupsen/logrus"
)

const (
	typeHTTPS     = 65
	echDNSServer  = "dns.alidns.com/dns-query"
	echDomainName = "cloudflare-ech.com"
)

// ECHDialer dials target addresses through a Cloudflare Worker ECH WebSocket tunnel.
// Each dialer corresponds to one Worker domain (= one exit IP).
type ECHDialer struct {
	name      string
	domain    string // e.g. "ech-workers.pikapk-f47.workers.dev:443"
	serverIP  string // optional: pin Cloudflare IP
	token     string
	echMu     sync.RWMutex
	echConfig []byte
}

// NewECHDialer creates a dialer for the given worker config.
func NewECHDialer(name, domain, ip, token string) (*ECHDialer, error) {
	d := &ECHDialer{
		name:     name,
		domain:   domain,
		serverIP: ip,
		token:    token,
	}
	if err := d.refreshECH(); err != nil {
		return nil, fmt.Errorf("ECH init for %s: %w", name, err)
	}
	return d, nil
}

// Dial connects to target (host:port) through the ECH WebSocket tunnel.
// Returns a net.Conn backed by the WebSocket binary frames.
func (d *ECHDialer) Dial(target string) (net.Conn, error) {
	wsConn, err := d.dialWebSocket(2)
	if err != nil {
		return nil, fmt.Errorf("ws dial for %s: %w", target, err)
	}

	// Send CONNECT request
	msg := fmt.Sprintf("CONNECT:%s|", target)
	if err := wsConn.WriteMessage(websocket.TextMessage, []byte(msg)); err != nil {
		wsConn.Close()
		return nil, fmt.Errorf("ws CONNECT: %w", err)
	}

	// Read response
	_, resp, err := wsConn.ReadMessage()
	if err != nil {
		wsConn.Close()
		return nil, fmt.Errorf("ws CONNECT response: %w", err)
	}
	if string(resp) != "CONNECTED" {
		wsConn.Close()
		return nil, fmt.Errorf("tunnel rejected: %s", string(resp))
	}

	return newWSConn(wsConn, d.name, target), nil
}

func (d *ECHDialer) refreshECH() error {
	echBase64, err := queryHTTPSRecord(echDomainName, echDNSServer)
	if err != nil {
		return fmt.Errorf("DNS query: %w", err)
	}
	if echBase64 == "" {
		return errors.New("no ECH params found")
	}
	raw, err := base64.StdEncoding.DecodeString(echBase64)
	if err != nil {
		return fmt.Errorf("ECH decode: %w", err)
	}
	d.echMu.Lock()
	d.echConfig = raw
	d.echMu.Unlock()
	log.Debugf("[proxypool] %s ECH config loaded (%d bytes)", d.name, len(raw))
	return nil
}

func (d *ECHDialer) getECH() ([]byte, error) {
	d.echMu.RLock()
	defer d.echMu.RUnlock()
	if len(d.echConfig) == 0 {
		return nil, errors.New("ECH config not loaded")
	}
	return d.echConfig, nil
}

func (d *ECHDialer) dialWebSocket(maxRetries int) (*websocket.Conn, error) {
	host, port, path, err := parseServerAddr(d.domain)
	if err != nil {
		return nil, err
	}
	wsURL := fmt.Sprintf("wss://%s:%s%s", host, port, path)

	for attempt := 1; attempt <= maxRetries; attempt++ {
		echBytes, echErr := d.getECH()
		if echErr != nil {
			if attempt < maxRetries {
				d.refreshECH()
				continue
			}
			return nil, echErr
		}

		tlsCfg, tlsErr := buildTLSConfigWithECH(host, echBytes)
		if tlsErr != nil {
			return nil, tlsErr
		}

		dialer := websocket.Dialer{
			TLSClientConfig:  tlsCfg,
			HandshakeTimeout: 10 * time.Second,
		}
		if d.token != "" {
			dialer.Subprotocols = []string{d.token}
		}
		if d.serverIP != "" {
			dialer.NetDial = func(network, address string) (net.Conn, error) {
				_, p, err := net.SplitHostPort(address)
				if err != nil {
					return nil, err
				}
				return net.DialTimeout(network, net.JoinHostPort(d.serverIP, p), 10*time.Second)
			}
		}

		wsConn, _, dialErr := dialer.Dial(wsURL, nil)
		if dialErr != nil {
			if strings.Contains(dialErr.Error(), "ECH") && attempt < maxRetries {
				d.refreshECH()
				time.Sleep(time.Second)
				continue
			}
			return nil, dialErr
		}
		return wsConn, nil
	}
	return nil, errors.New("max retries reached")
}

// --- TLS + ECH helpers (ported from ech-workers.go) ---

func buildTLSConfigWithECH(serverName string, echList []byte) (*tls.Config, error) {
	roots, err := x509.SystemCertPool()
	if err != nil {
		return nil, fmt.Errorf("load system certs: %w", err)
	}
	return &tls.Config{
		MinVersion:                     tls.VersionTLS13,
		ServerName:                     serverName,
		EncryptedClientHelloConfigList: echList,
		EncryptedClientHelloRejectionVerify: func(cs tls.ConnectionState) error {
			return errors.New("ECH rejected by server")
		},
		RootCAs: roots,
	}, nil
}

func parseServerAddr(addr string) (host, port, path string, err error) {
	path = "/"
	if idx := strings.Index(addr, "/"); idx != -1 {
		path = addr[idx:]
		addr = addr[:idx]
	}
	host, port, err = net.SplitHostPort(addr)
	if err != nil {
		return "", "", "", fmt.Errorf("invalid server addr: %v", err)
	}
	return host, port, path, nil
}

// --- DoH DNS query (ported from ech-workers.go) ---

func queryHTTPSRecord(domain, dnsServer string) (string, error) {
	dohURL := dnsServer
	if !strings.HasPrefix(dohURL, "https://") && !strings.HasPrefix(dohURL, "http://") {
		dohURL = "https://" + dohURL
	}
	u, err := url.Parse(dohURL)
	if err != nil {
		return "", fmt.Errorf("invalid DoH URL: %v", err)
	}
	dnsQuery := buildDNSQuery(domain, typeHTTPS)
	q := u.Query()
	q.Set("dns", base64.RawURLEncoding.EncodeToString(dnsQuery))
	u.RawQuery = q.Encode()

	req, err := http.NewRequest("GET", u.String(), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/dns-message")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("DoH request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("DoH status: %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	return parseDNSResponse(body)
}

func buildDNSQuery(domain string, qtype uint16) []byte {
	var buf bytes.Buffer
	buf.Write([]byte{0x00, 0x01, 0x01, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00})
	for _, label := range strings.Split(domain, ".") {
		buf.WriteByte(byte(len(label)))
		buf.WriteString(label)
	}
	buf.WriteByte(0x00)
	buf.WriteByte(byte(qtype >> 8))
	buf.WriteByte(byte(qtype))
	buf.Write([]byte{0x00, 0x01})
	return buf.Bytes()
}

func parseDNSResponse(response []byte) (string, error) {
	if len(response) < 12 {
		return "", errors.New("response too short")
	}
	ancount := binary.BigEndian.Uint16(response[6:8])
	if ancount == 0 {
		return "", errors.New("no answers")
	}
	offset := 12
	for offset < len(response) && response[offset] != 0 {
		offset += int(response[offset]) + 1
	}
	offset += 5

	for i := 0; i < int(ancount); i++ {
		if offset >= len(response) {
			break
		}
		if response[offset]&0xC0 == 0xC0 {
			offset += 2
		} else {
			for offset < len(response) && response[offset] != 0 {
				offset += int(response[offset]) + 1
			}
			offset++
		}
		if offset+10 > len(response) {
			break
		}
		rrType := binary.BigEndian.Uint16(response[offset : offset+2])
		offset += 8
		dataLen := binary.BigEndian.Uint16(response[offset : offset+2])
		offset += 2
		if offset+int(dataLen) > len(response) {
			break
		}
		data := response[offset : offset+int(dataLen)]
		offset += int(dataLen)
		if rrType == typeHTTPS {
			if ech := parseHTTPSRecordECH(data); ech != "" {
				return ech, nil
			}
		}
	}
	return "", nil
}

func parseHTTPSRecordECH(data []byte) string {
	if len(data) < 2 {
		return ""
	}
	offset := 2
	if offset < len(data) && data[offset] == 0 {
		offset++
	} else {
		for offset < len(data) && data[offset] != 0 {
			offset += int(data[offset]) + 1
		}
		offset++
	}
	for offset+4 <= len(data) {
		key := binary.BigEndian.Uint16(data[offset : offset+2])
		length := binary.BigEndian.Uint16(data[offset+2 : offset+4])
		offset += 4
		if offset+int(length) > len(data) {
			break
		}
		value := data[offset : offset+int(length)]
		offset += int(length)
		if key == 5 {
			return base64.StdEncoding.EncodeToString(value)
		}
	}
	return ""
}
