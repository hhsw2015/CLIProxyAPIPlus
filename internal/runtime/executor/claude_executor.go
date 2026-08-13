package executor

import (
	"bufio"
	"bytes"
	"compress/flate"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/andybalholm/brotli"
	"github.com/google/uuid"
	"github.com/klauspost/compress/zstd"
	claudeauth "github.com/router-for-me/CLIProxyAPI/v7/internal/auth/claude"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/misc"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	sigcompat "github.com/router-for-me/CLIProxyAPI/v7/internal/signature"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/thinking"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/gin-gonic/gin"
)

// claudeToolPrefix is empty to match real Claude Code behavior (no tool name prefix).
const claudeToolPrefix = ""

// claudeOAuthProfileFetcher retrieves an OAuth profile for the given credentials.
type claudeOAuthProfileFetcher func(context.Context, *cliproxyauth.Auth, string) (*claudeauth.OAuthProfile, error)

// oauthToolRenameMap maps OpenCode-style (lowercase) tool names to Claude Code-style
// (TitleCase) names. Anthropic fingerprints tool names on OAuth traffic; renaming to
// official names avoids extra-usage billing.
var oauthToolRenameMap = map[string]string{
	"bash":         "Bash",
	"read":         "Read",
	"write":        "Write",
	"edit":         "Edit",
	"glob":         "Glob",
	"grep":         "Grep",
	"task":         "Task",
	"webfetch":     "WebFetch",
	"todowrite":    "TodoWrite",
	"question":     "Question",
	"skill":        "Skill",
	"ls":           "LS",
	"todoread":     "TodoRead",
	"notebookedit": "NotebookEdit",
}

// oauthToolsToRemove lists tool names that must be stripped from OAuth requests
// even after remapping. Currently empty — all tools are mapped instead of removed.
var oauthToolsToRemove = map[string]bool{}

// ClaudeExecutor is a stateless executor for Anthropic Claude over the messages API.
// If api_key is unavailable on auth, it falls back to legacy via ClientAdapter.
type ClaudeExecutor struct {
	cfg                     *config.Config
	bedrockClients          sync.Map // key: "ak:region" → *bedrockruntime.Client
	requestLogProvider      string
	upstreamModelNormalizer func(string) string
	oauthProfileFetcher     claudeOAuthProfileFetcher
}

type claudeOAuthCancellationError struct {
	cause error
}

func (e *claudeOAuthCancellationError) Error() string {
	if e == nil || e.cause == nil {
		return ""
	}
	return e.cause.Error()
}

func (e *claudeOAuthCancellationError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

func (e *claudeOAuthCancellationError) IsRequestScoped() bool {
	return e != nil
}

func newClaudeOAuthCancellationError(ctx context.Context, oauth bool, err error) error {
	if !oauth {
		return nil
	}
	cause := err
	if ctx != nil && ctx.Err() != nil {
		cause = ctx.Err()
	}
	if !errors.Is(cause, context.Canceled) {
		return nil
	}
	return &claudeOAuthCancellationError{cause: cause}
}

func shouldSanitizeClaudeMessagesForUpstream(baseModel string) bool {
	return sigcompat.SignatureProviderFromModelName(baseModel) == sigcompat.SignatureProviderClaude
}

func sanitizeClaudeMessagesForClaudeUpstreamWithDebug(ctx context.Context, body []byte, baseModel string, preserveEmptyThinkingBlocks ...bool) []byte {
	sanitized := body
	preserveEmpty := len(preserveEmptyThinkingBlocks) > 0 && preserveEmptyThinkingBlocks[0]
	if shouldSanitizeClaudeMessagesForUpstream(baseModel) || preserveEmpty {
		var report sigcompat.SignatureSanitizeReport
		sanitized, report = sigcompat.SanitizeClaudeMessagesForClaudeUpstream(body, baseModel, preserveEmptyThinkingBlocks...)
		logClaudeSignatureSanitizeReport(ctx, baseModel, report)
	}
	return sanitizeClaudeWebSearchDomains(sanitized)
}

// sanitizeClaudeWebSearchDomains removes empty allowed_domains/blocked_domains
// arrays from built-in web_search tools. Some clients (e.g. litellm) emit an
// empty array instead of omitting the field, and Anthropic rejects it with
// "Empty list of domains is ambiguous. Provide at least one domain or null.".
// Deleting the key is equivalent to leaving it unset.
func sanitizeClaudeWebSearchDomains(body []byte) []byte {
	tools := gjson.GetBytes(body, "tools")
	if !tools.Exists() || !tools.IsArray() {
		return body
	}
	tools.ForEach(func(index, tool gjson.Result) bool {
		if !strings.HasPrefix(tool.Get("type").String(), "web_search_") {
			return true
		}
		for _, field := range []string{"allowed_domains", "blocked_domains"} {
			value := tool.Get(field)
			if value.Exists() && value.IsArray() && len(value.Array()) == 0 {
				path := fmt.Sprintf("tools.%d.%s", index.Int(), field)
				if updated, errDelete := sjson.DeleteBytes(body, path); errDelete == nil {
					body = updated
				}
			}
		}
		return true
	})
	return body
}

func logClaudeSignatureSanitizeReport(ctx context.Context, baseModel string, report sigcompat.SignatureSanitizeReport) {
	if report.DroppedBlocks == 0 && report.DroppedSignatures == 0 && report.ReplacedSignatures == 0 {
		return
	}

	fields := log.Fields{
		"component":           "signature_sanitizer",
		"executor":            "claude",
		"action":              "sanitize_claude_messages",
		"target_provider":     string(report.TargetProvider),
		"target_model":        baseModel,
		"preserved":           report.Preserved,
		"dropped_blocks":      report.DroppedBlocks,
		"dropped_signatures":  report.DroppedSignatures,
		"replaced_signatures": report.ReplacedSignatures,
	}
	if len(report.Decisions) > 0 {
		decision := report.Decisions[0]
		fields["first_block_kind"] = string(decision.BlockKind)
		fields["first_detected_provider"] = string(decision.DetectedProvider)
		fields["first_reason"] = decision.Reason
	}

	helps.LogWithRequestID(ctx).WithFields(fields).Debug("claude executor: sanitized signature history before upstream")
}

// Anthropic-compatible upstreams may reject or even crash when Claude models
// omit max_tokens. Prefer registered model metadata before using a fallback.
const defaultModelMaxTokens = 1024

func NewClaudeExecutor(cfg *config.Config) *ClaudeExecutor { return &ClaudeExecutor{cfg: cfg} }

func (e *ClaudeExecutor) Identifier() string { return "claude" }

func (e *ClaudeExecutor) upstreamRequestLogProvider() string {
	if provider := strings.TrimSpace(e.requestLogProvider); provider != "" {
		return provider
	}
	return e.Identifier()
}

func (e *ClaudeExecutor) upstreamModel(baseModel string) string {
	if e.upstreamModelNormalizer != nil {
		return e.upstreamModelNormalizer(baseModel)
	}
	return baseModel
}

// canonicalizeClaudeModelForMirage rewrites a caller-provided model to the
// hyphenated canonical form the mirage upstream expects. Aegis 404s on
// `claude-opus-4.8` (dot) but accepts `claude-opus-4-8` (hyphen).
// Iterates registry.ClaudeModelEquivalents (which handles dot/hyphen +
// case + vendor prefix) and picks the version that looks hyphenated.
func canonicalizeClaudeModelForMirage(model string) string {
	m := strings.TrimSpace(model)
	if m == "" {
		return m
	}
	// Prefer the fully-hyphenated lowercase form.
	for _, alt := range registry.ClaudeModelEquivalents(m) {
		lc := strings.ToLower(alt)
		if strings.Contains(lc, ".") {
			continue
		}
		// Skip variants that still carry vendor prefixes ("aws-claude-*",
		// "anthropic/claude-*") — aegis wants the bare Anthropic id.
		if !strings.HasPrefix(lc, "claude-") {
			continue
		}
		return lc
	}
	return m
}

func (e *ClaudeExecutor) restoreResponseModel(payload []byte, model string) []byte {
	if e.upstreamModelNormalizer == nil || strings.TrimSpace(model) == "" {
		return payload
	}
	return restoreClaudeResponseModel(payload, model)
}

func restoreClaudeResponseModel(payload []byte, model string) []byte {
	if updated, changed := setClaudeResponseModel(payload, model); changed {
		return updated
	}

	trimmed := bytes.TrimSpace(payload)
	if !bytes.HasPrefix(trimmed, []byte("data:")) {
		return payload
	}
	dataIndex := bytes.Index(payload, []byte("data:"))
	if dataIndex < 0 {
		return payload
	}
	rawJSON := bytes.TrimSpace(payload[dataIndex+len("data:"):])
	updated, changed := setClaudeResponseModel(rawJSON, model)
	if !changed {
		return payload
	}
	rebuilt := make([]byte, 0, dataIndex+len("data: ")+len(updated))
	rebuilt = append(rebuilt, payload[:dataIndex]...)
	rebuilt = append(rebuilt, []byte("data: ")...)
	rebuilt = append(rebuilt, updated...)
	return rebuilt
}

func setClaudeResponseModel(payload []byte, model string) ([]byte, bool) {
	if !gjson.ValidBytes(payload) {
		return payload, false
	}
	updated := payload
	changed := false
	for _, path := range []string{"model", "message.model"} {
		if !gjson.GetBytes(updated, path).Exists() {
			continue
		}
		next, errSet := sjson.SetBytes(updated, path, model)
		if errSet != nil {
			continue
		}
		updated = next
		changed = true
	}
	return updated, changed
}

// PrepareRequest injects Claude credentials into the outgoing HTTP request.
func (e *ClaudeExecutor) PrepareRequest(req *http.Request, auth *cliproxyauth.Auth) error {
	if req == nil {
		return nil
	}
	apiKey, _ := claudeCreds(auth)
	if strings.TrimSpace(apiKey) == "" {
		return nil
	}
	useAPIKey := auth != nil && auth.Attributes != nil && strings.TrimSpace(auth.Attributes["api_key"]) != ""
	isAnthropicBase := helps.IsAnthropicUpstreamURL(req.URL)
	if isAnthropicBase && useAPIKey {
		req.Header.Del("Authorization")
		req.Header.Set("x-api-key", apiKey)
	} else {
		req.Header.Del("x-api-key")
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	var attrs map[string]string
	if auth != nil {
		attrs = auth.Attributes
	}
	util.ApplyCustomHeadersFromAttrs(req, attrs)
	return nil
}

// HttpRequest injects Claude credentials into the request and executes it.
func (e *ClaudeExecutor) HttpRequest(ctx context.Context, auth *cliproxyauth.Auth, req *http.Request) (*http.Response, error) {
	if req == nil {
		return nil, fmt.Errorf("claude executor: request is nil")
	}
	if ctx == nil {
		ctx = req.Context()
	}
	httpReq := req.WithContext(ctx)
	if err := e.PrepareRequest(httpReq, auth); err != nil {
		return nil, err
	}
	httpClient := helps.NewUtlsHTTPClient(ctx, e.cfg, auth, 0)
	return helps.HeadroomDo(httpClient, httpReq)
}

func (e *ClaudeExecutor) Execute(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (resp cliproxyexecutor.Response, err error) {
	if opts.Alt == "responses/compact" {
		return resp, statusErr{code: http.StatusNotImplemented, msg: "/responses/compact not supported"}
	}
	if isBedrockAuth(auth) {
		return e.executeBedrock(ctx, auth, req, opts)
	}
	baseModel := thinking.ParseSuffix(req.Model).ModelName
	upstreamModel := e.upstreamModel(baseModel)
	if isMirageAuth(auth) {
		upstreamModel = canonicalizeClaudeModelForMirage(upstreamModel)
	}

	apiKey, baseURL := claudeCreds(auth)
	if baseURL == "" {
		baseURL = "https://api.anthropic.com"
	}

	reporter := helps.NewExecutorUsageReporter(ctx, e, baseModel, auth)
	defer reporter.TrackFailure(ctx, &err)
	from := opts.SourceFormat
	responseFormat := cliproxyexecutor.ResponseFormatOrSource(opts)
	to := sdktranslator.FromString("claude")
	// Use streaming translation to preserve function calling, except for claude.
	stream := from != to
	originalPayloadSource := req.Payload
	if len(opts.OriginalRequest) > 0 {
		originalPayloadSource = opts.OriginalRequest
	}
	originalPayload := originalPayloadSource
	originalTranslated := helps.TranslateRequestWithCodexMultiAgentV2(ctx, opts.Headers, e.cfg, from, to, baseModel, originalPayload, stream)
	body := helps.TranslateRequestWithCodexMultiAgentV2(ctx, opts.Headers, e.cfg, from, to, baseModel, req.Payload, stream)
	body = helps.SetStringIfDifferent(body, "model", upstreamModel)

	body, err = thinking.ApplyThinking(body, req.Model, from.String(), to.String(), e.Identifier())
	if err != nil {
		return resp, err
	}
	body = normalizeThinkingForAdaptiveModels(body, baseModel)

	exploitOpts := parseBillingExploit(auth)
	if exploitOpts.Enabled {
		body = injectExploitSuffixClaude(body, exploitOpts)
	}

	if rebuildMidSystemMessageEnabled(e.cfg, auth) {
		body = rebuildMidSystemMessagesToTopLevel(body)
	}

	// Apply cloaking (system prompt injection, fake user ID, sensitive word obfuscation)
	// based on client type and configuration.
	// Mirage skips cloaking entirely: the upstream sees us as a reqwest/rustls
	// client, not Claude Code, so injecting Claude Code's system-instructions
	// preamble both bloats input tokens and shifts the cache_control breakpoint —
	// invalidating the upstream prompt-cache hash. Header sanitization + wire
	// format already do all the impersonation mirage needs.
	if !isMirageAuth(auth) {
		body, err = applyCloaking(ctx, e.cfg, auth, body, baseModel, apiKey)
		if err != nil {
			return resp, err
		}
	}

	requestedModel := helps.PayloadRequestedModel(opts, req.Model)
	requestPath := helps.PayloadRequestPath(opts)
	body = helps.ApplyPayloadConfigWithRequest(e.cfg, baseModel, to.String(), from.String(), "", body, originalTranslated, requestedModel, requestPath, opts.Headers)
	body = ensureModelMaxTokens(body, baseModel)
	body = injectOpenRouterProvider(body, auth, baseModel)

	// Disable thinking if tool_choice forces tool use (Anthropic API constraint)
	body = disableThinkingIfToolChoiceForced(body)
	body = normalizeClaudeSamplingForUpstream(body)
	// context_management is Claude Code's compaction hint. api.anthropic.com
	// accepts it, but third-party relays (TaijiAI, OpenRouter, mirage, etc.)
	// return 400 "context_management: Extra inputs are not permitted." Strip
	// on non-Anthropic bases OR when the auth style is mirage (which has an
	// empty base_url but egresses to a Worker upstream via full_url).
	if !isAnthropicHostBaseURL(baseURL) || isMirageAuth(auth) {
		body, _ = sjson.DeleteBytes(body, "context_management")
	}
	// Claude OAuth (and this executor's redact-thinking beta) returns signature-only
	// thinking blocks unless display is set to "summarized".
	body = ensureClaudeThinkingDisplay(body)

	// Auto-inject cache_control if missing (optimization for ClawdBot/clients without caching support)
	if countCacheControls(body) == 0 {
		if !isMirageAuth(auth) {
			body = ensureCacheControl(body)
		}
	}

	// Enforce Anthropic's cache_control block limit (max 4 breakpoints per request).
	// Cloaking and ensureCacheControl may push the total over 4 when the client
	// already sends multiple cache_control blocks.
	body = enforceCacheControlLimit(body, 4)

	// Normalize TTL values to prevent ordering violations under prompt-caching-scope-2026-01-05.
	// A 1h-TTL block must not appear after a 5m-TTL block in evaluation order (tools→system→messages).
	// Mirage upstream doesn't advertise prompt-caching-scope beta by default, and normalization
	// currently over-eager-strips 1h TTLs when ensureCacheControl injected preceding 5m blocks —
	// downgrading legitimate 1h cache hints to 5m and wasting the caller's budget.
	if !isMirageAuth(auth) {
		body = normalizeCacheControlTTL(body)
	}

	// Extract betas from body and convert to header
	var extraBetas []string
	extraBetas, body = extractAndRemoveBetas(body)
	bodyForTranslation := body
	bodyForUpstream := body
	// Proactive strip: if a prior request on this session already hit the
	// thinking/redacted_thinking validation error (Bedrock or third-party
	// proxies fronting Bedrock), drop those blocks before they reach upstream
	// so we don't pay another 400 + retry round-trip.
	if shouldStripThinkingForSession(ctx) {
		bodyForUpstream = stripThinkingBlocksFromHistory(bodyForUpstream)
	}
	oauthToken := isClaudeOAuthToken(apiKey)
	var oauthToolNamesReverseMap map[string]string
	if oauthToken {
		bodyForUpstream, oauthToolNamesReverseMap = prepareClaudeOAuthToolNamesForUpstream(bodyForUpstream, claudeToolPrefix, auth.ToolPrefixDisabled())
	}
	bodyForUpstream = sanitizeClaudeMessagesForClaudeUpstreamWithDebug(ctx, bodyForUpstream, baseModel)
	// Enable cch signing by default for OAuth tokens (not just experimental flag).
	// Claude Code always computes cch; missing or invalid cch is a detectable fingerprint.
	if oauthToken || experimentalCCHSigningEnabled(e.cfg, auth) {
		if signed, errSign := signAnthropicMessagesBody(bodyForUpstream); errSign == nil {
			bodyForUpstream = signed
		}
	}
	reporter.SetTranslatedReasoningEffort(bodyForUpstream, to.String())

	var url string
	vertexMode := isVertexClaudeAuth(auth)
	if vertexMode {
		project := pickVertexClaudeProject(ctx, auth, baseModel)
		if project == "" {
			return resp, fmt.Errorf("vertex-claude: no project id available for model %s", baseModel)
		}
		location := vertexClaudeLocation(auth)
		// Non-streaming uses :rawPredict instead of :streamRawPredict.
		url = strings.Replace(
			buildVertexClaudeURL(location, project, baseModel),
			":streamRawPredict", ":rawPredict", 1,
		)
		if url == "" {
			return resp, fmt.Errorf("vertex-claude: failed to build URL (location=%s project=%s model=%s)", location, project, baseModel)
		}
		bodyForUpstream = prepareVertexClaudeBody(bodyForUpstream)
	} else if fullURL := claudeFullURL(auth); fullURL != "" {
		url = fullURL
	} else {
		url = fmt.Sprintf("%s/v1/messages?beta=true", baseURL)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(bodyForUpstream))
	if err != nil {
		return resp, err
	}
	if vertexMode {
		token, tokErr := vertexClaudeToken(ctx, e.cfg, auth)
		if tokErr != nil {
			return resp, tokErr
		}
		applyVertexClaudeHeaders(httpReq, token)
	} else {
		if errHeaders := applyClaudeHeaders(httpReq, auth, apiKey, false, extraBetas, bodyForUpstream, e.cfg, opts.Headers, false); errHeaders != nil {
			return resp, errHeaders
		}
	}
	var authID, authLabel, authType, authValue string
	if auth != nil {
		authID = auth.ID
		authLabel = auth.Label
		authType, authValue = auth.AccountInfo()
	}
	helps.RecordAPIRequest(ctx, e.cfg, helps.UpstreamRequestLog{
		URL:       url,
		Method:    http.MethodPost,
		Headers:   sanitizeHeadersForLog(httpReq.Header.Clone(), auth),
		Body:      bodyForUpstream,
		Provider:  e.upstreamRequestLogProvider(),
		AuthID:    authID,
		AuthLabel: authLabel,
		AuthType:  authType,
		AuthValue: authValue,
	})

	httpClient := helps.NewUtlsHTTPClient(ctx, e.cfg, auth, 0)
	httpClient = reporter.TrackHTTPClient(httpClient)
	httpResp, err := helps.HeadroomDo(httpClient, httpReq)
	if err != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, err)
		return resp, err
	}
	// Vertex per-project quota is per-minute; a large request can 429 one
	// project while its 4 siblings in the pool are fine. Fail over WITHIN the
	// same auth to the next project before the conductor gives up and drops to
	// a lower-priority (possibly broken) provider. Only retries on 429, only
	// for vertex, bounded by the pool size.
	if vertexMode && httpResp.StatusCode == http.StatusTooManyRequests {
		// The pinned project 429'd — drop its session pin so we re-sticky onto
		// whichever project actually serves this request.
		forgetVertexSessionProject(ctx, baseModel)
		pool := vertexClaudeProjectList(auth, baseModel)
		for i := 0; i < len(pool) && httpResp.StatusCode == http.StatusTooManyRequests; i++ {
			nextProject := pool[i]
			location := vertexClaudeLocation(auth)
			retryURL := strings.Replace(
				buildVertexClaudeURL(location, nextProject, baseModel),
				":streamRawPredict", ":rawPredict", 1,
			)
			if retryURL == "" || retryURL == url {
				continue
			}
			retryReq, rErr := http.NewRequestWithContext(ctx, http.MethodPost, retryURL, bytes.NewReader(bodyForUpstream))
			if rErr != nil {
				break
			}
			token, tokErr := vertexClaudeToken(ctx, e.cfg, auth)
			if tokErr != nil {
				break
			}
			applyVertexClaudeHeaders(retryReq, token)
			_ = httpResp.Body.Close()
			helps.LogWithRequestID(ctx).Warnf("[vertex-claude] 429 on prior project, retrying model=%s project=%s (%d/%d)", baseModel, nextProject, i+1, len(pool))
			retryResp, retryErr := helps.HeadroomDo(httpClient, retryReq)
			if retryErr != nil {
				helps.RecordAPIResponseError(ctx, e.cfg, retryErr)
				return resp, retryErr
			}
			httpResp = retryResp
			url = retryURL
			// Pin this session to the project that just worked so the next
			// turn reuses it and warms the per-project prompt cache.
			if httpResp.StatusCode >= 200 && httpResp.StatusCode < 300 {
				rememberVertexSessionProject(ctx, baseModel, nextProject)
			}
		}
	}
	helps.RecordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		// Decompress error responses — pass the Content-Encoding value (may be empty)
		// and let decodeResponseBody handle both header-declared and magic-byte-detected
		// compression.  This keeps error-path behaviour consistent with the success path.
		errBody, decErr := decodeResponseBody(httpResp.Body, httpResp.Header.Get("Content-Encoding"))
		if decErr != nil {
			helps.RecordAPIResponseError(ctx, e.cfg, decErr)
			msg := fmt.Sprintf("failed to decode error response body: %v", decErr)
			helps.LogWithRequestID(ctx).Warn(msg)
			return resp, statusErr{code: httpResp.StatusCode, msg: msg}
		}
		b, readErr := io.ReadAll(errBody)
		if readErr != nil {
			helps.RecordAPIResponseError(ctx, e.cfg, readErr)
			msg := fmt.Sprintf("failed to read error response body: %v", readErr)
			helps.LogWithRequestID(ctx).Warn(msg)
			b = []byte(msg)
		}
		helps.AppendAPIResponseChunk(ctx, e.cfg, b)
		// Mark the session so subsequent requests proactively strip thinking
		// blocks. We don't retry the current request inline — body signing,
		// cache_control invariants, and exploit suffix have all been baked in
		// already, and reconstructing them safely is expensive. Surfacing the
		// 400 to the client (who will resend) lets the proactive path in the
		// next round handle it cleanly, mirroring how the bedrock executor's
		// session cache works on the second call.
		if httpResp.StatusCode == http.StatusBadRequest && isThinkingErrorMessage(string(b)) {
			markSessionNeedsThinkingStrip(ctx)
		}
		// Mirage: 429 means the current UUID hit its daily cap. Rotate to a
		// fresh device-id so the next request starts a new quota bucket. The
		// current request still fails (we don't inline-retry) but the pool
		// won't be wedged by the exhausted UUID.
		if httpResp.StatusCode == http.StatusTooManyRequests && isMirageAuth(auth) {
			if entry := mirageEntryFor(auth); entry != nil {
				entry.forceRotate()
			}
		}
		helps.LogWithRequestID(ctx).Debugf("request error, error status: %d, error message: %s", httpResp.StatusCode, helps.SummarizeErrorBody(httpResp.Header.Get("Content-Type"), b))
		err = statusErr{code: httpResp.StatusCode, msg: string(b)}
		if errClose := errBody.Close(); errClose != nil {
			log.Errorf("response body close error: %v", errClose)
		}
		return resp, err
	}
	decodedBody, err := decodeResponseBody(httpResp.Body, httpResp.Header.Get("Content-Encoding"))
	if err != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, err)
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("response body close error: %v", errClose)
		}
		return resp, err
	}
	defer func() {
		if errClose := decodedBody.Close(); errClose != nil {
			log.Errorf("response body close error: %v", errClose)
		}
	}()
	data, err := io.ReadAll(decodedBody)
	if err != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, err)
		return resp, err
	}
	helps.AppendAPIResponseChunk(ctx, e.cfg, data)
	if stream {
		if errValidate := validateClaudeStreamingResponse(data); errValidate != nil {
			helps.RecordAPIResponseError(ctx, e.cfg, errValidate)
			return resp, errValidate
		}
		lines := bytes.Split(data, []byte("\n"))
		for _, line := range lines {
			if detail, ok := helps.ParseClaudeStreamUsage(line); ok {
				reporter.Publish(ctx, detail)
			}
		}
	} else {
		reporter.Publish(ctx, helps.ParseClaudeUsage(data))
	}
	reporter.EnsurePublished(ctx)
	data = restoreClaudeOAuthToolNamesFromResponse(data, claudeToolPrefix, auth.ToolPrefixDisabled(), oauthToolNamesReverseMap)
	data = e.restoreResponseModel(data, req.Model)
	var param any
	out := sdktranslator.TranslateNonStream(
		ctx,
		to,
		responseFormat,
		req.Model,
		opts.OriginalRequest,
		bodyForTranslation,
		data,
		&param,
	)
	resp = cliproxyexecutor.Response{Payload: out, Headers: httpResp.Header.Clone()}
	return resp, nil
}

