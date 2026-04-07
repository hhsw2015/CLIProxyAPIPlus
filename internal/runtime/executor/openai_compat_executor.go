package executor

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/cookiepool"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/thinking"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// OpenAICompatExecutor implements a stateless executor for OpenAI-compatible providers.
// It performs request/response translation and executes against the provider base URL
// using per-auth credentials (API key) and per-auth HTTP transport (proxy) from context.
type OpenAICompatExecutor struct {
	provider string
	cfg      *config.Config
}

// NewOpenAICompatExecutor creates an executor bound to a provider key (e.g., "openrouter").
func NewOpenAICompatExecutor(provider string, cfg *config.Config) *OpenAICompatExecutor {
	return &OpenAICompatExecutor{provider: provider, cfg: cfg}
}

// Identifier implements cliproxyauth.ProviderExecutor.
func (e *OpenAICompatExecutor) Identifier() string { return e.provider }

// PrepareRequest injects OpenAI-compatible credentials into the outgoing HTTP request.
func (e *OpenAICompatExecutor) PrepareRequest(req *http.Request, auth *cliproxyauth.Auth) error {
	if req == nil {
		return nil
	}
	_, apiKey := e.resolveCredentials(auth)
	if strings.TrimSpace(apiKey) != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	var attrs map[string]string
	if auth != nil {
		attrs = auth.Attributes
	}
	util.ApplyCustomHeadersFromAttrs(req, attrs)
	// Cookie pool: if this auth has a cookie pool, pick a cookie and inject its headers.
	applyCookiePoolHeaders(req, auth)
	return nil
}

// HttpRequest injects OpenAI-compatible credentials into the request and executes it.
func (e *OpenAICompatExecutor) HttpRequest(ctx context.Context, auth *cliproxyauth.Auth, req *http.Request) (*http.Response, error) {
	if req == nil {
		return nil, fmt.Errorf("openai compat executor: request is nil")
	}
	if ctx == nil {
		ctx = req.Context()
	}
	httpReq := req.WithContext(ctx)
	if err := e.PrepareRequest(httpReq, auth); err != nil {
		return nil, err
	}
	httpClient := helps.NewProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	return httpClient.Do(httpReq)
}

