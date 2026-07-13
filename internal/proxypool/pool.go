package proxypool

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
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
	dialer    *ECHDialer  // ECH WebSocket dialer; nil if entry is WARP or direct
	warp      *WARPDialer // WARP MASQUE dialer; nil if entry is ECH or direct
	transport *http.Transport
	healthy   atomic.Bool
}

// NewPool creates a connection pool with in-process ECH and/or WARP dialers.
// All entry types share the same weighted round-robin selector.
func NewPool(dialers []*ECHDialer, warpDialers []*WARPDialer, cfg config.ProxyPoolConfig) *Pool {
	p := &Pool{}

	weightECH := cfg.WeightECH
	if weightECH <= 0 {
		weightECH = 3
	}
	weightWARP := cfg.WeightWARP
	if weightWARP <= 0 {
		weightWARP = 2
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
			TLSClientConfig:     &tls.Config{},
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

	for _, w := range warpDialers {
		t := &http.Transport{
			DialContext: func(dialer *WARPDialer) func(ctx context.Context, network, addr string) (net.Conn, error) {
				return func(ctx context.Context, network, addr string) (net.Conn, error) {
					return dialer.DialContext(ctx, network, addr)
				}
			}(w),
			MaxIdleConns:        10,
			MaxIdleConnsPerHost: 5,
			IdleConnTimeout:     90 * time.Second,
			TLSHandshakeTimeout: 10 * time.Second,
			TLSClientConfig:     &tls.Config{},
		}
		e := &entry{
			name:      w.name,
			warp:      w,
			transport: t,
		}
		e.healthy.Store(true)
		p.entries = append(p.entries, e)
		p.weights = append(p.weights, weightWARP)
		p.total += weightWARP
	}

	if cfg.IncludeDirect {
		t := &http.Transport{
			MaxIdleConns:        10,
			MaxIdleConnsPerHost: 5,
			IdleConnTimeout:     90 * time.Second,
			TLSClientConfig:     &tls.Config{},
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
	var targetSideErr error        // remember the first target-side failure
	targetSideEntries := 0         // how many distinct workers agreed the target is bad
	const targetSideRetryLimit = 2 // fail fast after this many workers agree
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
			// Distinguish target-side vs worker-side failure. If the upstream
			// worker rejected because the *target* is bad (NXDOMAIN, refused,
			// blacklisted HTTP target, etc.), the worker itself is healthy —
			// marking it unhealthy would cascade into all workers being killed
			// by a single bad upstream domain, breaking every provider that
			// shares the pool. See #woyaochat storm (2026-07-04).
			if isTargetSideDialError(err) {
				// Two independent workers agreeing the target is bad is enough
				// to declare it dead; try N different workers in case one has a
				// stale/split-DNS view of the target host.
				targetSideEntries++
				if targetSideErr == nil {
					targetSideErr = err
				}
				log.Warnf("[proxypool] %s dial failed (target-side, worker kept, %d/%d): %v",
					e.name, targetSideEntries, targetSideRetryLimit, err)
				if targetSideEntries >= targetSideRetryLimit {
					return nil, targetSideErr
				}
				continue
			}
			log.Warnf("[proxypool] %s dial failed: %v", e.name, err)
			e.healthy.Store(false)
			continue
		}
		return conn, nil
	}
	if targetSideErr != nil {
		return nil, targetSideErr
	}
	return nil, net.ErrClosed
}

// isTargetSideDialError returns true when the dial failure is attributable to
// the target address (NXDOMAIN, connection refused, HTTP-scheme target rejected
// by the proxy, etc.) rather than the tunnel worker being unhealthy. In these
// cases we must NOT mark the worker unhealthy — the worker is fine.
//
// Prefers structured error checks (errors.As/Is) over substring matching, so
// wording differences across Go versions / OSes (Linux/Windows/macOS) don't
// silently regress the classification.
func isTargetSideDialError(err error) bool {
	if err == nil {
		return false
	}
	// Structured: DNS "no such host" (NXDOMAIN and friends).
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) && dnsErr.IsNotFound {
		return true
	}
	// Structured: kernel-level refused/unreachable.
	if errors.Is(err, syscall.ECONNREFUSED) ||
		errors.Is(err, syscall.EHOSTUNREACH) ||
		errors.Is(err, syscall.ENETUNREACH) {
		return true
	}
	// String fallback for backends that wrap errors without preserving the
	// structured form (Cloudflare Workers HTTP rejection, non-Go proxies, etc.).
	msg := strings.ToLower(err.Error())
	// Cloudflare Workers reject non-HTTPS / non-fetchable targets with:
	//   "proxy request failed, cannot connect to the specified address"
	//   "It looks like you might be trying to connect to a HTTP-based service"
	if strings.Contains(msg, "cannot connect to the specified address") ||
		strings.Contains(msg, "http-based service") ||
		strings.Contains(msg, "consider using fetch") {
		return true
	}
	// Localization-independent DNS/kernel wording fallback.
	if strings.Contains(msg, "no such host") ||
		strings.Contains(msg, "nxdomain") ||
		strings.Contains(msg, "connection refused") ||
		strings.Contains(msg, "actively refused") || // Windows phrasing
		strings.Contains(msg, "host unreachable") ||
		strings.Contains(msg, "network unreachable") {
		return true
	}
	return false
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
