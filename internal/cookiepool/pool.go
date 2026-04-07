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
// Keys are HTTP header names, values are header values.
// Example: {"x-skywork-cookies": "token=...", "X-Forwarded-For": "10.54.12.4"}
type Entry map[string]string

// Pool holds a set of cookie entries loaded from an external file.
// It supports concurrent reads and periodic hot-reload.
// Pick uses sticky selection: it remembers the last successful cookie and
// reuses it for cache locality. On failure (MarkDead), the preferred cookie
// is cleared and the next Pick selects a new random cookie.
type Pool struct {
	mu        sync.RWMutex
	entries   []Entry
	dead      map[int]time.Time // index → expiry of dead mark
	preferred int               // index of sticky preferred cookie, -1 = none
	filePath  string
	modTime   time.Time
	stopCh    chan struct{}
	stopped   atomic.Bool
}

// Load creates a new pool from the given JSON file path.
func Load(filePath string) (*Pool, error) {
	p := &Pool{
		filePath:  filePath,
		dead:      make(map[int]time.Time),
		preferred: -1,
		stopCh:    make(chan struct{}),
	}
	if err := p.reload(); err != nil {
		return nil, err
	}
	go p.watchLoop()
	return p, nil
}

// Pick returns a live cookie entry using sticky selection. It prefers the
// previously successful cookie (for prompt cache locality). If the preferred
// cookie is dead or unset, a new random cookie is selected and becomes the
// new preferred. Returns nil if pool is empty or all cookies are dead.
func (p *Pool) Pick() *Entry {
	p.mu.Lock()
	defer p.mu.Unlock()

	n := len(p.entries)
	if n == 0 {
		return nil
	}

	now := time.Now()

	// Try preferred cookie first (sticky for cache locality).
	if p.preferred >= 0 && p.preferred < n {
		if expiry, dead := p.dead[p.preferred]; !dead || now.After(expiry) {
			return &p.entries[p.preferred]
		}
	}

	// Preferred is dead or unset — pick a new random live cookie.
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
		p.preferred = idx
		return &p.entries[idx]
	}
	return nil
}

// MarkDead marks a cookie as temporarily dead for the specified duration.
// The cookie is identified by its entry ID (first header value).
// If the dead cookie was the preferred (sticky) cookie, the preference is
// cleared so the next Pick selects a new random cookie.
func (p *Pool) MarkDead(cookie string, duration time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i, e := range p.entries {
		if entryID(e) == cookie {
			p.dead[i] = time.Now().Add(duration)
			if p.preferred == i {
				p.preferred = -1
			}
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
		if len(e) > 0 && entryID(e) != "" {
			clean = append(clean, e)
		}
	}

	p.mu.Lock()
	p.entries = clean
	p.modTime = info.ModTime()
	p.preferred = -1 // reset sticky preference on reload
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

// entryID returns a stable identifier for a pool entry (the first non-empty header value).
// Used for MarkDead matching and sticky preference tracking.
func entryID(e Entry) string {
	// Prefer x-skywork-cookies as the canonical ID (it's the account token).
	for _, key := range []string{"x-skywork-cookies", "X-Skywork-Cookies", "cookie"} {
		if v := strings.TrimSpace(e[key]); v != "" {
			return v
		}
	}
	// Fallback: first non-empty value.
	for _, v := range e {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// EntryID returns the stable identifier for this entry (exported for callers like MarkDead).
func (e Entry) ID() string {
	return entryID(e)
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
