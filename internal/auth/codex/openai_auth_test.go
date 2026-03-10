package codex

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	log "github.com/sirupsen/logrus"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestRefreshTokensWithRetry_NonRetryableOnlyAttemptsOnce(t *testing.T) {
	var logs bytes.Buffer
	restoreLogger := swapCodexAuthTestLogger(&logs)
	t.Cleanup(restoreLogger)

	var calls int32
	auth := &CodexAuth{
		httpClient: &http.Client{
			Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				atomic.AddInt32(&calls, 1)
				return &http.Response{
					StatusCode: http.StatusBadRequest,
					Body:       io.NopCloser(strings.NewReader(`{"error":"invalid_grant","code":"refresh_token_reused"}`)),
					Header:     make(http.Header),
					Request:    req,
				}, nil
			}),
		},
	}

	_, err := auth.RefreshTokensWithRetry(context.Background(), "dummy_refresh_token", 3)
	if err == nil {
		t.Fatalf("expected error for non-retryable refresh failure")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "refresh_token_reused") {
		t.Fatalf("expected refresh_token_reused in error, got: %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("expected 1 refresh attempt, got %d", got)
	}
	if logs.Len() != 0 {
		t.Fatalf("expected non-retryable refresh failure to avoid warning logs, got %q", logs.String())
	}
}

func TestRefreshTokensWithRetry_RetryableWarningIsSingleLine(t *testing.T) {
	var logs bytes.Buffer
	restoreLogger := swapCodexAuthTestLogger(&logs)
	t.Cleanup(restoreLogger)

	auth := &CodexAuth{
		httpClient: &http.Client{
			Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusInternalServerError,
					Body:       io.NopCloser(strings.NewReader("{\n  \"error\": {\n    \"message\": \"temporary\"\n  }\n}")),
					Header:     make(http.Header),
					Request:    req,
				}, nil
			}),
		},
	}

	_, err := auth.RefreshTokensWithRetry(context.Background(), "dummy_refresh_token", 1)
	if err == nil {
		t.Fatal("expected retryable refresh error")
	}

	out := logs.String()
	if !strings.Contains(out, "Token refresh attempt 1 failed:") {
		t.Fatalf("expected retryable refresh warning, got %q", out)
	}
	if strings.Contains(strings.TrimSuffix(out, "\n"), "\n") {
		t.Fatalf("expected single-line warning output, got %q", out)
	}
	if strings.Contains(out, "\n  \"error\"") {
		t.Fatalf("expected warning output to collapse multiline body, got %q", out)
	}
}

func swapCodexAuthTestLogger(dst io.Writer) func() {
	logger := log.StandardLogger()
	prevOut := logger.Out
	prevLevel := logger.Level
	prevFormatter := logger.Formatter

	logger.SetOutput(dst)
	logger.SetLevel(log.WarnLevel)
	logger.SetFormatter(&log.TextFormatter{
		DisableTimestamp: true,
		DisableColors:    true,
	})

	return func() {
		logger.SetOutput(prevOut)
		logger.SetLevel(prevLevel)
		logger.SetFormatter(prevFormatter)
	}
}
