package proxypool

import (
	"context"
	"fmt"
	"os/exec"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	log "github.com/sirupsen/logrus"
)

// ECHManager starts and supervises ech-workers child processes.
type ECHManager struct {
	cfg    config.ProxyPoolConfig
	procs  []*echWorker
	mu     sync.Mutex
	cancel context.CancelFunc
}

type echWorker struct {
	cfg     config.ECHWorkerConfig
	binPath string
	cmd     *exec.Cmd
	cancel  context.CancelFunc
}

// NewECHManager creates a new ECH worker manager.
func NewECHManager(cfg config.ProxyPoolConfig) *ECHManager {
	return &ECHManager{cfg: cfg}
}

// Start launches all configured ECH workers and returns their SOCKS5 addresses.
func (m *ECHManager) Start(ctx context.Context) []string {
	if !m.cfg.Enabled || len(m.cfg.Workers) == 0 {
		return nil
	}

	binPath := m.cfg.ECHBin
	if binPath == "" {
		binPath = "./ech-workers"
	}

	var addrs []string
	parentCtx, cancel := context.WithCancel(ctx)
	m.cancel = cancel

	for _, wCfg := range m.cfg.Workers {
		w := &echWorker{
			cfg:     wCfg,
			binPath: binPath,
		}
		if err := w.start(parentCtx); err != nil {
			log.Warnf("[proxypool] failed to start %s: %v", wCfg.Name, err)
			continue
		}
		m.mu.Lock()
		m.procs = append(m.procs, w)
		m.mu.Unlock()

		addr := fmt.Sprintf("127.0.0.1:%d", wCfg.Port)
		addrs = append(addrs, addr)
		log.Infof("[proxypool] started %s on %s (domain=%s)", wCfg.Name, addr, wCfg.Domain)
	}

	go m.supervise(parentCtx)

	return addrs
}

// Stop terminates all managed ECH worker processes.
func (m *ECHManager) Stop() {
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

func (m *ECHManager) supervise(ctx context.Context) {
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
					log.Warnf("[proxypool] %s exited, restarting...", w.cfg.Name)
					_ = w.start(ctx)
				}
			}
			m.mu.Unlock()
		}
	}
}

func (w *echWorker) start(ctx context.Context) error {
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

	go func() {
		_ = w.cmd.Wait()
	}()

	return nil
}

func (w *echWorker) stop() {
	if w.cancel != nil {
		w.cancel()
	}
	if w.cmd != nil && w.cmd.Process != nil {
		_ = w.cmd.Process.Kill()
	}
}

func (w *echWorker) isRunning() bool {
	if w.cmd == nil || w.cmd.Process == nil {
		return false
	}
	return w.cmd.ProcessState == nil
}
