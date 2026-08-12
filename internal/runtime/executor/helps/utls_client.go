package helps

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	stdcrypttls "crypto/tls"
	tls "github.com/refraction-networking/utls"
	internalcache "github.com/router-for-me/CLIProxyAPI/v7/internal/cache"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/httpwire"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/proxypool"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/proxyutil"
	log "github.com/sirupsen/logrus"
	"golang.org/x/net/http2"
	"golang.org/x/net/proxy"
)

// warpUnavailableTransport is a fail-closed http.RoundTripper used when an
// auth explicitly requested WARP-only egress but no WARP tunnels are healthy.
// Falling through to a direct dial would leak the host's public IP to targets
// that specifically refuse non-WARP callers.
type warpUnavailableTransport struct{}

func (warpUnavailableTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, fmt.Errorf("warp-only egress requested but no WARP tunnels are available")
}

// mirageRustlsClientHelloSpec reproduces the ClientHello emitted by
// reqwest 0.13.4 + rustls 0.23.42 + ring — the exact TLS stack the mirage
// upstream expects. Emitted for mirage-uuid auths so the JA3 fingerprint
// matches an ordinary rust HTTP client.
//
// Derived from rustls source rather than a packet capture:
//   - Cipher suite order: rustls/crypto/ring/mod.rs ALL_CIPHER_SUITES
//   - KX groups: rustls/crypto/ring/mod.rs ALL_KX_GROUPS
//   - Signature schemes: rustls/crypto/ring/mod.rs ALL_SIGNATURE_SCHEMES
//   - Extension order: rustls/msgs/handshake.rs ClientExtensions field order
//   - Extension population: rustls/client/hs.rs
//
// The only randomness in this spec is the ClientRandom, key_share pubkey,
// and legacy_session_id — which are supposed to be fresh per handshake.
func mirageRustlsClientHelloSpec() *tls.ClientHelloSpec {
	return &tls.ClientHelloSpec{
		CipherSuites: []uint16{
			tls.TLS_AES_256_GCM_SHA384,
			tls.TLS_AES_128_GCM_SHA256,
			tls.TLS_CHACHA20_POLY1305_SHA256,
			tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,
			tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
			tls.TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305_SHA256,
			tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
			tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
			tls.TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305_SHA256,
		},
		CompressionMethods: []uint8{0},
		Extensions: []tls.TLSExtension{
			&tls.SNIExtension{},
			&tls.StatusRequestExtension{},
			&tls.SupportedCurvesExtension{Curves: []tls.CurveID{
				tls.X25519, tls.CurveP256, tls.CurveP384,
			}},
			&tls.SupportedPointsExtension{SupportedPoints: []byte{0}},
			&tls.SignatureAlgorithmsExtension{
				SupportedSignatureAlgorithms: []tls.SignatureScheme{
					tls.ECDSAWithP384AndSHA384,
					tls.ECDSAWithP256AndSHA256,
					tls.Ed25519,
					tls.PSSWithSHA512,
					tls.PSSWithSHA384,
					tls.PSSWithSHA256,
					tls.PKCS1WithSHA512,
					tls.PKCS1WithSHA384,
					tls.PKCS1WithSHA256,
				},
			},
			&tls.ALPNExtension{AlpnProtocols: []string{"h2", "http/1.1"}},
			&tls.ExtendedMasterSecretExtension{},
			&tls.SupportedVersionsExtension{Versions: []uint16{
				tls.VersionTLS13, tls.VersionTLS12,
			}},
			&tls.PSKKeyExchangeModesExtension{Modes: []uint8{tls.PskModeDHE}},
			&tls.KeyShareExtension{KeyShares: []tls.KeyShare{
				{Group: tls.X25519},
			}},
		},
	}
}

