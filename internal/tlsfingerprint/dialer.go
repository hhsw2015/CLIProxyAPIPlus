// Package tlsfingerprint provides TLS fingerprint simulation for HTTP clients.
// It uses the utls library to create TLS connections that mimic browser/runtime clients.
package tlsfingerprint

import (
	"bufio"
	"context"
	"encoding/base64"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"

	utls "github.com/refraction-networking/utls"
	"golang.org/x/net/proxy"
)

// Profile contains TLS fingerprint configuration.
// All slice fields use string names for readability; they are resolved
// to uint16 codes internally. Empty slices fall back to built-in defaults.
type Profile struct {
	Name                string   // Profile name for identification
	CipherSuites        []string // e.g. "TLS_AES_128_GCM_SHA256"
	Curves              []string // e.g. "X25519", "P-256"
	PointFormats        []string // e.g. "uncompressed"
	EnableGREASE        bool
	SignatureAlgorithms []string // e.g. "ecdsa_secp256r1_sha256"
	ALPNProtocols       []string // e.g. "h2", "http/1.1"
	SupportedVersions   []string // e.g. "TLS 1.3", "TLS 1.2"
	KeyShareGroups      []string // e.g. "X25519"
	PSKModes            []string // e.g. "psk_dhe_ke"
	Extensions          []uint16 // Extension type IDs in order; empty uses default Node.js 24.x order
}

// Dialer creates TLS connections with custom fingerprints.
type Dialer struct {
	profile    *Profile
	baseDialer func(ctx context.Context, network, addr string) (net.Conn, error)
}

// HTTPProxyDialer creates TLS connections through HTTP/HTTPS proxies with custom fingerprints.
// It handles the CONNECT tunnel establishment before performing TLS handshake.
type HTTPProxyDialer struct {
	profile  *Profile
	proxyURL *url.URL
}

// SOCKS5ProxyDialer creates TLS connections through SOCKS5 proxies with custom fingerprints.
// It uses golang.org/x/net/proxy to establish the SOCKS5 tunnel.
type SOCKS5ProxyDialer struct {
	profile  *Profile
	proxyURL *url.URL
}

// ---------- name-to-code resolution maps ----------

var cipherSuiteMap = map[string]uint16{
	"TLS_AES_128_GCM_SHA256":                        0x1301,
	"TLS_AES_256_GCM_SHA384":                        0x1302,
	"TLS_CHACHA20_POLY1305_SHA256":                   0x1303,
	"TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256":       0xc02b,
	"TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256":         0xc02f,
	"TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384":       0xc02c,
	"TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384":         0xc030,
	"TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305_SHA256": 0xcca9,
	"TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305_SHA256":   0xcca8,
	"TLS_ECDHE_ECDSA_WITH_AES_128_CBC_SHA":          0xc009,
	"TLS_ECDHE_RSA_WITH_AES_128_CBC_SHA":            0xc013,
	"TLS_ECDHE_ECDSA_WITH_AES_256_CBC_SHA":          0xc00a,
	"TLS_ECDHE_RSA_WITH_AES_256_CBC_SHA":            0xc014,
	"TLS_RSA_WITH_AES_128_GCM_SHA256":               0x009c,
	"TLS_RSA_WITH_AES_256_GCM_SHA384":               0x009d,
	"TLS_RSA_WITH_AES_128_CBC_SHA":                   0x002f,
	"TLS_RSA_WITH_AES_256_CBC_SHA":                   0x0035,
}

var curveMap = map[string]utls.CurveID{
	"X25519": utls.X25519,
	"P-256":  utls.CurveP256,
	"P-384":  utls.CurveP384,
	"P-521":  utls.CurveP521,
}

var pointFormatMap = map[string]uint16{
	"uncompressed": 0,
}

var signatureAlgorithmMap = map[string]utls.SignatureScheme{
	"ecdsa_secp256r1_sha256": 0x0403,
	"ecdsa_secp384r1_sha384": 0x0503,
	"ecdsa_secp521r1_sha512": 0x0603,
	"rsa_pss_rsae_sha256":    0x0804,
	"rsa_pss_rsae_sha384":    0x0805,
	"rsa_pss_rsae_sha512":    0x0806,
	"rsa_pkcs1_sha256":       0x0401,
	"rsa_pkcs1_sha384":       0x0501,
	"rsa_pkcs1_sha512":       0x0601,
	"rsa_pkcs1_sha1":         0x0201,
}

