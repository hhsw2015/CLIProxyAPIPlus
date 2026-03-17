package health

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"warp-pool/pool"
	"warp-pool/process"
)

const (
	// IP check endpoints - IPv4 only API
	ipv4CheckURL = "https://api.ipify.org"
	// Fallback to Cloudflare trace
	ipCheckURL = "https://cloudflare.com/cdn-cgi/trace"
	// Connection timeout for health checks
	checkTimeout = 10 * time.Second
)

// Checker performs health checks on WARP processes
type Checker struct {
	pool     *pool.Pool
	interval time.Duration
	ctx      context.Context
	cancel   context.CancelFunc
	wg       sync.WaitGroup

	client *http.Client
}

// New creates a new health checker
func New(p *pool.Pool, interval time.Duration) *Checker {
	return &Checker{
		pool:     p,
		interval: interval,
		client: &http.Client{
			Timeout: checkTimeout,
		},
	}
}

// Start begins periodic health checks
func (c *Checker) Start(ctx context.Context) {
	c.ctx, c.cancel = context.WithCancel(ctx)

	c.wg.Add(1)
	go c.run()
}

// Stop stops the health checker
func (c *Checker) Stop() {
	if c.cancel != nil {
		c.cancel()
	}
	c.wg.Wait()
}

func (c *Checker) run() {
	defer c.wg.Done()

	// Initial check
	c.checkAll()

	ticker := time.NewTicker(c.interval)
	defer ticker.Stop()

	for {
		select {
		case <-c.ctx.Done():
			return
		case <-ticker.C:
			c.checkAll()
		}
	}
}

func (c *Checker) checkAll() {
	processes := c.pool.All()

	var wg sync.WaitGroup
	for _, proc := range processes {
		wg.Add(1)
		go func(proc *process.Process) {
			defer wg.Done()
			c.checkOne(proc)
		}(proc)
	}
	wg.Wait()
}

func (c *Checker) checkOne(proc *process.Process) {
	// Skip if not in a checkable state
	state := proc.State()
	if state == process.StateStopped {
		return
	}

	// Create a client that uses this process's SOCKS proxy
	proxyURL, err := url.Parse(fmt.Sprintf("socks5://127.0.0.1:%d", proc.SocksPort()))
	if err != nil {
		proc.SetError(fmt.Sprintf("invalid proxy URL: %v", err))
		return
	}

	client := &http.Client{
		Timeout: checkTimeout,
		Transport: &http.Transport{
			Proxy: http.ProxyURL(proxyURL),
		},
	}

	// Try IPv4 only endpoint first
	ip, err := c.getIPv4(client)
	if err != nil {
		// Fallback to Cloudflare trace
		ip, err = c.getIPFromTrace(client)
	}

	if err != nil {
		// Process might still be starting
		if state == process.StateStarting {
			return
		}
		proc.SetError(fmt.Sprintf("health check failed: %v", err))
		if shouldAutoRecoverHealthFailure(err.Error()) {
			info := proc.Info()
			if !info.StartedAt.IsZero() && time.Since(info.StartedAt) < 15*time.Second {
				return
			}
			log.Printf("[health] Process %d unhealthy, attempting restart: %v", proc.ID(), err)
			if restartErr := proc.Restart(c.ctx); restartErr != nil {
				log.Printf("[health] Process %d restart failed: %v", proc.ID(), restartErr)
			}
		}
		return
	}

	// Update process status
	proc.SetIP(ip)
	proc.SetState(process.StateRunning)

	// Check memory usage - restart if RSS exceeds configured limit
	limitMB := proc.MemoryLimitMB()
	if limitMB > 0 {
		rssBytes := proc.RSSBytes()
		rssMB := rssBytes / (1024 * 1024)
		if rssMB > int64(limitMB) {
			log.Printf("[health] Process %d memory %dMB exceeds limit %dMB, restarting", proc.ID(), rssMB, limitMB)
			if restartErr := proc.Restart(c.ctx); restartErr != nil {
				log.Printf("[health] Process %d memory-limit restart failed: %v", proc.ID(), restartErr)
			}
			return
		}
	}

	log.Printf("[health] Process %d: IP=%s", proc.ID(), ip)
}

func shouldAutoRecoverHealthFailure(msg string) bool {
	lower := strings.ToLower(strings.TrimSpace(msg))
	switch {
	case strings.Contains(lower, "connection refused"):
		return true
	case strings.Contains(lower, "connection reset by peer"):
		return true
	case strings.HasSuffix(lower, " eof"), strings.Contains(lower, ": eof"):
		return true
	default:
		return false
	}
}

// getIPv4 gets IPv4 address from ipify (IPv4 only)
func (c *Checker) getIPv4(client *http.Client) (string, error) {
	req, err := http.NewRequestWithContext(c.ctx, "GET", ipv4CheckURL, nil)
	if err != nil {
		return "", err
	}

	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	ip := strings.TrimSpace(string(body))
	if ip == "" {
		return "", fmt.Errorf("empty IP response")
	}

	return ip, nil
}

// getIPFromTrace gets IP from Cloudflare trace (may return IPv4 or IPv6)
func (c *Checker) getIPFromTrace(client *http.Client) (string, error) {
	req, err := http.NewRequestWithContext(c.ctx, "GET", ipCheckURL, nil)
	if err != nil {
		return "", err
	}

	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	ip := parseIP(string(body))
	if ip == "" {
		return "", fmt.Errorf("failed to parse IP from response")
	}

	return ip, nil
}

// CheckOne performs a single health check on a process
func (c *Checker) CheckOne(proc *process.Process) error {
	c.checkOne(proc)
	if proc.State() == process.StateError {
		return fmt.Errorf("health check failed")
	}
	return nil
}

// parseIP extracts IP from Cloudflare trace response
func parseIP(trace string) string {
	// Cloudflare trace format:
	// fl=xxx
	// h=cloudflare.com
	// ip=1.2.3.4
	// ...
	var ip string
	for _, line := range splitLines(trace) {
		if len(line) > 3 && line[:3] == "ip=" {
			ip = line[3:]
			break
		}
	}
	return ip
}

func splitLines(s string) []string {
	var lines []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			line := s[start:i]
			if len(line) > 0 && line[len(line)-1] == '\r' {
				line = line[:len(line)-1]
			}
			lines = append(lines, line)
			start = i + 1
		}
	}
	if start < len(s) {
		lines = append(lines, s[start:])
	}
	return lines
}

// IPInfo represents IP geolocation info
type IPInfo struct {
	IP      string `json:"ip"`
	Country string `json:"country"`
	City    string `json:"city"`
	ISP     string `json:"isp"`
}

// GetIPInfo fetches detailed IP information using ip-api.com
func (c *Checker) GetIPInfo(proc *process.Process) (*IPInfo, error) {
	proxyURL, err := url.Parse(fmt.Sprintf("socks5://127.0.0.1:%d", proc.SocksPort()))
	if err != nil {
		return nil, err
	}

	client := &http.Client{
		Timeout: checkTimeout,
		Transport: &http.Transport{
			Proxy: http.ProxyURL(proxyURL),
		},
	}

	req, err := http.NewRequestWithContext(c.ctx, "GET", "http://ip-api.com/json/", nil)
	if err != nil {
		return nil, err
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var info IPInfo
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return nil, err
	}

	return &info, nil
}