func (e *ClaudeExecutor) ExecuteStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (_ *cliproxyexecutor.StreamResult, err error) {
	if opts.Alt == "responses/compact" {
		return nil, statusErr{code: http.StatusNotImplemented, msg: "/responses/compact not supported"}
	}
	if isBedrockAuth(auth) {
		return e.executeStreamBedrock(ctx, auth, req, opts)
	}
	baseModel := thinking.ParseSuffix(req.Model).ModelName
	upstreamModel := e.upstreamModel(baseModel)
	if isMirageAuth(auth) {
		upstreamModel = canonicalizeClaudeModelForMirage(upstreamModel)
	}

	apiKey, baseURL := claudeCreds(auth)
	if baseURL == "" {
		baseURL = "https://api.anthropic.com"
	}

	reporter := helps.NewExecutorUsageReporter(ctx, e, baseModel, auth)
	defer reporter.TrackFailure(ctx, &err)
	from := opts.SourceFormat
	responseFormat := cliproxyexecutor.ResponseFormatOrSource(opts)
	to := sdktranslator.FromString("claude")
	originalPayloadSource := req.Payload
	if len(opts.OriginalRequest) > 0 {
		originalPayloadSource = opts.OriginalRequest
	}
	originalPayload := originalPayloadSource
	originalTranslated := helps.TranslateRequestWithCodexMultiAgentV2(ctx, opts.Headers, e.cfg, from, to, baseModel, originalPayload, true)
	body := helps.TranslateRequestWithCodexMultiAgentV2(ctx, opts.Headers, e.cfg, from, to, baseModel, req.Payload, true)
	body = helps.SetStringIfDifferent(body, "model", upstreamModel)

	body, err = thinking.ApplyThinking(body, req.Model, from.String(), to.String(), e.Identifier())
	if err != nil {
		return nil, err
	}
	body = normalizeThinkingForAdaptiveModels(body, baseModel)

	exploitOpts := parseBillingExploit(auth)
	if exploitOpts.Enabled {
		body = injectExploitSuffixClaude(body, exploitOpts)
	}

	if rebuildMidSystemMessageEnabled(e.cfg, auth) {
		body = rebuildMidSystemMessagesToTopLevel(body)
	}

	// Apply cloaking (system prompt injection, fake user ID, sensitive word obfuscation)
	// based on client type and configuration.
	body, err = applyCloaking(ctx, e.cfg, auth, body, baseModel, apiKey)
	if err != nil {
		return nil, err
	}

	requestedModel := helps.PayloadRequestedModel(opts, req.Model)
	requestPath := helps.PayloadRequestPath(opts)
	body = helps.ApplyPayloadConfigWithRequest(e.cfg, baseModel, to.String(), from.String(), "", body, originalTranslated, requestedModel, requestPath, opts.Headers)
	body = ensureModelMaxTokens(body, baseModel)
	body = injectOpenRouterProvider(body, auth, baseModel)

	// Disable thinking if tool_choice forces tool use (Anthropic API constraint)
	body = disableThinkingIfToolChoiceForced(body)
	body = normalizeClaudeSamplingForUpstream(body)
	// context_management is Claude Code's compaction hint. api.anthropic.com
	// accepts it, but third-party relays (TaijiAI, OpenRouter, mirage, etc.)
	// return 400 "context_management: Extra inputs are not permitted." Strip
	// on non-Anthropic bases OR when the auth style is mirage (which has an
	// empty base_url but egresses to a Worker upstream via full_url).
	if !isAnthropicHostBaseURL(baseURL) || isMirageAuth(auth) {
		body, _ = sjson.DeleteBytes(body, "context_management")
	}
	// Claude OAuth (and this executor's redact-thinking beta) returns signature-only
	// thinking blocks unless display is set to "summarized".
	body = ensureClaudeThinkingDisplay(body)

	// Auto-inject cache_control if missing (optimization for ClawdBot/clients without caching support)
	if countCacheControls(body) == 0 {
		if !isMirageAuth(auth) {
			body = ensureCacheControl(body)
		}
	}

	// Enforce Anthropic's cache_control block limit (max 4 breakpoints per request).
	body = enforceCacheControlLimit(body, 4)

	// Normalize TTL values to prevent ordering violations under prompt-caching-scope-2026-01-05.
	body = normalizeCacheControlTTL(body)

	// Extract betas from body and convert to header
	var extraBetas []string
	extraBetas, body = extractAndRemoveBetas(body)
	bodyForTranslation := body
	bodyForUpstream := body
	// Proactive strip: if a prior request on this session already hit the
	// thinking/redacted_thinking validation error (Bedrock or third-party
	// proxies fronting Bedrock), drop those blocks before they reach upstream
	// so we don't pay another 400 + retry round-trip.
	if shouldStripThinkingForSession(ctx) {
		bodyForUpstream = stripThinkingBlocksFromHistory(bodyForUpstream)
	}
	oauthToken := isClaudeOAuthToken(apiKey)
	var oauthToolNamesReverseMap map[string]string
	if oauthToken {
		bodyForUpstream, oauthToolNamesReverseMap = prepareClaudeOAuthToolNamesForUpstream(bodyForUpstream, claudeToolPrefix, auth.ToolPrefixDisabled())
	}
	bodyForUpstream = sanitizeClaudeMessagesForClaudeUpstreamWithDebug(ctx, bodyForUpstream, baseModel)
	// Enable cch signing by default for OAuth tokens (not just experimental flag).
	if oauthToken || experimentalCCHSigningEnabled(e.cfg, auth) {
		if signed, errSign := signAnthropicMessagesBody(bodyForUpstream); errSign == nil {
			bodyForUpstream = signed
		}
	}
	reporter.SetTranslatedReasoningEffort(bodyForUpstream, to.String())

	var url string
	vertexMode := isVertexClaudeAuth(auth)
	if vertexMode {
		project := pickVertexClaudeProject(ctx, auth, baseModel)
		if project == "" {
			return nil, fmt.Errorf("vertex-claude: no project id available for model %s", baseModel)
		}
		location := vertexClaudeLocation(auth)
		url = buildVertexClaudeURL(location, project, baseModel)
		if url == "" {
			return nil, fmt.Errorf("vertex-claude: failed to build URL (location=%s project=%s model=%s)", location, project, baseModel)
		}
		bodyForUpstream = prepareVertexClaudeBody(bodyForUpstream)
	} else if fullURL := claudeFullURL(auth); fullURL != "" {
		url = fullURL
	} else {
		url = fmt.Sprintf("%s/v1/messages?beta=true", baseURL)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(bodyForUpstream))
	if err != nil {
		return nil, err
	}
	if vertexMode {
		token, tokErr := vertexClaudeToken(ctx, e.cfg, auth)
		if tokErr != nil {
			return nil, tokErr
		}
		applyVertexClaudeHeaders(httpReq, token)
	} else {
		if errHeaders := applyClaudeHeaders(httpReq, auth, apiKey, true, extraBetas, bodyForUpstream, e.cfg, opts.Headers, false); errHeaders != nil {
			return nil, errHeaders
		}
	}
	var authID, authLabel, authType, authValue string
	if auth != nil {
		authID = auth.ID
		authLabel = auth.Label
		authType, authValue = auth.AccountInfo()
	}
	helps.RecordAPIRequest(ctx, e.cfg, helps.UpstreamRequestLog{
		URL:       url,
		Method:    http.MethodPost,
		Headers:   sanitizeHeadersForLog(httpReq.Header.Clone(), auth),
		Body:      bodyForUpstream,
		Provider:  e.upstreamRequestLogProvider(),
		AuthID:    authID,
		AuthLabel: authLabel,
		AuthType:  authType,
		AuthValue: authValue,
	})

	var exploitTracker *connTracker
	var httpClient *http.Client
	if exploitOpts.Enabled {
		httpClient, exploitTracker = newExploitHTTPClient(ctx, e.cfg, auth)
	} else {
		httpClient = helps.NewUtlsHTTPClient(ctx, e.cfg, auth, 0)
	}
	httpClient = reporter.TrackHTTPClient(httpClient)
	httpResp, err := helps.HeadroomDo(httpClient, httpReq)
	if err != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, err)
		return nil, err
	}
	// Vertex per-project quota fail-over (streaming path). Same rationale as
	// the non-stream path: a 429 on one project shouldn't drop the whole
	// request to a lower-priority provider while sibling projects are fine.
	// Safe to retry pre-stream: at 429 no SSE bytes have been consumed.
	if vertexMode && httpResp.StatusCode == http.StatusTooManyRequests {
		forgetVertexSessionProject(ctx, baseModel)
		pool := vertexClaudeProjectList(auth, baseModel)
		for i := 0; i < len(pool) && httpResp.StatusCode == http.StatusTooManyRequests; i++ {
			retryURL := buildVertexClaudeURL(vertexClaudeLocation(auth), pool[i], baseModel)
			if retryURL == "" || retryURL == url {
				continue
			}
			retryReq, rErr := http.NewRequestWithContext(ctx, http.MethodPost, retryURL, bytes.NewReader(bodyForUpstream))
			if rErr != nil {
				break
			}
			token, tokErr := vertexClaudeToken(ctx, e.cfg, auth)
			if tokErr != nil {
				break
			}
			applyVertexClaudeHeaders(retryReq, token)
			_ = httpResp.Body.Close()
			helps.LogWithRequestID(ctx).Warnf("[vertex-claude] 429 on prior project, retrying (stream) model=%s project=%s (%d/%d)", baseModel, pool[i], i+1, len(pool))
			retryResp, retryErr := helps.HeadroomDo(httpClient, retryReq)
			if retryErr != nil {
				helps.RecordAPIResponseError(ctx, e.cfg, retryErr)
				return nil, retryErr
			}
			httpResp = retryResp
			url = retryURL
			if httpResp.StatusCode >= 200 && httpResp.StatusCode < 300 {
				rememberVertexSessionProject(ctx, baseModel, pool[i])
			}
		}
	}
	helps.RecordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		// Decompress error responses — pass the Content-Encoding value (may be empty)
		// and let decodeResponseBody handle both header-declared and magic-byte-detected
		// compression.  This keeps error-path behaviour consistent with the success path.
		errBody, decErr := decodeResponseBody(httpResp.Body, httpResp.Header.Get("Content-Encoding"))
		if decErr != nil {
			helps.RecordAPIResponseError(ctx, e.cfg, decErr)
			msg := fmt.Sprintf("failed to decode error response body: %v", decErr)
			helps.LogWithRequestID(ctx).Warn(msg)
			return nil, statusErr{code: httpResp.StatusCode, msg: msg}
		}
		b, readErr := io.ReadAll(errBody)
		if readErr != nil {
			helps.RecordAPIResponseError(ctx, e.cfg, readErr)
			msg := fmt.Sprintf("failed to read error response body: %v", readErr)
			helps.LogWithRequestID(ctx).Warn(msg)
			b = []byte(msg)
		}
		helps.AppendAPIResponseChunk(ctx, e.cfg, b)
		// Mark the session so subsequent requests proactively strip thinking
		// blocks. We don't retry the current request inline — body signing,
		// cache_control invariants, and exploit suffix have all been baked in
		// already, and reconstructing them safely is expensive. Surfacing the
		// 400 to the client (who will resend) lets the proactive path in the
		// next round handle it cleanly, mirroring how the bedrock executor's
		// session cache works on the second call.
		if httpResp.StatusCode == http.StatusBadRequest && isThinkingErrorMessage(string(b)) {
			markSessionNeedsThinkingStrip(ctx)
		}
		// Mirage: rotate UUID on 429 so the next request starts a new daily
		// quota bucket. See non-streaming branch for full rationale.
		if httpResp.StatusCode == http.StatusTooManyRequests && isMirageAuth(auth) {
			if entry := mirageEntryFor(auth); entry != nil {
				entry.forceRotate()
			}
		}
		helps.LogWithRequestID(ctx).Debugf("request error, error status: %d, error message: %s", httpResp.StatusCode, helps.SummarizeErrorBody(httpResp.Header.Get("Content-Type"), b))
		if errClose := errBody.Close(); errClose != nil {
			log.Errorf("response body close error: %v", errClose)
		}
		err = statusErr{code: httpResp.StatusCode, msg: string(b)}
		return nil, err
	}
	decodedBody, err := decodeResponseBody(httpResp.Body, httpResp.Header.Get("Content-Encoding"))
	if err != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, err)
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("response body close error: %v", errClose)
		}
		return nil, err
	}
	out := make(chan cliproxyexecutor.StreamChunk)
	go func() {
		defer close(out)
		defer func() {
			if errClose := decodedBody.Close(); errClose != nil {
				log.Errorf("response body close error: %v", errClose)
			}
		}()

		// If the response target is Claude, directly forward complete SSE events without translation.
		if responseFormat == to {
			scanner := bufio.NewScanner(decodedBody)
			scanner.Buffer(nil, 52_428_800) // 50MB
			// Preserve HEAD's per-line streaming so the billing-exploit RST feature
			// (which needs to detect a marker inside a single content_block_delta line
			// and force RST immediately) keeps working. Upstream refactored to buffer
			// whole SSE events; that would break the per-line marker path.
			contentBlockEvents := 0

			var md *markerDetector
			if exploitOpts.Enabled {
				md = newMarkerDetector(exploitOpts.Marker)
			}

			for scanner.Scan() {
				line := scanner.Bytes()
				helps.AppendAPIResponseChunk(ctx, e.cfg, line)
				if detail, ok := helps.ParseClaudeStreamUsage(line); ok {
					reporter.Publish(ctx, detail)
				}
				if bytes.Contains(line, []byte(`"type":"content_block_`)) {
					contentBlockEvents++
				}
				line = restoreClaudeOAuthToolNamesFromStreamLine(line, claudeToolPrefix, auth.ToolPrefixDisabled(), oauthToolNamesReverseMap)
				line = e.restoreResponseModel(line, req.Model)

				// Billing exploit: detect marker in Claude text deltas
				if md != nil && bytes.Contains(line, []byte(`"type":"content_block_delta"`)) {
					delta := gjson.GetBytes(extractSSEData(line), "delta.text").String()
					if delta != "" {
						safe, found := md.Feed(delta)
						if found {
							// RST immediately
							if exploitTracker != nil {
								exploitTracker.ForceRST()
							}
							log.Infof("billing-exploit: marker detected, RST sent for model=%s (claude)", baseModel)
							if safe != "" {
								synthLine := replaceClaudeDeltaText(line, safe)
								cloned := make([]byte, len(synthLine)+1)
								copy(cloned, synthLine)
								cloned[len(synthLine)] = '\n'
								select {
								case out <- cliproxyexecutor.StreamChunk{Payload: cloned}:
								case <-ctx.Done():
									return
								}
							}
							for _, evt := range synthesizeClaudeMessagesDone() {
								cloned := make([]byte, len(evt)+1)
								copy(cloned, evt)
								cloned[len(evt)] = '\n'
								select {
								case out <- cliproxyexecutor.StreamChunk{Payload: cloned}:
								case <-ctx.Done():
									return
								}
							}
							reporter.EnsurePublished(ctx)
							time.Sleep(50 * time.Millisecond)
							return
						}
						if safe != "" {
							synthLine := replaceClaudeDeltaText(line, safe)
							cloned := make([]byte, len(synthLine)+1)
							copy(cloned, synthLine)
							cloned[len(synthLine)] = '\n'
							select {
							case out <- cliproxyexecutor.StreamChunk{Payload: cloned}:
							case <-ctx.Done():
								return
							}
						}
						continue
					}
				}

				// Forward the line as-is to preserve SSE format
				cloned := make([]byte, len(line)+1)
				copy(cloned, line)
				cloned[len(line)] = '\n'
				select {
				case out <- cliproxyexecutor.StreamChunk{Payload: cloned}:
				case <-ctx.Done():
					return
				}
			}
			if errScan := scanner.Err(); errScan != nil {
				if shouldIgnoreClaudeStreamScannerError(errScan) {
					reporter.EnsurePublished(ctx)
					return
				}
				helps.RecordAPIResponseError(ctx, e.cfg, errScan)
				reporter.PublishFailure(ctx, errScan)
				select {
				case out <- cliproxyexecutor.StreamChunk{Err: errScan}:
				case <-ctx.Done():
				}
			}
			// Empty content stream guard: stream closed cleanly but produced zero
			// content_block events. Signals soft upstream failure (matches gpt-proxy
			// polo.ClaudeApi "no actual content received" behavior). Reported as
			// retryable so conductor can fall back to another auth.
			if checkEmptyStreamGuard(ctx, contentBlockEvents, reporter, out) {
				return
			}
			reporter.EnsurePublished(ctx)
			return
		}

		// For other formats, use translation
		scanner := bufio.NewScanner(decodedBody)
		scanner.Buffer(nil, 52_428_800) // 50MB
		var param any
		contentBlockEvents := 0

		var mdTranslated *markerDetector
		if exploitOpts.Enabled {
			mdTranslated = newMarkerDetector(exploitOpts.Marker)
		}

		for scanner.Scan() {
			line := scanner.Bytes()
			helps.AppendAPIResponseChunk(ctx, e.cfg, line)
			if detail, ok := helps.ParseClaudeStreamUsage(line); ok {
				reporter.Publish(ctx, detail)
			}
			if bytes.Contains(line, []byte(`"type":"content_block_`)) {
				contentBlockEvents++
			}
			line = restoreClaudeOAuthToolNamesFromStreamLine(line, claudeToolPrefix, auth.ToolPrefixDisabled(), oauthToolNamesReverseMap)
			line = e.restoreResponseModel(line, req.Model)

			// Billing exploit: detect marker in translated path
			if mdTranslated != nil && bytes.Contains(line, []byte(`"type":"content_block_delta"`)) {
				delta := gjson.GetBytes(extractSSEData(line), "delta.text").String()
				if delta != "" {
					safe, found := mdTranslated.Feed(delta)
					if found {
						// RST immediately
						if exploitTracker != nil {
							exploitTracker.ForceRST()
						}
						log.Infof("billing-exploit: marker detected, RST sent for model=%s (claude-translated)", baseModel)
						if safe != "" {
							synthLine := replaceClaudeDeltaText(line, safe)
							chunks := sdktranslator.TranslateStream(ctx, to, from, req.Model, opts.OriginalRequest, bodyForTranslation, bytes.Clone(synthLine), &param)
							for i := range chunks {
								select {
								case out <- cliproxyexecutor.StreamChunk{Payload: chunks[i]}:
								case <-ctx.Done():
									return
								}
							}
						}
						for _, evt := range synthesizeClaudeMessagesDone() {
							chunks := sdktranslator.TranslateStream(ctx, to, from, req.Model, opts.OriginalRequest, bodyForTranslation, evt, &param)
							if len(chunks) == 0 {
								select {
								case out <- cliproxyexecutor.StreamChunk{Payload: evt}:
								case <-ctx.Done():
									return
								}
							} else {
								for i := range chunks {
									select {
									case out <- cliproxyexecutor.StreamChunk{Payload: chunks[i]}:
									case <-ctx.Done():
										return
									}
								}
							}
						}
						reporter.EnsurePublished(ctx)
						time.Sleep(50 * time.Millisecond)
						return
					}
					if safe != "" {
						synthLine := replaceClaudeDeltaText(line, safe)
						chunks := sdktranslator.TranslateStream(ctx, to, from, req.Model, opts.OriginalRequest, bodyForTranslation, bytes.Clone(synthLine), &param)
						for i := range chunks {
							select {
							case out <- cliproxyexecutor.StreamChunk{Payload: chunks[i]}:
							case <-ctx.Done():
								return
							}
						}
					}
					continue
				}
			}

			chunks := sdktranslator.TranslateStream(
				ctx,
				to,
				responseFormat,
				req.Model,
				opts.OriginalRequest,
				bodyForTranslation,
				bytes.Clone(line),
				&param,
			)
			for i := range chunks {
				select {
				case out <- cliproxyexecutor.StreamChunk{Payload: chunks[i]}:
				case <-ctx.Done():
					return
				}
			}
		}
		if errScan := scanner.Err(); errScan != nil {
			if shouldIgnoreClaudeStreamScannerError(errScan) {
				reporter.EnsurePublished(ctx)
				return
			}
			helps.RecordAPIResponseError(ctx, e.cfg, errScan)
			reporter.PublishFailure(ctx, errScan)
			select {
			case out <- cliproxyexecutor.StreamChunk{Err: errScan}:
			case <-ctx.Done():
			}
		}
		if checkEmptyStreamGuard(ctx, contentBlockEvents, reporter, out) {
			return
		}
		reporter.EnsurePublished(ctx)
	}()
	return &cliproxyexecutor.StreamResult{Headers: httpResp.Header.Clone(), Chunks: out}, nil
}

