package proxypool

import (
	"context"
	"crypto/ecdsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"net"
	"net/netip"
	"sync/atomic"
	"time"

	usqueapi "github.com/hhsw2015/usque/v3/api"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	log "github.com/sirupsen/logrus"
	"golang.zx2c4.com/wireguard/tun/netstack"
)

// WARPDialer dials TCP connections through an in-process Cloudflare WARP
// MASQUE tunnel (one per registered usque account). It exposes the same
// Dial(addr) interface as ECHDialer so the proxy pool can mix them freely.
//
// Construction: NewWARPDialer parses the embedded config, brings up a
// netstack TUN device, and spawns api.MaintainTunnel in a goroutine. After
// Connect() returns, the dialer is ready and the QUIC tunnel reconnects
// automatically on drop.
type WARPDialer struct {
	name    string
	tunNet  *netstack.Net
	healthy atomic.Bool
	cancel  context.CancelFunc
}

// WARPInstance is the YAML-friendly subset of usque's config.json used by CPA.
// Only the four required fields (private-key, endpoint-pub-key, endpoint-v4,
// ipv4) must be set; ipv6 enables dual-stack inside the tunnel.
type WARPInstance = config.WARPInstance

// NewWARPDialer constructs a dialer from a single instance config and starts
// its MASQUE tunnel maintenance loop. Returns an error only on misconfigured
// fields (bad PEM / address) — transient connection failures are handled
// inside MaintainTunnel.
func NewWARPDialer(parent context.Context, inst WARPInstance) (*WARPDialer, error) {
	if inst.Name == "" {
		inst.Name = "warp"
	}

	privKey, err := decodeECDSAPriv(inst.PrivateKey)
	if err != nil {
		return nil, fmt.Errorf("warp %s: private-key: %w", inst.Name, err)
	}
	peerPubKey, err := decodeECDSAPub(inst.EndpointPubKey)
	if err != nil {
		return nil, fmt.Errorf("warp %s: endpoint-pub-key: %w", inst.Name, err)
	}

	endpointIP := net.ParseIP(inst.EndpointV4)
	if endpointIP == nil {
		return nil, fmt.Errorf("warp %s: invalid endpoint-v4 %q", inst.Name, inst.EndpointV4)
	}
	endpoint := &net.UDPAddr{IP: endpointIP, Port: 443}

	ipv4Addr, err := netip.ParseAddr(inst.IPv4)
	if err != nil {
		return nil, fmt.Errorf("warp %s: invalid ipv4 %q: %w", inst.Name, inst.IPv4, err)
	}
	localAddrs := []netip.Addr{ipv4Addr}
	if inst.IPv6 != "" {
		if v6, err := netip.ParseAddr(inst.IPv6); err == nil {
			localAddrs = append(localAddrs, v6)
		}
	}

	cert, err := usqueapi.GenerateCert(privKey, &privKey.PublicKey)
	if err != nil {
		return nil, fmt.Errorf("warp %s: generate cert: %w", inst.Name, err)
	}
	tlsCfg, err := usqueapi.PrepareTlsConfig(privKey, peerPubKey, cert, usqueapi.ConnectSNI, false)
	if err != nil {
		return nil, fmt.Errorf("warp %s: tls config: %w", inst.Name, err)
	}

	// MTU 1280 is Cloudflare WARP's documented spec, but MASQUE + connect-ip
	// capsule overhead makes the effective QUIC DATAGRAM payload budget ~1240
	// bytes. TLS handshake segments (~3-4KB) exceed that, and even with the
	// upstream PR #103 ICMP-handling fix (v3.0.1-cpa.3, no longer deadlocks
	// the pump), gVisor netstack still does not respond to ICMP Packet Too
	// Big — so TCP retransmits full-size segments in a loop until the
	// caller times out. MTU 1200 leaves comfortable headroom under the
	// DATAGRAM ceiling and keeps TLS handshakes flowing.
	const mtu = 1200
	// Use Quad9 rather than 1.1.1.1 for tunnel-internal DNS. 1.1.1.1 is
	// Cloudflare's resolver — sending DNS through a Cloudflare WARP tunnel
	// to Cloudflare's own resolver can hit internal-routing edge cases that
	// silently drop responses. Matches upstream usque's socks-mode defaults.
	dnsAddrs := []netip.Addr{
		netip.MustParseAddr("9.9.9.9"),
		netip.MustParseAddr("149.112.112.112"),
	}
	tunDev, tunNet, err := netstack.CreateNetTUN(localAddrs, dnsAddrs, mtu)
	if err != nil {
		return nil, fmt.Errorf("warp %s: create netstack tun: %w", inst.Name, err)
	}

	ctx, cancel := context.WithCancel(parent)
	d := &WARPDialer{
		name:   inst.Name,
		tunNet: tunNet,
		cancel: cancel,
	}
	d.healthy.Store(true)

	go usqueapi.MaintainTunnel(ctx, usqueapi.MaintainTunnelConfig{
		TLSConfig:         tlsCfg,
		KeepalivePeriod:   30 * time.Second,
		InitialPacketSize: 1242,
		Endpoint:          endpoint,
		Device:            usqueapi.NewNetstackAdapter(tunDev),
		MTU:               mtu,
		ReconnectDelay:    1 * time.Second,
		AlwaysReconnect:   true,
		UseHTTP2:          false,
	})

	return d, nil
}

