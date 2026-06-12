package proxypool

import (
	"context"
	"net"
	"net/http"
	"sync"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	log "github.com/sirupsen/logrus"
)

var (
	mu          sync.RWMutex
	globalPool  *Pool
	initialized bool
)

// Init creates in-process ECH and WARP dialers and the connection pool.
// At least one ECH worker OR one WARP instance must be configured.
func Init(ctx context.Context, cfg config.ProxyPoolConfig) error {
	if !cfg.Enabled {
		return nil
	}
	if len(cfg.Workers) == 0 && len(cfg.WARPInstances) == 0 {
		return nil
	}

	var dialers []*ECHDialer
	for _, w := range cfg.Workers {
		d, err := NewECHDialer(w.Name, w.Domain, w.IP, w.Token)
		if err != nil {
			log.Warnf("[proxypool] failed to init %s: %v", w.Name, err)
			continue
		}
		dialers = append(dialers, d)
		log.Infof("[proxypool] initialized %s (domain=%s)", w.Name, w.Domain)
	}

	var warpDialers []*WARPDialer
	for _, inst := range cfg.WARPInstances {
		d, err := NewWARPDialer(ctx, inst)
		if err != nil {
			log.Warnf("[proxypool] failed to init warp %s: %v", inst.Name, err)
			continue
		}
		warpDialers = append(warpDialers, d)
		log.Infof("[proxypool] initialized warp %s (endpoint=%s)", inst.Name, inst.EndpointV4)
	}

	if len(dialers) == 0 && len(warpDialers) == 0 {
		log.Warn("[proxypool] no dialers initialized")
		return nil
	}

	pool := NewPool(dialers, warpDialers, cfg)
	pool.StartHealthCheck(ctx)

	mu.Lock()
	globalPool = pool
	initialized = true
	mu.Unlock()

	log.Infof("[proxypool] ready with %d ECH + %d WARP dialers (zero IPC)", len(dialers), len(warpDialers))
	return nil
}

// GetTransport returns an http.Transport that routes through the next ECH tunnel.
func GetTransport() *http.Transport {
	mu.RLock()
	defer mu.RUnlock()
	if !initialized || globalPool == nil {
		return nil
	}
	return globalPool.NextTransport()
}


// GetDialContext returns a DialContext function for the pool (used by utls client).
func GetDialContext() func(ctx context.Context, network, addr string) (net.Conn, error) {
	mu.RLock()
	defer mu.RUnlock()
	if !initialized || globalPool == nil {
		return nil
	}
	return globalPool.DialContext
}

// GetProxyURL is kept for backward compat but returns "" since we no longer use SOCKS5.
// utls_client should use GetDialContext instead.
func GetProxyURL() string {
	return ""
}

// Stop cleans up the pool.
func Stop() {
	mu.Lock()
	defer mu.Unlock()
	globalPool = nil
	initialized = false
}