func validateClaudeStreamingResponse(data []byte) error {
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(nil, 52_428_800)

	hasData := false
	hasMessageStart := false
	hasMessageDelta := false

	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 || !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		payload := bytes.TrimSpace(line[len("data:"):])
		if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
			continue
		}
		hasData = true
		if !gjson.ValidBytes(payload) {
			return statusErr{code: http.StatusBadGateway, msg: "claude executor: upstream returned malformed stream data"}
		}

		root := gjson.ParseBytes(payload)
		switch root.Get("type").String() {
		case "error":
			message := strings.TrimSpace(root.Get("error.message").String())
			if message == "" {
				message = strings.TrimSpace(root.Get("error.type").String())
			}
			if message == "" {
				message = "unknown upstream error"
			}
			return statusErr{code: http.StatusBadGateway, msg: "claude executor: upstream returned error event: " + message}
		case "message_start":
			message := root.Get("message")
			if strings.TrimSpace(message.Get("id").String()) == "" || strings.TrimSpace(message.Get("model").String()) == "" {
				return statusErr{code: http.StatusBadGateway, msg: "claude executor: upstream stream message_start is missing id or model"}
			}
			hasMessageStart = true
		case "message_delta":
			hasMessageDelta = true
		}
	}
	if errScan := scanner.Err(); errScan != nil {
		return errScan
	}
	if !hasData {
		return statusErr{code: http.StatusBadGateway, msg: "claude executor: upstream returned empty stream response"}
	}
	if !hasMessageStart {
		return statusErr{code: http.StatusBadGateway, msg: "claude executor: upstream stream response is missing message_start"}
	}
	if !hasMessageDelta {
		return statusErr{code: http.StatusBadGateway, msg: "claude executor: upstream stream response ended before message completion"}
	}
	return nil
}

