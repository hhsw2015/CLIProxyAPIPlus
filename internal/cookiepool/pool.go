// Package cookiepool provides a lightweight, hot-reloadable pool of cookie
// credentials for Skywork-style providers. Instead of registering thousands of
// individual auth entries, a single openai-compatibility config entry references
// an external JSON file. On each request the executor picks a random live cookie.
package cookiepool

import (
	"encoding/json"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	log "github.com/sirupsen/logrus"
)

// Entry represents one cookie credential in the pool.
type Entry struct {
	Cookie string `json:"cookie"`
	XFF    string `json:"xff"`
}

// Pool holds a set of cookie entries loaded from an external file.
// It supports concurrent reads and periodic hot-reload.
type Pool struct {
	mu       sync.RWMutex
	entries  []Entry
	dead     map[int]time.Time // index → expiry of dead mark
	filePath string
	modTime  time.Time
	stopCh   chan struct{}
	stopped  atomic.Bool
}

// Load creates a new pool from the given JSON file path.
func Load(filePath string) (*Pool, error) {
	p := &Pool{
		filePath: filePath,
		dead:     make(map[int]time.Time),
		stopCh:   make(chan struct{}),
	}
	if err := p.reload(); err != nil {
		return nil, err
	}
	go p.watchLoop()
	return p, nil
}

// Pick returns a random live cookie entry. Returns nil if pool is empty or all dead.
func (p *Pool) Pick() *Entry {
	p.mu.RLock()
	defer p.mu.RUnlock()

	n := len(p.entries)
	if n == 0 {
		return nil
	}

	now := time.Now()
	// Try up to n times to find a live entry
	start := rand.Intn(n)
	for i := 0; i < n; i++ {
		idx := (start + i) % n
		if expiry, dead := p.dead[idx]; dead {
			if now.After(expiry) {
				// Expired dead mark
			} else {
				continue
			}
		}
		return &p.entries[idx]
	}
	return nil
}

// MarkDead marks a cookie at the given index as temporarily dead for the specified duration.
func (p *Pool) MarkDead(cookie string, duration time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i, e := range p.entries {
		if e.Cookie == cookie {
			p.dead[i] = time.Now().Add(duration)
			return
		}
	}
}

// Size returns the total number of entries (including dead ones).
func (p *Pool) Size() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return len(p.entries)
}

// Stop halts the background file watcher.
func (p *Pool) Stop() {
	if p.stopped.CompareAndSwap(false, true) {
		close(p.stopCh)
	}
}

func (p *Pool) reload() error {
	absPath, err := filepath.Abs(p.filePath)
	if err != nil {
		absPath = p.filePath
	}

	info, err := os.Stat(absPath)
	if err != nil {
		return err
	}

	data, err := os.ReadFile(absPath)
	if err != nil {
		return err
	}

	var entries []Entry
	if err := json.Unmarshal(data, &entries); err != nil {
		return err
	}

	// Filter out empty entries
	clean := make([]Entry, 0, len(entries))
	for _, e := range entries {
		if strings.TrimSpace(e.Cookie) != "" {
			clean = append(clean, e)
		}
	}

	p.mu.Lock()
	p.entries = clean
	p.modTime = info.ModTime()
	// Clear dead marks for entries that no longer exist
	for idx := range p.dead {
		if idx >= len(clean) {
			delete(p.dead, idx)
		}
	}
	p.mu.Unlock()

	log.Infof("cookie pool loaded: %d entries from %s", len(clean), filepath.Base(absPath))
	return nil
}

func (p *Pool) watchLoop() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-p.stopCh:
			return
		case <-ticker.C:
			p.checkReload()
		}
	}
}

func (p *Pool) checkReload() {
	absPath, err := filepath.Abs(p.filePath)
	if err != nil {
		return
	}
	info, err := os.Stat(absPath)
	if err != nil {
		return
	}

	p.mu.RLock()
	changed := info.ModTime().After(p.modTime)
	p.mu.RUnlock()

	if changed {
		if err := p.reload(); err != nil {
			log.Warnf("cookie pool reload failed for %s: %v", filepath.Base(absPath), err)
		}
	}
}