func (e *OpenAICompatExecutor) Execute(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (resp cliproxyexecutor.Response, err error) {
	baseModel := thinking.ParseSuffix(req.Model).ModelName

	reporter := helps.NewUsageReporter(ctx, e.Identifier(), baseModel, auth)
	defer reporter.TrackFailure(ctx, &err)

	baseURL, apiKey := e.resolveCredentials(auth)
	if baseURL == "" {
		err = statusErr{code: http.StatusUnauthorized, msg: "missing provider baseURL"}
		return
	}

	from := opts.SourceFormat
	to := sdktranslator.FromString("openai")
	endpoint := "/chat/completions"
	if opts.Alt == "responses/compact" {
		to = sdktranslator.FromString("openai-response")
		endpoint = "/responses/compact"
	}
	originalPayloadSource := req.Payload
	if len(opts.OriginalRequest) > 0 {
		originalPayloadSource = opts.OriginalRequest
	}
	originalPayload := originalPayloadSource
	originalTranslated := sdktranslator.TranslateRequest(from, to, baseModel, originalPayload, opts.Stream)
	translated := sdktranslator.TranslateRequest(from, to, baseModel, req.Payload, opts.Stream)
	requestedModel := helps.PayloadRequestedModel(opts, req.Model)
	translated = helps.ApplyPayloadConfigWithRoot(e.cfg, baseModel, to.String(), "", translated, originalTranslated, requestedModel)
	if opts.Alt == "responses/compact" {
		if updated, errDelete := sjson.DeleteBytes(translated, "stream"); errDelete == nil {
			translated = updated
		}
	}

	translated, err = thinking.ApplyThinking(translated, req.Model, from.String(), to.String(), e.Identifier())
	if err != nil {
		return resp, err
	}

	url := strings.TrimSuffix(baseURL, "/") + endpoint
	httpClient := helps.NewProxyAwareHTTPClient(ctx, e.cfg, auth, 0)

	var pool *cookiepool.Pool
	if auth != nil && auth.Attributes != nil {
		if poolName := strings.TrimSpace(auth.Attributes["cookie_pool_name"]); poolName != "" {
			pool = cookiepool.Get(poolName)
		}
	}

	for {
		if ctx.Err() != nil {
			err = ctx.Err()
			return
		}

		httpReq, errReq := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(translated))
		if errReq != nil {
			return resp, errReq
		}
		httpReq.Header.Set("Content-Type", "application/json")
		if apiKey != "" {
			httpReq.Header.Set("Authorization", "Bearer "+apiKey)
		}
		httpReq.Header.Set("User-Agent", "cli-proxy-openai-compat")
		var attrs map[string]string
		if auth != nil {
			attrs = auth.Attributes
		}
		util.ApplyCustomHeadersFromAttrs(httpReq, attrs)
		cookieEntry := applyCookiePoolHeaders(httpReq, auth)

		var authID, authLabel, authType, authValue string
		if auth != nil {
			authID = auth.ID
			authLabel = auth.Label
			authType, authValue = auth.AccountInfo()
		}
		helps.RecordAPIRequest(ctx, e.cfg, helps.UpstreamRequestLog{
			URL:       url,
			Method:    http.MethodPost,
			Headers:   httpReq.Header.Clone(),
			Body:      translated,
			Provider:  e.Identifier(),
			AuthID:    authID,
			AuthLabel: authLabel,
			AuthType:  authType,
			AuthValue: authValue,
		})

		httpResp, errExec := httpClient.Do(httpReq)
		if errExec != nil {
			helps.RecordAPIResponseError(ctx, e.cfg, errExec)
			if pool != nil && cookieEntry != nil && ctx.Err() == nil {
				log.Warnf("openai compat executor: request error: %v, retrying with next cookie", errExec)
				continue
			}
			return resp, errExec
		}

		helps.RecordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())
		if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
			b, _ := io.ReadAll(httpResp.Body)
			httpResp.Body.Close()
			helps.AppendAPIResponseChunk(ctx, e.cfg, b)
			helps.LogWithRequestID(ctx).Debugf("request error, error status: %d, error message: %s", httpResp.StatusCode, helps.SummarizeErrorBody(httpResp.Header.Get("Content-Type"), b))

			if pool != nil && cookieEntry != nil && ctx.Err() == nil {
				// Retry on account-specific errors: 401 (Expired), 403 (Banned), 429 (Rate Limit)
				if httpResp.StatusCode == 401 || httpResp.StatusCode == 403 || httpResp.StatusCode == 429 {
					duration := 24 * time.Hour
					if httpResp.StatusCode == 429 {
						duration = 10 * time.Minute
					}
					pool.MarkDead(cookieEntry.ID(), duration)
					log.Warnf("openai compat executor: cookie failed with status %d, marking dead and retrying", httpResp.StatusCode)
					continue
				}
			}

			err = statusErr{code: httpResp.StatusCode, msg: string(b)}
			return resp, err
		}

		body, errReadAll := io.ReadAll(httpResp.Body)
		httpResp.Body.Close()
		if errReadAll != nil {
			helps.RecordAPIResponseError(ctx, e.cfg, errReadAll)
			return resp, errReadAll
		}
		helps.AppendAPIResponseChunk(ctx, e.cfg, body)
		reporter.Publish(ctx, helps.ParseOpenAIUsage(body))
		reporter.EnsurePublished(ctx)
		var param any
		out := sdktranslator.TranslateNonStream(ctx, to, from, req.Model, opts.OriginalRequest, translated, body, &param)
		resp = cliproxyexecutor.Response{Payload: out, Headers: httpResp.Header.Clone()}
		return resp, nil
	}
}