// newMirageRustlsRoundTripper returns a RoundTripper that dials the given
// upstream through the provided DialContext and performs a rustls-compatible
// ClientHello. Used for mirage auths that must present a reqwest/rustls JA3.
//
// ALPN routing: reqwest 0.13.4 offers ["h2", "http/1.1"] in that order and
// upgrades to HTTP/2 whenever the server accepts it. Cloudflare Workers
// always accept h2. We route the negotiated protocol at the transport
// level: h2 → http2.Transport, http/1.1 → net/http.Transport.
func newMirageRustlsRoundTripper(dialCtx func(ctx context.Context, network, addr string) (net.Conn, error)) http.RoundTripper {
	sessionCache := tls.NewLRUClientSessionCache(32)
	dialTLS := func(ctx context.Context, network, addr string) (net.Conn, string, error) {
		rawConn, err := dialCtx(ctx, network, addr)
		if err != nil {
			return nil, "", fmt.Errorf("mirage-rustls: dial upstream: %w", err)
		}
		host, _, errSplit := net.SplitHostPort(addr)
		if errSplit != nil {
			_ = rawConn.Close()
			return nil, "", fmt.Errorf("mirage-rustls: split addr: %w", errSplit)
		}
		cfg := &tls.Config{
			ServerName:         host,
			NextProtos:         []string{"h2", "http/1.1"},
			ClientSessionCache: sessionCache,
		}
		tlsConn := tls.UClient(rawConn, cfg, tls.HelloCustom)
		if errPreset := tlsConn.ApplyPreset(mirageRustlsClientHelloSpec()); errPreset != nil {
			_ = tlsConn.Close()
			return nil, "", fmt.Errorf("mirage-rustls: apply preset: %w", errPreset)
		}
		if errHandshake := tlsConn.HandshakeContext(ctx); errHandshake != nil {
			_ = tlsConn.Close()
			return nil, "", fmt.Errorf("mirage-rustls: handshake: %w", errHandshake)
		}
		return tlsConn, tlsConn.ConnectionState().NegotiatedProtocol, nil
	}

	h2 := &http2.Transport{
		AllowHTTP: false,
		DialTLSContext: func(ctx context.Context, network, addr string, _ *stdcrypttls.Config) (net.Conn, error) {
			conn, alpn, err := dialTLS(ctx, network, addr)
			if err != nil {
				return nil, err
			}
			if alpn != "h2" {
				_ = conn.Close()
				return nil, fmt.Errorf("mirage-rustls: expected ALPN h2, got %q", alpn)
			}
			return conn, nil
		},
	}
	h1 := &http.Transport{
		DialTLSContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			conn, _, err := dialTLS(ctx, network, addr)
			return conn, err
		},
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
	return &mirageAlpnRouter{h2: h2, h1: h1, dial: dialTLS}
}

// mirageAlpnRouter picks the right protocol transport per request based on
// the ALPN outcome of a probe handshake. Real requests reuse pooled conns
// inside each protocol transport; the probe conn is discarded (worth it
// once per host to avoid speaking the wrong protocol).
type mirageAlpnRouter struct {
	h2   *http2.Transport
	h1   *http.Transport
	dial func(ctx context.Context, network, addr string) (net.Conn, string, error)
	mu   sync.Mutex
	// alpnByHost caches the negotiated ALPN so we don't probe every request.
	alpnByHost map[string]string
}