// Dial implements the same contract as ECHDialer.Dial — return a net.Conn
// for the upstream address routed through this WARP tunnel.
func (d *WARPDialer) Dial(addr string) (net.Conn, error) {
	return d.DialContext(context.Background(), "tcp", addr)
}

// DialContext is the http.Transport-friendly form. It feeds straight into
// the netstack net which handles SYN/ACK over the MASQUE tunnel.
//
// Force IPv4 for TCP: gVisor netstack IPv6 flow can stall the TLS handshake
// under MASQUE MTU 1280 (observed with third-party CF Workers upstreams).
// Sticking to IPv4 keeps handshake bytes fragmented in a way the tunnel is
// verified to forward reliably.
func (d *WARPDialer) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	forcedNetwork := network
	if network == "tcp" {
		forcedNetwork = "tcp4"
	}
	start := time.Now()
	log.Infof("[warp] %s dial start network=%s addr=%s", d.name, forcedNetwork, addr)
	conn, err := d.tunNet.DialContext(ctx, forcedNetwork, addr)
	elapsed := time.Since(start)
	if err != nil {
		log.Warnf("[warp] %s dial FAIL network=%s addr=%s elapsed=%s err=%v", d.name, forcedNetwork, addr, elapsed, err)
		return nil, err
	}
	log.Infof("[warp] %s dial OK network=%s addr=%s elapsed=%s local=%s remote=%s", d.name, forcedNetwork, addr, elapsed, conn.LocalAddr(), conn.RemoteAddr())
	return conn, nil
}

// Close cancels the MaintainTunnel goroutine. The netstack TUN closes when
// MaintainTunnel returns.
func (d *WARPDialer) Close() {
	if d.cancel != nil {
		d.cancel()
	}
}

func decodeECDSAPriv(b64 string) (*ecdsa.PrivateKey, error) {
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return nil, fmt.Errorf("base64: %w", err)
	}
	key, err := x509.ParseECPrivateKey(raw)
	if err != nil {
		return nil, fmt.Errorf("parse ec private: %w", err)
	}
	return key, nil
}

func decodeECDSAPub(pemStr string) (*ecdsa.PublicKey, error) {
	block, _ := pem.Decode([]byte(pemStr))
	if block == nil {
		return nil, fmt.Errorf("pem decode failed")
	}
	pub, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse pkix: %w", err)
	}
	ec, ok := pub.(*ecdsa.PublicKey)
	if !ok {
		return nil, fmt.Errorf("not ecdsa public key")
	}
	return ec, nil
}
