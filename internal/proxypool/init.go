package proxypool

import (
	"context"
	"net"
	"net/http"
	"sync"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	log "github.com/sirupsen/logrus"
)

var (
	mu          sync.RWMutex
	globalPool  *Pool
	initialized bool
)

// Init creates in-process ECH dialers and the connection pool.
func Init(ctx context.Context, cfg config.ProxyPoolConfig) error {
	if !cfg.Enabled || len(cfg.Workers) == 0 {
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

	if len(dialers) == 0 {
		log.Warn("[proxypool] no ECH dialers initialized")
		return nil
	}

	pool := NewPool(dialers, cfg)
	pool.StartHealthCheck(ctx)

	mu.Lock()
	globalPool = pool
	initialized = true
	mu.Unlock()

	log.Infof("[proxypool] ready with %d in-process ECH dialers (zero IPC)", len(dialers))

	// Warmup: pre-establish tunnels to common upstream hosts
	warmupHosts := []string{
		"bedrock-runtime.us-east-1.amazonaws.com:443",
		"bedrock-runtime.us-west-2.amazonaws.com:443",
		"bedrock-runtime.ap-northeast-1.amazonaws.com:443",
		"api.anthropic.com:443",
	}
	go func() {
		pool.Warmup(warmupHosts)
		log.Infof("[proxypool] warmup complete")
	}()

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

// GetTransportForHost returns a sticky transport for the given host.
// Same host always maps to same ECH worker → maximizes connection reuse.
func GetTransportForHost(host string) *http.Transport {
	mu.RLock()
	defer mu.RUnlock()
	if !initialized || globalPool == nil {
		return nil
	}
	return globalPool.TransportForHost(host)
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
