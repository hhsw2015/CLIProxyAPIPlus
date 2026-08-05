package executor

import (
	"bytes"
	"encoding/json"
	"strings"

	"github.com/tidwall/gjson"
)

// Helpers ported from upstream's claude_executor_request.go / _cloaking.go split
// files that this fork keeps as single-file executor. Any additional symbol the
// upstream refactor moved into its per-file split should be added here so the
// fork stays buildable while retaining the monolithic executor structure.

const (
	claudeFastModeBeta           = "fast-mode-2026-02-01"
	claudeTokenCountingBeta      = "token-counting-2024-11-01"
	claudeOAuthBeta              = "oauth-2025-04-20"
	claudeCodeBeta               = "claude-code-20250219"
	claudeContext1MBeta          = "context-1m-2025-08-07"
	claudeMidConvSystemBeta      = "mid-conversation-system-2026-04-07"
	claudeAdvancedToolUseBeta    = "advanced-tool-use-2025-11-20"
	claudeEffortBeta             = "effort-2025-11-24"
	claudeServerSideFallbackBeta = "server-side-fallback-2026-06-01"
	claudeFallbackCreditBeta     = "fallback-credit-2026-06-01"
	claudeStructuredOutputsBeta  = "structured-outputs-2025-12-15"
	claudeExtendedCacheTTLBeta   = "extended-cache-ttl-2025-04-11"
	claudeCacheDiagnosisBeta     = "cache-diagnosis-2026-04-07"
)

// claudeRequestedBetas parses the incoming Anthropic-Beta header plus any
// extras hoisted out of the request body into a set for cheap membership checks.
func claudeRequestedBetas(incomingBetas string, extraBetas []string) map[string]bool {
	requested := make(map[string]bool)
	for _, beta := range strings.Split(incomingBetas, ",") {
		if beta = strings.TrimSpace(beta); beta != "" {
			requested[beta] = true
		}
	}
	for _, beta := range extraBetas {
		if beta = strings.TrimSpace(beta); beta != "" {
			requested[beta] = true
		}
	}
	return requested
}

// claudeRequestUsesFastMode reports whether the caller is asking for Fast mode,
// either via the dedicated beta or the top-level `speed` field.
func claudeRequestUsesFastMode(body []byte, requested map[string]bool) bool {
	if requested[claudeFastModeBeta] {
		return true
	}
	speed := gjson.GetBytes(body, "speed")
	return speed.Type == gjson.String && strings.EqualFold(strings.TrimSpace(speed.String()), "fast")
}

// marshalJSONStringWithoutHTMLEscape encodes a Go string as a JSON string
// literal without escaping HTML-unsafe characters (`<`, `>`, `&`).
func marshalJSONStringWithoutHTMLEscape(value string) string {
	var encoded bytes.Buffer
	encoder := json.NewEncoder(&encoded)
	encoder.SetEscapeHTML(false)
	_ = encoder.Encode(value)
	return strings.TrimSuffix(encoded.String(), "\n")
}