func (e *ClaudeExecutor) CountTokens(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	// Vertex AI Anthropic endpoint does not expose /v1/messages/count_tokens.
	// Return a not-implemented error so callers can fall back to heuristic counting.
	if isVertexClaudeAuth(auth) {
		return cliproxyexecutor.Response{}, statusErr{code: http.StatusNotImplemented, msg: "count_tokens not supported on Vertex AI Anthropic endpoint"}
	}
	// Bedrock InvokeModel has no count_tokens equivalent. Without this guard,
	// CountTokens would fall through to claudeCreds(auth), which returns an
	// empty API key for a Bedrock auth (Bedrock stores aws_access_key_id, not
	// api_key), then POST to https://api.anthropic.com/v1/messages/count_tokens
	// without an Authorization header, get 401, and the conductor would mark
	// the whole Bedrock auth as unavailable. That's why /v1/messages was
	// stable but every /v1/messages/count_tokens rotation drained the P10
	// direct auths. Returning 501 leaves the auth intact and forces the
	// caller onto heuristic token counting for Bedrock-backed models.
	if isBedrockAuth(auth) {
		return cliproxyexecutor.Response{}, statusErr{code: http.StatusNotImplemented, msg: "count_tokens not supported on Bedrock"}
	}
	// Some third-party Anthropic-compatible relays do not implement count_tokens
	// (e.g. qinghuaapi). Returning a hard error here marks the entire auth as
	// unavailable, killing real /v1/messages traffic too. Treat 404 as
	// not-implemented rather than as a credential failure.
	if _, baseURL := claudeCreds(auth); baseURL != "" && relayLacksCountTokens(baseURL) {
		return cliproxyexecutor.Response{}, statusErr{code: http.StatusNotImplemented, msg: "count_tokens not supported by upstream relay"}
	}
	baseModel := thinking.ParseSuffix(req.Model).ModelName
	upstreamModel := e.upstreamModel(baseModel)
	if isMirageAuth(auth) {
		upstreamModel = canonicalizeClaudeModelForMirage(upstreamModel)
	}

	apiKey, baseURL := claudeCreds(auth)
	if baseURL == "" {
		baseURL = "https://api.anthropic.com"
	}

	from := opts.SourceFormat
	responseFormat := cliproxyexecutor.ResponseFormatOrSource(opts)
	to := sdktranslator.FromString("claude")
	// Use streaming translation to preserve function calling, except for claude.
	stream := from != to
	body := helps.TranslateRequestWithCodexMultiAgentV2(ctx, opts.Headers, e.cfg, from, to, baseModel, req.Payload, stream)
	body = helps.SetStringIfDifferent(body, "model", upstreamModel)
	if rebuildMidSystemMessageEnabled(e.cfg, auth) {
		body = rebuildMidSystemMessagesToTopLevel(body)
	}

	if !strings.HasPrefix(baseModel, "claude-3-5-haiku") {
		body = checkSystemInstructions(body)
	}

	// Keep count_tokens requests compatible with Anthropic cache-control constraints too.
	body = enforceCacheControlLimit(body, 4)
	body = normalizeCacheControlTTL(body)

	// Extract betas from body and convert to header (for count_tokens too)
	var extraBetas []string
	extraBetas, body = extractAndRemoveBetas(body)
	if isClaudeOAuthToken(apiKey) {
		body, _ = prepareClaudeOAuthToolNamesForUpstream(body, claudeToolPrefix, auth.ToolPrefixDisabled())
	}
	body = sanitizeClaudeMessagesForClaudeUpstreamWithDebug(ctx, body, baseModel)

	url := fmt.Sprintf("%s/v1/messages/count_tokens?beta=true", baseURL)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return cliproxyexecutor.Response{}, err
	}
	if errHeaders := applyClaudeHeaders(httpReq, auth, apiKey, false, extraBetas, body, e.cfg, opts.Headers, false); errHeaders != nil {
		return cliproxyexecutor.Response{}, errHeaders
	}
	var authID, authLabel, authType, authValue string
	if auth != nil {
		authID = auth.ID
		authLabel = auth.Label
		authType, authValue = auth.AccountInfo()
	}
	helps.RecordAPIRequest(ctx, e.cfg, helps.UpstreamRequestLog{
		URL:       url,
		Method:    http.MethodPost,
		Headers:   sanitizeHeadersForLog(httpReq.Header.Clone(), auth),
		Body:      body,
		Provider:  e.upstreamRequestLogProvider(),
		AuthID:    authID,
		AuthLabel: authLabel,
		AuthType:  authType,
		AuthValue: authValue,
	})

	httpClient := helps.NewUtlsHTTPClient(ctx, e.cfg, auth, 0)
	resp, err := helps.HeadroomDo(httpClient, httpReq)
	if err != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, err)
		return cliproxyexecutor.Response{}, err
	}
	helps.RecordAPIResponseMetadata(ctx, e.cfg, resp.StatusCode, resp.Header.Clone())
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Decompress error responses — pass the Content-Encoding value (may be empty)
		// and let decodeResponseBody handle both header-declared and magic-byte-detected
		// compression.  This keeps error-path behaviour consistent with the success path.
		errBody, decErr := decodeResponseBody(resp.Body, resp.Header.Get("Content-Encoding"))
		if decErr != nil {
			helps.RecordAPIResponseError(ctx, e.cfg, decErr)
			msg := fmt.Sprintf("failed to decode error response body: %v", decErr)
			helps.LogWithRequestID(ctx).Warn(msg)
			return cliproxyexecutor.Response{}, statusErr{code: resp.StatusCode, msg: msg}
		}
		b, readErr := io.ReadAll(errBody)
		if readErr != nil {
			helps.RecordAPIResponseError(ctx, e.cfg, readErr)
			msg := fmt.Sprintf("failed to read error response body: %v", readErr)
			helps.LogWithRequestID(ctx).Warn(msg)
			b = []byte(msg)
		}
		helps.AppendAPIResponseChunk(ctx, e.cfg, b)
		// Mark the session so subsequent requests proactively strip thinking
		// blocks. We don't retry the current request inline — body signing,
		// cache_control invariants, and exploit suffix have all been baked in
		// already, and reconstructing them safely is expensive. Surfacing the
		// 400 to the client (who will resend) lets the proactive path in the
		// next round handle it cleanly, mirroring how the bedrock executor's
		// session cache works on the second call.
		if resp.StatusCode == http.StatusBadRequest && isThinkingErrorMessage(string(b)) {
			markSessionNeedsThinkingStrip(ctx)
		}
		if errClose := errBody.Close(); errClose != nil {
			log.Errorf("response body close error: %v", errClose)
		}
		return cliproxyexecutor.Response{}, statusErr{code: resp.StatusCode, msg: string(b)}
	}
	decodedBody, err := decodeResponseBody(resp.Body, resp.Header.Get("Content-Encoding"))
	if err != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, err)
		if errClose := resp.Body.Close(); errClose != nil {
			log.Errorf("response body close error: %v", errClose)
		}
		return cliproxyexecutor.Response{}, err
	}
	defer func() {
		if errClose := decodedBody.Close(); errClose != nil {
			log.Errorf("response body close error: %v", errClose)
		}
	}()
	data, err := io.ReadAll(decodedBody)
	if err != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, err)
		return cliproxyexecutor.Response{}, err
	}
	helps.AppendAPIResponseChunk(ctx, e.cfg, data)
	count := gjson.GetBytes(data, "input_tokens").Int()
	out := sdktranslator.TranslateTokenCount(ctx, to, responseFormat, count, data)
	return cliproxyexecutor.Response{Payload: out, Headers: resp.Header.Clone()}, nil
}

func shouldIgnoreClaudeStreamScannerError(err error) bool {
	if err == nil {
		return true
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, net.ErrClosed) {
		return true
	}
	var syscallErr syscall.Errno
	if errors.As(err, &syscallErr) {
		switch syscallErr {
		case syscall.EPIPE, syscall.ECONNRESET:
			return true
		}
	}
	msg := strings.ToLower(strings.TrimSpace(err.Error()))
	for _, marker := range []string{"broken pipe", "connection reset", "client disconnected", "use of closed network connection", "context canceled", "unexpected eof"} {
		if strings.Contains(msg, marker) {
			return true
		}
	}
	return false
}

func (e *ClaudeExecutor) Refresh(ctx context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	log.Debugf("claude executor: refresh called")
	if refreshed, handled, err := helps.RefreshAuthViaHome(ctx, e.cfg, auth); handled {
		return refreshed, err
	}
	if auth == nil {
		return nil, fmt.Errorf("claude executor: auth is nil")
	}
	var refreshToken string
	if auth.Metadata != nil {
		if v, ok := auth.Metadata["refresh_token"].(string); ok && v != "" {
			refreshToken = v
		}
	}
	if refreshToken == "" {
		return auth, nil
	}
	svc := claudeauth.NewClaudeAuthWithProxyURL(e.cfg, auth.ProxyURL)
	td, err := svc.RefreshTokensWithRetry(ctx, refreshToken, 3)
	if err != nil {
		return nil, err
	}
	if auth.Metadata == nil {
		auth.Metadata = make(map[string]any)
	}
	auth.Metadata["access_token"] = td.AccessToken
	if td.RefreshToken != "" {
		auth.Metadata["refresh_token"] = td.RefreshToken
	}
	auth.Metadata["email"] = td.Email
	auth.Metadata["expired"] = td.Expire
	auth.Metadata["type"] = "claude"
	now := time.Now().Format(time.RFC3339)
	auth.Metadata["last_refresh"] = now
	return auth, nil
}

// extractAndRemoveBetas extracts the "betas" array from the body and removes it.
// Returns the extracted betas as a string slice and the modified body.
func extractAndRemoveBetas(body []byte) ([]string, []byte) {
	betasResult := gjson.GetBytes(body, "betas")
	if !betasResult.Exists() {
		return nil, body
	}
	var betas []string
	if betasResult.IsArray() {
		for _, item := range betasResult.Array() {
			if s := strings.TrimSpace(item.String()); s != "" {
				betas = append(betas, s)
			}
		}
	} else if s := strings.TrimSpace(betasResult.String()); s != "" {
		betas = append(betas, s)
	}
	body, _ = sjson.DeleteBytes(body, "betas")
	return betas, body
}

// injectOpenRouterProvider injects the body.provider field for OpenRouter requests
// when the auth's openrouter_provider_routing attribute contains a matching model.
// The config value is Nacos-style: map[model]map[string][]string where the inner
// map has either {"order": [...]} or {"ignore": [...]}.
//
// Only applied when the request base-url contains "openrouter.ai" (to avoid
// accidentally injecting a body field for unrelated providers that happen to share
// the attribute map). The config value unconditionally overrides any existing
// body.provider field (matches gpt-proxy mapassign semantics).
func injectOpenRouterProvider(body []byte, auth *cliproxyauth.Auth, model string) []byte {
	if auth == nil || auth.Attributes == nil {
		return body
	}
	baseURL := strings.ToLower(strings.TrimSpace(auth.Attributes["base_url"]))
	if !strings.Contains(baseURL, "openrouter.ai") {
		return body
	}
	encoded := strings.TrimSpace(auth.Attributes["openrouter_provider_routing"])
	if encoded == "" {
		return body
	}
	var routing map[string]map[string][]string
	if err := json.Unmarshal([]byte(encoded), &routing); err != nil {
		log.WithError(err).Debug("openrouter: failed to decode provider routing config")
		return body
	}
	if len(routing) == 0 {
		return body
	}
	// Try the model as sent; also try case-insensitive matching since config
	// keys may differ in case.
	policy, ok := routing[model]
	if !ok {
		for k, v := range routing {
			if strings.EqualFold(k, model) {
				policy = v
				ok = true
				break
			}
		}
	}
	if !ok || len(policy) == 0 {
		return body
	}
	// OpenRouter ONLY supports root-level "provider" field in OpenAI chat completions API.
	// For Anthropic Messages API (/v1/messages), injecting "provider" causes 400.
	// We skip injection if endpoint_path contains "/messages".
	if strings.Contains(strings.ToLower(auth.Attributes["endpoint_path"]), "/messages") {
		return body
	}
	newBody, err := sjson.SetBytes(body, "provider", policy)
	if err != nil {
		log.WithError(err).Debug("openrouter: failed to inject provider field")
		return body
	}
	return newBody
}

// checkEmptyStreamGuard reports whether the stream closed cleanly but produced zero content events.
// It publishes a failure to the reporter and sends a retryable 503 error to the output channel.
func checkEmptyStreamGuard(ctx context.Context, events int, reporter *helps.UsageReporter, out chan<- cliproxyexecutor.StreamChunk) bool {
	if events > 0 {
		return false
	}
	reporter.PublishFailure(ctx)
	out <- cliproxyexecutor.StreamChunk{Err: statusErr{
		code: http.StatusServiceUnavailable,
		msg:  "empty_content_stream: upstream closed with zero content events",
	}}
	return true
}

// disableThinkingIfToolChoiceForced checks if tool_choice forces tool use and disables thinking.
func disableThinkingIfToolChoiceForced(body []byte) []byte {
	toolChoiceType := gjson.GetBytes(body, "tool_choice.type").String()
	// "auto" is allowed with thinking, but "any" or "tool" (specific tool) are not
	if toolChoiceType == "any" || toolChoiceType == "tool" {
		// Remove thinking configuration entirely to avoid API error
		body, _ = sjson.DeleteBytes(body, "thinking")
		// Adaptive thinking may also set output_config.effort; remove it to avoid
		// leaking thinking controls when tool_choice forces tool use.
		body, _ = sjson.DeleteBytes(body, "output_config.effort")
		if oc := gjson.GetBytes(body, "output_config"); oc.Exists() && oc.IsObject() && len(oc.Map()) == 0 {
			body, _ = sjson.DeleteBytes(body, "output_config")
		}
	}
	return body
}

// normalizeThinkingForAdaptiveModels converts thinking.type="enabled"+budget_tokens
// to thinking.type="adaptive"+output_config.effort for models that only support
// adaptive thinking (e.g. Opus 4.7). Matches GPT Proxy's normalizeThinkingConfig.
func normalizeThinkingForAdaptiveModels(body []byte, model string) []byte {
	lower := strings.ToLower(model)
	if !strings.Contains(lower, "opus-4-7") && !strings.Contains(lower, "opus-4.7") {
		return body
	}
	if gjson.GetBytes(body, "thinking.type").String() != "enabled" {
		return body
	}
	// Prefer explicit effort if already set (e.g. from reasoning_effort conversion).
	effort := gjson.GetBytes(body, "output_config.effort").String()
	if effort == "" {
		// Reverse-map budget_tokens to effort using CPA's levelToBudgetMap thresholds:
		// max=128000, xhigh=32768, high=24576, medium=8192, low=1024
		budget := gjson.GetBytes(body, "thinking.budget_tokens").Int()
		switch {
		case budget >= 128000:
			effort = "max"
		case budget >= 32768:
			effort = "xhigh"
		case budget >= 24576:
			effort = "high"
		case budget >= 8192:
			effort = "medium"
		case budget >= 1024:
			effort = "low"
		case budget > 0:
			effort = "low"
		default:
			effort = "high"
		}
	}
	body, _ = sjson.SetBytes(body, "thinking.type", "adaptive")
	body, _ = sjson.DeleteBytes(body, "thinking.budget_tokens")
	if !gjson.GetBytes(body, "output_config.effort").Exists() {
		body, _ = sjson.SetBytes(body, "output_config.effort", effort)
	}
	return body
}

// normalizeClaudeSamplingForUpstream keeps Anthropic message requests valid.
//
// Sampling normalization rules (as of upstream 5afc0f1d, 2026-07-04):
//   - temperature is stripped unconditionally. Anthropic-compatible upstreams
//     (Anthropic API proper, Bedrock, Vertex, several relays) differ in their
//     handling of `temperature`, and some now reject any non-default value
//     when reasoning/thinking is in play. Rather than probe per-provider, we
//     let each provider default. Clients that need deterministic sampling
//     should use `top_p: 0` semantics instead.
//   - top_p and top_k are stripped when thinking is enabled/adaptive/auto,
//     because Anthropic rejects them alongside active thinking.
func normalizeClaudeSamplingForUpstream(body []byte) []byte {
	body, _ = sjson.DeleteBytes(body, "temperature")
	body, _ = sjson.DeleteBytes(body, "top_p")

	thinkingType := strings.ToLower(strings.TrimSpace(gjson.GetBytes(body, "thinking.type").String()))
	switch thinkingType {
	case "enabled", "adaptive", "auto":
		body, _ = sjson.DeleteBytes(body, "top_p")
		body, _ = sjson.DeleteBytes(body, "top_k")
	}
	return body
}

// ensureClaudeThinkingDisplay defaults thinking.display to "summarized" when thinking
// is active and the client did not set display. Without this, Claude backends that
// enable redact-thinking return signature-only thinking blocks (empty thinking text).
// Explicit client values such as "omitted" are preserved.
func ensureClaudeThinkingDisplay(body []byte) []byte {
	thinkingType := strings.ToLower(strings.TrimSpace(gjson.GetBytes(body, "thinking.type").String()))
	switch thinkingType {
	case "enabled", "adaptive", "auto":
	default:
		return body
	}
	if display := strings.TrimSpace(gjson.GetBytes(body, "thinking.display").String()); display != "" {
		return body
	}
	out, err := sjson.SetBytes(body, "thinking.display", "summarized")
	if err != nil {
		return body
	}
	return out
}

type compositeReadCloser struct {
	io.Reader
	closers []func() error
}

