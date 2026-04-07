// Package cookiepool provides a lightweight, hot-reloadable pool of cookie
// credentials for Skywork-style providers. Instead of registering thousands of
// individual auth entries, a single openai-compatibility config entry references
// an external JSON file. On each request the executor picks a random live cookie.
package cookiepool

import (
	"bytes"
	"encoding/json"
	"io"
	"math/rand"
	"net/http"
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
	mu             sync.RWMutex
	entries        []Entry
	dead           map[int]time.Time // index → expiry of dead mark
	preferred      int               // index of sticky preferred cookie, -1 = none
	filePath       string
	healthCheckURL string // base URL for zero-token health checks (from config)
	modTime        time.Time
	stopCh         chan struct{}
	stopped        atomic.Bool
}

// Load creates a new pool from the given JSON file path.
// healthCheckURL is the base URL used for zero-token cookie validation (optional).
func Load(filePath, healthCheckURL string) (*Pool, error) {
	p := &Pool{
		filePath:       filePath,
		healthCheckURL: strings.TrimSpace(healthCheckURL),
		dead:           make(map[int]time.Time),
		preferred:      -1,
		stopCh:         make(chan struct{}),
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
// new preferred.
//
// When a healthCheckURL is configured and a new cookie is being selected
// (not the sticky preferred), Pick validates the cookie with a zero-token
// request before returning it. Invalid cookies are marked dead immediately.
//
// Returns nil if pool is empty or all cookies are dead.
func (p *Pool) Pick() *Entry {
	p.mu.Lock()

	n := len(p.entries)
	if n == 0 {
		p.mu.Unlock()
		return nil
	}

	now := time.Now()

	// Try preferred cookie first (sticky for cache locality, no health check needed).
	if p.preferred >= 0 && p.preferred < n {
		if expiry, dead := p.dead[p.preferred]; !dead || now.After(expiry) {
			entry := &p.entries[p.preferred]
			p.mu.Unlock()
			return entry
		}
	}

	// Preferred is dead or unset — pick a new random live cookie.
	// If healthCheckURL is set, validate before returning.
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
		entry := p.entries[idx]
		if p.healthCheckURL != "" {
			// Release lock during network call.
			p.mu.Unlock()
			if !p.checkCookieAlive(entry) {
				p.MarkDead(entryID(entry), 24*time.Hour)
				log.Debugf("cookie pool: cookie failed health check, trying next")
				p.mu.Lock()
				continue
			}
			p.mu.Lock()
		}
		p.preferred = idx
		result := &p.entries[idx]
		p.mu.Unlock()
		return result
	}
	p.mu.Unlock()
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

// entryID returns a stable identifier for a pool entry (the first non-empty value).
// Used for MarkDead matching and sticky preference tracking.
func entryID(e Entry) string {
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

// checkCookieAlive sends a zero-token request to verify if a cookie is still valid.
// Returns true if the cookie is alive (HTTP 500 = auth ok, param error; or 200).
// Returns false if expired (HTTP 401).
func (p *Pool) checkCookieAlive(entry Entry) bool {
	url := p.healthCheckURL
	body := []byte(`{"model":"x","messages":[]}`)

	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return true // can't check, assume alive
	}
	req.Header.Set("Content-Type", "application/json")
	for key, value := range entry {
		if strings.TrimSpace(key) != "" && strings.TrimSpace(value) != "" {
			req.Header.Set(key, value)
		}
	}

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return true // network error, assume alive
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	// 401 = cookie expired; anything else (500, 200, etc.) = alive
	return resp.StatusCode != http.StatusUnauthorized
}
