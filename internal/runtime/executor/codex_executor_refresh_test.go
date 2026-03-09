package executor

import (
	"context"
	"strings"
	"testing"
	"time"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func TestCodexExecutorRefresh_KeepsAuthWhenRefreshTokenReusedButAccessTokenStillUsable(t *testing.T) {
	originalRefresh := refreshCodexTokensWithRetryFunc
	originalValidate := codexAccessTokenUsableFunc
	t.Cleanup(func() {
		refreshCodexTokensWithRetryFunc = originalRefresh
		codexAccessTokenUsableFunc = originalValidate
	})

	refreshCodexTokensWithRetryFunc = func(context.Context, *CodexExecutor, string, int) (*codexRefreshTokenData, error) {
		return nil, &cliproxyauth.Error{
			Code:       "refresh_token_reused",
			Message:    "token refresh failed with status 401: refresh_token_reused",
			HTTPStatus: 401,
		}
	}
	codexAccessTokenUsableFunc = func(context.Context, *CodexExecutor, *cliproxyauth.Auth, string) (bool, error) {
		return true, nil
	}

	auth := &cliproxyauth.Auth{
		ID:       "codex-refresh-keep",
		Provider: "codex",
		Metadata: map[string]any{
			"refresh_token": "refresh-token",
			"access_token":  "access-token",
			"expired":       time.Now().Add(2 * time.Hour).Format(time.RFC3339),
		},
	}

	got, err := (&CodexExecutor{}).Refresh(context.Background(), auth)
	if err != nil {
		t.Fatalf("Refresh() error = %v, want nil", err)
	}
	if got == nil {
		t.Fatal("Refresh() auth = nil")
	}
	if got.NextRefreshAfter.IsZero() {
		t.Fatal("Refresh() NextRefreshAfter = zero, want delayed retry until expiry")
	}
}

func TestCodexExecutorRefresh_ReturnsInvalidWhenRefreshTokenReusedAndAccessTokenUnusable(t *testing.T) {
	originalRefresh := refreshCodexTokensWithRetryFunc
	originalValidate := codexAccessTokenUsableFunc
	t.Cleanup(func() {
		refreshCodexTokensWithRetryFunc = originalRefresh
		codexAccessTokenUsableFunc = originalValidate
	})

	refreshCodexTokensWithRetryFunc = func(context.Context, *CodexExecutor, string, int) (*codexRefreshTokenData, error) {
		return nil, &cliproxyauth.Error{
			Code:       "refresh_token_reused",
			Message:    "token refresh failed with status 401: refresh_token_reused",
			HTTPStatus: 401,
		}
	}
	codexAccessTokenUsableFunc = func(context.Context, *CodexExecutor, *cliproxyauth.Auth, string) (bool, error) {
		return false, nil
	}

	auth := &cliproxyauth.Auth{
		ID:       "codex-refresh-delete",
		Provider: "codex",
		Metadata: map[string]any{
			"refresh_token": "refresh-token",
			"access_token":  "access-token",
		},
	}

	got, err := (&CodexExecutor{}).Refresh(context.Background(), auth)
	if got != nil {
		t.Fatalf("Refresh() auth = %v, want nil on invalid credential", got)
	}
	var authErr *cliproxyauth.Error
	if !strings.Contains(err.Error(), "refresh_token_reused") {
		t.Fatalf("Refresh() error = %v, want refresh_token_reused", err)
	}
	if !strings.Contains(strings.ToLower(err.Error()), "access token") {
		t.Fatalf("Refresh() error = %v, want access token context", err)
	}
	if err == nil || !strings.Contains(err.Error(), "refresh_token_reused") {
		t.Fatalf("Refresh() error = %v, want refresh_token_reused", err)
	}
	if e, ok := err.(*cliproxyauth.Error); ok {
		authErr = e
	}
	if authErr == nil {
		t.Fatalf("Refresh() error type = %T, want *auth.Error", err)
	}
	if authErr.HTTPStatus != 401 {
		t.Fatalf("Refresh() HTTPStatus = %d, want 401", authErr.HTTPStatus)
	}
}
