package process

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

// State represents the current state of a WARP process
type State int

const (
	StateStopped State = iota
	StateStarting
	StateRunning
	StateError
)

func (s State) String() string {
	switch s {
	case StateStopped:
		return "stopped"
	case StateStarting:
		return "starting"
	case StateRunning:
		return "running"
	case StateError:
		return "error"
	default:
		return "unknown"
	}
}

// Info contains runtime information about a WARP instance
type Info struct {
	ID          int       `json:"id"`
	State       State     `json:"state"`
	StateStr    string    `json:"state_str"`
	SocksPort   int       `json:"socks_port"`
	HTTPPort    int       `json:"http_port"`
	PID         int       `json:"pid,omitempty"`
	IP          string    `json:"ip,omitempty"`
	StartedAt   time.Time `json:"started_at,omitempty"`
	LastCheck   time.Time `json:"last_check,omitempty"`
	LastUsed    time.Time `json:"last_used,omitempty"`
	ErrorMsg    string    `json:"error_msg,omitempty"`
	RequestCnt  int64     `json:"request_count"`
	SessionReqs int64     `json:"session_requests"` // Requests since last restart
}

// Process manages a single WARP instance
type Process struct {
	mu sync.RWMutex

	id        int
	warpBin   string
	dataDir   string
	socksPort int
	httpPort  int
	bindAddr  string // "127.0.0.1" or "0.0.0.0"

	state       State
	cmd         *exec.Cmd
	cancel      context.CancelFunc
	ip          string
	startedAt   time.Time
	lastCheck   time.Time
	lastUsed    time.Time
	errorMsg    string
	reqCount    int64
	sessionReqs int64 // Requests since last restart

	// Output channels for logging
	stdout chan string
	stderr chan string
}

// New creates a new WARP process manager
func New(id int, warpBin, dataDir string, socksPort, httpPort int) *Process {
	return &Process{
		id:        id,
		warpBin:   warpBin,
		dataDir:   filepath.Join(dataDir, fmt.Sprintf("warp-%d", id)),
		socksPort: socksPort,
		httpPort:  httpPort,
		bindAddr:  "127.0.0.1",
		state:     StateStopped,
		stdout:    make(chan string, 100),
		stderr:    make(chan string, 100),
	}
}

// NewWithBind creates a new WARP process manager with custom bind address
func NewWithBind(id int, warpBin, dataDir string, socksPort, httpPort int, bindAddr string) *Process {
	return &Process{
		id:        id,
		warpBin:   warpBin,
		dataDir:   filepath.Join(dataDir, fmt.Sprintf("warp-%d", id)),
		socksPort: socksPort,
		httpPort:  httpPort,
		bindAddr:  bindAddr,
		state:     StateStopped,
		stdout:    make(chan string, 100),
		stderr:    make(chan string, 100),
	}
}

// ID returns the process ID
func (p *Process) ID() int {
	return p.id
}

// SocksPort returns the SOCKS5 proxy port
func (p *Process) SocksPort() int {
	return p.socksPort
}

// HTTPPort returns the HTTP proxy port
func (p *Process) HTTPPort() int {
	return p.httpPort
}

// State returns the current state
func (p *Process) State() State {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.state
}

// Info returns current process information
func (p *Process) Info() Info {
	p.mu.RLock()
	defer p.mu.RUnlock()

	info := Info{
		ID:          p.id,
		State:       p.state,
		StateStr:    p.state.String(),
		SocksPort:   p.socksPort,
		HTTPPort:    p.httpPort,
		IP:          p.ip,
		StartedAt:   p.startedAt,
		LastCheck:   p.lastCheck,
		LastUsed:    p.lastUsed,
		ErrorMsg:    p.errorMsg,
		RequestCnt:  p.reqCount,
		SessionReqs: p.sessionReqs,
	}

	if p.cmd != nil && p.cmd.Process != nil {
		info.PID = p.cmd.Process.Pid
	}

	return info
}