var tlsVersionMap = map[string]uint16{
	"TLS 1.3": utls.VersionTLS13,
	"TLS 1.2": utls.VersionTLS12,
	"TLS 1.1": utls.VersionTLS11,
	"TLS 1.0": utls.VersionTLS10,
}

var pskModeMap = map[string]uint16{
	"psk_dhe_ke": uint16(utls.PskModeDHE),
}

// ---------- resolution helpers ----------

func resolveCipherSuites(names []string) []uint16 {
	out := make([]uint16, 0, len(names))
	for _, n := range names {
		if v, ok := cipherSuiteMap[n]; ok {
			out = append(out, v)
		}
	}
	return out
}

func resolveCurves(names []string) []utls.CurveID {
	out := make([]utls.CurveID, 0, len(names))
	for _, n := range names {
		if v, ok := curveMap[n]; ok {
			out = append(out, v)
		}
	}
	return out
}

func resolvePointFormats(names []string) []uint16 {
	out := make([]uint16, 0, len(names))
	for _, n := range names {
		if v, ok := pointFormatMap[n]; ok {
			out = append(out, v)
		}
	}
	return out
}

func resolveSignatureAlgorithms(names []string) []utls.SignatureScheme {
	out := make([]utls.SignatureScheme, 0, len(names))
	for _, n := range names {
		if v, ok := signatureAlgorithmMap[n]; ok {
			out = append(out, v)
		}
	}
	return out
}

func resolveTLSVersions(names []string) []uint16 {
	out := make([]uint16, 0, len(names))
	for _, n := range names {
		if v, ok := tlsVersionMap[n]; ok {
			out = append(out, v)
		}
	}
	return out
}

func resolveKeyShareGroups(names []string) []utls.CurveID {
	return resolveCurves(names) // same mapping
}

func resolvePSKModes(names []string) []uint16 {
	out := make([]uint16, 0, len(names))
	for _, n := range names {
		if v, ok := pskModeMap[n]; ok {
			out = append(out, v)
		}
	}
	return out
}

// ---------- defaults (Node.js 24.x captured fingerprint) ----------

var (
	defaultCipherSuites = []uint16{
		0x1301, // TLS_AES_128_GCM_SHA256
		0x1302, // TLS_AES_256_GCM_SHA384
		0x1303, // TLS_CHACHA20_POLY1305_SHA256
		0xc02b, // TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256
		0xc02f, // TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256
		0xc02c, // TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384
		0xc030, // TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384
		0xcca9, // TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305_SHA256
		0xcca8, // TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305_SHA256
		0xc009, // TLS_ECDHE_ECDSA_WITH_AES_128_CBC_SHA
		0xc013, // TLS_ECDHE_RSA_WITH_AES_128_CBC_SHA
		0xc00a, // TLS_ECDHE_ECDSA_WITH_AES_256_CBC_SHA
		0xc014, // TLS_ECDHE_RSA_WITH_AES_256_CBC_SHA
		0x009c, // TLS_RSA_WITH_AES_128_GCM_SHA256
		0x009d, // TLS_RSA_WITH_AES_256_GCM_SHA384
		0x002f, // TLS_RSA_WITH_AES_128_CBC_SHA
		0x0035, // TLS_RSA_WITH_AES_256_CBC_SHA
	}

	defaultCurves = []utls.CurveID{
		utls.X25519,
		utls.CurveP256,
		utls.CurveP384,
	}

	defaultPointFormats = []uint16{
		0, // uncompressed
	}

	defaultSignatureAlgorithms = []utls.SignatureScheme{
		0x0403, // ecdsa_secp256r1_sha256
		0x0804, // rsa_pss_rsae_sha256
		0x0401, // rsa_pkcs1_sha256
		0x0503, // ecdsa_secp384r1_sha384
		0x0805, // rsa_pss_rsae_sha384
		0x0501, // rsa_pkcs1_sha384
		0x0806, // rsa_pss_rsae_sha512
		0x0601, // rsa_pkcs1_sha512
		0x0201, // rsa_pkcs1_sha1
	}
)

// ---------- constructors ----------

