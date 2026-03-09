package cliproxy

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	codexauth "github.com/router-for-me/CLIProxyAPI/v6/internal/auth/codex"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/config"
	"github.com/tidwall/gjson"
)

const codexUsageProbeURL = "https://chatgpt.com/backend-api/wham/usage"

func probeCodexUsage(ctx context.Context, cfg *config.Config, auth *coreauth.Auth) coreauth.Result {
	return probeCodexUsageWithURL(ctx, cfg, auth, codexUsageProbeURL)
}

func probeCodexUsageWithURL(ctx context.Context, cfg *config.Config, auth *coreauth.Auth, usageURL string) coreauth.Result {
	result := coreauth.Result{
		Provider: "codex",
		Model:    "gpt-5",
		Success:  false,
	}
	if auth != nil {
		result.AuthID = auth.ID
		if strings.TrimSpace(auth.Provider) != "" {
			result.Provider = auth.Provider
		}
	}
	if auth == nil {
		result.Error = &coreauth.Error{Code: "auth_not_found", Message: "auth is nil"}
		return result
	}

	token := strings.TrimSpace(codexProbeToken(auth))
	if token == "" {
		result.Error = &coreauth.Error{Code: "invalid_credential", Message: "missing codex access token"}
		return result
	}

	if ctx == nil {
		ctx = context.Background()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, usageURL, nil)
	if err != nil {
		result.Error = &coreauth.Error{Code: "probe_request_error", Message: err.Error()}
		return result
	}

	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Version", "0.101.0")
	req.Header.Set("User-Agent", "codex_cli_rs/0.101.0 (pool-probe)")
	req.Header.Set("Originator", "codex_cli_rs")
	if auth.Metadata != nil {
		if accountID, ok := auth.Metadata["account_id"].(string); ok && strings.TrimSpace(accountID) != "" {
			req.Header.Set("Chatgpt-Account-Id", strings.TrimSpace(accountID))
		}
	}

	httpClient := &http.Client{Timeout: 20 * time.Second}
	if cfg != nil {
		util.SetProxy(&cfg.SDKConfig, httpClient)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		result.Error = &coreauth.Error{
			Code:      "probe_network_error",
			Message:   err.Error(),
			Retryable: true,
		}
		return result
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		result.Error = &coreauth.Error{
			Code:      "probe_read_error",
			Message:   err.Error(),
			Retryable: true,
		}
		return result
	}

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		result.Success = true
		return result
	}

	probeErr := &coreauth.Error{
		Message:    strings.TrimSpace(string(body)),
		Retryable:  resp.StatusCode >= 500,
		HTTPStatus: resp.StatusCode,
	}
	if resp.StatusCode == http.StatusUnauthorized {
		probeErr.Code = "unauthorized"
	} else if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusPaymentRequired {
		probeErr.Code = "quota_exceeded"
		if retryAfter := parseCodexUsageRetryAfter(body, time.Now()); retryAfter != nil {
			result.RetryAfter = retryAfter
		}
	} else {
		probeErr.Code = fmt.Sprintf("http_%d", resp.StatusCode)
	}
	result.Error = probeErr
	return result
}

func codexProbeToken(auth *coreauth.Auth) string {
	if auth == nil {
		return ""
	}
	if auth.Attributes != nil {
		if token := strings.TrimSpace(auth.Attributes["api_key"]); token != "" {
			return token
		}
	}
	if auth.Metadata != nil {
		if token, ok := auth.Metadata["access_token"].(string); ok {
			return strings.TrimSpace(token)
		}
	}
	return ""
}

func parseCodexUsageRetryAfter(body []byte, now time.Time) *time.Duration {
	if strings.TrimSpace(gjson.GetBytes(body, "error.type").String()) != "usage_limit_reached" {
		return nil
	}
	if resetsAt := gjson.GetBytes(body, "error.resets_at").Int(); resetsAt > 0 {
		resetAtTime := time.Unix(resetsAt, 0)
		if resetAtTime.After(now) {
			retryAfter := resetAtTime.Sub(now)
			return &retryAfter
		}
	}
	if resetsInSeconds := gjson.GetBytes(body, "error.resets_in_seconds").Int(); resetsInSeconds > 0 {
		retryAfter := time.Duration(resetsInSeconds) * time.Second
		return &retryAfter
	}
	return nil
}

func refreshCodexAuthTokens(ctx context.Context, cfg *config.Config, auth *coreauth.Auth) (*codexauth.CodexTokenData, error) {
	if auth == nil || auth.Metadata == nil {
		return nil, fmt.Errorf("codex probe: auth metadata missing")
	}
	refreshToken, _ := auth.Metadata["refresh_token"].(string)
	refreshToken = strings.TrimSpace(refreshToken)
	if refreshToken == "" {
		return nil, fmt.Errorf("codex probe: refresh token missing")
	}
	if cfg == nil {
		cfg = &config.Config{}
	}
	svc := codexauth.NewCodexAuth(cfg)
	return svc.RefreshTokensWithRetry(ctx, refreshToken, 3)
}