func (m *mirageAlpnRouter) RoundTrip(req *http.Request) (*http.Response, error) {
	host := req.URL.Host
	if host == "" {
		host = req.Host
	}
	// Cloudflare Workers (*.workers.dev) always accept h2 and mirage's whole
	// premise is that the upstream is a Worker. Skip the probe handshake for
	// them: it costs a second TLS round-trip on cold start and pins the
	// first request into a slower path.
	hostname := host
	if h, _, err := net.SplitHostPort(host); err == nil {
		hostname = h
	}
	if strings.HasSuffix(strings.ToLower(hostname), ".workers.dev") {
		return m.h2.RoundTrip(req)
	}
	m.mu.Lock()
	if m.alpnByHost == nil {
		m.alpnByHost = map[string]string{}
	}
	alpn, cached := m.alpnByHost[host]
	m.mu.Unlock()
	if !cached {
		addr := host
		if _, _, errSplit := net.SplitHostPort(addr); errSplit != nil {
			addr = net.JoinHostPort(host, "443")
		}
		conn, negotiated, err := m.dial(req.Context(), "tcp", addr)
		if err != nil {
			return nil, err
		}
		_ = conn.Close()
		alpn = negotiated
		m.mu.Lock()
		m.alpnByHost[host] = alpn
		m.mu.Unlock()
	}
	if alpn == "h2" {
		return m.h2.RoundTrip(req)
	}
	return m.h1.RoundTrip(req)
}

// utlsRoundTripper implements http.RoundTripper using a Chrome fingerprint for
// providers that require a browser-like TLS and HTTP/2 transport. Each request
// gets a dedicated connection that is closed with the response body.
type utlsRoundTripper struct {
	dialer proxy.Dialer
}

type closeConnectionBody struct {
	io.ReadCloser
	closeConnection func() error
	once            sync.Once
	err             error
}

func (b *closeConnectionBody) Close() error {
	if b == nil {
		return nil
	}
	b.once.Do(func() {
		var errConnection error
		if b.closeConnection != nil {
			errConnection = b.closeConnection()
		}
		var errBody error
		if b.ReadCloser != nil {
			errBody = b.ReadCloser.Close()
		}
		b.err = errors.Join(errBody, errConnection)
	})
	return b.err
}

func newUtlsRoundTripper(proxyURL string) *utlsRoundTripper {
	var dialer proxy.Dialer = proxy.Direct
	if proxyURL != "" {
		proxyDialer, mode, errBuild := proxyutil.BuildDialer(proxyURL)
		if errBuild != nil {
			log.Errorf("utls: failed to configure proxy dialer for %q: %v", proxyutil.Redact(proxyURL), errBuild)
		} else if mode != proxyutil.ModeInherit && proxyDialer != nil {
			dialer = proxyDialer
		}
	}
	return &utlsRoundTripper{dialer: dialer}
}

func (t *utlsRoundTripper) createConnection(ctx context.Context, host, addr string) (*http2.ClientConn, error) {
	contextDialer, ok := t.dialer.(proxy.ContextDialer)
	if !ok {
		return nil, fmt.Errorf("utls: dialer does not support context cancellation")
	}
	conn, errDial := contextDialer.DialContext(ctx, "tcp", addr)
	if errDial != nil {
		return nil, fmt.Errorf("utls: dial upstream: %w", errDial)
	}

	tlsConfig := &tls.Config{ServerName: host}
	tlsConn := tls.UClient(conn, tlsConfig, tls.HelloChrome_Auto)

	if errHandshake := tlsConn.HandshakeContext(ctx); errHandshake != nil {
		if errors.Is(errHandshake, context.Canceled) || errors.Is(errHandshake, context.DeadlineExceeded) {
			return nil, fmt.Errorf("utls: TLS handshake: %w", errHandshake)
		}
		if errClose := conn.Close(); errClose != nil {
			return nil, fmt.Errorf("utls: TLS handshake: %w; close connection: %v", errHandshake, errClose)
		}
		return nil, fmt.Errorf("utls: TLS handshake: %w", errHandshake)
	}

	tr := &http2.Transport{}
	h2Conn, errClientConn := tr.NewClientConn(tlsConn)
	if errClientConn != nil {
		if errClose := tlsConn.Close(); errClose != nil {
			return nil, fmt.Errorf("utls: initialize HTTP/2 connection: %w; close TLS connection: %v", errClientConn, errClose)
		}
		return nil, fmt.Errorf("utls: initialize HTTP/2 connection: %w", errClientConn)
	}

	return h2Conn, nil
}

