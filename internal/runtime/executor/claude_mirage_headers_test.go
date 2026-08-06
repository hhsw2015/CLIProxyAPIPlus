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

// TestApplyClaudeHeaders_MirageWireFormat verifies that the mirage-uuid auth
// path emits *only* the header set the mirage upstream expects (reqwest 0.13.4
// wire format). Any additional Claude Code fingerprint header
// (X-Stainless-*, X-App, Anthropic-Beta, X-Claude-Code-Session-Id) would
// defeat the anonymous UUID rotation and let the upstream correlate our
// requests to claude-cli.
func TestApplyClaudeHeaders_MirageWireFormat(t *testing.T) {
	auth := &cliproxyauth.Auth{
		ID: "test-mirage-auth",
		Attributes: map[string]string{
			"auth_style": mirageAuthStyle,
			"full_url":   "https://mirage-upstream.example/v1/anthropic/messages",
		},
	}

	// Seed the request with a bunch of fingerprint headers to confirm they
	// get scrubbed on the way out.
	req := httptest.NewRequest(http.MethodPost, "https://mirage-upstream.example/v1/anthropic/messages", strings.NewReader(`{}`))
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

	// Header keys must be exact-case lowercase in the map — Go's
	// http.Header.Set canonicalizes ("Content-Type"), but reqwest 0.13.4
	// emits lowercase on the wire. Over HTTP/2 HPACK normalizes anyway;
	// over HTTP/1.1 canonical case would fingerprint us.
	want := map[string]string{
		"content-type":       "application/json",
		mirageDeviceHeader:   "", // present but value is a UUID
		"anthropic-version":  "2023-06-01",
		"user-agent":         "reqwest/0.13.4",
		"accept":             "*/*",
	}

	// Confirm every "want" key is present with exact case.
	for k, wantVal := range want {
		vals, ok := req.Header[k]
		if !ok {
			t.Errorf("header %q missing (exact lowercase case required); got keys=%v", k, headerKeys(req.Header))
			continue
		}
		if len(vals) == 0 {
			t.Errorf("header %q present but empty", k)
			continue
		}
		if wantVal != "" && vals[0] != wantVal {
			t.Errorf("header %q = %q, want %q", k, vals[0], wantVal)
		}
	}

	// Confirm no fingerprint header survived. Check both canonical and
	// lowercase forms — someone might reintroduce either via Set() or map
	// write.
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
		if _, present := req.Header[k]; present {
			t.Errorf("forbidden fingerprint header (canonical case) present: %q", k)
		}
		if _, present := req.Header[strings.ToLower(k)]; present {
			t.Errorf("forbidden fingerprint header (lowercase) present: %q", strings.ToLower(k))
		}
	}

	// Confirm no canonical-case aliases of our wire-format headers slipped in
	// via Set(). We wrote lowercase directly to the map, so canonical forms
	// must be absent.
	for _, canon := range []string{"Content-Type", "Anthropic-Version", "User-Agent", "Accept", "Accept-Encoding"} {
		if _, present := req.Header[canon]; present {
			t.Errorf("canonical-case duplicate leaked: %q (should only be lowercase)", canon)
		}
	}

	// Accept-Encoding must be present-but-nil so Go http.Transport does not
	// auto-add "gzip".
	if ae, ok := req.Header["accept-encoding"]; !ok {
		t.Error("accept-encoding must be present (nil slice) to suppress Go auto-gzip")
	} else if ae != nil {
		t.Errorf("accept-encoding = %v, want nil slice", ae)
	}

	// Confirm the device-id is a plausible UUID (36 chars, 4 dashes).
	deviceID := req.Header[mirageDeviceHeader][0]
	if len(deviceID) != 36 || strings.Count(deviceID, "-") != 4 {
		t.Errorf("mirage device-id header = %q, want a UUID v4 string", deviceID)
	}
}

func headerKeys(h http.Header) []string {
	out := make([]string, 0, len(h))
	for k := range h {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TestApplyClaudeHeaders_MirageThinkingBeta verifies dynamic emission of
// anthropic-beta: interleaved-thinking-2025-05-14 based on the request body.
// The beta must NOT be sent when the body has no thinking fields, and MUST
// be sent for adaptive/enabled/effort/budget_tokens shapes (including xhigh
// on Opus 4.7+).
func TestApplyClaudeHeaders_MirageThinkingBeta(t *testing.T) {
	auth := &cliproxyauth.Auth{
		ID:         "test-thinking-beta",
		Attributes: map[string]string{"auth_style": mirageAuthStyle},
	}
	cases := []struct {
		name string
		body string
		want bool
	}{
		{"no thinking", `{"model":"claude-fable-5","messages":[]}`, false},
		{"adaptive + max", `{"thinking":{"type":"adaptive"},"output_config":{"effort":"max"}}`, true},
		{"adaptive + xhigh", `{"thinking":{"type":"adaptive"},"output_config":{"effort":"xhigh"}}`, true},
		{"adaptive + low", `{"thinking":{"type":"adaptive"},"output_config":{"effort":"low"}}`, true},
		{"enabled + budget", `{"thinking":{"type":"enabled","budget_tokens":8000}}`, true},
		{"effort only", `{"output_config":{"effort":"high"}}`, true},
		{"budget only", `{"thinking":{"budget_tokens":16384}}`, true},
		{"empty body", ``, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "https://mirage-upstream.example/v1/anthropic/messages", strings.NewReader(tc.body))
			var bodyBytes []byte
			if tc.body != "" {
				bodyBytes = []byte(tc.body)
			}
			if err := applyClaudeHeaders(req, auth, "", false, nil, bodyBytes, &config.Config{}, nil, false); err != nil {
				t.Fatalf("applyClaudeHeaders err: %v", err)
			}
			vals, present := req.Header["anthropic-beta"]
			if tc.want && !present {
				t.Errorf("anthropic-beta missing (want interleaved-thinking beta for body %q)", tc.body)
			}
			if !tc.want && present {
				t.Errorf("anthropic-beta unexpectedly present: %v (body has no thinking)", vals)
			}
			if tc.want && present && len(vals) > 0 && !strings.Contains(vals[0], "interleaved-thinking-2025-05-14") {
				t.Errorf("anthropic-beta = %q, want to contain interleaved-thinking-2025-05-14", vals[0])
			}
		})
	}
}

