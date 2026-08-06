package cliproxy

import (
	"fmt"
	"net/http"
	"strings"
	"sync"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/proxypool"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/proxyutil"
	log "github.com/sirupsen/logrus"
)

// defaultRoundTripperProvider returns a per-auth HTTP RoundTripper based on
// the Auth.ProxyURL value. It caches transports per proxy URL string.
type defaultRoundTripperProvider struct {
	mu    sync.RWMutex
	cache map[string]http.RoundTripper
}

func newDefaultRoundTripperProvider() *defaultRoundTripperProvider {
	return &defaultRoundTripperProvider{cache: make(map[string]http.RoundTripper)}
}

// warpUnavailableRoundTripper is a fail-closed transport for the WARP-only
// sentinel path.
type warpUnavailableRoundTripper struct{}

func (warpUnavailableRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, fmt.Errorf("warp-only egress requested but no WARP tunnels are available")
}

// RoundTripperFor implements coreauth.RoundTripperProvider.
func (p *defaultRoundTripperProvider) RoundTripperFor(auth *coreauth.Auth) http.RoundTripper {
	if auth == nil {
		return nil
	}
	proxyStr := strings.TrimSpace(auth.ProxyURL)
	if proxyStr == "" {
		return nil
	}
	// Sentinel "warp": route through WARP-only tunnels. Cached under a
	// distinct key so a real proxy URL never accidentally shares this
	// transport.
	if strings.EqualFold(proxyStr, "warp") {
		const key = "__warp__"
		p.mu.RLock()
		rt := p.cache[key]
		p.mu.RUnlock()
		if rt != nil {
			return rt
		}
		transport := proxypool.GetWARPTransport()
		if transport == nil {
			// Fail closed. Returning nil lets the caller fall back to a
			// default transport which would leak the host's public IP to
			// targets that specifically refuse non-WARP callers.
			log.Errorf("rtprovider: WARP-only requested for auth=%s but no WARP transport available; failing closed", auth.ID)
			return warpUnavailableRoundTripper{}
		}
		p.mu.Lock()
		p.cache[key] = transport
		p.mu.Unlock()
		return transport
	}
	p.mu.RLock()
	rt := p.cache[proxyStr]
	p.mu.RUnlock()
	if rt != nil {
		return rt
	}
	transport, _, errBuild := proxyutil.BuildHTTPTransport(proxyStr)
	if errBuild != nil {
		log.Errorf("%v", errBuild)
		return nil
	}
	if transport == nil {
		return nil
	}
	p.mu.Lock()
	p.cache[proxyStr] = transport
	p.mu.Unlock()
	return transport
}
