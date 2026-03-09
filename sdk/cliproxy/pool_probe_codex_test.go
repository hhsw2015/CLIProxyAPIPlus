package cliproxy

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/config"
)

func TestProbeCodexUsageWithURL_Success(t *testing.T) {
	resetAt := time.Now().Add(2 * time.Hour).Unix()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer access-token" {
			t.Fatalf("Authorization = %q, want %q", got, "Bearer access-token")
		}
		if got := r.Header.Get("Chatgpt-Account-Id"); got != "acct-1" {
			t.Fatalf("Chatgpt-Account-Id = %q, want %q", got, "acct-1")
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(fmt.Sprintf(`{"plan_type":"Free","rate_limit":{"primary_window":{"used_percent":85,"reset_at":%d}}}`, resetAt)))
	}))
	defer server.Close()

	auth := &coreauth.Auth{
		ID:       "codex-auth",
		Provider: "codex",
		Metadata: map[string]any{
			"access_token": "access-token",
			"account_id":   "acct-1",
		},
	}

	result := probeCodexUsageWithURL(context.Background(), &config.Config{}, auth, server.URL)
	if !result.Success {
		t.Fatalf("expected success result, got %+v", result)
	}
	if result.Error != nil {
		t.Fatalf("expected nil error, got %+v", result.Error)
	}
	if got, ok := auth.Metadata[poolQuotaPlanTypeKey].(string); !ok || got != "Free" {
		t.Fatalf("plan_type = %#v, want Free", auth.Metadata[poolQuotaPlanTypeKey])
	}
	if got, ok := auth.Metadata[poolQuotaWeeklyUsedPercentKey].(int); !ok || got != 85 {
		t.Fatalf("used_percent = %#v, want 85", auth.Metadata[poolQuotaWeeklyUsedPercentKey])
	}
	if got, ok := auth.Metadata[poolQuotaWeeklyRemainingPercentKey].(int); !ok || got != 15 {
		t.Fatalf("remaining_percent = %#v, want 15", auth.Metadata[poolQuotaWeeklyRemainingPercentKey])
	}
	if got, ok := auth.Metadata[poolQuotaWeeklyResetAtKey].(string); !ok || got == "" {
		t.Fatalf("reset_at = %#v, want non-empty string", auth.Metadata[poolQuotaWeeklyResetAtKey])
	}
}

func TestProbeCodexUsageWithURL_Unauthorized(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"unauthorized"}}`))
	}))
	defer server.Close()

	auth := &coreauth.Auth{
		ID:       "codex-auth",
		Provider: "codex",
		Metadata: map[string]any{
			"access_token": "access-token",
		},
	}

	result := probeCodexUsageWithURL(context.Background(), &config.Config{}, auth, server.URL)
	if result.Success {
		t.Fatalf("expected unauthorized result, got success %+v", result)
	}
	if result.Error == nil || result.Error.HTTPStatus != http.StatusUnauthorized || result.Error.Code != "unauthorized" {
		t.Fatalf("unexpected error: %+v", result.Error)
	}
}

func TestProbeCodexUsageWithURL_ParsesRetryAfter(t *testing.T) {
	resetAt := time.Now().Add(2 * time.Minute).Unix()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = fmt.Fprintf(w, `{"error":{"type":"usage_limit_reached","resets_at":%d}}`, resetAt)
	}))
	defer server.Close()

	auth := &coreauth.Auth{
		ID:       "codex-auth",
		Provider: "codex",
		Metadata: map[string]any{
			"access_token": "access-token",
		},
	}

	result := probeCodexUsageWithURL(context.Background(), &config.Config{}, auth, server.URL)
	if result.Success {
		t.Fatalf("expected failure result, got %+v", result)
	}
	if result.Error == nil || result.Error.HTTPStatus != http.StatusTooManyRequests || result.Error.Code != "quota_exceeded" {
		t.Fatalf("unexpected error: %+v", result.Error)
	}
	if result.RetryAfter == nil || *result.RetryAfter <= 0 {
		t.Fatalf("expected positive RetryAfter, got %+v", result.RetryAfter)
	}
}