// TestApplyClaudeHeaders_MirageExtendedCacheTTLBeta verifies that cache_control
// blocks with ttl trigger the extended-cache-ttl-2025-04-11 beta so the
// upstream honors 1h caching instead of silently downgrading to 5min.
func TestApplyClaudeHeaders_MirageExtendedCacheTTLBeta(t *testing.T) {
	auth := &cliproxyauth.Auth{
		ID:         "test-cache-ttl",
		Attributes: map[string]string{"auth_style": mirageAuthStyle},
	}
	cases := []struct {
		name        string
		body        string
		wantBeta    bool
		wantContain string
	}{
		{"no cache_control", `{"messages":[{"role":"user","content":"hi"}]}`, false, ""},
		{"cache_control but no ttl", `{"system":[{"type":"text","text":"x","cache_control":{"type":"ephemeral"}}]}`, false, ""},
		{"system ttl 1h", `{"system":[{"type":"text","text":"x","cache_control":{"type":"ephemeral","ttl":"1h"}}]}`, true, "extended-cache-ttl-2025-04-11"},
		{"tools ttl", `{"tools":[{"name":"t","cache_control":{"ttl":"1h"}}]}`, true, "extended-cache-ttl-2025-04-11"},
		{"messages content ttl", `{"messages":[{"role":"user","content":[{"type":"text","text":"x","cache_control":{"ttl":"1h"}}]}]}`, true, "extended-cache-ttl-2025-04-11"},
		{"thinking + ttl → both betas", `{"thinking":{"type":"adaptive"},"output_config":{"effort":"max"},"system":[{"type":"text","text":"x","cache_control":{"ttl":"1h"}}]}`, true, "extended-cache-ttl-2025-04-11"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "https://mirage-upstream.example/v1/anthropic/messages", strings.NewReader(tc.body))
			if err := applyClaudeHeaders(req, auth, "", false, nil, []byte(tc.body), &config.Config{}, nil, false); err != nil {
				t.Fatalf("applyClaudeHeaders err: %v", err)
			}
			vals, present := req.Header["anthropic-beta"]
			if tc.wantBeta && !present {
				t.Errorf("anthropic-beta missing for %s", tc.name)
			}
			if !tc.wantBeta && present {
				t.Errorf("anthropic-beta unexpectedly present: %v", vals)
			}
			if tc.wantBeta && present && tc.wantContain != "" && !strings.Contains(vals[0], tc.wantContain) {
				t.Errorf("anthropic-beta = %q, want to contain %q", vals[0], tc.wantContain)
			}
		})
	}
}

// TestApplyClaudeHeaders_MirageStreamSuppressesAcceptEncoding confirms that
// streaming requests still leave Accept-Encoding as a nil-slice sentinel so
// Go's http.Transport does not auto-add its gzip default. reqwest 0.13.4
// omits Accept-Encoding entirely when compression is disabled.
func TestApplyClaudeHeaders_MirageStreamSuppressesAcceptEncoding(t *testing.T) {
	auth := &cliproxyauth.Auth{
		ID:         "test-mirage-stream",
		Attributes: map[string]string{"auth_style": mirageAuthStyle},
	}
	req := httptest.NewRequest(http.MethodPost, "https://example.invalid/", strings.NewReader(`{}`))
	req = req.WithContext(context.Background())

	if err := applyClaudeHeaders(req, auth, "unused", true, nil, nil, &config.Config{}, nil, false); err != nil {
		t.Fatalf("applyClaudeHeaders(stream=true) returned error: %v", err)
	}
	ae, ok := req.Header["accept-encoding"]
	if !ok {
		t.Error("stream: accept-encoding must be present (nil slice) to suppress Go auto-gzip")
	} else if ae != nil {
		t.Errorf("stream: accept-encoding = %v, want nil slice (no explicit value)", ae)
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
		vals := req.Header[mirageDeviceHeader]
		if len(vals) == 0 {
			t.Fatalf("iter %d: %q header missing (case-sensitive lookup)", i, mirageDeviceHeader)
		}
		seen[vals[0]]++
	}
	if len(seen) < 2 {
		t.Fatalf("expected multiple UUIDs from rotating pool, got %d unique: %v", len(seen), seen)
	}
}