// NewDialer creates a new TLS fingerprint dialer.
// baseDialer is used for TCP connection establishment (supports proxy scenarios).
// If baseDialer is nil, direct TCP dial is used.
func NewDialer(profile *Profile, baseDialer func(ctx context.Context, network, addr string) (net.Conn, error)) *Dialer {
	if baseDialer == nil {
		baseDialer = (&net.Dialer{}).DialContext
	}
	return &Dialer{profile: profile, baseDialer: baseDialer}
}

// NewHTTPProxyDialer creates a new TLS fingerprint dialer that works through HTTP/HTTPS proxies.
// It establishes a CONNECT tunnel before performing TLS handshake with custom fingerprint.
func NewHTTPProxyDialer(profile *Profile, proxyURL *url.URL) *HTTPProxyDialer {
	return &HTTPProxyDialer{profile: profile, proxyURL: proxyURL}
}

// NewSOCKS5ProxyDialer creates a new TLS fingerprint dialer that works through SOCKS5 proxies.
// It establishes a SOCKS5 tunnel before performing TLS handshake with custom fingerprint.
func NewSOCKS5ProxyDialer(profile *Profile, proxyURL *url.URL) *SOCKS5ProxyDialer {
	return &SOCKS5ProxyDialer{profile: profile, proxyURL: proxyURL}
}

// ---------- dial methods ----------

// DialTLSContext establishes a TLS connection through SOCKS5 proxy with the configured fingerprint.
func (d *SOCKS5ProxyDialer) DialTLSContext(ctx context.Context, network, addr string) (net.Conn, error) {
	slog.Debug("tls_fingerprint_socks5_connecting", "proxy", d.proxyURL.Host, "target", addr)

	var auth *proxy.Auth
	if d.proxyURL.User != nil {
		username := d.proxyURL.User.Username()
		password, _ := d.proxyURL.User.Password()
		auth = &proxy.Auth{
			User:     username,
			Password: password,
		}
	}

	proxyAddr := d.proxyURL.Host
	if d.proxyURL.Port() == "" {
		proxyAddr = net.JoinHostPort(d.proxyURL.Hostname(), "1080")
	}

	socksDialer, err := proxy.SOCKS5("tcp", proxyAddr, auth, proxy.Direct)
	if err != nil {
		slog.Debug("tls_fingerprint_socks5_dialer_failed", "error", err)
		return nil, fmt.Errorf("create SOCKS5 dialer: %w", err)
	}

	slog.Debug("tls_fingerprint_socks5_establishing_tunnel", "target", addr)
	conn, err := socksDialer.Dial("tcp", addr)
	if err != nil {
		slog.Debug("tls_fingerprint_socks5_connect_failed", "error", err)
		return nil, fmt.Errorf("SOCKS5 connect: %w", err)
	}
	slog.Debug("tls_fingerprint_socks5_tunnel_established")

	return performTLSHandshake(ctx, conn, d.profile, addr)
}

// DialTLSContext establishes a TLS connection through HTTP proxy with the configured fingerprint.
func (d *HTTPProxyDialer) DialTLSContext(ctx context.Context, network, addr string) (net.Conn, error) {
	slog.Debug("tls_fingerprint_http_proxy_connecting", "proxy", d.proxyURL.Host, "target", addr)

	var proxyAddr string
	if d.proxyURL.Port() != "" {
		proxyAddr = d.proxyURL.Host
	} else {
		if d.proxyURL.Scheme == "https" {
			proxyAddr = net.JoinHostPort(d.proxyURL.Hostname(), "443")
		} else {
			proxyAddr = net.JoinHostPort(d.proxyURL.Hostname(), "80")
		}
	}

	dialer := &net.Dialer{}
	conn, err := dialer.DialContext(ctx, "tcp", proxyAddr)
	if err != nil {
		slog.Debug("tls_fingerprint_http_proxy_connect_failed", "error", err)
		return nil, fmt.Errorf("connect to proxy: %w", err)
	}
	slog.Debug("tls_fingerprint_http_proxy_connected", "proxy_addr", proxyAddr)

	req := &http.Request{
		Method: "CONNECT",
		URL:    &url.URL{Opaque: addr},
		Host:   addr,
		Header: make(http.Header),
	}

	if d.proxyURL.User != nil {
		username := d.proxyURL.User.Username()
		password, _ := d.proxyURL.User.Password()
		authStr := base64.StdEncoding.EncodeToString([]byte(username + ":" + password))
		req.Header.Set("Proxy-Authorization", "Basic "+authStr)
	}

	slog.Debug("tls_fingerprint_http_proxy_sending_connect", "target", addr)
	if err := req.Write(conn); err != nil {
		_ = conn.Close()
		slog.Debug("tls_fingerprint_http_proxy_write_failed", "error", err)
		return nil, fmt.Errorf("write CONNECT request: %w", err)
	}

	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		_ = conn.Close()
		slog.Debug("tls_fingerprint_http_proxy_read_response_failed", "error", err)
		return nil, fmt.Errorf("read CONNECT response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		_ = conn.Close()
		slog.Debug("tls_fingerprint_http_proxy_connect_failed_status", "status_code", resp.StatusCode, "status", resp.Status)
		return nil, fmt.Errorf("proxy CONNECT failed: %s", resp.Status)
	}
	slog.Debug("tls_fingerprint_http_proxy_tunnel_established")

	return performTLSHandshake(ctx, conn, d.profile, addr)
}

