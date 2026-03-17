package license

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"os/exec"
	"strings"
	"sync"
	"time"

	"warp-pool/pool"
)

// Manager handles WARP+ license keys
type Manager struct {
	mu sync.RWMutex

	keyURL      string
	keys        []string
	invalidKeys map[string]bool // Keys that failed with "Bad Request"
	usedKeys    map[string]bool // Keys successfully applied
	currentIdx  int
}

// New creates a new license manager
func New(keyURL string) *Manager {
	return &Manager{
		keyURL:      keyURL,
		invalidKeys: make(map[string]bool),
		usedKeys:    make(map[string]bool),
	}
}

// FetchKeys downloads the license key list
func (m *Manager) FetchKeys(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, "GET", m.keyURL, nil)
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to fetch keys: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("failed to fetch keys: status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("failed to read response: %w", err)
	}

	var keys []string
	scanner := bufio.NewScanner(strings.NewReader(string(body)))
	for scanner.Scan() {
		key := strings.TrimSpace(scanner.Text())
		if key != "" && len(key) > 10 {
			keys = append(keys, key)
		}
	}

	m.mu.Lock()
	m.keys = keys
	m.currentIdx = 0
	m.mu.Unlock()

	log.Printf("[license] Fetched %d license keys", len(keys))
	return nil
}

// NextKey returns the next available license key (not used and not invalid)
func (m *Manager) NextKey() (string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	for i := 0; i < len(m.keys); i++ {
		idx := (m.currentIdx + i) % len(m.keys)
		key := m.keys[idx]
		if !m.usedKeys[key] && !m.invalidKeys[key] {
			m.currentIdx = (idx + 1) % len(m.keys)
			return key, true
		}
	}

	return "", false
}

// MarkUsed marks a key as successfully used
func (m *Manager) MarkUsed(key string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.usedKeys[key] = true
}

// MarkInvalid marks a key as invalid (failed with Bad Request)
func (m *Manager) MarkInvalid(key string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.invalidKeys[key] = true
}

// ResetUsed resets the used keys tracking
func (m *Manager) ResetUsed() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.usedKeys = make(map[string]bool)
}

// KeyCount returns the number of available keys
func (m *Manager) KeyCount() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.keys)
}

// ValidKeyCount returns the number of keys not marked as invalid
func (m *Manager) ValidKeyCount() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	count := 0
	for _, key := range m.keys {
		if !m.invalidKeys[key] {
			count++
		}
	}
	return count
}

// Stats returns license manager statistics
func (m *Manager) Stats() map[string]int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return map[string]int{
		"total":   len(m.keys),
		"valid":   len(m.keys) - len(m.invalidKeys),
		"invalid": len(m.invalidKeys),
		"used":    len(m.usedKeys),
	}
}

// ProcessLicenseApplier interface for applying license
type ProcessLicenseApplier interface {
	ID() int
	WarpBin() string
	DataDir() string
}

// ApplyLicenseWithRetry tries multiple keys until one works
func (m *Manager) ApplyLicenseWithRetry(ctx context.Context, proc ProcessLicenseApplier, maxRetries int) error {
	for i := 0; i < maxRetries; i++ {
		key, ok := m.NextKey()
		if !ok {
			return fmt.Errorf("no more valid keys available")
		}

		log.Printf("[license] Process %d: Trying key %s... (attempt %d)", proc.ID(), key[:8], i+1)

		// Run warp update command
		cmd := exec.CommandContext(ctx, proc.WarpBin(), "update",
			"--name", fmt.Sprintf("pool-%d", proc.ID()),
			"--license", key,
			"--data-dir", proc.DataDir(),
		)
		output, err := cmd.CombinedOutput()

		if err != nil {
			outputStr := string(output)
			// Check if it's a "Bad Request" error (invalid/exhausted key)
			if strings.Contains(outputStr, "Failed to generate primary identity") ||
				strings.Contains(outputStr, "Bad Request") {
				log.Printf("[license] Process %d: Key %s... invalid, trying next", proc.ID(), key[:8])
				m.MarkInvalid(key)
				continue
			}
			// Other error
			return fmt.Errorf("license update failed: %w: %s", err, outputStr)
		}

		// Success!
		m.MarkUsed(key)
		log.Printf("[license] Process %d: Successfully applied key %s...", proc.ID(), key[:8])
		return nil
	}

	return fmt.Errorf("failed after %d attempts, no valid key found", maxRetries)
}

// ApplyToPool applies license keys to all processes in the pool
func (m *Manager) ApplyToPool(ctx context.Context, p *pool.Pool) error {
	processes := p.All()

	var wg sync.WaitGroup
	errCh := make(chan error, len(processes))
	successCount := 0
	var successMu sync.Mutex

	for _, proc := range processes {
		wg.Add(1)
		go func(proc ProcessLicenseApplier) {
			defer wg.Done()

			// Try up to 20 keys per process
			if err := m.ApplyLicenseWithRetry(ctx, proc, 20); err != nil {
				errCh <- fmt.Errorf("process %d: %w", proc.ID(), err)
				return
			}

			successMu.Lock()
			successCount++
			successMu.Unlock()
		}(proc)
	}

	wg.Wait()
	close(errCh)

	var errs []error
	for err := range errCh {
		errs = append(errs, err)
	}

	stats := m.Stats()
	log.Printf("[license] Applied: %d/%d processes, Keys: %d valid / %d invalid / %d total",
		successCount, len(processes), stats["valid"], stats["invalid"], stats["total"])

	if len(errs) > 0 {
		return fmt.Errorf("failed to apply %d licenses: %v", len(errs), errs)
	}

	return nil
}
