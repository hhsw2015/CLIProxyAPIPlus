package executor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

// TestApplyClaudeHeaders_MirageEmitsPeekyWireFormat verifies that the
// mirage-uuid auth path emits *only* the header set the Peeky client uses
// against aegis-proxy. Any additional Claude Code fingerprint headers
// (X-Stainless-*, X-App, Anthropic-Beta, X-Claude-Code-Session-Id) would
// defeat the anonymous UUID rotation and let the upstream correlate our
// requests to claude-cli.
func TestApplyClaudeHeaders_MirageEmitsPeekyWireFormat(t *testing.T) {
	auth := &cliproxyauth.Auth{
		ID: "test-mirage-auth",
		Attributes: map[string]string{
			"auth_style": mirageAuthStyle,
			"full_url":   "https://aegis-proxy.example.workers.dev/v1/anthropic/messages",
		},
	}

	// Seed the request with a bunch of fingerprint headers to confirm they
	// get scrubbed on the way out.
	req := httptest.NewRequest(http.MethodPost, "https://aegis-proxy.example.workers.dev/v1/anthropic/messages", strings.NewReader(`{}`))
	req = req.WithContext(context.Background())
	req.Header.Set("Authorization", "Bearer leaked")
	req.Header.Set("x-api-key", "leaked")
	req.Header.Set("Anthropic-Beta", "claude-code-20250219,oauth-2025-04-20")
	req.Header.Set("X-App", "cli")
	req.Header.Set("X-Stainless-Runtime", "node")
	req.Header.Set("X-Claude-Code-Session-Id", "leaked-session")
	req.Header.Set("User-Agent", "claude-cli/1.0")

	if err := applyClaudeHeaders(req, auth, "unused-key", false, nil, nil, &config.Config{}, nil, false); err != nil {
		t.Fatalf("applyClaudeHeaders returned error: %v", err)
	}

	want := map[string]string{
		"Content-Type":       "application/json",
		mirageDeviceHeader:   "", // present but value is a UUID
		"Anthropic-Version":  "2023-06-01",
		"User-Agent":         "reqwest/0.12.24",
		"Accept":             "*/*",
	}

	got := map[string]string{}
	for k, v := range req.Header {
		if len(v) > 0 {
			got[http.CanonicalHeaderKey(k)] = v[0]
		}
	}

	// Confirm every "want" key is present.
	for k, wantVal := range want {
		canon := http.CanonicalHeaderKey(k)
		gotVal, ok := got[canon]
		if !ok {
			t.Errorf("header %q missing; got %v", canon, sortedKeys(got))
			continue
		}
		if wantVal != "" && gotVal != wantVal {
			t.Errorf("header %q = %q, want %q", canon, gotVal, wantVal)
		}
	}

	// Confirm no fingerprint header survived.
	forbidden := []string{
		"Authorization",
		"X-Api-Key",
		"Anthropic-Beta",
		"X-App",
		"X-Stainless-Retry-Count",
		"X-Stainless-Runtime",
		"X-Stainless-Lang",
		"X-Stainless-Timeout",
		"X-Stainless-Package-Version",
		"X-Stainless-Runtime-Version",
		"X-Stainless-Arch",
		"X-Stainless-Os",
		"X-Stainless-Helper-Method",
		"X-Claude-Code-Session-Id",
		"X-Client-Request-Id",
		"Anthropic-Dangerous-Direct-Browser-Access",
	}
	for _, k := range forbidden {
		canon := http.CanonicalHeaderKey(k)
		if _, present := got[canon]; present {
			t.Errorf("forbidden fingerprint header still present: %q = %q", canon, got[canon])
		}
	}

	// Confirm the UA is the reqwest one, not any leftover claude-cli UA.
	if ua := req.Header.Get("User-Agent"); ua != "reqwest/0.12.24" {
		t.Errorf("User-Agent = %q, want reqwest/0.12.24", ua)
	}

	// Confirm the device-id is a plausible UUID (36 chars, 4 dashes).
	deviceID := req.Header.Get(mirageDeviceHeader)
	if len(deviceID) != 36 || strings.Count(deviceID, "-") != 4 {
		t.Errorf("mirage device-id header = %q, want a UUID v4 string", deviceID)
	}
}

// TestApplyClaudeHeaders_MirageStreamAddsAcceptEncoding confirms streaming
// requests add Accept-Encoding: identity so the aegis-proxy SSE body isn't
// double-encoded (Peeky's client sends this).
func TestApplyClaudeHeaders_MirageStreamAddsAcceptEncoding(t *testing.T) {
	auth := &cliproxyauth.Auth{
		ID:         "test-mirage-stream",
		Attributes: map[string]string{"auth_style": mirageAuthStyle},
	}
	req := httptest.NewRequest(http.MethodPost, "https://example.invalid/", strings.NewReader(`{}`))
	req = req.WithContext(context.Background())

	if err := applyClaudeHeaders(req, auth, "unused", true, nil, nil, &config.Config{}, nil, false); err != nil {
		t.Fatalf("applyClaudeHeaders(stream=true) returned error: %v", err)
	}
	if got := req.Header.Get("Accept-Encoding"); got != "identity" {
		t.Errorf("stream Accept-Encoding = %q, want identity", got)
	}
}

// TestApplyClaudeHeaders_MirageRotatesDeviceIDAcrossCalls confirms the pool
// counter advances between calls (though within the same auth it still
// returns the same UUID until the threshold, so we verify by rotating past
// the threshold).
func TestApplyClaudeHeaders_MirageRotatesDeviceIDAcrossCalls(t *testing.T) {
	auth := &cliproxyauth.Auth{
		ID: "test-mirage-rotate",
		Attributes: map[string]string{
			"auth_style":       mirageAuthStyle,
			"mirage_rotate_at": "2", // rotate every 2 requests
		},
	}
	// Wipe any prior state for this auth.
	mirageMu.Lock()
	delete(miragePool, auth.ID)
	mirageMu.Unlock()

	seen := map[string]int{}
	for i := 0; i < 6; i++ {
		req := httptest.NewRequest(http.MethodPost, "https://example.invalid/", strings.NewReader(`{}`))
		req = req.WithContext(context.Background())
		if err := applyClaudeHeaders(req, auth, "", false, nil, nil, &config.Config{}, nil, false); err != nil {
			t.Fatalf("applyClaudeHeaders err on iter %d: %v", i, err)
		}
		seen[req.Header.Get(mirageDeviceHeader)]++
	}
	if len(seen) < 2 {
		t.Fatalf("expected multiple UUIDs from rotating pool, got %d unique: %v", len(seen), seen)
	}
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