func (e *OpenAICompatExecutor) ExecuteStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (_ *cliproxyexecutor.StreamResult, err error) {
	baseModel := thinking.ParseSuffix(req.Model).ModelName

	reporter := helps.NewUsageReporter(ctx, e.Identifier(), baseModel, auth)
	defer reporter.TrackFailure(ctx, &err)

	baseURL, apiKey := e.resolveCredentials(auth)
	if baseURL == "" {
		err = statusErr{code: http.StatusUnauthorized, msg: "missing provider baseURL"}
		return nil, err
	}

	from := opts.SourceFormat
	to := sdktranslator.FromString("openai")
	originalPayloadSource := req.Payload
	if len(opts.OriginalRequest) > 0 {
		originalPayloadSource = opts.OriginalRequest
	}
	originalPayload := originalPayloadSource
	originalTranslated := sdktranslator.TranslateRequest(from, to, baseModel, originalPayload, true)
	translated := sdktranslator.TranslateRequest(from, to, baseModel, req.Payload, true)
	requestedModel := helps.PayloadRequestedModel(opts, req.Model)
	translated = helps.ApplyPayloadConfigWithRoot(e.cfg, baseModel, to.String(), "", translated, originalTranslated, requestedModel)

	translated, err = thinking.ApplyThinking(translated, req.Model, from.String(), to.String(), e.Identifier())
	if err != nil {
		return nil, err
	}

	// Request usage data in the final streaming chunk so that token statistics
	// are captured even when the upstream is an OpenAI-compatible provider.
	translated, _ = sjson.SetBytes(translated, "stream_options.include_usage", true)

	url := strings.TrimSuffix(baseURL, "/") + "/chat/completions"
	httpClient := helps.NewProxyAwareHTTPClient(ctx, e.cfg, auth, 0)

	var pool *cookiepool.Pool
	if auth != nil && auth.Attributes != nil {
		if poolName := strings.TrimSpace(auth.Attributes["cookie_pool_name"]); poolName != "" {
			pool = cookiepool.Get(poolName)
		}
	}

	for {
		if ctx.Err() != nil {
			err = ctx.Err()
			return nil, err
		}

		httpReq, errReq := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(translated))
		if errReq != nil {
			return nil, errReq
		}
		httpReq.Header.Set("Content-Type", "application/json")
		if apiKey != "" {
			httpReq.Header.Set("Authorization", "Bearer "+apiKey)
		}
		httpReq.Header.Set("User-Agent", "cli-proxy-openai-compat")
		var attrs map[string]string
		if auth != nil {
			attrs = auth.Attributes
		}
		util.ApplyCustomHeadersFromAttrs(httpReq, attrs)
		httpReq.Header.Set("Accept", "text/event-stream")
		httpReq.Header.Set("Cache-Control", "no-cache")
		cookieEntry := applyCookiePoolHeaders(httpReq, auth)

		var authID, authLabel, authType, authValue string
		if auth != nil {
			authID = auth.ID
			authLabel = auth.Label
			authType, authValue = auth.AccountInfo()
		}
		helps.RecordAPIRequest(ctx, e.cfg, helps.UpstreamRequestLog{
			URL:       url,
			Method:    http.MethodPost,
			Headers:   httpReq.Header.Clone(),
			Body:      translated,
			Provider:  e.Identifier(),
			AuthID:    authID,
			AuthLabel: authLabel,
			AuthType:  authType,
			AuthValue: authValue,
		})

		httpResp, errExec := httpClient.Do(httpReq)
		if errExec != nil {
			helps.RecordAPIResponseError(ctx, e.cfg, errExec)
			if pool != nil && cookieEntry != nil && ctx.Err() == nil {
				log.Warnf("openai compat executor (stream): request error: %v, retrying with next cookie", errExec)
				continue
			}
			return nil, errExec
		}

		helps.RecordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())
		if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
			b, _ := io.ReadAll(httpResp.Body)
			httpResp.Body.Close()
			helps.AppendAPIResponseChunk(ctx, e.cfg, b)
			helps.LogWithRequestID(ctx).Debugf("request error, error status: %d, error message: %s", httpResp.StatusCode, helps.SummarizeErrorBody(httpResp.Header.Get("Content-Type"), b))

			if pool != nil && cookieEntry != nil && ctx.Err() == nil {
				if httpResp.StatusCode == 401 || httpResp.StatusCode == 403 || httpResp.StatusCode == 429 {
					duration := 24 * time.Hour
					if httpResp.StatusCode == 429 {
						duration = 10 * time.Minute
					}
					pool.MarkDead(cookieEntry.ID(), duration)
					log.Warnf("openai compat executor (stream): cookie failed with status %d, marking dead and retrying", httpResp.StatusCode)
					continue
				}
			}

			err = statusErr{code: httpResp.StatusCode, msg: string(b)}
			return nil, err
		}

		out := make(chan cliproxyexecutor.StreamChunk)
		go func() {
			defer close(out)
			defer func() {
				if errClose := httpResp.Body.Close(); errClose != nil {
					log.Errorf("openai compat executor: close response body error: %v", errClose)
				}
			}()
			scanner := bufio.NewScanner(httpResp.Body)
			scanner.Buffer(nil, 52_428_800) // 50MB
			var param any
			for scanner.Scan() {
				line := scanner.Bytes()
				helps.AppendAPIResponseChunk(ctx, e.cfg, line)
				if detail, ok := helps.ParseOpenAIStreamUsage(line); ok {
					reporter.Publish(ctx, detail)
				}
				// Detect rate_limit_error hidden inside SSE JSON lines (GPT Proxy §4).
				// The upstream may return HTTP 200 but embed a rate limit error in the stream.
				if rateLimitErr := detectSSERateLimitError(line); rateLimitErr != nil {
					reporter.PublishFailure(ctx)
					out <- cliproxyexecutor.StreamChunk{Err: rateLimitErr}
					return
				}
				if len(line) == 0 {
					continue
				}

				if !bytes.HasPrefix(line, []byte("data:")) {
					continue
				}

				// OpenAI-compatible streams are SSE: lines typically prefixed with "data: ".
				// Pass through translator; it yields one or more chunks for the target schema.
				chunks := sdktranslator.TranslateStream(ctx, to, from, req.Model, opts.OriginalRequest, translated, bytes.Clone(line), &param)
				for i := range chunks {
					out <- cliproxyexecutor.StreamChunk{Payload: chunks[i]}
				}
			}
			if errScan := scanner.Err(); errScan != nil {
				helps.RecordAPIResponseError(ctx, e.cfg, errScan)
				reporter.PublishFailure(ctx)
				out <- cliproxyexecutor.StreamChunk{Err: errScan}
			} else {
				// In case the upstream closes the stream without a terminal [DONE] marker.
				// Feed a synthetic done marker through the translator so pending
				// response.completed events are still emitted exactly once.
				chunks := sdktranslator.TranslateStream(ctx, to, from, req.Model, opts.OriginalRequest, translated, []byte("data: [DONE]"), &param)
				for i := range chunks {
					out <- cliproxyexecutor.StreamChunk{Payload: chunks[i]}
				}
			}
			// Ensure we record the request if no usage chunk was ever seen
			reporter.EnsurePublished(ctx)
		}()
		return &cliproxyexecutor.StreamResult{Headers: httpResp.Header.Clone(), Chunks: out}, nil
	}
}

