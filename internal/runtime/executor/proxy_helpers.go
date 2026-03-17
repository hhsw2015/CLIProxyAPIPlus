package executor

import (
	"context"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/proxyutil"
	log "github.com/sirupsen/logrus"
)

// httpClientCache caches HTTP clients by proxy URL to enable connection reuse
var (
	httpClientCache      = make(map[string]*http.Client)
	httpClientCacheMutex sync.RWMutex
)

// newProxyAwareHTTPClient creates an HTTP client with proper proxy configuration priority:
// 1. Use auth.ProxyURL if configured (highest priority)
// 2. Use cfg.ProxyURL if auth proxy is not configured
// 3. Use RoundTripper from context if neither are configured
//
// When a proxy is configured, the client wraps the transport with automatic
// fallback to direct connection if the proxy is unreachable.
func newProxyAwareHTTPClient(ctx context.Context, cfg *config.Config, auth *cliproxyauth.Auth, timeout time.Duration) *http.Client {
	// Priority 1: Use auth.ProxyURL if configured
	var proxyURL string
	if auth != nil {
		proxyURL = strings.TrimSpace(auth.ProxyURL)
	}

	// Priority 2: Use cfg.ProxyURL if auth proxy is not configured
	if proxyURL == "" && cfg != nil {
		proxyURL = strings.TrimSpace(cfg.ProxyURL)
	}

	// Build cache key from proxy URL (empty string for no proxy)
	cacheKey := proxyURL

	// Check cache first
	httpClientCacheMutex.RLock()
	if cachedClient, ok := httpClientCache[cacheKey]; ok {
		httpClientCacheMutex.RUnlock()
		// Return a wrapper with the requested timeout but shared transport
		if timeout > 0 {
			return &http.Client{
				Transport: cachedClient.Transport,
				Timeout:   timeout,
			}
		}
		return cachedClient
	}
	httpClientCacheMutex.RUnlock()

	// Create new client
	httpClient := &http.Client{}
	if timeout > 0 {
		httpClient.Timeout = timeout
	}

	// If we have a proxy URL configured, set up the transport with fallback
	if proxyURL != "" {
		// "direct" / "none" = explicit bypass, no proxy and no fallback
		lower := strings.ToLower(proxyURL)
		if lower == "direct" || lower == "none" {
			httpClient.Transport = &http.Transport{}
			httpClientCacheMutex.Lock()
			httpClientCache[cacheKey] = httpClient
			httpClientCacheMutex.Unlock()
			return httpClient
		}

		transport := buildProxyTransport(proxyURL)
		if transport != nil {
			httpClient.Transport = &proxyFallbackTransport{
				proxy:  transport,
				direct: http.DefaultTransport,
			}
			// Cache the client
			httpClientCacheMutex.Lock()
			httpClientCache[cacheKey] = httpClient
			httpClientCacheMutex.Unlock()
			return httpClient
		}
		// If proxy setup failed, log and fall through to context RoundTripper
		log.Debugf("failed to setup proxy from URL: %s, falling back to context transport", proxyURL)
	}

	// Priority 3: Use RoundTripper from context (typically from RoundTripperFor)
	if rt, ok := ctx.Value("cliproxy.roundtripper").(http.RoundTripper); ok && rt != nil {
		httpClient.Transport = rt
	}

	// Cache the client for no-proxy case
	if proxyURL == "" {
		httpClientCacheMutex.Lock()
		httpClientCache[cacheKey] = httpClient
		httpClientCacheMutex.Unlock()
	}

	return httpClient
}

// proxyFallbackTransport wraps a proxy transport with automatic fallback
// to direct connection when the proxy itself is unreachable.
type proxyFallbackTransport struct {
	proxy  http.RoundTripper
	direct http.RoundTripper
}

func (t *proxyFallbackTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.proxy.RoundTrip(req)
	if err != nil && isProxyConnectionError(err) {
		log.Warnf("proxy unreachable, falling back to direct connection: %v", err)
		return t.direct.RoundTrip(req)
	}
	return resp, err
}

// isProxyConnectionError returns true if the error indicates the proxy itself
// is unreachable (not that the target behind the proxy is unreachable).
func isProxyConnectionError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	// Connection refused to the proxy port
	if strings.Contains(msg, "connection refused") {
		return true
	}
	// Proxy dial timeout / no route
	if strings.Contains(msg, "connect: connection timed out") {
		return true
	}
	if strings.Contains(msg, "no route to host") {
		return true
	}
	// SOCKS proxy specific errors
	if strings.Contains(msg, "socks connect") && strings.Contains(msg, "EOF") {
		return true
	}
	// Check for net.OpError targeting the proxy address (local port like 1080)
	var opErr *net.OpError
	if ok := errorAs(err, &opErr); ok {
		if opErr.Op == "dial" || opErr.Op == "connect" {
			if addr, ok := opErr.Addr.(*net.TCPAddr); ok {
				// Proxy ports are typically local (127.0.0.1)
				if addr.IP.IsLoopback() {
					return true
				}
			}
		}
	}
	return false
}

// errorAs is a helper to work around Go's errors.As with interface types.
func errorAs(err error, target interface{}) bool {
	if err == nil {
		return false
	}
	// Walk the error chain
	for {
		if t, ok := err.(*net.OpError); ok {
			if p, ok2 := target.(**net.OpError); ok2 {
				*p = t
				return true
			}
		}
		if u, ok := err.(interface{ Unwrap() error }); ok {
			err = u.Unwrap()
			if err == nil {
				return false
			}
		} else {
			return false
		}
	}
}

// buildProxyTransport creates an HTTP transport configured for the given proxy URL.
func buildProxyTransport(proxyURL string) *http.Transport {
	transport, _, errBuild := proxyutil.BuildHTTPTransport(proxyURL)
	if errBuild != nil {
		log.Errorf("%v", errBuild)
		return nil
	}
	return transport
}
