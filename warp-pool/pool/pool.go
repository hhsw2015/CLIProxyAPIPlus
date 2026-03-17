package pool

import (
	"context"
	"fmt"
	"log"
	"net"
	"strings"
	"sync"
	"time"

	"warp-pool/config"
	"warp-pool/process"
)

// Pool manages multiple WARP processes
type Pool struct {
	mu sync.RWMutex

	cfg       *config.Config
	processes []*process.Process
	current   int // Current index for round-robin
	ctx       context.Context
	cancel    context.CancelFunc

	// Auto rotation
	rotationStop chan struct{}
}

// New creates a new WARP process pool
func New(cfg *config.Config) *Pool {
	processes := make([]*process.Process, cfg.PoolSize)

	// Determine bind address based on direct mode config
	bindAddr := "127.0.0.1"
	if cfg.Direct.Enabled && cfg.Direct.ExposeExternal {
		bindAddr = "0.0.0.0"
	}

	for i := 0; i < cfg.PoolSize; i++ {
		processes[i] = process.NewWithBind(
			i,
			cfg.WarpBin,
			cfg.DataDir,
			cfg.SocksBasePort+i,
			cfg.HTTPBasePort+i,
			bindAddr,
		)
	}

	return &Pool{
		cfg:       cfg,
		processes: processes,
		current:   0,
	}
}

// Start launches all WARP processes in the pool
func (p *Pool) Start(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.ctx, p.cancel = context.WithCancel(ctx)

	var wg sync.WaitGroup
	errCh := make(chan error, len(p.processes))

	for _, proc := range p.processes {
		wg.Add(1)
		go func(proc *process.Process) {
			defer wg.Done()
			if err := proc.Start(p.ctx); err != nil {
				errCh <- fmt.Errorf("failed to start process %d: %w", proc.ID(), err)
			}
		}(proc)
	}

	wg.Wait()
	close(errCh)

	// Collect errors
	var errs []error
	for err := range errCh {
		errs = append(errs, err)
	}

	if len(errs) > 0 {
		return fmt.Errorf("failed to start %d processes: %v", len(errs), errs)
	}

	return nil
}

// Stop terminates all WARP processes
func (p *Pool) Stop() error {
	p.mu.Lock()
	defer p.mu.Unlock()

	// Stop auto rotation
	if p.rotationStop != nil {
		close(p.rotationStop)
		p.rotationStop = nil
	}

	if p.cancel != nil {
		p.cancel()
	}

	var wg sync.WaitGroup
	for _, proc := range p.processes {
		wg.Add(1)
		go func(proc *process.Process) {
			defer wg.Done()
			_ = proc.Stop()
		}(proc)
	}
	wg.Wait()

	return nil
}

// Size returns the pool size
func (p *Pool) Size() int {
	return len(p.processes)
}

// Get returns a process by ID
func (p *Pool) Get(id int) *process.Process {
	p.mu.RLock()
	defer p.mu.RUnlock()

	if id < 0 || id >= len(p.processes) {
		return nil
	}
	return p.processes[id]
}

// All returns all processes
func (p *Pool) All() []*process.Process {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.processes
}

// Next returns the next healthy process using round-robin
func (p *Pool) Next() *process.Process {
	p.mu.Lock()
	defer p.mu.Unlock()

	// Try to find a running process starting from current index
	for i := 0; i < len(p.processes); i++ {
		idx := (p.current + i) % len(p.processes)
		proc := p.processes[idx]
		if proc.State() == process.StateRunning {
			p.current = (idx + 1) % len(p.processes)
			return proc
		}
	}

	// No running process found, return any process
	proc := p.processes[p.current]
	p.current = (p.current + 1) % len(p.processes)
	return proc
}

// Healthy returns all processes in running state
func (p *Pool) Healthy() []*process.Process {
	p.mu.RLock()
	defer p.mu.RUnlock()

	var healthy []*process.Process
	for _, proc := range p.processes {
		if proc.State() == process.StateRunning {
			healthy = append(healthy, proc)
		}
	}
	return healthy
}

// HealthyCount returns the number of healthy processes
func (p *Pool) HealthyCount() int {
	return len(p.Healthy())
}