func (t *utlsRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	hostname := req.URL.Hostname()
	port := req.URL.Port()
	if port == "" {
		port = "443"
	}
	addr := net.JoinHostPort(hostname, port)

	h2Conn, err := t.createConnection(req.Context(), hostname, addr)
	if err != nil {
		return nil, err
	}

	resp, err := h2Conn.RoundTrip(req)
	if err != nil {
		if errClose := h2Conn.Close(); errClose != nil {
			log.Debugf("utls: close connection after round trip failure: %v", errClose)
		}
		return nil, err
	}
	if resp == nil {
		if errClose := h2Conn.Close(); errClose != nil {
			log.Debugf("utls: close connection after empty response: %v", errClose)
		}
		return nil, fmt.Errorf("utls: upstream returned an empty response")
	}
	if resp.Body == nil {
		resp.Body = http.NoBody
	}
	resp.Body = &closeConnectionBody{
		ReadCloser:      resp.Body,
		closeConnection: h2Conn.Close,
	}
	return resp, nil
}

// claudeCodeSessionCacheCapacity bounds the per-transport TLS session cache for
// the Anthropic inference plane.
const claudeCodeSessionCacheCapacity = 32

// newClaudeCodeTLSConfig builds the uTLS config for one inference-plane dial.
//
// OmitEmptyPsk keeps the pre_shared_key extension silent until a session is
// cached, so an unresumed ClientHello stays byte-identical to the captured
// native handshake. PreferSkipResumptionOnNilExtension turns uTLS's HelloCustom
// "resume without the matching extension" panic into a skipped resumption.
func newClaudeCodeTLSConfig(host string, sessionCache tls.ClientSessionCache) *tls.Config {
	return &tls.Config{
		ServerName:                         host,
		ClientSessionCache:                 sessionCache,
		OmitEmptyPsk:                       true,
		PreferSkipResumptionOnNilExtension: true,
	}
}

// claudeCodeTLSClientHelloSpec reproduces the deterministic Node/OpenSSL
// ClientHello emitted by Claude Code 2.1.220 on macOS arm64. Keep this spec in
// sync with a fresh native capture whenever the advertised Claude Code version
// changes.
func claudeCodeTLSClientHelloSpec() *tls.ClientHelloSpec {
	return &tls.ClientHelloSpec{
		CipherSuites: []uint16{
			tls.TLS_AES_128_GCM_SHA256,
			tls.TLS_AES_256_GCM_SHA384,
			tls.TLS_CHACHA20_POLY1305_SHA256,
			tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
			tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
			tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,
			tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
			tls.TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305_SHA256,
			tls.TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305_SHA256,
			tls.TLS_ECDHE_ECDSA_WITH_AES_128_CBC_SHA,
			tls.TLS_ECDHE_RSA_WITH_AES_128_CBC_SHA,
			tls.TLS_ECDHE_ECDSA_WITH_AES_256_CBC_SHA,
			tls.TLS_ECDHE_RSA_WITH_AES_256_CBC_SHA,
			tls.TLS_RSA_WITH_AES_128_GCM_SHA256,
			tls.TLS_RSA_WITH_AES_256_GCM_SHA384,
			tls.TLS_RSA_WITH_AES_128_CBC_SHA,
			tls.TLS_RSA_WITH_AES_256_CBC_SHA,
		},
		CompressionMethods: []uint8{0},
		Extensions: []tls.TLSExtension{
			&tls.SNIExtension{},
			&tls.ExtendedMasterSecretExtension{},
			&tls.RenegotiationInfoExtension{Renegotiation: tls.RenegotiateOnceAsClient},
			&tls.SupportedCurvesExtension{Curves: []tls.CurveID{tls.X25519, tls.CurveP256, tls.CurveP384}},
			&tls.SupportedPointsExtension{SupportedPoints: []byte{0}},
			&tls.SessionTicketExtension{},
			&tls.ALPNExtension{AlpnProtocols: []string{"http/1.1"}},
			&tls.StatusRequestExtension{},
			&tls.SignatureAlgorithmsExtension{SupportedSignatureAlgorithms: []tls.SignatureScheme{
				tls.ECDSAWithP256AndSHA256,
				tls.PSSWithSHA256,
				tls.PKCS1WithSHA256,
				tls.ECDSAWithP384AndSHA384,
				tls.PSSWithSHA384,
				tls.PKCS1WithSHA384,
				tls.PSSWithSHA512,
				tls.PKCS1WithSHA512,
				tls.PKCS1WithSHA1,
			}},
			&tls.SCTExtension{},
			&tls.KeyShareExtension{KeyShares: []tls.KeyShare{{Group: tls.X25519}}},
			&tls.PSKKeyExchangeModesExtension{Modes: []uint8{tls.PskModeDHE}},
			&tls.SupportedVersionsExtension{Versions: []uint16{tls.VersionTLS13, tls.VersionTLS12}},
			&tls.UtlsPaddingExtension{GetPaddingLen: tls.BoringPaddingStyle},
			// pre_shared_key MUST be the final extension (RFC 8446 4.2.11), after
			// padding. It contributes zero bytes until a cached session exists.
			&tls.UtlsPreSharedKeyExtension{},
		},
	}
}

