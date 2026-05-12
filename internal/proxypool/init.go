package proxypool

import (
	"context"
	"fmt"
	"net/http"
	"sync"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	log "github.com/sirupsen/logrus"
)

var (
	mu            sync.RWMutex
	globalManager *ECHManager
	globalPool    *Pool
	initialized   bool
)

// Init starts ECH workers and creates the connection pool.
// Safe to call even if cfg.Enabled is false (no-op).
// If ECHBin is "none" or empty and workers are already running externally,
// the pool connects to existing worker ports without starting new processes.
func Init(ctx context.Context, cfg config.ProxyPoolConfig) error {
	if !cfg.Enabled || len(cfg.Workers) == 0 {
		return nil
	}

	var addrs []string
	skipStart := cfg.ECHBin == "none" || cfg.ECHBin == ""

	if skipStart {
		// Workers managed externally (e.g. by warp-pool), just use their ports
		for _, w := range cfg.Workers {
			addrs = append(addrs, fmt.Sprintf("127.0.0.1:%d", w.Port))
		}
		log.Infof("[proxypool] using %d externally managed workers", len(addrs))
	} else {
		manager := NewECHManager(cfg)
		addrs = manager.Start(ctx)
		if len(addrs) == 0 {
			log.Warn("[proxypool] no ECH workers started successfully")
			return nil
		}
		mu.Lock()
		globalManager = manager
		mu.Unlock()
	}

	pool := NewPool(addrs, cfg)
	pool.StartHealthCheck(ctx)

	mu.Lock()
	globalPool = pool
	initialized = true
	mu.Unlock()

	log.Infof("[proxypool] initialized with %d workers", len(addrs))
	return nil
}

// GetTransport returns a transport from the pool via round-robin.
// Returns nil if pool is not initialized or disabled.
func GetTransport() *http.Transport {
	mu.RLock()
	defer mu.RUnlock()
	if !initialized || globalPool == nil {
		return nil
	}
	return globalPool.NextTransport()
}

// GetProxyURL returns a SOCKS5 proxy URL from the pool via round-robin.
// Used by utls client which needs a proxy URL string rather than a transport.
// Returns "" if pool is not initialized or disabled.
func GetProxyURL() string {
	mu.RLock()
	defer mu.RUnlock()
	if !initialized || globalPool == nil {
		return ""
	}
	return globalPool.NextProxyURL()
}

// Stop terminates all ECH workers and cleans up.
func Stop() {
	mu.Lock()
	defer mu.Unlock()
	if globalManager != nil {
		globalManager.Stop()
		globalManager = nil
	}
	globalPool = nil
	initialized = false
}