// Stats returns pool statistics
func (p *Pool) Stats() Stats {
	p.mu.RLock()
	defer p.mu.RUnlock()

	stats := Stats{
		Total:     len(p.processes),
		Processes: make([]process.Info, len(p.processes)),
	}

	for i, proc := range p.processes {
		info := proc.Info()
		stats.Processes[i] = info

		switch proc.State() {
		case process.StateRunning:
			stats.Running++
		case process.StateStarting:
			stats.Starting++
		case process.StateError:
			stats.Error++
		case process.StateStopped:
			stats.Stopped++
		}
		stats.TotalRequests += info.RequestCnt
	}

	return stats
}

// Restart restarts a specific process
func (p *Pool) Restart(id int) error {
	proc := p.Get(id)
	if proc == nil {
		return fmt.Errorf("process %d not found", id)
	}
	return proc.Restart(p.ctx)
}

// RestartAll restarts all processes
func (p *Pool) RestartAll() error {
	p.mu.RLock()
	procs := p.processes
	p.mu.RUnlock()

	var wg sync.WaitGroup
	errCh := make(chan error, len(procs))

	for _, proc := range procs {
		wg.Add(1)
		go func(proc *process.Process) {
			defer wg.Done()
			if err := proc.Restart(p.ctx); err != nil {
				errCh <- fmt.Errorf("failed to restart process %d: %w", proc.ID(), err)
			}
		}(proc)
	}

	wg.Wait()
	close(errCh)

	var errs []error
	for err := range errCh {
		errs = append(errs, err)
	}

	if len(errs) > 0 {
		return fmt.Errorf("failed to restart %d processes", len(errs))
	}

	return nil
}

// WaitForHealthy waits until at least one process is healthy or timeout
func (p *Pool) WaitForHealthy(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if p.HealthyCount() > 0 {
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("timeout waiting for healthy process")
}

// StartAutoRotation starts the automatic IP rotation background task
func (p *Pool) StartAutoRotation() {
	if !p.cfg.Rotation.Enabled {
		return
	}

	p.rotationStop = make(chan struct{})
	go p.autoRotationLoop()
}

func (p *Pool) autoRotationLoop() {
	// Check every 10 seconds
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-p.rotationStop:
			return
		case <-ticker.C:
			p.checkAndRotate()
		}
	}
}

func (p *Pool) checkAndRotate() {
	threshold := int64(p.cfg.Rotation.RequestsThreshold)
	if threshold <= 0 {
		return
	}

	minInterval := time.Duration(p.cfg.Rotation.MinInterval) * time.Second
	onlyUsed := p.cfg.Rotation.OnlyUsed

	for _, proc := range p.All() {
		// Skip if not running
		if proc.State() != process.StateRunning {
			continue
		}

		// Check if should rotate based on request count
		sessionReqs := proc.SessionRequests()
		if sessionReqs < threshold {
			continue
		}

		// Check if only rotating used instances
		if onlyUsed && !proc.HasBeenUsed() {
			continue
		}

		// Check minimum interval since last restart
		info := proc.Info()
		if time.Since(info.StartedAt) < minInterval {
			continue
		}

		// Rotate this instance
		log.Printf("[pool] Auto-rotating process %d (requests: %d, threshold: %d)",
			proc.ID(), sessionReqs, threshold)

		go func(proc *process.Process) {
			if err := proc.Restart(p.ctx); err != nil {
				log.Printf("[pool] Failed to auto-rotate process %d: %v", proc.ID(), err)
			}
		}(proc)

		// Only rotate one at a time to maintain availability
		return
	}
}

