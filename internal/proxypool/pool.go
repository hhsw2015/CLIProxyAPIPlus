package proxypool

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	log "github.com/sirupsen/logrus"
)

// Pool manages in-process ECH dialers with weighted round-robin.
// Each dialer corresponds to one Cloudflare Worker (= one exit IP).
// Connections go directly through ECH WebSocket tunnels -- no IPC, no SOCKS5.
type Pool struct {
	entries []*entry
	weights []int
	total   int
	counter atomic.Uint64
}

type entry struct {
	name      string
	dialer    *ECHDialer // nil for direct entry
	transport *http.Transport
	healthy   atomic.Bool
}

// NewPool creates a connection pool with in-process ECH dialers.
func NewPool(dialers []*ECHDialer, cfg config.ProxyPoolConfig) *Pool {
	p := &Pool{}

	weightECH := cfg.WeightECH
	if weightECH <= 0 {
		weightECH = 3
	}
	weightDirect := cfg.WeightDirect
	if weightDirect <= 0 {
		weightDirect = 1
	}

	for _, d := range dialers {
		t := &http.Transport{
			DialContext: func(dialer *ECHDialer) func(ctx context.Context, network, addr string) (net.Conn, error) {
				return func(ctx context.Context, network, addr string) (net.Conn, error) {
					return dialer.Dial(addr)
				}
			}(d),
			MaxIdleConns:        10,
			MaxIdleConnsPerHost: 5,
			IdleConnTimeout:     90 * time.Second,
			TLSHandshakeTimeout: 10 * time.Second,
			TLSClientConfig:    &tls.Config{},
		}
		e := &entry{
			name:      d.name,
			dialer:    d,
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
		e := &entry{
			name:      "direct",
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
	return p.entries[0].transport
}


// DialContext dials target through the next ECH tunnel (for utls client).
func (p *Pool) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	if len(p.entries) == 0 {
		return (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, network, addr)
	}
	start := p.counter.Add(1)
	for attempts := 0; attempts < len(p.entries)*2; attempts++ {
		idx := p.weightedIndex(start + uint64(attempts))
		e := p.entries[idx]
		if !e.healthy.Load() {
			continue
		}
		if e.dialer == nil {
			// direct entry
			return (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, network, addr)
		}
		conn, err := e.dialer.Dial(addr)
		if err != nil {
			log.Warnf("[proxypool] %s dial failed: %v", e.name, err)
			e.healthy.Store(false)
			continue
		}
		return conn, nil
	}
	return nil, net.ErrClosed
}

// StartHealthCheck runs periodic health checks.
func (p *Pool) StartHealthCheck(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(60 * time.Second)
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
	for _, e := range p.entries {
		if e.dialer == nil {
			continue
		}
		// Try refreshing ECH config as health check
		if err := e.dialer.refreshECH(); err != nil {
			if e.healthy.Load() {
				log.Warnf("[proxypool] %s unhealthy: %v", e.name, err)
			}
			e.healthy.Store(false)
		} else {
			if !e.healthy.Load() {
				log.Infof("[proxypool] %s recovered", e.name)
			}
			e.healthy.Store(true)
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
