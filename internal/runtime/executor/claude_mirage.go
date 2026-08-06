package executor

import (
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/google/uuid"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
)

// mirage is a stateful UUID rotator for auth entries whose upstream identifies
// callers only by an opaque device-id header. Each auth entry gets its own pool.
// After `threshold` uses the pool auto-rotates to a fresh UUID; a 429 signal
// can also force rotation on demand.
const (
	mirageAuthStyle = "mirage-uuid"
	// mirageDeviceHeader is the wire-format header name the upstream uses
	// to identify callers. Do not rename — the string value is dictated by
	// the remote service and any change breaks the protocol.
	mirageDeviceHeader    = "x-peeky-device-id"
	mirageDefaultRotateAt = 19
)

type mirageEntry struct {
	mu        sync.Mutex
	deviceID  string
	counter   int
	threshold int
}

var (
	mirageMu   sync.RWMutex
	miragePool = map[string]*mirageEntry{}
)

func mirageEntryFor(auth *cliproxyauth.Auth) *mirageEntry {
	if auth == nil {
		return nil
	}
	key := auth.ID
	if key == "" {
		return nil
	}
	mirageMu.RLock()
	e, ok := miragePool[key]
	mirageMu.RUnlock()
	if ok {
		return e
	}
	threshold := mirageDefaultRotateAt
	if auth.Attributes != nil {
		if v := strings.TrimSpace(auth.Attributes["mirage_rotate_at"]); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				threshold = n
			}
		}
	}
	mirageMu.Lock()
	defer mirageMu.Unlock()
	if e, ok := miragePool[key]; ok {
		return e
	}
	e = &mirageEntry{threshold: threshold}
	miragePool[key] = e
	return e
}

// next returns the current UUID, rotating to a fresh one if the counter would
// exceed the threshold. Called once per outgoing request.
func (e *mirageEntry) next() string {
	if e == nil {
		return uuid.NewString()
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.deviceID == "" || e.counter >= e.threshold {
		e.deviceID = uuid.NewString()
		e.counter = 0
	}
	e.counter++
	return e.deviceID
}

// forceRotate discards the current UUID immediately. Used after a 429.
func (e *mirageEntry) forceRotate() string {
	if e == nil {
		return uuid.NewString()
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.deviceID = uuid.NewString()
	e.counter = 1
	return e.deviceID
}

// mirageThinkingActive reports whether the request body invokes a Claude
// thinking feature that requires the interleaved-thinking beta. Two shapes
// come out of the thinking pipeline:
//
//   - thinking.type is "enabled" or "adaptive" (adaptive is the Claude 4+
//     shape; enabled is the older Claude 3.7 shape).
//   - output_config.effort is set (low/medium/high/max) — the adaptive
//     path uses effort instead of budget_tokens.
//
// A budget_tokens > 0 also implies thinking; we accept it as a fallback for
// callers that skipped output_config.
func mirageThinkingActive(body []byte) bool {
	if len(body) == 0 {
		return false
	}
	if t := gjson.GetBytes(body, "thinking.type"); t.Exists() {
		switch strings.ToLower(strings.TrimSpace(t.String())) {
		case "enabled", "adaptive":
			return true
		}
	}
	if e := gjson.GetBytes(body, "output_config.effort"); e.Exists() {
		if strings.TrimSpace(e.String()) != "" {
			return true
		}
	}
	if bt := gjson.GetBytes(body, "thinking.budget_tokens"); bt.Exists() && bt.Int() > 0 {
		return true
	}
	return false
}

// isMirageAuth reports whether this auth uses the mirage-uuid rotation scheme.
func isMirageAuth(auth *cliproxyauth.Auth) bool {
	if auth == nil || auth.Attributes == nil {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(auth.Attributes["auth_style"]), mirageAuthStyle)
}

// sanitizeHeadersForLog redacts rotation-sensitive headers before persisting
// upstream request headers to the request log. The whole point of the mirage
// UUID rotation is to make N requests look like N unrelated devices; without
// this scrub, any log sink that keeps request headers correlates all rotating
// UUIDs to one auth entry and the anonymity guarantee is gone.
func sanitizeHeadersForLog(h http.Header, auth *cliproxyauth.Auth) http.Header {
	if h == nil {
		return h
	}
	if isMirageAuth(auth) {
		if h.Get(mirageDeviceHeader) != "" {
			h.Set(mirageDeviceHeader, "[REDACTED]")
		}
	}
	return h
}