const claudeCodeRoundTripperCacheCapacity = 64

var claudeCodeRoundTripperCache = internalcache.NewBoundedLRU[string, http.RoundTripper](
	claudeCodeRoundTripperCacheCapacity,
	func(_ string, roundTripper http.RoundTripper) {
		if transport, ok := roundTripper.(interface{ CloseIdleConnections() }); ok {
			transport.CloseIdleConnections()
		}
	},
)

var claudeCodeMessagesHeaderOrder = []string{
	"Accept",
	"Authorization",
	"Content-Type",
	"User-Agent",
	"X-Claude-Code-Session-Id",
	"X-Stainless-Arch",
	"X-Stainless-Lang",
	"X-Stainless-OS",
	"X-Stainless-Package-Version",
	"X-Stainless-Retry-Count",
	"X-Stainless-Runtime",
	"X-Stainless-Runtime-Version",
	"X-Stainless-Timeout",
	"anthropic-beta",
	"anthropic-dangerous-direct-browser-access",
	"anthropic-version",
	"x-app",
	"x-client-request-id",
	"Connection",
	"Host",
	"Accept-Encoding",
	"Content-Length",
}

var claudeCodeCountTokensHeaderOrder = []string{
	"Accept",
	"Authorization",
	"Content-Type",
	"User-Agent",
	"X-Claude-Code-Session-Id",
	"X-Stainless-Arch",
	"X-Stainless-Lang",
	"X-Stainless-OS",
	"X-Stainless-Package-Version",
	"X-Stainless-Retry-Count",
	"X-Stainless-Runtime",
	"X-Stainless-Runtime-Version",
	"anthropic-beta",
	"anthropic-dangerous-direct-browser-access",
	"anthropic-version",
	"x-app",
	"x-client-request-id",
	"Connection",
	"Host",
	"Accept-Encoding",
	"Content-Length",
}

func claudeCodeRequestHeaderOrder(_, requestTarget string) []string {
	if strings.HasPrefix(requestTarget, "/v1/messages/count_tokens") {
		return claudeCodeCountTokensHeaderOrder
	}
	return claudeCodeMessagesHeaderOrder
}

func cachedClaudeCodeRoundTripper(proxyURL string) http.RoundTripper {
	return claudeCodeRoundTripperCache.GetOrAdd(proxyURL, func() http.RoundTripper {
		return newClaudeCodeRoundTripper(proxyURL)
	})
}