// Start launches the WARP process
func (p *Process) Start(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.state == StateRunning || p.state == StateStarting {
		return fmt.Errorf("process %d already running or starting", p.id)
	}

	// Ensure data directory exists
	if err := os.MkdirAll(p.dataDir, 0755); err != nil {
		return fmt.Errorf("failed to create data dir: %w", err)
	}

	// Check if identity exists, if not generate one
	identityFile := filepath.Join(p.dataDir, "wgcf-identity.json")
	if _, err := os.Stat(identityFile); os.IsNotExist(err) {
		genCmd := exec.CommandContext(ctx, p.warpBin, "generate", "--data-dir", p.dataDir)
		if output, err := genCmd.CombinedOutput(); err != nil {
			return fmt.Errorf("failed to generate identity: %w: %s", err, string(output))
		}
	}

	// Create context for this process
	procCtx, cancel := context.WithCancel(ctx)
	p.cancel = cancel

	// Build command arguments for standalone WARP binary
	// Format: warp run --socks-addr ADDR:PORT --http-addr ADDR:PORT --data-dir DIR
	args := []string{
		"run",
		"--socks-addr", fmt.Sprintf("%s:%d", p.bindAddr, p.socksPort),
		"--http-addr", fmt.Sprintf("%s:%d", p.bindAddr, p.httpPort),
		"--data-dir", p.dataDir,
	}

	p.cmd = exec.CommandContext(procCtx, p.warpBin, args...)
	p.cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid: true,
	}

	// Reset session requests on start
	p.sessionReqs = 0

	// Capture stdout/stderr
	stdout, err := p.cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("failed to get stdout pipe: %w", err)
	}

	stderr, err := p.cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("failed to get stderr pipe: %w", err)
	}

	p.state = StateStarting
	p.errorMsg = ""

	if err := p.cmd.Start(); err != nil {
		p.state = StateError
		p.errorMsg = err.Error()
		return fmt.Errorf("failed to start warp: %w", err)
	}

	p.startedAt = time.Now()

	// Stream output in background
	go p.streamOutput(stdout, p.stdout)
	go p.streamOutput(stderr, p.stderr)

	// Monitor process in background
	go p.monitor(procCtx)

	return nil
}

// Stop terminates the WARP process
func (p *Process) Stop() error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.state == StateStopped {
		return nil
	}

	if p.cancel != nil {
		p.cancel()
	}

	if p.cmd != nil && p.cmd.Process != nil {
		// Send SIGTERM first
		if err := p.cmd.Process.Signal(syscall.SIGTERM); err != nil {
			// If SIGTERM fails, try SIGKILL
			_ = p.cmd.Process.Kill()
		}

		// Wait for process to exit (with timeout)
		done := make(chan error, 1)
		go func() {
			done <- p.cmd.Wait()
		}()

		select {
		case <-done:
		case <-time.After(5 * time.Second):
			_ = p.cmd.Process.Kill()
		}
	}

	p.state = StateStopped
	p.cmd = nil
	p.cancel = nil
	p.ip = ""

	return nil
}

// Restart stops and starts the process
func (p *Process) Restart(ctx context.Context) error {
	if err := p.Stop(); err != nil {
		return err
	}
	time.Sleep(500 * time.Millisecond)
	return p.Start(ctx)
}

// SetIP updates the detected IP address
func (p *Process) SetIP(ip string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ip = ip
	p.lastCheck = time.Now()
}

// SetState updates the process state
func (p *Process) SetState(state State) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.state = state
}

// SetError sets an error state with message
func (p *Process) SetError(msg string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.state = StateError
	p.errorMsg = msg
}

// IncrementRequests increments the request counter
func (p *Process) IncrementRequests() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.reqCount++
	p.sessionReqs++
	p.lastUsed = time.Now()
}

// SessionRequests returns the number of requests since last restart
func (p *Process) SessionRequests() int64 {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.sessionReqs
}

// LastUsed returns the time of last request
func (p *Process) LastUsed() time.Time {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.lastUsed
}

// HasBeenUsed returns true if the process has handled any requests since restart
func (p *Process) HasBeenUsed() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.sessionReqs > 0
}

// BindAddr returns the bind address
func (p *Process) BindAddr() string {
	return p.bindAddr
}

// Stdout returns the stdout channel
func (p *Process) Stdout() <-chan string {
	return p.stdout
}

// Stderr returns the stderr channel
func (p *Process) Stderr() <-chan string {
	return p.stderr
}

// DataDir returns the data directory path
func (p *Process) DataDir() string {
	return p.dataDir
}

// WarpBin returns the warp binary path
func (p *Process) WarpBin() string {
	return p.warpBin
}

// ApplyLicense applies a WARP+ license key
func (p *Process) ApplyLicense(ctx context.Context, license string) error {
	cmd := exec.CommandContext(ctx, p.warpBin, "update",
		"--name", fmt.Sprintf("pool-%d", p.id),
		"--license", license,
		"--data-dir", p.dataDir,
	)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("license update failed: %w: %s", err, string(output))
	}
	return nil
}

func (p *Process) streamOutput(pipe io.ReadCloser, ch chan<- string) {
	scanner := bufio.NewScanner(pipe)
	for scanner.Scan() {
		select {
		case ch <- scanner.Text():
		default:
			// Drop if channel is full
		}
	}
}

func (p *Process) monitor(ctx context.Context) {
	if p.cmd == nil {
		return
	}

	err := p.cmd.Wait()

	p.mu.Lock()
	defer p.mu.Unlock()

	select {
	case <-ctx.Done():
		// Normal shutdown
		p.state = StateStopped
	default:
		// Unexpected exit
		p.state = StateError
		if err != nil {
			p.errorMsg = fmt.Sprintf("process exited: %v", err)
		} else {
			p.errorMsg = "process exited unexpectedly"
		}
	}
}