// DialTLSContext establishes a TLS connection with the configured fingerprint.
// This method is designed to be used as http.Transport.DialTLSContext.
func (d *Dialer) DialTLSContext(ctx context.Context, network, addr string) (net.Conn, error) {
	slog.Debug("tls_fingerprint_dialing_tcp", "addr", addr)
	conn, err := d.baseDialer(ctx, network, addr)
	if err != nil {
		slog.Debug("tls_fingerprint_tcp_dial_failed", "error", err)
		return nil, err
	}
	slog.Debug("tls_fingerprint_tcp_connected", "addr", addr)

	return performTLSHandshake(ctx, conn, d.profile, addr)
}

// ---------- TLS handshake ----------

func performTLSHandshake(ctx context.Context, conn net.Conn, profile *Profile, addr string) (net.Conn, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}

	spec := buildClientHelloSpecFromProfile(profile)
	tlsConn := utls.UClient(conn, &utls.Config{ServerName: host}, utls.HelloCustom)

	if err := tlsConn.ApplyPreset(spec); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("apply TLS preset: %w", err)
	}

	if err := tlsConn.HandshakeContext(ctx); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("TLS handshake failed: %w", err)
	}

	state := tlsConn.ConnectionState()
	slog.Debug("tls_fingerprint_handshake_success",
		"host", host,
		"version", state.Version,
		"cipher_suite", state.CipherSuite,
		"alpn", state.NegotiatedProtocol)

	return tlsConn, nil
}

// ---------- ClientHello spec builder ----------

var defaultExtensionOrder = []uint16{
	0,     // server_name
	65037, // encrypted_client_hello
	23,    // extended_master_secret
	65281, // renegotiation_info
	10,    // supported_groups
	11,    // ec_point_formats
	35,    // session_ticket
	16,    // alpn
	5,     // status_request
	13,    // signature_algorithms
	18,    // signed_certificate_timestamp
	51,    // key_share
	45,    // psk_key_exchange_modes
	43,    // supported_versions
}

func isGREASEValue(v uint16) bool {
	return v&0x0f0f == 0x0a0a && v>>8 == v&0xff
}

