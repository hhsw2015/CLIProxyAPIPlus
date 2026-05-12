package proxypool

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	log "github.com/sirupsen/logrus"
	"golang.org/x/net/proxy"
)

// Pool manages persistent HTTP transports for each ECH worker,
// selecting them via weighted round-robin.
type Pool struct {
	entries []entry
	weights []int
	total   int
	counter atomic.Uint64
}

type entry struct {
	name      string
	addr      string
	proxyURL  string
	transport *http.Transport
	healthy   atomic.Bool
}

// NewPool creates a connection pool with persistent transports per worker.
func NewPool(workerAddrs []string, cfg config.ProxyPoolConfig) *Pool {
	p := &Pool{}

	weightECH := cfg.WeightECH
	if weightECH <= 0 {
		weightECH = 3
	}
	weightDirect := cfg.WeightDirect
	if weightDirect <= 0 {
		weightDirect = 1
	}

	for i, addr := range workerAddrs {
		name := fmt.Sprintf("ech-%d", i+1)
		if i < len(cfg.Workers) {
			name = cfg.Workers[i].Name
		}

		t := buildSOCKS5Transport(addr)
		e := entry{
			name:      name,
			addr:      addr,
			proxyURL:  fmt.Sprintf("socks5://127.0.0.1:%s", portFromAddr(addr)),
			transport: t,
		}
		e.healthy.Store(true)
		p.entries = append(p.entries, e)
		p.weights = append(p.weights, weightECH)
		p.total += weightECH
	}

	if cfg.IncludeDirect {
		t := &http.Transport{
			MaxIdleConns:        10,
			MaxIdleConnsPerHost: 5,
			IdleConnTimeout:     90 * time.Second,
			TLSClientConfig:    &tls.Config{},
		}
		e := entry{
			name:      "direct",
			addr:      "",
			proxyURL:  "",
			transport: t,
		}
		e.healthy.Store(true)
		p.entries = append(p.entries, e)
		p.weights = append(p.weights, weightDirect)
		p.total += weightDirect
	}

	return p
}

// NextTransport returns the next transport via weighted round-robin.
// Returns nil if no healthy entries available.
func (p *Pool) NextTransport() *http.Transport {
	if len(p.entries) == 0 {
		return nil
	}

	start := p.counter.Add(1)
	for attempts := 0; attempts < len(p.entries)*2; attempts++ {
		idx := p.weightedIndex(start + uint64(attempts))
		if p.entries[idx].healthy.Load() {
			return p.entries[idx].transport
		}
	}

	// All unhealthy, return first entry anyway
	return p.entries[0].transport
}

// NextProxyURL returns the SOCKS5 URL of the next worker (for utls client).
// Returns "" if pool is empty or the selected entry is direct.
func (p *Pool) NextProxyURL() string {
	if len(p.entries) == 0 {
		return ""
	}

	start := p.counter.Add(1)
	for attempts := 0; attempts < len(p.entries)*2; attempts++ {
		idx := p.weightedIndex(start + uint64(attempts))
		if p.entries[idx].healthy.Load() && p.entries[idx].proxyURL != "" {
			return p.entries[idx].proxyURL
		}
	}
	return ""
}

// StartHealthCheck runs periodic health checks on all workers.
func (p *Pool) StartHealthCheck(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				p.checkAll()
			}
		}
	}()
}

func (p *Pool) checkAll() {
	for i := range p.entries {
		if p.entries[i].addr == "" {
			continue // direct entry, always healthy
		}
		conn, err := net.DialTimeout("tcp", p.entries[i].addr, 3*time.Second)
		if err != nil {
			if p.entries[i].healthy.Load() {
				log.Warnf("[proxypool] %s unhealthy: %v", p.entries[i].name, err)
			}
			p.entries[i].healthy.Store(false)
		} else {
			conn.Close()
			if !p.entries[i].healthy.Load() {
				log.Infof("[proxypool] %s recovered", p.entries[i].name)
			}
			p.entries[i].healthy.Store(true)
		}
	}
}

func (p *Pool) weightedIndex(n uint64) int {
	if p.total == 0 {
		return int(n % uint64(len(p.entries)))
	}
	pos := int(n % uint64(p.total))
	for i, w := range p.weights {
		pos -= w
		if pos < 0 {
			return i
		}
	}
	return len(p.entries) - 1
}

func buildSOCKS5Transport(addr string) *http.Transport {
	dialer, err := proxy.SOCKS5("tcp", addr, nil, proxy.Direct)
	if err != nil {
		log.Errorf("[proxypool] failed to create SOCKS5 dialer for %s: %v", addr, err)
		return &http.Transport{}
	}

	ctxDialer, ok := dialer.(proxy.ContextDialer)
	if !ok {
		log.Errorf("[proxypool] SOCKS5 dialer for %s does not support ContextDialer", addr)
		return &http.Transport{}
	}

	return &http.Transport{
		DialContext:         ctxDialer.DialContext,
		MaxIdleConns:        10,
		MaxIdleConnsPerHost: 5,
		IdleConnTimeout:     90 * time.Second,
		TLSClientConfig:    &tls.Config{},
		TLSHandshakeTimeout: 10 * time.Second,
	}
}

func portFromAddr(addr string) string {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	return port
}
