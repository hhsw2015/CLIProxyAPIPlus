package auth

import (
	"context"
	"math"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/tidwall/gjson"
)

// SerializerConfig controls per-auth request serialization.
type SerializerConfig struct {
	Enabled    bool
	Mode       string // "serialize" (lock+delay) or "throttle" (delay only)
	MinDelayMs int    // default 200
	MaxDelayMs int    // default 2000
	BaseRPM    int    // default 60
}

// AuthSerializer prevents concurrent real user messages from hitting the same auth.
// Completely in-memory -- no Redis dependency.
type AuthSerializer struct {
	mu    sync.Mutex
	locks map[string]*authLock
	cfg   SerializerConfig
}

type authLock struct {
	mu         sync.Mutex
	rpmMu      sync.Mutex // protects rpmWindow and lastDoneMs in throttle mode
	lastDoneMs int64      // last request completion timestamp (ms)
	rpmWindow  []int64    // timestamps within current 1-minute window
}

// NewAuthSerializer creates a new AuthSerializer with the given config.
// Zero-value fields get safe defaults.
func NewAuthSerializer(cfg SerializerConfig) *AuthSerializer {
	if cfg.MinDelayMs <= 0 {
		cfg.MinDelayMs = 200
	}
	if cfg.MaxDelayMs <= 0 {
		cfg.MaxDelayMs = 2000
	}
	if cfg.MinDelayMs > cfg.MaxDelayMs {
		cfg.MinDelayMs, cfg.MaxDelayMs = cfg.MaxDelayMs, cfg.MinDelayMs
	}
	if cfg.BaseRPM <= 0 {
		cfg.BaseRPM = 60
	}
	return &AuthSerializer{
		locks: make(map[string]*authLock),
		cfg:   cfg,
	}
}

// Acquire obtains a per-auth serialization slot for a real user message.
// Returns a release function that MUST be called when the request completes
// (typically via defer). If serialization is not needed (disabled, or not a
// real user message), the returned function is a no-op.
func (s *AuthSerializer) Acquire(ctx context.Context, authID string, body []byte) func() {
	noop := func() {}

	if !s.cfg.Enabled {
		return noop
	}
	if !IsRealUserMessage(body) {
		return noop
	}

	lock := s.getOrCreateLock(authID)

	// In "serialize" mode, acquire the per-auth mutex so only one real
	// user message is in-flight at a time.
	if s.cfg.Mode == "serialize" {
		lock.mu.Lock()
	}

	// RPM-aware delay between sequential requests.
	lock.rpmMu.Lock()
	rpm := lock.currentRPM()
	lastDone := lock.lastDoneMs
	lock.rpmMu.Unlock()
	delay := s.calculateDelay(rpm)
	if delay > 0 && lastDone > 0 {
		elapsed := time.Duration(nowMs()-lastDone) * time.Millisecond
		remaining := delay - elapsed
		if remaining > 0 {
			timer := time.NewTimer(remaining)
			defer timer.Stop()
			select {
			case <-ctx.Done():
				// Context cancelled while waiting -- still return a
				// proper release so the caller's defer works.
				if s.cfg.Mode == "serialize" {
					lock.mu.Unlock()
				}
				return noop
			case <-timer.C:
			}
		}
	}

	// Record that a request is starting in the RPM window.
	now := nowMs()
	lock.rpmMu.Lock()
	lock.recordStart(now)
	lock.rpmMu.Unlock()

	released := false
	return func() {
		if released {
			return
		}
		released = true
		lock.rpmMu.Lock()
		lock.lastDoneMs = nowMs()
		lock.rpmMu.Unlock()
		if s.cfg.Mode == "serialize" {
			lock.mu.Unlock()
		}
	}
}

// IsRealUserMessage detects whether body represents a genuine user turn
// (as opposed to a tool_result / tool_use_result relay). Logic mirrors
// sub2api's user_msg_queue_service.go lines 60-102.
func IsRealUserMessage(body []byte) bool {
	messages := gjson.GetBytes(body, "messages")
	if !messages.Exists() || !messages.IsArray() {
		return false
	}
	arr := messages.Array()
	if len(arr) == 0 {
		return false
	}
	last := arr[len(arr)-1]
	if last.Get("role").String() != "user" {
		return false
	}
	content := last.Get("content")
	if content.IsArray() {
		for _, item := range content.Array() {
			t := item.Get("type").String()
			if t == "tool_result" || t == "tool_use_result" {
				return false
			}
		}
	}
	return true
}

// getOrCreateLock returns the authLock for the given authID, creating one if needed.
func (s *AuthSerializer) getOrCreateLock(authID string) *authLock {
	s.mu.Lock()
	defer s.mu.Unlock()
	lock, ok := s.locks[authID]
	if !ok {
		lock = &authLock{}
		s.locks[authID] = lock
	}
	return lock
}

// currentRPM returns the number of requests recorded in the last 60 seconds
// and prunes expired entries.
func (l *authLock) currentRPM() int {
	cutoff := nowMs() - 60_000
	n := 0
	for _, ts := range l.rpmWindow {
		if ts >= cutoff {
			n++
		}
	}
	// Compact: keep only entries within the window.
	if len(l.rpmWindow) > 0 {
		kept := l.rpmWindow[:0]
		for _, ts := range l.rpmWindow {
			if ts >= cutoff {
				kept = append(kept, ts)
			}
		}
		l.rpmWindow = kept
	}
	return n
}

// recordStart appends a timestamp to the RPM sliding window.
func (l *authLock) recordStart(ms int64) {
	l.rpmWindow = append(l.rpmWindow, ms)
}

// calculateDelay computes an RPM-aware delay with jitter.
// Adapted from sub2api's CalculateRPMAwareDelay (lines 200-241).
//
//	ratio < 0.5        -> minDelay
//	0.5 <= ratio < 0.8 -> linear interpolation minDelay..maxDelay
//	ratio >= 0.8       -> maxDelay
//	Final value gets +/-15% jitter.
func (s *AuthSerializer) calculateDelay(rpm int) time.Duration {
	minDelay := time.Duration(s.cfg.MinDelayMs) * time.Millisecond
	maxDelay := time.Duration(s.cfg.MaxDelayMs) * time.Millisecond

	if s.cfg.BaseRPM <= 0 {
		return applySerializerJitter(minDelay, 0.15)
	}

	ratio := float64(rpm) / float64(s.cfg.BaseRPM)

	var base time.Duration
	switch {
	case ratio < 0.5:
		base = minDelay
	case ratio >= 0.8:
		base = maxDelay
	default:
		// Linear interpolation: 0.5 -> minDelay, 0.8 -> maxDelay
		t := (ratio - 0.5) / 0.3
		interpolated := float64(minDelay) + t*float64(maxDelay-minDelay)
		base = time.Duration(math.Round(interpolated))
	}

	return applySerializerJitter(base, 0.15)
}

// applySerializerJitter applies +/-jitterPct random jitter to d.
// Mirrors sub2api's applyJitter (lines 302-309).
func applySerializerJitter(d time.Duration, jitterPct float64) time.Duration {
	if d <= 0 || jitterPct <= 0 {
		return d
	}
	// [-jitterPct, +jitterPct]
	jitter := (rand.Float64()*2 - 1) * jitterPct
	return time.Duration(float64(d) * (1 + jitter))
}

// nowMs returns the current time in milliseconds. Factored out to allow
// deterministic testing if needed via a package-level variable.
var nowMs = func() int64 {
	return time.Now().UnixMilli()
}