// EnsureUniqueIPv4 ensures all processes have unique IPv4 addresses
// It will restart processes with duplicate IPs until they get unique ones
func (p *Pool) EnsureUniqueIPv4() error {
	if !p.cfg.UniqueIPv4.Enabled {
		return nil
	}

	maxRetries := p.cfg.UniqueIPv4.MaxRetries
	retryDelay := time.Duration(p.cfg.UniqueIPv4.RetryDelay) * time.Second

	log.Printf("[pool] Ensuring unique IPv4 addresses for %d instances...", len(p.processes))

	for attempt := 0; attempt < maxRetries; attempt++ {
		// Collect current IPv4s
		ipv4Map := make(map[string][]*process.Process)
		for _, proc := range p.processes {
			if proc.State() != process.StateRunning {
				continue
			}
			ip := proc.Info().IP
			ipv4 := extractIPv4(ip)
			if ipv4 != "" {
				ipv4Map[ipv4] = append(ipv4Map[ipv4], proc)
			}
		}

		// Check for duplicates
		var duplicates []*process.Process
		for ip, procs := range ipv4Map {
			if len(procs) > 1 {
				// Keep the first one, mark others as duplicates
				log.Printf("[pool] Found %d processes with same IPv4 %s", len(procs), ip)
				duplicates = append(duplicates, procs[1:]...)
			}
		}

		if len(duplicates) == 0 {
			// All unique!
			uniqueIPs := make([]string, 0, len(ipv4Map))
			for ip := range ipv4Map {
				uniqueIPs = append(uniqueIPs, ip)
			}
			log.Printf("[pool] All instances have unique IPv4: %v", uniqueIPs)
			return nil
		}

		// Restart duplicates one by one
		log.Printf("[pool] Attempt %d: Restarting %d processes with duplicate IPs...", attempt+1, len(duplicates))
		for _, proc := range duplicates {
			log.Printf("[pool] Restarting process %d to get new IP...", proc.ID())
			if err := proc.Restart(p.ctx); err != nil {
				log.Printf("[pool] Failed to restart process %d: %v", proc.ID(), err)
				continue
			}
			// Wait for it to become healthy
			time.Sleep(retryDelay)
		}

		// Wait a bit more for health checks to update IPs
		time.Sleep(5 * time.Second)
	}

	// Final check
	ipv4Map := make(map[string]int)
	for _, proc := range p.processes {
		if proc.State() != process.StateRunning {
			continue
		}
		ip := proc.Info().IP
		ipv4 := extractIPv4(ip)
		if ipv4 != "" {
			ipv4Map[ipv4]++
		}
	}

	uniqueCount := len(ipv4Map)
	log.Printf("[pool] Final result: %d unique IPv4 addresses", uniqueCount)

	if uniqueCount < len(p.processes) {
		return fmt.Errorf("could not get %d unique IPv4s, only got %d", len(p.processes), uniqueCount)
	}

	return nil
}

// extractIPv4 extracts IPv4 from an IP string (handles both IPv4 and IPv6)
func extractIPv4(ip string) string {
	if ip == "" {
		return ""
	}
	// Check if it's IPv4
	parsed := net.ParseIP(ip)
	if parsed == nil {
		return ""
	}
	if parsed.To4() != nil {
		return ip
	}
	// It's IPv6, not IPv4
	return ""
}

// isIPv4 checks if the string is an IPv4 address
func isIPv4(ip string) bool {
	return !strings.Contains(ip, ":")
}

// DirectEndpoints returns the list of direct connection endpoints
func (p *Pool) DirectEndpoints() []DirectEndpoint {
	p.mu.RLock()
	defer p.mu.RUnlock()

	endpoints := make([]DirectEndpoint, len(p.processes))
	for i, proc := range p.processes {
		info := proc.Info()
		endpoints[i] = DirectEndpoint{
			ID:          proc.ID(),
			SocksPort:   proc.SocksPort(),
			HTTPPort:    proc.HTTPPort(),
			IP:          info.IP,
			State:       info.StateStr,
			SessionReqs: info.SessionReqs,
		}
	}
	return endpoints
}

// Config returns the pool configuration
func (p *Pool) Config() *config.Config {
	return p.cfg
}

// Stats represents pool statistics
type Stats struct {
	Total         int            `json:"total"`
	Running       int            `json:"running"`
	Starting      int            `json:"starting"`
	Error         int            `json:"error"`
	Stopped       int            `json:"stopped"`
	TotalRequests int64          `json:"total_requests"`
	Processes     []process.Info `json:"processes"`
}

// DirectEndpoint represents a direct connection endpoint
type DirectEndpoint struct {
	ID          int    `json:"id"`
	SocksPort   int    `json:"socks_port"`
	HTTPPort    int    `json:"http_port"`
	IP          string `json:"ip,omitempty"`
	State       string `json:"state"`
	SessionReqs int64  `json:"session_requests"`
}
