package ech

import (
	"context"
	"fmt"
	"log"
	"os/exec"
	"sync"
	"time"

	"warp-pool/config"
	"warp-pool/proxy"
)

// Manager starts and supervises ech-workers child processes.
type Manager struct {
	cfg     config.ECHWorkersConfig
	procs   []*worker
	mu      sync.Mutex
	cancel  context.CancelFunc
}

type worker struct {
	cfg    config.ECHWorkerConfig
	binPath string
	cmd    *exec.Cmd
	cancel context.CancelFunc
}

// New creates a new ech-workers manager.
func New(cfg config.ECHWorkersConfig) *Manager {
	return &Manager{cfg: cfg}
}

// Start launches all configured ech-workers and returns their addresses
// as ExtraBackend entries for the proxy route table.
func (m *Manager) Start(ctx context.Context) []proxy.ExtraBackend {
	if !m.cfg.Enabled || len(m.cfg.Workers) == 0 {
		return nil
	}

	binPath := m.cfg.BinPath
	if binPath == "" {
		binPath = "./ech-workers"
	}

	var backends []proxy.ExtraBackend
	parentCtx, cancel := context.WithCancel(ctx)
	m.cancel = cancel

	for _, wCfg := range m.cfg.Workers {
		w := &worker{
			cfg:     wCfg,
			binPath: binPath,
		}
		if err := w.start(parentCtx); err != nil {
			log.Printf("[ech] Failed to start %s: %v", wCfg.Name, err)
			continue
		}
		m.mu.Lock()
		m.procs = append(m.procs, w)
		m.mu.Unlock()

		addr := fmt.Sprintf("127.0.0.1:%d", wCfg.Port)
		backends = append(backends, proxy.ExtraBackend{
			Name:       wCfg.Name,
			Addr:       addr,
			ECHManaged: true,
		})
		log.Printf("[ech] Started %s on %s (domain=%s)", wCfg.Name, addr, wCfg.Domain)
	}

	// Supervisor goroutine: restart crashed workers
	go m.supervise(parentCtx)

	return backends
}

// Stop terminates all managed ech-workers processes.
func (m *Manager) Stop() {
	if m.cancel != nil {
		m.cancel()
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, w := range m.procs {
		w.stop()
	}
	m.procs = nil
}

func (m *Manager) supervise(ctx context.Context) {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.mu.Lock()
			for _, w := range m.procs {
				if !w.isRunning() {
					log.Printf("[ech] %s exited, restarting...", w.cfg.Name)
					_ = w.start(ctx)
				}
			}
			m.mu.Unlock()
		}
	}
}

func (w *worker) start(ctx context.Context) error {
	procCtx, cancel := context.WithCancel(ctx)
	w.cancel = cancel

	args := []string{
		"-f", w.cfg.Domain,
		"-l", fmt.Sprintf("127.0.0.1:%d", w.cfg.Port),
		"-token", w.cfg.Token,
	}
	if w.cfg.IP != "" {
		args = append(args, "-ip", w.cfg.IP)
	}

	w.cmd = exec.CommandContext(procCtx, w.binPath, args...)
	if err := w.cmd.Start(); err != nil {
		cancel()
		return fmt.Errorf("exec %s: %w", w.binPath, err)
	}

	// Reap in background
	go func() {
		_ = w.cmd.Wait()
	}()

	return nil
}

func (w *worker) stop() {
	if w.cancel != nil {
		w.cancel()
	}
	if w.cmd != nil && w.cmd.Process != nil {
		_ = w.cmd.Process.Kill()
	}
}

func (w *worker) isRunning() bool {
	if w.cmd == nil || w.cmd.Process == nil {
		return false
	}
	// ProcessState is set after Wait() completes
	return w.cmd.ProcessState == nil
}