func newClaudeCodeRoundTripper(proxyURL string) http.RoundTripper {
	// The cache is scoped to this round tripper, which is already keyed by proxy,
	// so resumption never crosses proxy boundaries.
	sessionCache := tls.NewLRUClientSessionCache(claudeCodeSessionCacheCapacity)
	var dialer proxy.Dialer = proxy.Direct
	if proxyURL != "" {
		proxyDialer, mode, errBuild := proxyutil.BuildDialer(proxyURL)
		if errBuild != nil {
			log.Errorf("claude tls: failed to configure proxy dialer for %q: %v", proxyutil.Redact(proxyURL), errBuild)
		} else if mode != proxyutil.ModeInherit && proxyDialer != nil {
			dialer = proxyDialer
		}
	}

	transport := &http.Transport{
		ForceAttemptHTTP2: false,
		DialTLSContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			var (
				conn net.Conn
				err  error
			)
			if contextDialer, ok := dialer.(proxy.ContextDialer); ok {
				conn, err = contextDialer.DialContext(ctx, network, addr)
			} else {
				conn, err = dialer.Dial(network, addr)
			}
			if err != nil {
				return nil, fmt.Errorf("claude tls: dial upstream: %w", err)
			}

			host, _, errSplit := net.SplitHostPort(addr)
			if errSplit != nil {
				if errClose := conn.Close(); errClose != nil {
					log.Debugf("claude tls: close failed connection: %v", errClose)
				}
				return nil, fmt.Errorf("claude tls: split upstream address: %w", errSplit)
			}
			tlsConn := tls.UClient(conn, newClaudeCodeTLSConfig(host, sessionCache), tls.HelloCustom)
			if errPreset := tlsConn.ApplyPreset(claudeCodeTLSClientHelloSpec()); errPreset != nil {
				if errClose := tlsConn.Close(); errClose != nil {
					log.Debugf("claude tls: close connection after preset failure: %v", errClose)
				}
				return nil, fmt.Errorf("claude tls: apply Claude Code ClientHello: %w", errPreset)
			}
			if errHandshake := tlsConn.HandshakeContext(ctx); errHandshake != nil {
				if errClose := tlsConn.Close(); errClose != nil {
					log.Debugf("claude tls: close connection after handshake failure: %v", errClose)
				}
				return nil, fmt.Errorf("claude tls: handshake upstream: %w", errHandshake)
			}
			return httpwire.NewOrderedRequestConn(tlsConn, claudeCodeRequestHeaderOrder), nil
		},
	}
	return transport
}

// fallbackRoundTripper uses provider-specific TLS fingerprints for protected
// HTTPS hosts and falls back to the standard transport for all other requests.
type fallbackRoundTripper struct {
	anthropic http.RoundTripper
	chrome    http.RoundTripper
	fallback  http.RoundTripper
}

func (f *fallbackRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	if IsAnthropicUpstreamURL(req.URL) {
		return f.anthropic.RoundTrip(req)
	}
	if req.URL.Scheme == "https" && strings.EqualFold(req.URL.Hostname(), "chatgpt.com") {
		return f.chrome.RoundTrip(req)
	}
	return f.fallback.RoundTrip(req)
}