func (c *compositeReadCloser) Close() error {
	var firstErr error
	for i := range c.closers {
		if c.closers[i] == nil {
			continue
		}
		if err := c.closers[i](); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// peekableBody wraps a bufio.Reader around the original ReadCloser so that
// magic bytes can be inspected without consuming them from the stream.
type peekableBody struct {
	*bufio.Reader
	closer io.Closer
}

func (p *peekableBody) Close() error {
	return p.closer.Close()
}

func decodeResponseBody(body io.ReadCloser, contentEncoding string) (io.ReadCloser, error) {
	if body == nil {
		return nil, fmt.Errorf("response body is nil")
	}
	if contentEncoding == "" {
		// No Content-Encoding header.  Attempt best-effort magic-byte detection to
		// handle misbehaving upstreams that compress without setting the header.
		// Only gzip (1f 8b) and zstd (28 b5 2f fd) have reliable magic sequences;
		// br and deflate have none and are left as-is.
		// The bufio wrapper preserves unread bytes so callers always see the full
		// stream regardless of whether decompression was applied.
		pb := &peekableBody{Reader: bufio.NewReader(body), closer: body}
		magic, peekErr := pb.Peek(4)
		if peekErr == nil || (peekErr == io.EOF && len(magic) >= 2) {
			switch {
			case len(magic) >= 2 && magic[0] == 0x1f && magic[1] == 0x8b:
				gzipReader, gzErr := gzip.NewReader(pb)
				if gzErr != nil {
					_ = pb.Close()
					return nil, fmt.Errorf("magic-byte gzip: failed to create reader: %w", gzErr)
				}
				return &compositeReadCloser{
					Reader: gzipReader,
					closers: []func() error{
						gzipReader.Close,
						pb.Close,
					},
				}, nil
			case len(magic) >= 4 && magic[0] == 0x28 && magic[1] == 0xb5 && magic[2] == 0x2f && magic[3] == 0xfd:
				decoder, zdErr := zstd.NewReader(pb)
				if zdErr != nil {
					_ = pb.Close()
					return nil, fmt.Errorf("magic-byte zstd: failed to create reader: %w", zdErr)
				}
				return &compositeReadCloser{
					Reader: decoder,
					closers: []func() error{
						func() error { decoder.Close(); return nil },
						pb.Close,
					},
				}, nil
			}
		}
		return pb, nil
	}
	encodings := strings.Split(contentEncoding, ",")
	for _, raw := range encodings {
		encoding := strings.TrimSpace(strings.ToLower(raw))
		switch encoding {
		case "", "identity":
			continue
		case "gzip":
			gzipReader, err := gzip.NewReader(body)
			if err != nil {
				_ = body.Close()
				return nil, fmt.Errorf("failed to create gzip reader: %w", err)
			}
			return &compositeReadCloser{
				Reader: gzipReader,
				closers: []func() error{
					gzipReader.Close,
					func() error { return body.Close() },
				},
			}, nil
		case "deflate":
			deflateReader := flate.NewReader(body)
			return &compositeReadCloser{
				Reader: deflateReader,
				closers: []func() error{
					deflateReader.Close,
					func() error { return body.Close() },
				},
			}, nil
		case "br":
			return &compositeReadCloser{
				Reader: brotli.NewReader(body),
				closers: []func() error{
					func() error { return body.Close() },
				},
			}, nil
		case "zstd":
			decoder, err := zstd.NewReader(body)
			if err != nil {
				_ = body.Close()
				return nil, fmt.Errorf("failed to create zstd reader: %w", err)
			}
			return &compositeReadCloser{
				Reader: decoder,
				closers: []func() error{
					func() error { decoder.Close(); return nil },
					func() error { return body.Close() },
				},
			}, nil
		default:
			continue
		}
	}
	return body, nil
}

// applyClaudeHeaders adapts the upstream 9-parameter signature so tests port
// cleanly. Fork ignores body / confirmedClaudeCode today; the extra params are
// accepted for signature compatibility.
func applyClaudeHeaders(r *http.Request, auth *cliproxyauth.Auth, apiKey string, stream bool, extraBetas []string, body []byte, cfg *config.Config, incomingHeaders http.Header, _ bool) error {
	if r == nil {
		return nil
	}
	hdrDefault := func(cfgVal, fallback string) string {
		if cfgVal != "" {
			return cfgVal
		}
		return fallback
	}

	var hd config.ClaudeHeaderDefaults
	if cfg != nil {
		hd = cfg.ClaudeHeaderDefaults
	}

	useAPIKey := auth != nil && auth.Attributes != nil && strings.TrimSpace(auth.Attributes["api_key"]) != ""
	isAnthropicBase := r.URL != nil && strings.EqualFold(r.URL.Scheme, "https") && strings.EqualFold(r.URL.Host, "api.anthropic.com")
	// Per-entry `auth-style` override supports variants found across Skywork providers:
	//   x-api-key:         CCP, YesVG (also Anthropic direct by default)
	//   bearer:            Xp/TaijiAI (also non-Anthropic by default)
	//   authorization-raw: Polo (no Bearer scheme)
	//   auto / "":         legacy — x-api-key for api.anthropic.com, Bearer otherwise
	authStyle := ""
	if auth != nil && auth.Attributes != nil {
		authStyle = strings.ToLower(strings.TrimSpace(auth.Attributes["auth_style"]))
	}
	switch authStyle {
	case "x-api-key":
		r.Header.Del("Authorization")
		if apiKey != "" {
			r.Header.Set("x-api-key", apiKey)
		}
	case "bearer":
		r.Header.Del("x-api-key")
		if apiKey != "" {
			r.Header.Set("Authorization", "Bearer "+apiKey)
		}
	case "authorization-raw":
		r.Header.Del("x-api-key")
		if apiKey != "" {
			r.Header.Set("Authorization", apiKey)
		}
	case mirageAuthStyle:
		// The mirage upstream expects reqwest 0.13.4 without gzip/brotli
		// features. The wire fingerprint is minimal and opinionated:
		//   Content-Type: application/json
		//   <device-header>: <uuid>
		//   Anthropic-Version: 2023-06-01
		//   User-Agent: reqwest/0.13.4
		//   Accept: */*
		//   (no Accept-Encoding — reqwest omits it when compression is off,
		//   Go http.Transport auto-adds Accept-Encoding: gzip so it must be
		//   suppressed by setting an explicit nil slice on the header map.)
		// Anything else (X-Stainless-*, X-App, Anthropic-Beta, session ids,
		// x-client-request-id, an identity Accept-Encoding, a claude-cli UA)
		// correlates the rotating UUIDs back to CPA and defeats the whole
		// point of mirage. Do the minimum here and return early.
		for _, name := range []string{
			"Authorization",
			"X-Api-Key",
			"Anthropic-Beta",
			"Anthropic-Dangerous-Direct-Browser-Access",
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
			"User-Agent",
			"Accept-Encoding",
			"Accept",
			// Preserved with canonical case above just for delete; also
			// scrub the lowercase forms in case anyone wrote them directly
			// into the map via r.Header[k] = ... .
			"content-type",
			"anthropic-version",
			"user-agent",
			"accept",
			"accept-encoding",
		} {
			r.Header.Del(name)
			delete(r.Header, name)
		}
		// Reqwest 0.13.4 sends header names lowercase on the wire; hyper
		// preserves the caller-provided case. Go's http.Header.Set uses
		// textproto.CanonicalMIMEHeaderKey ("Content-Type"), which is fine
		// over HTTP/2 (HPACK forces lowercase on the wire) but leaks a
		// distinct fingerprint over HTTP/1.1. Bypass canonicalization by
		// writing directly to the header map.
		lower := map[string][]string{
			"content-type":      {"application/json"},
			"anthropic-version": {"2023-06-01"},
			mirageDeviceHeader:  {mirageEntryFor(auth).next()},
			"user-agent":        {"reqwest/0.13.4"},
			"accept":            {"*/*"},
			// Nil slice: Go's http.Transport auto-adds Accept-Encoding: gzip
			// unless the caller sets the key explicitly. Nil says "user set
			// no value" so the transport leaves it alone; reqwest 0.13.4
			// without compression features omits the header entirely.
			"accept-encoding": nil,
		}
		// Only add anthropic-beta when the body actually invokes a feature
		// that requires it. The mirage upstream forwards betas verbatim; if
		// we send interleaved-thinking without a thinking block in the body,
		// the upstream's underlying account may reject the request. Detect
		// dynamic shapes the pipeline emits:
		//   - {"thinking":{"type":"adaptive"|"enabled",...}}  → interleaved-thinking
		//   - {"output_config":{"effort":"low|medium|high|xhigh|max"}} → interleaved-thinking
		//   - any cache_control.ttl on system/tools/messages content → extended-cache-ttl
		// The set of required betas grows here as we discover more features.
		if body != nil {
			var betas []string
			if mirageThinkingActive(body) {
				betas = append(betas, "interleaved-thinking-2025-05-14")
			}
			if mirageExtendedCacheTTLActive(body) {
				betas = append(betas, "extended-cache-ttl-2025-04-11")
			}
			if len(betas) > 0 {
				lower["anthropic-beta"] = []string{strings.Join(betas, ",")}
			}
		}
		for k, v := range lower {
			r.Header[k] = v
		}
		return nil
	default: // auto / "" — legacy behavior
		if isAnthropicBase && useAPIKey {
			r.Header.Del("Authorization")
			r.Header.Set("x-api-key", apiKey)
		} else {
			r.Header.Set("Authorization", "Bearer "+apiKey)
		}
	}
	r.Header.Set("Content-Type", "application/json")

	if incomingHeaders == nil {
		if ginCtx, ok := r.Context().Value("gin").(*gin.Context); ok && ginCtx != nil && ginCtx.Request != nil {
			incomingHeaders = ginCtx.Request.Header
		}
	}
	stabilizeDeviceProfile := helps.ClaudeDeviceProfileStabilizationEnabled(cfg)
	var deviceProfile helps.ClaudeDeviceProfile
	if stabilizeDeviceProfile {
		var errDeviceProfile error
		deviceProfile, errDeviceProfile = helps.ResolveClaudeDeviceProfileRequired(r.Context(), auth, apiKey, incomingHeaders, cfg)
		if errDeviceProfile != nil {
			return errDeviceProfile
		}
	}

	baseBetas := "claude-code-20250219,oauth-2025-04-20,interleaved-thinking-2025-05-14,context-management-2025-06-27,prompt-caching-scope-2026-01-05,structured-outputs-2025-12-15,fast-mode-2026-02-01,redact-thinking-2026-02-12,token-efficient-tools-2026-03-28"
	if val := strings.TrimSpace(strings.Join(incomingHeaders.Values("Anthropic-Beta"), ",")); val != "" {
		baseBetas = val
		if !strings.Contains(val, "oauth") {
			baseBetas += ",oauth-2025-04-20"
		}
	}
	if !strings.Contains(baseBetas, "interleaved-thinking") {
		baseBetas += ",interleaved-thinking-2025-05-14"
	}

	// Merge extra betas from request body and request flags.
	if len(extraBetas) > 0 {
		existingSet := make(map[string]bool)
		for _, b := range strings.Split(baseBetas, ",") {
			betaName := strings.TrimSpace(b)
			if betaName != "" {
				existingSet[betaName] = true
			}
		}
		for _, beta := range extraBetas {
			beta = strings.TrimSpace(beta)
			if beta != "" && !existingSet[beta] {
				baseBetas += "," + beta
				existingSet[beta] = true
			}
		}
	}
	// Anthropic-Beta header handling:
	//   - strip_anthropic_beta=true (per-entry opt-in): explicitly delete the header.
	//     Used by third-party proxies that forward to Bedrock (e.g. TaijiAI).
	//   - otherwise: set the merged base betas. Anthropic direct and Anthropic-compatible
	//     proxies (polo, openrouter /api/v1/messages, etc.) all accept these betas.
	stripBeta := auth != nil && auth.Attributes != nil && auth.Attributes["strip_anthropic_beta"] == "true"
	if stripBeta {
		r.Header.Del("Anthropic-Beta")
	} else {
		r.Header.Set("Anthropic-Beta", baseBetas)
	}

	misc.EnsureHeader(r.Header, incomingHeaders, "Anthropic-Version", "2023-06-01")
	// Only set browser access header for API key mode; real Claude Code CLI does not send it.
	if useAPIKey {
		misc.EnsureHeader(r.Header, incomingHeaders, "Anthropic-Dangerous-Direct-Browser-Access", "true")
	}
	misc.EnsureHeader(r.Header, incomingHeaders, "X-App", "cli")
	// Values below match Claude Code 2.1.63 / @anthropic-ai/sdk 0.74.0 (updated 2026-02-28).
	misc.EnsureHeader(r.Header, incomingHeaders, "X-Stainless-Retry-Count", "0")
	misc.EnsureHeader(r.Header, incomingHeaders, "X-Stainless-Runtime", "node")
	misc.EnsureHeader(r.Header, incomingHeaders, "X-Stainless-Lang", "js")
	misc.EnsureHeader(r.Header, incomingHeaders, "X-Stainless-Timeout", hdrDefault(hd.Timeout, "600"))
	// Session ID: stable per auth/apiKey, matches Claude Code's X-Claude-Code-Session-Id header.
	sessionID, errSessionID := helps.CachedSessionIDRequired(r.Context(), apiKey)
	if errSessionID != nil {
		return errSessionID
	}
	misc.EnsureHeader(r.Header, incomingHeaders, "X-Claude-Code-Session-Id", sessionID)
	// Per-request UUID, matches Claude Code's x-client-request-id for first-party API.
	if isAnthropicBase {
		misc.EnsureHeader(r.Header, incomingHeaders, "x-client-request-id", uuid.New().String())
	}
	r.Header.Set("Connection", "keep-alive")
	if stream {
		r.Header.Set("Accept", "text/event-stream")
		// SSE streams must not be compressed: the downstream scanner reads
		// line-delimited text and cannot parse compressed bytes.  Using
		// "identity" tells the upstream to send an uncompressed stream.
		r.Header.Set("Accept-Encoding", "identity")
	} else {
		r.Header.Set("Accept", "application/json")
		r.Header.Set("Accept-Encoding", "gzip, deflate, br, zstd")
	}
	// Legacy mode keeps OS/Arch runtime-derived; stabilized mode pins OS/Arch
	// to the configured baseline while still allowing newer official
	// User-Agent/package/runtime tuples to upgrade the software fingerprint.
	if stabilizeDeviceProfile {
		helps.ApplyClaudeDeviceProfileHeaders(r, deviceProfile)
	} else {
		helps.ApplyClaudeLegacyDeviceHeaders(r, incomingHeaders, cfg, false)
	}
	var attrs map[string]string
	if auth != nil {
		attrs = auth.Attributes
	}
	util.ApplyCustomHeadersFromAttrs(r, attrs)
	// Re-enforce Accept-Encoding: identity after ApplyCustomHeadersFromAttrs, which
	// may override it with a user-configured value.  Compressed SSE breaks the line
	// scanner regardless of user preference, so this is non-negotiable for streams.
	if stream {
		r.Header.Set("Accept-Encoding", "identity")
	}
	return nil
}

// relayLacksCountTokens reports whether the upstream Anthropic-compatible
// relay at baseURL is known to NOT implement /v1/messages/count_tokens.
// Hitting count_tokens on these returns 404 which would otherwise be classified
// as an auth failure and disable the channel for legitimate /v1/messages
// traffic too.
func relayLacksCountTokens(baseURL string) bool {
	const knownNoCountTokens = "qinghuaapi.com"
	return strings.Contains(baseURL, knownNoCountTokens)
}

// isAnthropicHostBaseURL reports whether a base URL points at Anthropic's
// official API endpoint. Only real Anthropic accepts every Anthropic Messages
// field (context_management, betas, anthropic_beta); third-party relays reject
// unknown fields. Anthropic uses api.anthropic.com; some clients also point at
// api-*.anthropic.com previews.
func isAnthropicHostBaseURL(baseURL string) bool {
	if baseURL == "" {
		return false
	}
	lower := strings.ToLower(baseURL)
	return strings.Contains(lower, "anthropic.com")
}

// claudeFullURL returns the per-auth `full_url` override when set. When empty,
// the caller falls back to the default `{baseURL}/v1/messages?beta=true` path.
func claudeFullURL(a *cliproxyauth.Auth) string {
	if a == nil || a.Attributes == nil {
		return ""
	}
	return strings.TrimSpace(a.Attributes["full_url"])
}

func claudeCreds(a *cliproxyauth.Auth) (apiKey, baseURL string) {
	if a == nil {
		return "", ""
	}
	if a.Attributes != nil {
		apiKey = a.Attributes["api_key"]
		baseURL = a.Attributes["base_url"]
	}
	if apiKey == "" && a.Metadata != nil {
		if v, ok := a.Metadata["access_token"].(string); ok {
			apiKey = v
		}
	}
	return
}

func checkSystemInstructions(payload []byte) []byte {
	return checkSystemInstructionsWithSigningMode(payload, false, false, false, "2.1.63", "", "")
}

func rebuildMidSystemMessagesToTopLevel(payload []byte) []byte {
	messages := gjson.GetBytes(payload, "messages")
	if !messages.IsArray() {
		return payload
	}

	var movedSystemParts []string
	keptMessages := make([]string, 0, int(messages.Get("#").Int()))
	messages.ForEach(func(_, message gjson.Result) bool {
		if strings.EqualFold(strings.TrimSpace(message.Get("role").String()), "system") {
			movedSystemParts = append(movedSystemParts, claudeSystemTextParts(message.Get("content"))...)
			return true
		}
		keptMessages = append(keptMessages, message.Raw)
		return true
	})
	if len(movedSystemParts) == 0 {
		return payload
	}

	systemParts := claudeSystemTextParts(gjson.GetBytes(payload, "system"))
	systemParts = append(systemParts, movedSystemParts...)
	if len(systemParts) > 0 {
		if updated, errSetSystem := sjson.SetRawBytes(payload, "system", rawJSONArray(systemParts)); errSetSystem == nil {
			payload = updated
		}
	}
	if updated, errSetMessages := sjson.SetRawBytes(payload, "messages", rawJSONArray(keptMessages)); errSetMessages == nil {
		payload = updated
	}
	return payload
}

func claudeSystemTextParts(content gjson.Result) []string {
	if !content.Exists() {
		return nil
	}
	if content.Type == gjson.String {
		text := content.String()
		if strings.TrimSpace(text) == "" {
			return nil
		}
		block := []byte(`{"type":"text","text":""}`)
		block, _ = sjson.SetBytes(block, "text", text)
		return []string{string(block)}
	}
	if !content.IsArray() {
		return nil
	}

	var parts []string
	content.ForEach(func(_, item gjson.Result) bool {
		if item.Type == gjson.String {
			text := item.String()
			if strings.TrimSpace(text) != "" {
				block := []byte(`{"type":"text","text":""}`)
				block, _ = sjson.SetBytes(block, "text", text)
				parts = append(parts, string(block))
			}
			return true
		}
		if item.IsObject() && item.Get("type").String() == "text" && strings.TrimSpace(item.Get("text").String()) != "" {
			parts = append(parts, item.Raw)
		}
		return true
	})
	return parts
}

func rawJSONArray(items []string) []byte {
	if len(items) == 0 {
		return []byte("[]")
	}
	var builder strings.Builder
	builder.WriteByte('[')
	for i, item := range items {
		if i > 0 {
			builder.WriteByte(',')
		}
		builder.WriteString(item)
	}
	builder.WriteByte(']')
	return []byte(builder.String())
}

func isClaudeOAuthToken(apiKey string) bool {
	return strings.Contains(apiKey, "sk-ant-oat")
}

// prepareClaudeOAuthToolNamesForUpstream applies the Claude OAuth tool-name
// transforms in the same order across request paths. Remap runs before prefixing
// so any future non-empty prefix still composes correctly with the per-request
// reverse map.
func prepareClaudeOAuthToolNamesForUpstream(body []byte, prefix string, prefixDisabled bool) ([]byte, map[string]string) {
	body, reverseMap := remapOAuthToolNames(body)
	if !prefixDisabled {
		body = applyClaudeToolPrefix(body, prefix)
	}
	return body, reverseMap
}

// restoreClaudeOAuthToolNamesFromResponse undoes the Claude OAuth tool-name
// transforms for non-stream responses in reverse order.
func restoreClaudeOAuthToolNamesFromResponse(body []byte, prefix string, prefixDisabled bool, reverseMap map[string]string) []byte {
	if !prefixDisabled {
		body = stripClaudeToolPrefixFromResponse(body, prefix)
	}
	return reverseRemapOAuthToolNames(body, reverseMap)
}

// restoreClaudeOAuthToolNamesFromStreamLine undoes the Claude OAuth tool-name
// transforms for SSE lines in reverse order.
func restoreClaudeOAuthToolNamesFromStreamLine(line []byte, prefix string, prefixDisabled bool, reverseMap map[string]string) []byte {
	if !prefixDisabled {
		line = stripClaudeToolPrefixFromStreamLine(line, prefix)
	}
	return reverseRemapOAuthToolNamesFromStreamLine(line, reverseMap)
}

// remapOAuthToolNames renames third-party tool names to Claude Code equivalents
// and removes tools without an official counterpart. This prevents Anthropic from
// fingerprinting the request as a third-party client via tool naming patterns.
//
// It operates on: tools[].name, tool_choice.name, and all tool_use/tool_reference
// references in messages. Removed tools' corresponding tool_result blocks are preserved
// (they just become orphaned, which is safe for Claude).
//
// The returned map is keyed on the upstream (TitleCase) name and maps to the
// client-supplied original name. Callers MUST pass this map to the reverse
// functions so only names the client actually caused us to rewrite are restored
// on the response. A global reverse map (the previous implementation) incorrectly
// rewrote names the client originally sent in TitleCase (e.g. `Bash`)
// when any OTHER tool in the same request triggered a forward rename (e.g.
// `glob` -> `Glob`), because the global reverse map contained `Bash` -> `bash`
// regardless of what the client originally sent.
func remapOAuthToolNames(body []byte) ([]byte, map[string]string) {
	reverseMap := make(map[string]string, len(oauthToolRenameMap))
	recordRename := func(original, renamed string) {
		// Preserve the first-seen original name if the same upstream name is
		// produced from multiple call sites; they all map back identically.
		if _, exists := reverseMap[renamed]; !exists {
			reverseMap[renamed] = original
		}
	}

	// 1. Rewrite tools array in a single pass (if present).
	// IMPORTANT: do not mutate names first and then rebuild from an older gjson
	// snapshot. gjson results are snapshots of the original bytes; rebuilding from a
	// stale snapshot will preserve removals but overwrite renamed names back to their
	// original lowercase values.
	tools := gjson.GetBytes(body, "tools")
	toolsNeedRewrite := false
	if tools.Exists() && tools.IsArray() {
		tools.ForEach(func(_, tool gjson.Result) bool {
			if tool.Get("type").Exists() && tool.Get("type").String() != "" {
				return true
			}
			name := tool.Get("name").String()
			toolsNeedRewrite = oauthToolsToRemove[name]
			if !toolsNeedRewrite {
				newName, ok := oauthToolRenameMap[name]
				toolsNeedRewrite = ok && newName != name
			}
			return !toolsNeedRewrite
		})
	}
	if toolsNeedRewrite {
		var toolsJSON strings.Builder
		toolsJSON.WriteByte('[')
		toolCount := 0
		tools.ForEach(func(_, tool gjson.Result) bool {
			// Keep Anthropic built-in tools (web_search, code_execution, etc.) unchanged.
			if tool.Get("type").Exists() && tool.Get("type").String() != "" {
				if toolCount > 0 {
					toolsJSON.WriteByte(',')
				}
				toolsJSON.WriteString(tool.Raw)
				toolCount++
				return true
			}

			name := tool.Get("name").String()
			if oauthToolsToRemove[name] {
				return true
			}

			toolJSON := tool.Raw
			if newName, ok := oauthToolRenameMap[name]; ok && newName != name {
				updatedTool, err := sjson.Set(toolJSON, "name", newName)
				if err == nil {
					toolJSON = updatedTool
					recordRename(name, newName)
				}
			}

			if toolCount > 0 {
				toolsJSON.WriteByte(',')
			}
			toolsJSON.WriteString(toolJSON)
			toolCount++
			return true
		})
		toolsJSON.WriteByte(']')
		body, _ = sjson.SetRawBytes(body, "tools", []byte(toolsJSON.String()))
	}

	// 2. Rename tool_choice if it references a known tool
	toolChoiceType := gjson.GetBytes(body, "tool_choice.type").String()
	if toolChoiceType == "tool" {
		tcName := gjson.GetBytes(body, "tool_choice.name").String()
		if oauthToolsToRemove[tcName] {
			// The chosen tool was removed from the tools array, so drop tool_choice to
			// keep the payload internally consistent and fall back to normal auto tool use.
			body, _ = sjson.DeleteBytes(body, "tool_choice")
		} else if newName, ok := oauthToolRenameMap[tcName]; ok && newName != tcName {
			body, _ = sjson.SetBytes(body, "tool_choice.name", newName)
			recordRename(tcName, newName)
		}
	}

	// 3. Rename tool references in messages
	messages := gjson.GetBytes(body, "messages")
	if messages.Exists() && messages.IsArray() {
		messages.ForEach(func(msgIndex, msg gjson.Result) bool {
			content := msg.Get("content")
			if !content.Exists() || !content.IsArray() {
				return true
			}
			content.ForEach(func(contentIndex, part gjson.Result) bool {
				partType := part.Get("type").String()
				switch partType {
				case "tool_use":
					name := part.Get("name").String()
					if newName, ok := oauthToolRenameMap[name]; ok && newName != name {
						path := fmt.Sprintf("messages.%d.content.%d.name", msgIndex.Int(), contentIndex.Int())
						body, _ = sjson.SetBytes(body, path, newName)
						recordRename(name, newName)
					}
				case "tool_reference":
					toolName := part.Get("tool_name").String()
					if newName, ok := oauthToolRenameMap[toolName]; ok && newName != toolName {
						path := fmt.Sprintf("messages.%d.content.%d.tool_name", msgIndex.Int(), contentIndex.Int())
						body, _ = sjson.SetBytes(body, path, newName)
						recordRename(toolName, newName)
					}
				case "tool_result":
					// Handle nested tool_reference blocks inside tool_result.content[]
					toolID := part.Get("tool_use_id").String()
					_ = toolID // tool_use_id stays as-is
					nestedContent := part.Get("content")
					if nestedContent.Exists() && nestedContent.IsArray() {
						nestedContent.ForEach(func(nestedIndex, nestedPart gjson.Result) bool {
							if nestedPart.Get("type").String() == "tool_reference" {
								nestedToolName := nestedPart.Get("tool_name").String()
								if newName, ok := oauthToolRenameMap[nestedToolName]; ok && newName != nestedToolName {
									nestedPath := fmt.Sprintf("messages.%d.content.%d.content.%d.tool_name", msgIndex.Int(), contentIndex.Int(), nestedIndex.Int())
									body, _ = sjson.SetBytes(body, nestedPath, newName)
									recordRename(nestedToolName, newName)
								}
							}
							return true
						})
					}
				}
				return true
			})
			return true
		})
	}

	return body, reverseMap
}

// reverseRemapOAuthToolNames reverses the tool name mapping for non-stream responses
// using the per-request map produced by remapOAuthToolNames. Names the client sent
// that were NOT forward-renamed are passed through unchanged.
func reverseRemapOAuthToolNames(body []byte, reverseMap map[string]string) []byte {
	if len(reverseMap) == 0 {
		return body
	}
	content := gjson.GetBytes(body, "content")
	if !content.Exists() || !content.IsArray() {
		return body
	}
	content.ForEach(func(index, part gjson.Result) bool {
		partType := part.Get("type").String()
		switch partType {
		case "tool_use":
			name := part.Get("name").String()
			if origName, ok := reverseMap[name]; ok {
				path := fmt.Sprintf("content.%d.name", index.Int())
				body, _ = sjson.SetBytes(body, path, origName)
			}
		case "tool_reference":
			toolName := part.Get("tool_name").String()
			if origName, ok := reverseMap[toolName]; ok {
				path := fmt.Sprintf("content.%d.tool_name", index.Int())
				body, _ = sjson.SetBytes(body, path, origName)
			}
		}
		return true
	})
	return body
}

// reverseRemapOAuthToolNamesFromStreamLine reverses the tool name mapping for SSE
// stream lines, using the per-request reverseMap produced by remapOAuthToolNames.
func reverseRemapOAuthToolNamesFromStreamLine(line []byte, reverseMap map[string]string) []byte {
	if len(reverseMap) == 0 {
		return line
	}
	payload := helps.JSONPayload(line)
	if len(payload) == 0 || !gjson.ValidBytes(payload) {
		return line
	}

	contentBlock := gjson.GetBytes(payload, "content_block")
	if !contentBlock.Exists() {
		return line
	}

	blockType := contentBlock.Get("type").String()
	var updated []byte
	var err error

	switch blockType {
	case "tool_use":
		name := contentBlock.Get("name").String()
		if origName, ok := reverseMap[name]; ok {
			updated, err = sjson.SetBytes(payload, "content_block.name", origName)
			if err != nil {
				return line
			}
		} else {
			return line
		}
	case "tool_reference":
		toolName := contentBlock.Get("tool_name").String()
		if origName, ok := reverseMap[toolName]; ok {
			updated, err = sjson.SetBytes(payload, "content_block.tool_name", origName)
			if err != nil {
				return line
			}
		} else {
			return line
		}
	default:
		return line
	}

	trimmed := bytes.TrimSpace(line)
	if bytes.HasPrefix(trimmed, []byte("data:")) {
		return append([]byte("data: "), updated...)
	}
	return updated
}

func applyClaudeToolPrefix(body []byte, prefix string) []byte {
	if prefix == "" {
		return body
	}

	// Collect built-in tool names from the authoritative fallback seed list and
	// augment it with any typed built-ins present in the current request body.
	builtinTools := helps.AugmentClaudeBuiltinToolRegistry(body, nil)

	if tools := gjson.GetBytes(body, "tools"); tools.Exists() && tools.IsArray() {
		tools.ForEach(func(index, tool gjson.Result) bool {
			// Skip built-in tools (web_search, code_execution, etc.) which have
			// a "type" field and require their name to remain unchanged.
			if tool.Get("type").Exists() && tool.Get("type").String() != "" {
				if n := tool.Get("name").String(); n != "" {
					builtinTools[n] = true
				}
				return true
			}
			name := tool.Get("name").String()
			if name == "" || strings.HasPrefix(name, prefix) {
				return true
			}
			path := fmt.Sprintf("tools.%d.name", index.Int())
			body, _ = sjson.SetBytes(body, path, prefix+name)
			return true
		})
	}

	if gjson.GetBytes(body, "tool_choice.type").String() == "tool" {
		name := gjson.GetBytes(body, "tool_choice.name").String()
		if name != "" && !strings.HasPrefix(name, prefix) && !builtinTools[name] {
			body, _ = sjson.SetBytes(body, "tool_choice.name", prefix+name)
		}
	}

	if messages := gjson.GetBytes(body, "messages"); messages.Exists() && messages.IsArray() {
		messages.ForEach(func(msgIndex, msg gjson.Result) bool {
			content := msg.Get("content")
			if !content.Exists() || !content.IsArray() {
				return true
			}
			content.ForEach(func(contentIndex, part gjson.Result) bool {
				partType := part.Get("type").String()
				switch partType {
				case "tool_use":
					name := part.Get("name").String()
					if name == "" || strings.HasPrefix(name, prefix) || builtinTools[name] {
						return true
					}
					path := fmt.Sprintf("messages.%d.content.%d.name", msgIndex.Int(), contentIndex.Int())
					body, _ = sjson.SetBytes(body, path, prefix+name)
				case "tool_reference":
					toolName := part.Get("tool_name").String()
					if toolName == "" || strings.HasPrefix(toolName, prefix) || builtinTools[toolName] {
						return true
					}
					path := fmt.Sprintf("messages.%d.content.%d.tool_name", msgIndex.Int(), contentIndex.Int())
					body, _ = sjson.SetBytes(body, path, prefix+toolName)
				case "tool_result":
					// Handle nested tool_reference blocks inside tool_result.content[]
					nestedContent := part.Get("content")
					if nestedContent.Exists() && nestedContent.IsArray() {
						nestedContent.ForEach(func(nestedIndex, nestedPart gjson.Result) bool {
							if nestedPart.Get("type").String() == "tool_reference" {
								nestedToolName := nestedPart.Get("tool_name").String()
								if nestedToolName != "" && !strings.HasPrefix(nestedToolName, prefix) && !builtinTools[nestedToolName] {
									nestedPath := fmt.Sprintf("messages.%d.content.%d.content.%d.tool_name", msgIndex.Int(), contentIndex.Int(), nestedIndex.Int())
									body, _ = sjson.SetBytes(body, nestedPath, prefix+nestedToolName)
								}
							}
							return true
						})
					}
				}
				return true
			})
			return true
		})
	}

	return body
}

func stripClaudeToolPrefixFromResponse(body []byte, prefix string) []byte {
	if prefix == "" {
		return body
	}
	content := gjson.GetBytes(body, "content")
	if !content.Exists() || !content.IsArray() {
		return body
	}
	content.ForEach(func(index, part gjson.Result) bool {
		partType := part.Get("type").String()
		switch partType {
		case "tool_use":
			name := part.Get("name").String()
			if !strings.HasPrefix(name, prefix) {
				return true
			}
			path := fmt.Sprintf("content.%d.name", index.Int())
			body, _ = sjson.SetBytes(body, path, strings.TrimPrefix(name, prefix))
		case "tool_reference":
			toolName := part.Get("tool_name").String()
			if !strings.HasPrefix(toolName, prefix) {
				return true
			}
			path := fmt.Sprintf("content.%d.tool_name", index.Int())
			body, _ = sjson.SetBytes(body, path, strings.TrimPrefix(toolName, prefix))
		case "tool_result":
			// Handle nested tool_reference blocks inside tool_result.content[]
			nestedContent := part.Get("content")
			if nestedContent.Exists() && nestedContent.IsArray() {
				nestedContent.ForEach(func(nestedIndex, nestedPart gjson.Result) bool {
					if nestedPart.Get("type").String() == "tool_reference" {
						nestedToolName := nestedPart.Get("tool_name").String()
						if strings.HasPrefix(nestedToolName, prefix) {
							nestedPath := fmt.Sprintf("content.%d.content.%d.tool_name", index.Int(), nestedIndex.Int())
							body, _ = sjson.SetBytes(body, nestedPath, strings.TrimPrefix(nestedToolName, prefix))
						}
					}
					return true
				})
			}
		}
		return true
	})
	return body
}

func stripClaudeToolPrefixFromStreamLine(line []byte, prefix string) []byte {
	if prefix == "" {
		return line
	}
	payload := helps.JSONPayload(line)
	if len(payload) == 0 || !gjson.ValidBytes(payload) {
		return line
	}
	contentBlock := gjson.GetBytes(payload, "content_block")
	if !contentBlock.Exists() {
		return line
	}

	blockType := contentBlock.Get("type").String()
	var updated []byte
	var err error

	switch blockType {
	case "tool_use":
		name := contentBlock.Get("name").String()
		if !strings.HasPrefix(name, prefix) {
			return line
		}
		updated, err = sjson.SetBytes(payload, "content_block.name", strings.TrimPrefix(name, prefix))
		if err != nil {
			return line
		}
	case "tool_reference":
		toolName := contentBlock.Get("tool_name").String()
		if !strings.HasPrefix(toolName, prefix) {
			return line
		}
		updated, err = sjson.SetBytes(payload, "content_block.tool_name", strings.TrimPrefix(toolName, prefix))
		if err != nil {
			return line
		}
	default:
		return line
	}

	trimmed := bytes.TrimSpace(line)
	if bytes.HasPrefix(trimmed, []byte("data:")) {
		return append([]byte("data: "), updated...)
	}
	return updated
}

// getClientUserAgent extracts the client User-Agent from the gin context.
func getClientUserAgent(ctx context.Context) string {
	if ginCtx, ok := ctx.Value("gin").(*gin.Context); ok && ginCtx != nil && ginCtx.Request != nil {
		return ginCtx.GetHeader("User-Agent")
	}
	return ""
}

// parseEntrypointFromUA extracts the entrypoint from a Claude Code User-Agent.
// Format: "claude-cli/x.y.z (external, cli)" → "cli"
// Format: "claude-cli/x.y.z (external, vscode)" → "vscode"
// Returns "cli" if parsing fails or UA is not Claude Code.
func parseEntrypointFromUA(userAgent string) string {
	// Find content inside parentheses
	start := strings.Index(userAgent, "(")
	end := strings.LastIndex(userAgent, ")")
	if start < 0 || end <= start {
		return "cli"
	}
	inner := userAgent[start+1 : end]
	// Split by comma, take the second part (entrypoint is at index 1, after USER_TYPE)
	// Format: "(USER_TYPE, ENTRYPOINT[, extra...])"
	parts := strings.Split(inner, ",")
	if len(parts) >= 2 {
		ep := strings.TrimSpace(parts[1])
		if ep != "" {
			return ep
		}
	}
	return "cli"
}

// getWorkloadFromContext extracts workload identifier from the gin request headers.
func getWorkloadFromContext(ctx context.Context) string {
	if ginCtx, ok := ctx.Value("gin").(*gin.Context); ok && ginCtx != nil && ginCtx.Request != nil {
		return strings.TrimSpace(ginCtx.GetHeader("X-CPA-Claude-Workload"))
	}
	return ""
}

// getCloakConfigFromAuth extracts cloak configuration from the auth's attributes,
// falling back to its stored metadata (the raw OAuth/token JSON). Returns
// (cloakMode, strictMode, sensitiveWords, cacheUserID); an empty cloakMode means
// the credential did not explicitly configure a mode.
func getCloakConfigFromAuth(auth *cliproxyauth.Auth) (cloakMode string, strictMode bool, sensitiveWords []string, cacheUserID bool) {
	if auth == nil {
		return "", false, nil, false
	}

	// lookupCloakAttr prefers the executor-facing Attributes, then falls back to the
	// raw metadata blob (e.g. the OAuth/token JSON) so file-based credentials can
	// carry cloak settings without a matching claude-api-key config entry.
	lookupCloakAttr := func(key string) string {
		if auth.Attributes != nil {
			if value := strings.TrimSpace(auth.Attributes[key]); value != "" {
				return value
			}
		}
		if auth.Metadata != nil {
			if value, ok := auth.Metadata[key].(string); ok {
				return strings.TrimSpace(value)
			}
		}
		return ""
	}

	// An empty cloakMode means this credential did not explicitly configure a mode,
	// allowing the caller to fall back to the global/default behavior.
	cloakMode = lookupCloakAttr("cloak_mode")

	strictMode = strings.EqualFold(lookupCloakAttr("cloak_strict_mode"), "true")

	if wordsStr := lookupCloakAttr("cloak_sensitive_words"); wordsStr != "" {
		sensitiveWords = strings.Split(wordsStr, ",")
		for i := range sensitiveWords {
			sensitiveWords[i] = strings.TrimSpace(sensitiveWords[i])
		}
	}

	cacheUserID = strings.EqualFold(lookupCloakAttr("cloak_cache_user_id"), "true")

	return cloakMode, strictMode, sensitiveWords, cacheUserID
}

// injectFakeUserID generates and injects a fake user ID into the request metadata.
// When useCache is false, a new user ID is generated for every call.
func injectFakeUserID(ctx context.Context, payload []byte, apiKey string, useCache bool) ([]byte, error) {
	generateID := func() (string, error) {
		if useCache {
			return helps.CachedUserIDRequired(ctx, apiKey)
		}
		return helps.GenerateFakeUserID(), nil
	}

	metadata := gjson.GetBytes(payload, "metadata")
	if !metadata.Exists() {
		userID, errUserID := generateID()
		if errUserID != nil {
			return nil, errUserID
		}
		payload, _ = sjson.SetBytes(payload, "metadata.user_id", userID)
		return payload, nil
	}

	existingUserID := gjson.GetBytes(payload, "metadata.user_id").String()
	if existingUserID == "" || !helps.IsValidUserID(existingUserID) {
		userID, errUserID := generateID()
		if errUserID != nil {
			return nil, errUserID
		}
		payload, _ = sjson.SetBytes(payload, "metadata.user_id", userID)
	}
	return payload, nil
}

// fingerprintSalt is the salt used by Claude Code to compute the 3-char build fingerprint.
const fingerprintSalt = "59cf53e54c78"

// computeFingerprint computes the 3-char build fingerprint that Claude Code embeds in cc_version.
// Algorithm: SHA256(salt + messageText[4] + messageText[7] + messageText[20] + version)[:3]
func computeFingerprint(messageText, version string) string {
	indices := [3]int{4, 7, 20}
	runes := []rune(messageText)
	var sb strings.Builder
	for _, idx := range indices {
		if idx < len(runes) {
			sb.WriteRune(runes[idx])
		} else {
			sb.WriteRune('0')
		}
	}
	input := fingerprintSalt + sb.String() + version
	h := sha256.Sum256([]byte(input))
	return hex.EncodeToString(h[:])[:3]
}

// generateBillingHeader creates the x-anthropic-billing-header text block that
// real Claude Code prepends to every system prompt array.
// Format: x-anthropic-billing-header: cc_version=<ver>.<build>; cc_entrypoint=<ep>; cch=<hash>; [cc_workload=<wl>;]
func generateBillingHeader(payload []byte, experimentalCCHSigning bool, version, messageText, entrypoint, workload string) string {
	if entrypoint == "" {
		entrypoint = "cli"
	}
	buildHash := computeFingerprint(messageText, version)
	workloadPart := ""
	if workload != "" {
		workloadPart = fmt.Sprintf(" cc_workload=%s;", workload)
	}

	if experimentalCCHSigning {
		return fmt.Sprintf("x-anthropic-billing-header: cc_version=%s.%s; cc_entrypoint=%s; cch=00000;%s", version, buildHash, entrypoint, workloadPart)
	}

	// Generate a deterministic cch hash from the payload content (system + messages + tools).
	h := sha256.Sum256(payload)
	cch := hex.EncodeToString(h[:])[:5]
	return fmt.Sprintf("x-anthropic-billing-header: cc_version=%s.%s; cc_entrypoint=%s; cch=%s;%s", version, buildHash, entrypoint, cch, workloadPart)
}

func checkSystemInstructionsWithMode(payload []byte, strictMode bool) []byte {
	return checkSystemInstructionsWithSigningMode(payload, strictMode, false, false, "2.1.63", "", "")
}

// checkSystemInstructionsWithSigningMode injects Claude Code-style system blocks:
//
//	system[0]: billing header (no cache_control)
//	system[1]: agent identifier (cache_control ephemeral, scope=org)
//	system[2]: core intro prompt (cache_control ephemeral, scope=global)
//	system[3]: system instructions (no cache_control)
//	system[4]: doing tasks (no cache_control)
//	system[5]: user system messages moved to first user message
func checkSystemInstructionsWithSigningMode(payload []byte, strictMode bool, experimentalCCHSigning bool, oauthMode bool, version, entrypoint, workload string) []byte {
	system := gjson.GetBytes(payload, "system")

	// Extract original message text for fingerprint computation (before billing injection).
	// Use the first system text block's content as the fingerprint source.
	messageText := ""
	if system.IsArray() {
		system.ForEach(func(_, part gjson.Result) bool {
			if part.Get("type").String() == "text" {
				messageText = part.Get("text").String()
				return false
			}
			return true
		})
	} else if system.Type == gjson.String {
		messageText = system.String()
	}

	// Skip if already injected
	firstText := gjson.GetBytes(payload, "system.0.text").String()
	if strings.HasPrefix(firstText, "x-anthropic-billing-header:") {
		return payload
	}

	billingText := generateBillingHeader(payload, experimentalCCHSigning, version, messageText, entrypoint, workload)
	billingBlock := buildTextBlock(billingText, nil)

	// Build system blocks matching real Claude Code structure.
	// Important: Claude Code's internal cacheScope='org' does NOT serialize to
	// scope='org' in the API request. Only scope='global' is sent explicitly.
	// The system prompt prefix block is sent without cache_control.
	agentBlock := buildTextBlock("You are Claude Code, Anthropic's official CLI for Claude.", nil)
	staticPrompt := strings.Join([]string{
		helps.ClaudeCodeIntro,
		helps.ClaudeCodeSystem,
		helps.ClaudeCodeDoingTasks,
		helps.ClaudeCodeToneAndStyle,
		helps.ClaudeCodeOutputEfficiency,
	}, "\n\n")
	staticBlock := buildTextBlock(staticPrompt, nil)

	systemResult := "[" + billingBlock + "," + agentBlock + "," + staticBlock + "]"
	payload, _ = sjson.SetRawBytes(payload, "system", []byte(systemResult))

	// Collect user system instructions and prepend to first user message
	if !strictMode {
		var userSystemParts []string
		if system.IsArray() {
			system.ForEach(func(_, part gjson.Result) bool {
				if part.Get("type").String() == "text" {
					txt := strings.TrimSpace(part.Get("text").String())
					if txt != "" {
						userSystemParts = append(userSystemParts, txt)
					}
				}
				return true
			})
		} else if system.Type == gjson.String && strings.TrimSpace(system.String()) != "" {
			userSystemParts = append(userSystemParts, strings.TrimSpace(system.String()))
		}

		if len(userSystemParts) > 0 {
			combined := strings.Join(userSystemParts, "\n\n")
			if oauthMode {
				combined = sanitizeForwardedSystemPrompt(combined)
			}
			if strings.TrimSpace(combined) != "" {
				payload = prependToFirstUserMessage(payload, combined)
			}
		}
	}

	return payload
}

// sanitizeForwardedSystemPrompt reduces forwarded third-party system context to a
// tiny neutral reminder for Claude OAuth cloaking. The goal is to preserve only
// the minimum tool/task guidance while removing virtually all client-specific
// prompt structure that Anthropic may classify as third-party agent traffic.
func sanitizeForwardedSystemPrompt(text string) string {
	if strings.TrimSpace(text) == "" {
		return ""
	}
	return strings.TrimSpace(`Use the available tools when needed to help with software engineering tasks.
Keep responses concise and focused on the user's request.
Prefer acting on the user's task over describing product-specific workflows.`)
}

// buildTextBlock constructs a JSON text block object with proper escaping.
// Uses sjson.SetBytes to handle multi-line text, quotes, and control characters.
// cacheControl is optional; pass nil to omit cache_control.
func buildTextBlock(text string, cacheControl map[string]string) string {
	block := []byte(`{"type":"text"}`)
	block, _ = sjson.SetBytes(block, "text", text)
	if cacheControl != nil && len(cacheControl) > 0 {
		// Build cache_control JSON manually to avoid sjson map marshaling issues.
		// sjson.SetBytes with map[string]string may not produce expected structure.
		cc := `{"type":"ephemeral"`
		if t, ok := cacheControl["ttl"]; ok {
			cc += fmt.Sprintf(`,"ttl":"%s"`, t)
		}
		cc += "}"
		block, _ = sjson.SetRawBytes(block, "cache_control", []byte(cc))
	}
	return string(block)
}

// prependToFirstUserMessage prepends text content to the first user message.
// This avoids putting non-Claude-Code system instructions in system[] which
// triggers Anthropic's extra usage billing for OAuth-proxied requests.
func prependToFirstUserMessage(payload []byte, text string) []byte {
	messages := gjson.GetBytes(payload, "messages")
	if !messages.Exists() || !messages.IsArray() {
		return payload
	}

	// Find the first user message index
	firstUserIdx := -1
	messages.ForEach(func(idx, msg gjson.Result) bool {
		if msg.Get("role").String() == "user" {
			firstUserIdx = int(idx.Int())
			return false
		}
		return true
	})

	if firstUserIdx < 0 {
		return payload
	}

	prefixBlock := fmt.Sprintf(`<system-reminder>
As you answer the user's questions, you can use the following context from the system:
%s

IMPORTANT: this context may or may not be relevant to your tasks. You should not respond to this context unless it is highly relevant to your task.
</system-reminder>
`, text)

	contentPath := fmt.Sprintf("messages.%d.content", firstUserIdx)
	content := gjson.GetBytes(payload, contentPath)

	if content.IsArray() {
		newBlock := fmt.Sprintf(`{"type":"text","text":%q}`, prefixBlock)
		var newArray string
		if content.Raw == "[]" || content.Raw == "" {
			newArray = "[" + newBlock + "]"
		} else {
			newArray = "[" + newBlock + "," + content.Raw[1:]
		}
		payload, _ = sjson.SetRawBytes(payload, contentPath, []byte(newArray))
	} else if content.Type == gjson.String {
		newText := prefixBlock + content.String()
		payload, _ = sjson.SetBytes(payload, contentPath, newText)
	}

	return payload
}

// applyCloaking applies cloaking transformations to the payload based on config and client.
// Cloaking includes: system prompt injection, fake user ID, and sensitive word obfuscation.
func applyCloaking(ctx context.Context, cfg *config.Config, auth *cliproxyauth.Auth, payload []byte, model string, apiKey string) ([]byte, error) {
	clientUserAgent := getClientUserAgent(ctx)
	// Enable cch signing for OAuth tokens by default (not just experimental flag).
	oauthToken := isClaudeOAuthToken(apiKey)
	useCCHSigning := oauthToken || experimentalCCHSigningEnabled(cfg, auth)

	// Get cloak config from ClaudeKey configuration
	cloakCfg := resolveClaudeKeyCloakConfig(cfg, auth)
	attrMode, attrStrict, attrWords, attrCache := getCloakConfigFromAuth(auth)

	// Determine cloak settings. Precedence (low -> high):
	//   built-in "auto" default
	//   -> global disable-claude-cloak-mode switch (forces "never")
	//   -> per-credential settings from auth attributes/metadata
	//   -> per claude-api-key cloak config
	cloakMode := "auto"
	if cfg != nil && cfg.DisableClaudeCloakMode {
		cloakMode = "never"
	}
	strictMode := attrStrict
	sensitiveWords := attrWords
	cacheUserID := attrCache

	if attrMode != "" {
		cloakMode = attrMode
	}

	if cloakCfg != nil {
		if mode := strings.TrimSpace(cloakCfg.Mode); mode != "" {
			cloakMode = mode
		}
		if cloakCfg.StrictMode {
			strictMode = true
		}
		if len(cloakCfg.SensitiveWords) > 0 {
			sensitiveWords = cloakCfg.SensitiveWords
		}
		if cloakCfg.CacheUserID != nil {
			cacheUserID = *cloakCfg.CacheUserID
		}
	}

	// Determine if cloaking should be applied
	if !helps.ShouldCloak(cloakMode, clientUserAgent) {
		return payload, nil
	}

	// Skip system instructions for claude-3-5-haiku models
	if !strings.HasPrefix(model, "claude-3-5-haiku") {
		billingVersion := helps.DefaultClaudeVersion(cfg)
		entrypoint := parseEntrypointFromUA(clientUserAgent)
		workload := getWorkloadFromContext(ctx)
		payload = checkSystemInstructionsWithSigningMode(payload, strictMode, useCCHSigning, oauthToken, billingVersion, entrypoint, workload)
	}

	// Inject fake user ID
	var errFakeUserID error
	payload, errFakeUserID = injectFakeUserID(ctx, payload, apiKey, cacheUserID)
	if errFakeUserID != nil {
		return nil, errFakeUserID
	}

	// Apply sensitive word obfuscation
	if len(sensitiveWords) > 0 {
		matcher := helps.BuildSensitiveWordMatcher(sensitiveWords)
		payload = helps.ObfuscateSensitiveWords(payload, matcher)
	}

	return payload, nil
}

// ensureCacheControl injects cache_control breakpoints into the payload for optimal prompt caching.
// According to Anthropic's documentation, cache prefixes are created in order: tools -> system -> messages.
// This function adds cache_control to:
// 1. The LAST non-deferred tool in the tools array (caches all preceding tool definitions)
// 2. The LAST system prompt element
// 3. The SECOND-TO-LAST user turn (caches conversation history for multi-turn)
//
// Up to 4 cache breakpoints are allowed per request. Tools, System, and Messages are INDEPENDENT breakpoints.
// This enables up to 90% cost reduction on cached tokens (cache read = 0.1x base price).
// See: https://docs.anthropic.com/en/docs/build-with-claude/prompt-caching
func ensureCacheControl(payload []byte) []byte {
	// 1. Inject cache_control into the LAST non-deferred tool
	// Tools are cached first in the hierarchy, so this is the most important breakpoint.
	payload = injectToolsCacheControl(payload)

	// 2. Inject cache_control into the LAST system prompt element
	// System is the second level in the cache hierarchy.
	payload = injectSystemCacheControl(payload)

	// 3. Inject cache_control into messages for multi-turn conversation caching
	// This caches the conversation history up to the second-to-last user turn.
	payload = injectMessagesCacheControl(payload)

	return payload
}

func countCacheControls(payload []byte) int {
	count := 0

	// Check system
	system := gjson.GetBytes(payload, "system")
	if system.IsArray() {
		system.ForEach(func(_, item gjson.Result) bool {
			if item.Get("cache_control").Exists() {
				count++
			}
			return true
		})
	}

	// Check tools
	tools := gjson.GetBytes(payload, "tools")
	if tools.IsArray() {
		tools.ForEach(func(_, item gjson.Result) bool {
			if item.Get("cache_control").Exists() {
				count++
			}
			return true
		})
	}

	// Check messages
	messages := gjson.GetBytes(payload, "messages")
	if messages.IsArray() {
		messages.ForEach(func(_, msg gjson.Result) bool {
			content := msg.Get("content")
			if content.IsArray() {
				content.ForEach(func(_, item gjson.Result) bool {
					if item.Get("cache_control").Exists() {
						count++
					}
					return true
				})
			}
			return true
		})
	}

	return count
}

// normalizeCacheControlTTL ensures cache_control TTL values don't violate the
// prompt-caching-scope-2026-01-05 ordering constraint: a 1h-TTL block must not
// appear after a 5m-TTL block anywhere in the evaluation order.
//
// Anthropic evaluates blocks in order: tools → system (index 0..N) → messages.
// Within each section, blocks are evaluated in array order. A 5m (default) block
// followed by a 1h block at ANY later position is an error — including within
// the same section (e.g. system[1]=5m then system[3]=1h).
//
// Strategy: walk all cache_control blocks in evaluation order. Once a 5m block
// is seen, strip ttl from ALL subsequent 1h blocks (downgrading them to 5m).
func normalizeCacheControlTTL(payload []byte) []byte {
	if len(payload) == 0 || !gjson.ValidBytes(payload) {
		return payload
	}

	original := payload
	seen5m := false
	modified := false

	processBlock := func(path string, obj gjson.Result) {
		cc := obj.Get("cache_control")
		if !cc.Exists() {
			return
		}
		if !cc.IsObject() {
			seen5m = true
			return
		}
		ttl := cc.Get("ttl")
		if ttl.Type != gjson.String || ttl.String() != "1h" {
			seen5m = true
			return
		}
		if !seen5m {
			return
		}
		ttlPath := path + ".cache_control.ttl"
		updated, errDel := sjson.DeleteBytes(payload, ttlPath)
		if errDel != nil {
			return
		}
		payload = updated
		modified = true
	}

	tools := gjson.GetBytes(payload, "tools")
	if tools.IsArray() {
		tools.ForEach(func(idx, item gjson.Result) bool {
			processBlock(fmt.Sprintf("tools.%d", int(idx.Int())), item)
			return true
		})
	}

	system := gjson.GetBytes(payload, "system")
	if system.IsArray() {
		system.ForEach(func(idx, item gjson.Result) bool {
			processBlock(fmt.Sprintf("system.%d", int(idx.Int())), item)
			return true
		})
	}

	messages := gjson.GetBytes(payload, "messages")
	if messages.IsArray() {
		messages.ForEach(func(msgIdx, msg gjson.Result) bool {
			content := msg.Get("content")
			if !content.IsArray() {
				return true
			}
			content.ForEach(func(itemIdx, item gjson.Result) bool {
				processBlock(fmt.Sprintf("messages.%d.content.%d", int(msgIdx.Int()), int(itemIdx.Int())), item)
				return true
			})
			return true
		})
	}

	if !modified {
		return original
	}
	return payload
}

// enforceCacheControlLimit removes excess cache_control blocks from a payload
// so the total does not exceed the Anthropic API limit (currently 4).
//
// Anthropic evaluates cache breakpoints in order: tools → system → messages.
// The most valuable breakpoints are:
//  1. Last tool         — caches ALL tool definitions
//  2. Last system block — caches ALL system content
//  3. Recent messages   — cache conversation context
//
// Removal priority (strip lowest-value first):
//
//	Phase 1: system blocks earliest-first, preserving the last one.
//	Phase 2: tool blocks earliest-first, preserving the last one.
//	Phase 3: message content blocks earliest-first.
//	Phase 4: remaining system blocks (last system).
//	Phase 5: remaining tool blocks (last tool).
func enforceCacheControlLimit(payload []byte, maxBlocks int) []byte {
	if len(payload) == 0 || !gjson.ValidBytes(payload) {
		return payload
	}

	total := countCacheControls(payload)
	if total <= maxBlocks {
		return payload
	}

	excess := total - maxBlocks

	system := gjson.GetBytes(payload, "system")
	if system.IsArray() {
		lastIdx := -1
		system.ForEach(func(idx, item gjson.Result) bool {
			if item.Get("cache_control").Exists() {
				lastIdx = int(idx.Int())
			}
			return true
		})
		if lastIdx >= 0 {
			system.ForEach(func(idx, item gjson.Result) bool {
				if excess <= 0 {
					return false
				}
				i := int(idx.Int())
				if i == lastIdx {
					return true
				}
				if !item.Get("cache_control").Exists() {
					return true
				}
				path := fmt.Sprintf("system.%d.cache_control", i)
				updated, errDel := sjson.DeleteBytes(payload, path)
				if errDel != nil {
					return true
				}
				payload = updated
				excess--
				return true
			})
		}
	}
	if excess <= 0 {
		return payload
	}

	tools := gjson.GetBytes(payload, "tools")
	if tools.IsArray() {
		lastIdx := -1
		tools.ForEach(func(idx, item gjson.Result) bool {
			if item.Get("cache_control").Exists() {
				lastIdx = int(idx.Int())
			}
			return true
		})
		if lastIdx >= 0 {
			tools.ForEach(func(idx, item gjson.Result) bool {
				if excess <= 0 {
					return false
				}
				i := int(idx.Int())
				if i == lastIdx {
					return true
				}
				if !item.Get("cache_control").Exists() {
					return true
				}
				path := fmt.Sprintf("tools.%d.cache_control", i)
				updated, errDel := sjson.DeleteBytes(payload, path)
				if errDel != nil {
					return true
				}
				payload = updated
				excess--
				return true
			})
		}
	}
	if excess <= 0 {
		return payload
	}

	messages := gjson.GetBytes(payload, "messages")
	if messages.IsArray() {
		messages.ForEach(func(msgIdx, msg gjson.Result) bool {
			if excess <= 0 {
				return false
			}
			content := msg.Get("content")
			if !content.IsArray() {
				return true
			}
			content.ForEach(func(itemIdx, item gjson.Result) bool {
				if excess <= 0 {
					return false
				}
				if !item.Get("cache_control").Exists() {
					return true
				}
				path := fmt.Sprintf("messages.%d.content.%d.cache_control", int(msgIdx.Int()), int(itemIdx.Int()))
				updated, errDel := sjson.DeleteBytes(payload, path)
				if errDel != nil {
					return true
				}
				payload = updated
				excess--
				return true
			})
			return true
		})
	}
	if excess <= 0 {
		return payload
	}

	system = gjson.GetBytes(payload, "system")
	if system.IsArray() {
		system.ForEach(func(idx, item gjson.Result) bool {
			if excess <= 0 {
				return false
			}
			if !item.Get("cache_control").Exists() {
				return true
			}
			path := fmt.Sprintf("system.%d.cache_control", int(idx.Int()))
			updated, errDel := sjson.DeleteBytes(payload, path)
			if errDel != nil {
				return true
			}
			payload = updated
			excess--
			return true
		})
	}
	if excess <= 0 {
		return payload
	}

	tools = gjson.GetBytes(payload, "tools")
	if tools.IsArray() {
		tools.ForEach(func(idx, item gjson.Result) bool {
			if excess <= 0 {
				return false
			}
			if !item.Get("cache_control").Exists() {
				return true
			}
			path := fmt.Sprintf("tools.%d.cache_control", int(idx.Int()))
			updated, errDel := sjson.DeleteBytes(payload, path)
			if errDel != nil {
				return true
			}
			payload = updated
			excess--
			return true
		})
	}

	return payload
}

// injectMessagesCacheControl adds cache_control to the second-to-last user turn for multi-turn caching.
// Per Anthropic docs: "Place cache_control on the second-to-last User message to let the model reuse the earlier cache."
// This enables caching of conversation history, which is especially beneficial for long multi-turn conversations.
// Only adds cache_control if:
// - There are at least 2 user turns in the conversation
// - No message content already has cache_control
func injectMessagesCacheControl(payload []byte) []byte {
	messages := gjson.GetBytes(payload, "messages")
	if !messages.Exists() || !messages.IsArray() {
		return payload
	}

	// Check if ANY message content already has cache_control
	hasCacheControlInMessages := false
	messages.ForEach(func(_, msg gjson.Result) bool {
		content := msg.Get("content")
		if content.IsArray() {
			content.ForEach(func(_, item gjson.Result) bool {
				if item.Get("cache_control").Exists() {
					hasCacheControlInMessages = true
					return false
				}
				return true
			})
		}
		return !hasCacheControlInMessages
	})
	if hasCacheControlInMessages {
		return payload
	}

	// Find all user message indices
	var userMsgIndices []int
	messages.ForEach(func(index gjson.Result, msg gjson.Result) bool {
		if msg.Get("role").String() == "user" {
			userMsgIndices = append(userMsgIndices, int(index.Int()))
		}
		return true
	})

	// Need at least 2 user turns to cache the second-to-last
	if len(userMsgIndices) < 2 {
		return payload
	}

	// Get the second-to-last user message index
	secondToLastUserIdx := userMsgIndices[len(userMsgIndices)-2]

	// Get the content of this message
	contentPath := fmt.Sprintf("messages.%d.content", secondToLastUserIdx)
	content := gjson.GetBytes(payload, contentPath)

	if content.IsArray() {
		// Add cache_control to the last content block of this message
		contentCount := int(content.Get("#").Int())
		if contentCount > 0 {
			cacheControlPath := fmt.Sprintf("messages.%d.content.%d.cache_control", secondToLastUserIdx, contentCount-1)
			result, err := sjson.SetBytes(payload, cacheControlPath, map[string]string{"type": "ephemeral"})
			if err != nil {
				log.Warnf("failed to inject cache_control into messages: %v", err)
				return payload
			}
			payload = result
		}
	} else if content.Type == gjson.String {
		// Convert string content to array with cache_control
		text := content.String()
		newContent := []map[string]interface{}{
			{
				"type": "text",
				"text": text,
				"cache_control": map[string]string{
					"type": "ephemeral",
				},
			},
		}
		result, err := sjson.SetBytes(payload, contentPath, newContent)
		if err != nil {
			log.Warnf("failed to inject cache_control into message string content: %v", err)
			return payload
		}
		payload = result
	}

	return payload
}

// injectToolsCacheControl adds cache_control to the last non-deferred tool in the tools array.
// Deferred tools cannot use prompt caching, so trailing deferred tools are skipped.
// This only adds cache_control if NO tool in the array already has it.
func injectToolsCacheControl(payload []byte) []byte {
	tools := gjson.GetBytes(payload, "tools")
	if !tools.Exists() || !tools.IsArray() {
		return payload
	}

	// Check if ANY tool already has cache_control and find the last eligible tool.
	hasCacheControlInTools := false
	lastEligibleToolIndex := -1
	tools.ForEach(func(index, tool gjson.Result) bool {
		if tool.Get("cache_control").Exists() {
			hasCacheControlInTools = true
			return false
		}
		if !tool.Get("defer_loading").Bool() {
			lastEligibleToolIndex = int(index.Int())
		}
		return true
	})
	if hasCacheControlInTools || lastEligibleToolIndex < 0 {
		return payload
	}

	lastToolPath := fmt.Sprintf("tools.%d.cache_control", lastEligibleToolIndex)
	result, err := sjson.SetBytes(payload, lastToolPath, map[string]string{"type": "ephemeral"})
	if err != nil {
		log.Warnf("failed to inject cache_control into tools array: %v", err)
		return payload
	}

	return result
}

// injectSystemCacheControl adds cache_control to the last element in the system prompt.
// Converts string system prompts to array format if needed.
// This only adds cache_control if NO system element already has it.
func injectSystemCacheControl(payload []byte) []byte {
	system := gjson.GetBytes(payload, "system")
	if !system.Exists() {
		return payload
	}

	if system.IsArray() {
		count := int(system.Get("#").Int())
		if count == 0 {
			return payload
		}

		// Check if ANY system element already has cache_control
		hasCacheControlInSystem := false
		system.ForEach(func(_, item gjson.Result) bool {
			if item.Get("cache_control").Exists() {
				hasCacheControlInSystem = true
				return false
			}
			return true
		})
		if hasCacheControlInSystem {
			return payload
		}

		// Add cache_control to the last system element
		lastSystemPath := fmt.Sprintf("system.%d.cache_control", count-1)
		result, err := sjson.SetBytes(payload, lastSystemPath, map[string]string{"type": "ephemeral"})
		if err != nil {
			log.Warnf("failed to inject cache_control into system array: %v", err)
			return payload
		}
		payload = result
	} else if system.Type == gjson.String {
		// Convert string system prompt to array with cache_control
		// "system": "text" -> "system": [{"type": "text", "text": "text", "cache_control": {"type": "ephemeral"}}]
		text := system.String()
		newSystem := []map[string]interface{}{
			{
				"type": "text",
				"text": text,
				"cache_control": map[string]string{
					"type": "ephemeral",
				},
			},
		}
		result, err := sjson.SetBytes(payload, "system", newSystem)
		if err != nil {
			log.Warnf("failed to inject cache_control into system string: %v", err)
			return payload
		}
		payload = result
	}

	return payload
}

func ensureModelMaxTokens(body []byte, modelID string) []byte {
	if len(body) == 0 || !gjson.ValidBytes(body) {
		return body
	}

	if maxTokens := gjson.GetBytes(body, "max_tokens"); maxTokens.Exists() {
		return body
	}

	for _, provider := range registry.GetGlobalRegistry().GetModelProviders(strings.TrimSpace(modelID)) {
		if strings.EqualFold(provider, "claude") {
			maxTokens := defaultModelMaxTokens
			if info := registry.GetGlobalRegistry().GetModelInfo(strings.TrimSpace(modelID), "claude"); info != nil && info.MaxCompletionTokens > 0 {
				maxTokens = info.MaxCompletionTokens
			}
			body, _ = sjson.SetBytes(body, "max_tokens", maxTokens)
			return body
		}
	}

	return body
}