func (e *OpenAICompatExecutor) CountTokens(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	baseModel := thinking.ParseSuffix(req.Model).ModelName

	from := opts.SourceFormat
	to := sdktranslator.FromString("openai")
	translated := sdktranslator.TranslateRequest(from, to, baseModel, req.Payload, false)

	modelForCounting := baseModel

	translated, err := thinking.ApplyThinking(translated, req.Model, from.String(), to.String(), e.Identifier())
	if err != nil {
		return cliproxyexecutor.Response{}, err
	}

	enc, err := helps.TokenizerForModel(modelForCounting)
	if err != nil {
		return cliproxyexecutor.Response{}, fmt.Errorf("openai compat executor: tokenizer init failed: %w", err)
	}

	count, err := helps.CountOpenAIChatTokens(enc, translated)
	if err != nil {
		return cliproxyexecutor.Response{}, fmt.Errorf("openai compat executor: token counting failed: %w", err)
	}

	usageJSON := helps.BuildOpenAIUsageJSON(count)
	translatedUsage := sdktranslator.TranslateTokenCount(ctx, to, from, count, usageJSON)
	return cliproxyexecutor.Response{Payload: translatedUsage}, nil
}

// Refresh is a no-op for API-key based compatibility providers.
func (e *OpenAICompatExecutor) Refresh(ctx context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	log.Debugf("openai compat executor: refresh called")
	_ = ctx
	return auth, nil
}