func buildClientHelloSpecFromProfile(profile *Profile) *utls.ClientHelloSpec {
	cipherSuites := defaultCipherSuites
	if profile != nil && len(profile.CipherSuites) > 0 {
		if resolved := resolveCipherSuites(profile.CipherSuites); len(resolved) > 0 {
			cipherSuites = resolved
		}
	}

	curves := defaultCurves
	if profile != nil && len(profile.Curves) > 0 {
		if resolved := resolveCurves(profile.Curves); len(resolved) > 0 {
			curves = resolved
		}
	}

	pointFormats := defaultPointFormats
	if profile != nil && len(profile.PointFormats) > 0 {
		if resolved := resolvePointFormats(profile.PointFormats); len(resolved) > 0 {
			pointFormats = resolved
		}
	}

	signatureAlgorithms := defaultSignatureAlgorithms
	if profile != nil && len(profile.SignatureAlgorithms) > 0 {
		if resolved := resolveSignatureAlgorithms(profile.SignatureAlgorithms); len(resolved) > 0 {
			signatureAlgorithms = resolved
		}
	}

	alpnProtocols := []string{"http/1.1"}
	if profile != nil && len(profile.ALPNProtocols) > 0 {
		alpnProtocols = profile.ALPNProtocols
	}

	supportedVersions := []uint16{utls.VersionTLS13, utls.VersionTLS12}
	if profile != nil && len(profile.SupportedVersions) > 0 {
		if resolved := resolveTLSVersions(profile.SupportedVersions); len(resolved) > 0 {
			supportedVersions = resolved
		}
	}

	keyShareGroups := []utls.CurveID{utls.X25519}
	if profile != nil && len(profile.KeyShareGroups) > 0 {
		if resolved := resolveKeyShareGroups(profile.KeyShareGroups); len(resolved) > 0 {
			keyShareGroups = resolved
		}
	}

	pskModes := []uint16{uint16(utls.PskModeDHE)}
	if profile != nil && len(profile.PSKModes) > 0 {
		if resolved := resolvePSKModes(profile.PSKModes); len(resolved) > 0 {
			pskModes = resolved
		}
	}

	enableGREASE := profile != nil && profile.EnableGREASE

	keyShares := make([]utls.KeyShare, len(keyShareGroups))
	for i, g := range keyShareGroups {
		keyShares[i] = utls.KeyShare{Group: g}
	}

	extOrder := defaultExtensionOrder
	if profile != nil && len(profile.Extensions) > 0 {
		extOrder = profile.Extensions
	}

	extensions := make([]utls.TLSExtension, 0, len(extOrder)+2)
	for _, id := range extOrder {
		if isGREASEValue(id) {
			extensions = append(extensions, &utls.UtlsGREASEExtension{})
			continue
		}
		switch id {
		case 0:
			extensions = append(extensions, &utls.SNIExtension{})
		case 5:
			extensions = append(extensions, &utls.StatusRequestExtension{})
		case 10:
			extensions = append(extensions, &utls.SupportedCurvesExtension{Curves: curves})
		case 11:
			extensions = append(extensions, &utls.SupportedPointsExtension{SupportedPoints: toUint8s(pointFormats)})
		case 13:
			extensions = append(extensions, &utls.SignatureAlgorithmsExtension{SupportedSignatureAlgorithms: signatureAlgorithms})
		case 16:
			extensions = append(extensions, &utls.ALPNExtension{AlpnProtocols: alpnProtocols})
		case 18:
			extensions = append(extensions, &utls.SCTExtension{})
		case 23:
			extensions = append(extensions, &utls.ExtendedMasterSecretExtension{})
		case 35:
			extensions = append(extensions, &utls.SessionTicketExtension{})
		case 43:
			extensions = append(extensions, &utls.SupportedVersionsExtension{Versions: supportedVersions})
		case 45:
			extensions = append(extensions, &utls.PSKKeyExchangeModesExtension{Modes: toUint8s(pskModes)})
		case 50:
			extensions = append(extensions, &utls.SignatureAlgorithmsCertExtension{SupportedSignatureAlgorithms: signatureAlgorithms})
		case 51:
			extensions = append(extensions, &utls.KeyShareExtension{KeyShares: keyShares})
		case 0xfe0d:
			extensions = append(extensions, &utls.GREASEEncryptedClientHelloExtension{})
		case 0xff01:
			extensions = append(extensions, &utls.RenegotiationInfoExtension{})
		default:
			extensions = append(extensions, &utls.GenericExtension{Id: id})
		}
	}

	if enableGREASE && (profile == nil || len(profile.Extensions) == 0) {
		extensions = append([]utls.TLSExtension{&utls.UtlsGREASEExtension{}}, extensions...)
		extensions = append(extensions, &utls.UtlsGREASEExtension{})
	}

	return &utls.ClientHelloSpec{
		CipherSuites:       cipherSuites,
		CompressionMethods: []uint8{0},
		Extensions:         extensions,
		TLSVersMax:         utls.VersionTLS13,
		TLSVersMin:         utls.VersionTLS10,
	}
}

func toUint8s(vals []uint16) []uint8 {
	out := make([]uint8, len(vals))
	for i, v := range vals {
		out[i] = uint8(v)
	}
	return out
}