// NewUtlsHTTPClient creates an HTTP client using provider-specific TLS
// fingerprints for protected hosts. It uses Claude Code's Node/OpenSSL profile
// for Anthropic and a Chrome profile for ChatGPT, with a standard-transport
// fallback for other hosts.
func NewUtlsHTTPClient(ctx context.Context, cfg *config.Config, auth *cliproxyauth.Auth, timeout time.Duration) *http.Client {
	var proxyURL string
	bypassPool := false
	warpOnly := false
	if auth != nil {
		proxyURL = strings.TrimSpace(auth.ProxyURL)
		// Sentinel "direct" forces a raw direct connection, skipping the ECH
		// pool. Needed for upstreams that reject CF-Worker-to-CF-Worker raw
		// TCP tunnels (e.g. workers.dev targets that only accept fetch()).
		if strings.EqualFold(proxyURL, "direct") {
			proxyURL = ""
			bypassPool = true
		} else if strings.EqualFold(proxyURL, "warp") {
			// Sentinel "warp" routes only through WARP tunnels (real WARP IPs),
			// skipping ECH entries whose Worker origin some upstreams refuse.
			proxyURL = ""
			warpOnly = true
		}
	}
	if warpOnly {
		// WARP tunnels give us the exit IP; the rustls-compatible round
		// tripper gives us the TLS fingerprint. Both are required to look
		// like an ordinary rust HTTP client end to end (IP + JA3).
		if dialCtx := proxypool.GetWARPDialContext(); dialCtx != nil {
			log.Infof("[utls_client] using WARP+rustls transport for auth=%s", auth.ID)
			client := &http.Client{Transport: newMirageRustlsRoundTripper(dialCtx)}
			if timeout > 0 {
				client.Timeout = timeout
			}
			return client
		}
		// WARP-only was explicitly requested but no WARP tunnels are up.
		// Return a client whose transport fails every request: silently
		// falling back to a direct dial would leak the host's public IP
		// to targets that specifically refuse non-WARP callers (e.g.
		// third-party Cloudflare Worker upstreams).
		log.Errorf("[utls_client] WARP-only requested but no WARP transport available for auth=%s; failing closed", auth.ID)
		client := &http.Client{Transport: warpUnavailableTransport{}}
		if timeout > 0 {
			client.Timeout = timeout
		}
		return client
	}
	if proxyURL == "" && !bypassPool {
		if dialCtx := proxypool.GetDialContext(); dialCtx != nil {
			// Pool provides in-process ECH tunnel dialer -- use it directly
			return buildUtlsClientWithDialer(dialCtx, timeout)
		}
	}
	if proxyURL == "" && !bypassPool && cfg != nil {
		proxyURL = strings.TrimSpace(cfg.ProxyURL)
	}

	var ctxRoundTripper http.RoundTripper
	if ctx != nil {
		ctxRoundTripper, _ = ctx.Value("cliproxy.roundtripper").(http.RoundTripper)
	}

	var chromeRT http.RoundTripper = newUtlsRoundTripper(proxyURL)
	var anthropicRT http.RoundTripper = cachedClaudeCodeRoundTripper(proxyURL)
	var standardTransport http.RoundTripper = http.DefaultTransport
	if proxyURL != "" {
		if transport := buildProxyTransport(proxyURL); transport != nil {
			standardTransport = transport
		}
	} else if ctxRoundTripper != nil {
		chromeRT = ctxRoundTripper
		anthropicRT = ctxRoundTripper
		standardTransport = ctxRoundTripper
	}

	client := &http.Client{
		Transport: &fallbackRoundTripper{
			anthropic: anthropicRT,
			chrome:    chromeRT,
			fallback:  standardTransport,
		},
	}
	if timeout > 0 {
		client.Timeout = timeout
	}
	return client
}

// buildUtlsClientWithDialer creates an HTTP client using the pool's ECH DialContext.
// The ECH tunnel provides the connection; TLS is handled within the tunnel.
func buildUtlsClientWithDialer(dialCtx func(ctx context.Context, network, addr string) (net.Conn, error), timeout time.Duration) *http.Client {
	// Opus-5 adaptive xhigh reasoning on a 194KB tool-heavy request routinely
	// spends 40-90s producing the first response header (non-stream :rawPredict
	// path). 30s here was too aggressive for that shape of traffic and made
	// vertex look "broken" while the request was actually still running. CC
	// client sets X-Stainless-Timeout: 300, so match that ceiling.
	transport := &http.Transport{
		DialContext:           dialCtx,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: 300 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
	client := &http.Client{Transport: transport}
	if timeout > 0 {
		client.Timeout = timeout
	}
	return client
}