func (e *OpenAICompatExecutor) resolveCredentials(auth *cliproxyauth.Auth) (baseURL, apiKey string) {
	if auth == nil {
		return "", ""
	}
	if auth.Attributes != nil {
		baseURL = strings.TrimSpace(auth.Attributes["base_url"])
		apiKey = strings.TrimSpace(auth.Attributes["api_key"])
	}
	return
}

func (e *OpenAICompatExecutor) resolveCompatConfig(auth *cliproxyauth.Auth) *config.OpenAICompatibility {
	if auth == nil || e.cfg == nil {
		return nil
	}
	candidates := make([]string, 0, 3)
	if auth.Attributes != nil {
		if v := strings.TrimSpace(auth.Attributes["compat_name"]); v != "" {
			candidates = append(candidates, v)
		}
		if v := strings.TrimSpace(auth.Attributes["provider_key"]); v != "" {
			candidates = append(candidates, v)
		}
	}
	if v := strings.TrimSpace(auth.Provider); v != "" {
		candidates = append(candidates, v)
	}
	for i := range e.cfg.OpenAICompatibility {
		compat := &e.cfg.OpenAICompatibility[i]
		for _, candidate := range candidates {
			if candidate != "" && strings.EqualFold(strings.TrimSpace(candidate), compat.Name) {
				return compat
			}
		}
	}
	return nil
}

func (e *OpenAICompatExecutor) overrideModel(payload []byte, model string) []byte {
	if len(payload) == 0 || model == "" {
		return payload
	}
	payload, _ = sjson.SetBytes(payload, "model", model)
	return payload
}

type statusErr struct {
	code       int
	msg        string
	retryAfter *time.Duration
}

func (e statusErr) Error() string {
	if e.msg != "" {
		return e.msg
	}
	return fmt.Sprintf("status %d", e.code)
}
func (e statusErr) StatusCode() int            { return e.code }
func (e statusErr) RetryAfter() *time.Duration { return e.retryAfter }

func (e *OpenAICompatExecutor) requiresAnthropicImageContent(auth *cliproxyauth.Auth) bool {
	if auth == nil {
		return false
	}
	baseURL, _ := e.resolveCredentials(auth)
	if strings.Contains(strings.ToLower(strings.TrimSpace(baseURL)), "desktop-llm.skywork.ai") {
		return true
	}
	return strings.EqualFold(strings.TrimSpace(auth.Provider), "skywork")
}

// detectSSERateLimitError checks an SSE line for an embedded rate_limit_error.
// GPT Proxy detects this via exact 16-byte match on error.type (IDA §4).
// Returns a statusErr with 429 if detected, nil otherwise.
func detectSSERateLimitError(line []byte) error {
	trimmed := bytes.TrimSpace(line)
	if len(trimmed) == 0 {
		return nil
	}
	if bytes.HasPrefix(trimmed, []byte("data:")) {
		trimmed = bytes.TrimSpace(trimmed[5:])
	}
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return nil
	}
	errType := gjson.GetBytes(trimmed, "error.type").String()
	if errType == "rate_limit_error" {
		msg := gjson.GetBytes(trimmed, "error.message").String()
		if msg == "" {
			msg = "rate limited (detected in SSE stream)"
		}
		return statusErr{code: 429, msg: msg}
	}
	return nil
}

// applyCookiePoolHeaders checks if the auth is backed by a cookie pool and, if so,
// picks a cookie to inject into the request headers. The pool entry is a map of
// header names to values, applied directly to the request. Returns the picked
// entry or nil.
func applyCookiePoolHeaders(req *http.Request, auth *cliproxyauth.Auth) *cookiepool.Entry {
	if req == nil || auth == nil || auth.Attributes == nil {
		return nil
	}
	poolName := strings.TrimSpace(auth.Attributes["cookie_pool_name"])
	if poolName == "" {
		return nil
	}
	pool := cookiepool.Get(poolName)
	if pool == nil {
		return nil
	}
	entry := pool.Pick()
	if entry == nil {
		return nil
	}
	for key, value := range *entry {
		if strings.TrimSpace(key) != "" && strings.TrimSpace(value) != "" {
			req.Header.Set(key, value)
		}
	}
	return entry
}
