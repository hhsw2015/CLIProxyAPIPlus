package executor

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/textproto"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/thinking"
	chatcompletions "github.com/router-for-me/CLIProxyAPI/v6/internal/translator/claude/openai/chat-completions"
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
	httpClient := newProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	return httpClient.Do(httpReq)
}

func (e *OpenAICompatExecutor) Execute(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (resp cliproxyexecutor.Response, err error) {
	baseModel := thinking.ParseSuffix(req.Model).ModelName

	reporter := newUsageReporter(ctx, e.Identifier(), baseModel, auth)
	defer reporter.trackFailure(ctx, &err)

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
	requestedModel := payloadRequestedModel(opts, req.Model)
	translated = applyPayloadConfigWithRoot(e.cfg, baseModel, to.String(), "", translated, originalTranslated, requestedModel)
	var extraBetas []string
	if isClaudeFamilyModel(baseModel) {
		translated, extraBetas = prepareOpenAICompatAnthropicPassthrough(originalPayload, translated)
		translated = e.adaptCompatPayload(auth, translated)
	}
	translated = adaptCrossFamilyReasoningEffort(translated, baseModel, requestedModel)
	if opts.Alt == "responses/compact" {
		if updated, errDelete := sjson.DeleteBytes(translated, "stream"); errDelete == nil {
			translated = updated
		}
	}

	translated, err = thinking.ApplyThinking(translated, req.Model, from.String(), to.String(), e.Identifier())
	if err != nil {
		return resp, err
	}
	if e.usesSingularityTransport(auth) {
		if opts.Alt != "" {
			return resp, statusErr{code: http.StatusBadRequest, msg: "singularity provider does not support " + opts.Alt}
		}
		translated = adaptSingularityPayload(translated, baseModel)
		return e.executeSingularityNonStream(ctx, auth, req, opts, translated, to, baseURL, apiKey, extraBetas, reporter)
	}

	url := openAICompatRequestURL(baseURL, endpoint)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(translated))
	if err != nil {
		return resp, err
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
	if isClaudeFamilyModel(baseModel) {
		applyOpenAICompatAnthropicPassthroughHeaders(httpReq, ctx, extraBetas)
		ensureSkyworkClaude1MBeta(httpReq, auth, baseModel)
	}
	applySingularityHeaders(httpReq, auth, apiKey, false)
	logInfo := upstreamRequestLog{
		URL:      url,
		Method:   http.MethodPost,
		Headers:  httpReq.Header.Clone(),
		Body:     translated,
		Provider: e.Identifier(),
	}
	fillAuthLogInfo(&logInfo, auth)
	recordAPIRequest(ctx, e.cfg, logInfo)

	httpClient := newProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	httpResp, err := httpClient.Do(httpReq)
	if err != nil {
		err = normalizeOpenAICompatTransportError(err)
		recordAPIResponseError(ctx, e.cfg, err)
		return resp, err
	}
	defer func() {
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("openai compat executor: close response body error: %v", errClose)
		}
	}()
	recordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		b, _ := io.ReadAll(httpResp.Body)
		appendAPIResponseChunk(ctx, e.cfg, b)
		logWithRequestID(ctx).Debugf("request error, error status: %d, error message: %s", httpResp.StatusCode, summarizeErrorBody(httpResp.Header.Get("Content-Type"), b))
		err = statusErr{code: httpResp.StatusCode, msg: string(b)}
		return resp, err
	}
	body, err := io.ReadAll(httpResp.Body)
	if err != nil {
		err = normalizeOpenAICompatTransportError(err)
		recordAPIResponseError(ctx, e.cfg, err)
		return resp, err
	}
	appendAPIResponseChunk(ctx, e.cfg, body)
	if wrappedErr := parseOpenAICompatWrappedErrorPayload(body); wrappedErr != nil {
		recordAPIResponseError(ctx, e.cfg, wrappedErr)
		return resp, wrappedErr
	}
	reporter.publish(ctx, parseOpenAIUsage(body))
	// Ensure we at least record the request even if upstream doesn't return usage
	reporter.ensurePublished(ctx)
	// Translate response back to source format when needed
	var param any
	out := sdktranslator.TranslateNonStream(ctx, to, from, req.Model, opts.OriginalRequest, translated, body, &param)
	resp = cliproxyexecutor.Response{Payload: out, Headers: httpResp.Header.Clone()}
	return resp, nil
}

func (e *OpenAICompatExecutor) ExecuteStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (_ *cliproxyexecutor.StreamResult, err error) {
	baseModel := thinking.ParseSuffix(req.Model).ModelName

	reporter := newUsageReporter(ctx, e.Identifier(), baseModel, auth)
	defer reporter.trackFailure(ctx, &err)

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
	requestedModel := payloadRequestedModel(opts, req.Model)
	translated = applyPayloadConfigWithRoot(e.cfg, baseModel, to.String(), "", translated, originalTranslated, requestedModel)
	var extraBetas []string
	if isClaudeFamilyModel(baseModel) {
		translated, extraBetas = prepareOpenAICompatAnthropicPassthrough(originalPayload, translated)
		translated = e.adaptCompatPayload(auth, translated)
	}
	translated = adaptCrossFamilyReasoningEffort(translated, baseModel, requestedModel)
	if streamResult, webErr := e.handleMaybeInterceptedWebSearchStream(ctx, auth, req, opts, originalPayload); streamResult != nil || webErr != nil {
		return streamResult, webErr
	}

	translated, err = thinking.ApplyThinking(translated, req.Model, from.String(), to.String(), e.Identifier())
	if err != nil {
		return nil, err
	}
	if e.usesSingularityTransport(auth) {
		translated = adaptSingularityPayload(translated, baseModel)
	}

	// Request usage data in the final streaming chunk so that token statistics
	// are captured even when the upstream is an OpenAI-compatible provider.
	translated, _ = sjson.SetBytes(translated, "stream_options.include_usage", true)

	streamEndpoint := "/chat/completions"
	if opts.Alt == "responses/compact" {
		streamEndpoint = "/responses/compact"
	}
	url := openAICompatRequestURL(baseURL, streamEndpoint)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(translated))
	if err != nil {
		return nil, err
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
	if isClaudeFamilyModel(baseModel) {
		applyOpenAICompatAnthropicPassthroughHeaders(httpReq, ctx, extraBetas)
		ensureSkyworkClaude1MBeta(httpReq, auth, baseModel)
	}
	applySingularityHeaders(httpReq, auth, apiKey, true)
	httpReq.Header.Set("Accept", "text/event-stream")
	httpReq.Header.Set("Cache-Control", "no-cache")
	logInfo := upstreamRequestLog{
		URL:      url,
		Method:   http.MethodPost,
		Headers:  httpReq.Header.Clone(),
		Body:     translated,
		Provider: e.Identifier(),
	}
	fillAuthLogInfo(&logInfo, auth)
	recordAPIRequest(ctx, e.cfg, logInfo)

	httpClient := newProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	httpResp, err := httpClient.Do(httpReq)
	if err != nil {
		err = normalizeOpenAICompatTransportError(err)
		recordAPIResponseError(ctx, e.cfg, err)
		return nil, err
	}
	recordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		b, _ := io.ReadAll(httpResp.Body)
		appendAPIResponseChunk(ctx, e.cfg, b)
		logWithRequestID(ctx).Debugf("request error, error status: %d, error message: %s", httpResp.StatusCode, summarizeErrorBody(httpResp.Header.Get("Content-Type"), b))
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("openai compat executor: close response body error: %v", errClose)
		}
		err = statusErr{code: httpResp.StatusCode, msg: string(b)}
		return nil, err
	}
	if strings.Contains(strings.ToLower(strings.TrimSpace(httpResp.Header.Get("Content-Type"))), "application/json") {
		body, readErr := io.ReadAll(httpResp.Body)
		if readErr != nil {
			readErr = normalizeOpenAICompatTransportError(readErr)
			recordAPIResponseError(ctx, e.cfg, readErr)
			return nil, readErr
		}
		appendAPIResponseChunk(ctx, e.cfg, body)
		if wrappedErr := parseOpenAICompatWrappedErrorPayload(body); wrappedErr != nil {
			recordAPIResponseError(ctx, e.cfg, wrappedErr)
			return nil, wrappedErr
		}
		err = statusErr{code: http.StatusBadGateway, msg: "unexpected JSON response for stream request"}
		recordAPIResponseError(ctx, e.cfg, err)
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
		var nonData bytes.Buffer
		sawData := false
		for scanner.Scan() {
			line := scanner.Bytes()
			appendAPIResponseChunk(ctx, e.cfg, line)
			if streamErr := parseOpenAICompatStreamError(line); streamErr != nil {
				recordAPIResponseError(ctx, e.cfg, streamErr)
				reporter.publishFailure(ctx)
				out <- cliproxyexecutor.StreamChunk{Err: streamErr}
				return
			}
			if detail, ok := parseOpenAIStreamUsage(line); ok {
				reporter.publish(ctx, detail)
			}
			if len(line) == 0 {
				continue
			}

			if !bytes.HasPrefix(line, []byte("data:")) {
				nonData.Write(bytes.TrimSpace(line))
				continue
			}
			sawData = true

			// OpenAI-compatible streams are SSE: lines typically prefixed with "data: ".
			// Pass through translator; it yields one or more chunks for the target schema.
			chunks := sdktranslator.TranslateStream(ctx, to, from, req.Model, opts.OriginalRequest, translated, bytes.Clone(line), &param)
			for i := range chunks {
				out <- cliproxyexecutor.StreamChunk{Payload: chunks[i]}
			}
		}
		if errScan := scanner.Err(); errScan != nil {
			errScan = normalizeOpenAICompatTransportError(errScan)
			recordAPIResponseError(ctx, e.cfg, errScan)
			reporter.publishFailure(ctx)
			out <- cliproxyexecutor.StreamChunk{Err: errScan}
			return
		}
		if !sawData && nonData.Len() > 0 {
			if wrappedErr := parseOpenAICompatWrappedErrorPayload(nonData.Bytes()); wrappedErr != nil {
				recordAPIResponseError(ctx, e.cfg, wrappedErr)
				reporter.publishFailure(ctx)
				out <- cliproxyexecutor.StreamChunk{Err: wrappedErr}
				return
			}
			unexpectedErr := statusErr{code: http.StatusBadGateway, msg: "unexpected non-SSE stream response"}
			recordAPIResponseError(ctx, e.cfg, unexpectedErr)
			reporter.publishFailure(ctx)
			out <- cliproxyexecutor.StreamChunk{Err: unexpectedErr}
			return
		}
		// Ensure we record the request if no usage chunk was ever seen
		reporter.ensurePublished(ctx)
	}()
	return &cliproxyexecutor.StreamResult{Headers: httpResp.Header.Clone(), Chunks: out}, nil
}

func (e *OpenAICompatExecutor) handleMaybeInterceptedWebSearchStream(
	ctx context.Context,
	auth *cliproxyauth.Auth,
	req cliproxyexecutor.Request,
	opts cliproxyexecutor.Options,
	originalPayload []byte,
) (*cliproxyexecutor.StreamResult, error) {
	if !e.shouldInterceptWebSearch(auth, opts, originalPayload) {
		return nil, nil
	}
	return e.handleInterceptedWebSearchStream(ctx, auth, req, opts, originalPayload)
}

var statusCodePattern = regexp.MustCompile(`StatusCode:\s*([0-9]{3})`)

func parseOpenAICompatStreamError(line []byte) error {
	trimmed := bytes.TrimSpace(line)
	if len(trimmed) == 0 {
		return nil
	}
	if bytes.HasPrefix(trimmed, []byte("data:")) {
		trimmed = bytes.TrimSpace(trimmed[len("data:"):])
	}
	for {
		switch {
		case bytes.HasSuffix(trimmed, []byte(`\n`)):
			trimmed = bytes.TrimSpace(trimmed[:len(trimmed)-2])
		case bytes.HasSuffix(trimmed, []byte(`\r`)):
			trimmed = bytes.TrimSpace(trimmed[:len(trimmed)-2])
		default:
			goto normalized
		}
	}
normalized:
	if len(trimmed) == 0 || trimmed[0] != '{' || !json.Valid(trimmed) {
		return nil
	}
	root := gjson.ParseBytes(trimmed)
	if root.Get("type").String() != "error" {
		return nil
	}
	message := strings.TrimSpace(root.Get("error.message").String())
	if message == "" {
		message = strings.TrimSpace(root.Get("message").String())
	}
	if message == "" {
		message = string(trimmed)
	}
	code := http.StatusBadGateway
	switch strings.TrimSpace(root.Get("error.type").String()) {
	case "invalid_request_error":
		code = http.StatusBadRequest
	}
	if matches := statusCodePattern.FindStringSubmatch(message); len(matches) == 2 {
		if parsed, err := strconv.Atoi(matches[1]); err == nil && parsed >= 100 && parsed <= 599 {
			code = parsed
		}
	}
	return statusErr{code: code, msg: message}
}

func parseOpenAICompatWrappedErrorPayload(payload []byte) error {
	trimmed := bytes.TrimSpace(payload)
	if len(trimmed) == 0 || !json.Valid(trimmed) {
		return nil
	}
	if streamErr := parseOpenAICompatStreamError(trimmed); streamErr != nil {
		return streamErr
	}
	root := gjson.ParseBytes(trimmed)
	if errNode := root.Get("error"); errNode.Exists() {
		code := http.StatusBadRequest
		if rawCode := int(errNode.Get("code").Int()); rawCode >= 100 && rawCode <= 599 {
			code = rawCode
		}
		message := strings.TrimSpace(errNode.Get("message").String())
		if raw := strings.TrimSpace(errNode.Get("metadata.raw").String()); raw != "" {
			if nestedCode, nestedMessage := parseWrappedProviderRaw(raw); nestedMessage != "" {
				message = nestedMessage
				if nestedCode != 0 {
					code = nestedCode
				}
			}
		}
		if message != "" {
			return statusErr{code: code, msg: message}
		}
	}
	rawCode := int(root.Get("code").Int())
	if rawCode == 0 {
		return nil
	}
	message := strings.TrimSpace(root.Get("data.error_message").String())
	if message == "" {
		message = strings.TrimSpace(root.Get("code_msg").String())
	}
	if message == "" {
		message = strings.TrimSpace(root.Get("message").String())
	}
	if message == "" {
		message = string(trimmed)
	}
	return statusErr{code: normalizeWrappedErrorHTTPStatus(rawCode, message), msg: message}
}

func parseWrappedProviderRaw(raw string) (int, string) {
	raw = strings.TrimSpace(raw)
	if raw == "" || !json.Valid([]byte(raw)) {
		return 0, ""
	}
	root := gjson.Parse(raw)
	code := int(root.Get("error.code").Int())
	if code < 100 || code > 599 {
		code = 0
	}
	message := strings.TrimSpace(root.Get("error.message").String())
	if message == "" {
		message = strings.TrimSpace(root.Get("message").String())
	}
	return code, message
}

func normalizeWrappedErrorHTTPStatus(rawCode int, message string) int {
	if rawCode >= 100 && rawCode <= 599 {
		return rawCode
	}
	lower := strings.ToLower(strings.TrimSpace(message))
	switch {
	case strings.Contains(lower, "quota"), strings.Contains(lower, "limit"):
		return http.StatusTooManyRequests
	case strings.Contains(lower, "auth"), strings.Contains(lower, "author"), strings.Contains(lower, "token"), strings.Contains(message, "验证"):
		return http.StatusUnauthorized
	default:
		return http.StatusBadRequest
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

	enc, err := tokenizerForModel(modelForCounting)
	if err != nil {
		return cliproxyexecutor.Response{}, fmt.Errorf("openai compat executor: tokenizer init failed: %w", err)
	}

	count, err := countOpenAIChatTokens(enc, translated)
	if err != nil {
		return cliproxyexecutor.Response{}, fmt.Errorf("openai compat executor: token counting failed: %w", err)
	}

	usageJSON := buildOpenAIUsageJSON(count)
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

func (e *OpenAICompatExecutor) usesSingularityTransport(auth *cliproxyauth.Auth) bool {
	if auth == nil {
		return false
	}
	candidates := []string{
		strings.TrimSpace(auth.Provider),
	}
	if auth.Attributes != nil {
		candidates = append(candidates,
			strings.TrimSpace(auth.Attributes["provider_key"]),
			strings.TrimSpace(auth.Attributes["compat_name"]),
		)
	}
	for _, candidate := range candidates {
		if strings.EqualFold(candidate, "singularity") {
			return true
		}
	}
	return false
}

func prepareOpenAICompatAnthropicPassthrough(originalPayload, translated []byte) ([]byte, []string) {
	originalBetas, _ := extractAndRemoveBetas(originalPayload)
	translatedBetas, translated := extractAndRemoveBetas(translated)
	translated = restoreOpenAICompatPassThroughBodyField(originalPayload, translated, "speed")
	return translated, splitHeaderValues(mergeUniqueCSV("", originalBetas, translatedBetas))
}

func restoreOpenAICompatPassThroughBodyField(originalPayload, translated []byte, field string) []byte {
	if len(originalPayload) == 0 || len(translated) == 0 || strings.TrimSpace(field) == "" {
		return translated
	}
	value := gjson.GetBytes(originalPayload, field)
	if !value.Exists() || strings.TrimSpace(value.Raw) == "" {
		return translated
	}
	if updated, err := sjson.SetRawBytes(translated, field, []byte(value.Raw)); err == nil {
		return updated
	}
	return translated
}

func applyOpenAICompatAnthropicPassthroughHeaders(r *http.Request, ctx context.Context, extraBetas []string) {
	if r == nil {
		return
	}
	headers := inboundRequestHeaders(ctx)
	merged := ""
	if headers != nil {
		merged = mergeUniqueCSV(merged, splitHeaderValues(headers.Get("Anthropic-Beta")))
		merged = mergeUniqueCSV(merged, extraBetas)
		if _, ok := headers[textproto.CanonicalMIMEHeaderKey("X-CPA-CLAUDE-1M")]; ok {
			merged = mergeUniqueCSV(merged, []string{"context-1m-2025-08-07"})
		}
	} else {
		merged = mergeUniqueCSV(merged, extraBetas)
	}
	merged = mergeUniqueCSV(merged, splitHeaderValues(r.Header.Get("Anthropic-Beta")))
	if strings.TrimSpace(merged) != "" {
		r.Header.Set("Anthropic-Beta", merged)
	}
}

func inboundRequestHeaders(ctx context.Context) http.Header {
	if ctx == nil {
		return nil
	}
	ginCtx, ok := ctx.Value("gin").(*gin.Context)
	if !ok || ginCtx == nil || ginCtx.Request == nil {
		return nil
	}
	return ginCtx.Request.Header
}

func mergeUniqueCSV(base string, groups ...[]string) string {
	seen := make(map[string]struct{})
	merged := make([]string, 0)
	appendValues := func(values []string) {
		for _, value := range values {
			value = strings.TrimSpace(value)
			if value == "" {
				continue
			}
			if _, ok := seen[value]; ok {
				continue
			}
			seen[value] = struct{}{}
			merged = append(merged, value)
		}
	}

	appendValues(splitHeaderValues(base))
	for _, group := range groups {
		appendValues(group)
	}
	return strings.Join(merged, ",")
}

func splitHeaderValues(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if value := strings.TrimSpace(part); value != "" {
			out = append(out, value)
		}
	}
	return out
}

func openAICompatRequestURL(baseURL, endpoint string) string {
	baseURL = strings.TrimSpace(baseURL)
	endpoint = strings.TrimSpace(endpoint)
	if baseURL == "" {
		return endpoint
	}
	if endpoint == "" {
		return strings.TrimSuffix(baseURL, "/")
	}
	trimmedEndpoint := strings.TrimPrefix(endpoint, "/")

	// Parse URL to separate path from query string so that suffix
	// matching works correctly for URLs with query parameters (e.g.,
	// Azure: ...?api-version=2024-08-01-preview).
	parsed, err := url.Parse(baseURL)
	if err != nil {
		trimmedBase := strings.TrimSuffix(baseURL, "/")
		return trimmedBase + "/" + trimmedEndpoint
	}
	trimmedPath := strings.TrimSuffix(parsed.Path, "/")
	if strings.EqualFold(trimmedPath, endpoint) || strings.HasSuffix(strings.ToLower(trimmedPath), "/"+strings.ToLower(trimmedEndpoint)) {
		return strings.TrimSuffix(baseURL, "/")
	}
	// Azure Responses API uses /responses instead of /responses/compact.
	// Match if the base path already ends with /responses.
	if trimmedEndpoint == "responses/compact" && strings.HasSuffix(strings.ToLower(trimmedPath), "/responses") {
		return strings.TrimSuffix(baseURL, "/")
	}
	parsed.Path = trimmedPath + "/" + trimmedEndpoint
	return parsed.String()
}

func applySingularityHeaders(r *http.Request, auth *cliproxyauth.Auth, apiKey string, stream bool) {
	if r == nil || !isSingularityAuth(auth) {
		return
	}
	r.Header.Del("Authorization")
	if strings.TrimSpace(r.Header.Get("X-Skywork-Cookies")) == "" && strings.TrimSpace(apiKey) != "" {
		r.Header.Set("X-Skywork-Cookies", "token="+strings.TrimSpace(apiKey))
	}
	r.Header.Del("X-Skywork-Billing-Source")
	if stream {
		r.Header.Set("Accept", "text/event-stream")
		r.Header.Set("Cache-Control", "no-cache")
	}
}

func isSingularityAuth(auth *cliproxyauth.Auth) bool {
	if auth == nil {
		return false
	}
	if strings.EqualFold(strings.TrimSpace(auth.Provider), "singularity") {
		return true
	}
	if auth.Attributes == nil {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(auth.Attributes["provider_key"]), "singularity") ||
		strings.EqualFold(strings.TrimSpace(auth.Attributes["compat_name"]), "singularity")
}

func adaptSingularityPayload(payload []byte, modelName string) []byte {
	if len(payload) == 0 {
		return payload
	}
	payload, _ = sjson.SetBytes(payload, "stream", true)
	// Only convert reasoning_effort to thinking.budget_tokens for Claude-family models.
	// GPT models use reasoning_effort natively and should keep it as-is.
	if !isClaudeFamilyModel(modelName) {
		return payload
	}
	effort := strings.ToLower(strings.TrimSpace(gjson.GetBytes(payload, "reasoning_effort").String()))
	budgetMap := map[string]int64{
		"minimal": 256,
		"low":     1024,
		"medium":  4096,
		"high":    16384,
		"xhigh":   32768,
		"max":     32768,
	}
	if budget, ok := budgetMap[effort]; ok {
		payload, _ = sjson.SetBytes(payload, "thinking.type", "enabled")
		payload, _ = sjson.SetBytes(payload, "thinking.budget_tokens", budget)
		payload, _ = sjson.DeleteBytes(payload, "reasoning_effort")
	} else if effort == "off" || effort == "none" || effort == "disabled" {
		payload, _ = sjson.DeleteBytes(payload, "reasoning_effort")
		payload, _ = sjson.DeleteBytes(payload, "thinking")
	}
	return payload
}

func (e *OpenAICompatExecutor) executeSingularityNonStream(
	ctx context.Context,
	auth *cliproxyauth.Auth,
	req cliproxyexecutor.Request,
	opts cliproxyexecutor.Options,
	translated []byte,
	to sdktranslator.Format,
	baseURL, apiKey string,
	extraBetas []string,
	reporter *usageReporter,
) (cliproxyexecutor.Response, error) {
	url := openAICompatRequestURL(baseURL, "/chat/completions")
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(translated))
	if err != nil {
		return cliproxyexecutor.Response{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("User-Agent", "cli-proxy-openai-compat")
	var attrs map[string]string
	if auth != nil {
		attrs = auth.Attributes
	}
	util.ApplyCustomHeadersFromAttrs(httpReq, attrs)
	if isClaudeFamilyModel(req.Model) {
		applyOpenAICompatAnthropicPassthroughHeaders(httpReq, ctx, extraBetas)
		ensureSkyworkClaude1MBeta(httpReq, auth, req.Model)
	}
	applySingularityHeaders(httpReq, auth, apiKey, true)

	logInfo := upstreamRequestLog{
		URL:      url,
		Method:   http.MethodPost,
		Headers:  httpReq.Header.Clone(),
		Body:     translated,
		Provider: e.Identifier(),
	}
	fillAuthLogInfo(&logInfo, auth)
	recordAPIRequest(ctx, e.cfg, logInfo)

	httpClient := newProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	httpResp, err := httpClient.Do(httpReq)
	if err != nil {
		recordAPIResponseError(ctx, e.cfg, err)
		return cliproxyexecutor.Response{}, err
	}
	defer func() {
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("openai compat executor: close response body error: %v", errClose)
		}
	}()
	recordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		b, _ := io.ReadAll(httpResp.Body)
		appendAPIResponseChunk(ctx, e.cfg, b)
		return cliproxyexecutor.Response{}, statusErr{code: httpResp.StatusCode, msg: string(b)}
	}

	body, err := io.ReadAll(httpResp.Body)
	if err != nil {
		recordAPIResponseError(ctx, e.cfg, err)
		return cliproxyexecutor.Response{}, err
	}
	appendAPIResponseChunk(ctx, e.cfg, body)
	if wrappedErr := parseOpenAICompatWrappedErrorPayload(body); wrappedErr != nil {
		recordAPIResponseError(ctx, e.cfg, wrappedErr)
		return cliproxyexecutor.Response{}, wrappedErr
	}
	aggregated, aggErr := aggregateOpenAICompatSSE(body)
	if aggErr != nil {
		recordAPIResponseError(ctx, e.cfg, aggErr)
		return cliproxyexecutor.Response{}, aggErr
	}
	reporter.publish(ctx, parseOpenAIUsage(aggregated))
	reporter.ensurePublished(ctx)

	var param any
	out := sdktranslator.TranslateNonStream(ctx, to, opts.SourceFormat, req.Model, opts.OriginalRequest, translated, aggregated, &param)
	return cliproxyexecutor.Response{Payload: []byte(out), Headers: httpResp.Header.Clone()}, nil
}

func aggregateOpenAICompatSSE(body []byte) ([]byte, error) {
	type toolCallBuilder struct {
		ID           string
		Name         string
		Arguments    string
		ExtraRawJSON string
	}

	lines := bytes.Split(body, []byte("\n"))
	toolCalls := make(map[int]*toolCallBuilder)
	content := strings.Builder{}
	var id string
	var model string
	var finishReason string
	var usageRaw string
	var created int64

	for _, line := range lines {
		trimmed := bytes.TrimSpace(line)
		if len(trimmed) == 0 {
			continue
		}
		if streamErr := parseOpenAICompatStreamError(trimmed); streamErr != nil {
			return nil, streamErr
		}
		if !bytes.HasPrefix(trimmed, []byte("data:")) {
			continue
		}
		payload := bytes.TrimSpace(trimmed[len("data:"):])
		if bytes.Equal(payload, []byte("[DONE]")) || !json.Valid(payload) {
			continue
		}
		root := gjson.ParseBytes(payload)
		if v := strings.TrimSpace(root.Get("id").String()); v != "" {
			id = v
		}
		if v := strings.TrimSpace(root.Get("model").String()); v != "" {
			model = v
		}
		if v := root.Get("created"); v.Exists() {
			created = v.Int()
		}
		if usage := root.Get("usage"); usage.Exists() {
			usageRaw = usage.Raw
		}
		choice := root.Get("choices.0")
		if !choice.Exists() {
			if v := strings.TrimSpace(root.Get("finish_reason").String()); v != "" {
				finishReason = v
			}
			continue
		}
		if delta := choice.Get("delta.content"); delta.Exists() {
			content.WriteString(delta.String())
		}
		if v := strings.TrimSpace(choice.Get("finish_reason").String()); v != "" {
			finishReason = v
		}
		for _, call := range choice.Get("delta.tool_calls").Array() {
			index := int(call.Get("index").Int())
			builder := toolCalls[index]
			if builder == nil {
				builder = &toolCallBuilder{}
				toolCalls[index] = builder
			}
			if v := strings.TrimSpace(call.Get("id").String()); v != "" {
				builder.ID = v
			}
			if v := strings.TrimSpace(call.Get("function.name").String()); v != "" {
				builder.Name = v
			}
			if v := call.Get("function.arguments"); v.Exists() {
				builder.Arguments += v.String()
			}
			if v := call.Get("extra_content"); v.Exists() {
				builder.ExtraRawJSON = v.Raw
			}
		}
	}

	if id == "" {
		id = "resp_singularity"
	}
	if model == "" {
		model = "unknown"
	}
	if finishReason == "" {
		finishReason = "stop"
	}

	out := []byte(`{"id":"","object":"chat.completion","created":0,"model":"","choices":[{"index":0,"message":{"role":"assistant","content":""},"finish_reason":""}]}`)
	out, _ = sjson.SetBytes(out, "id", id)
	out, _ = sjson.SetBytes(out, "created", created)
	out, _ = sjson.SetBytes(out, "model", model)
	out, _ = sjson.SetBytes(out, "choices.0.message.content", content.String())
	out, _ = sjson.SetBytes(out, "choices.0.finish_reason", finishReason)

	if len(toolCalls) > 0 {
		ordered := make([]int, 0, len(toolCalls))
		for idx := range toolCalls {
			ordered = append(ordered, idx)
		}
		sort.Ints(ordered)
		toolCallsJSON := "[]"
		for _, idx := range ordered {
			builder := toolCalls[idx]
			node := `{"id":"","type":"function","function":{"name":"","arguments":""}}`
			node, _ = sjson.Set(node, "id", builder.ID)
			node, _ = sjson.Set(node, "function.name", builder.Name)
			node, _ = sjson.Set(node, "function.arguments", builder.Arguments)
			if strings.TrimSpace(builder.ExtraRawJSON) != "" {
				node, _ = sjson.SetRaw(node, "extra_content", builder.ExtraRawJSON)
			}
			toolCallsJSON, _ = sjson.SetRaw(toolCallsJSON, "-1", node)
		}
		out, _ = sjson.SetRawBytes(out, "choices.0.message.tool_calls", []byte(toolCallsJSON))
	}
	if strings.TrimSpace(usageRaw) != "" {
		out, _ = sjson.SetRawBytes(out, "usage", []byte(usageRaw))
	}
	return out, nil
}

func (e *OpenAICompatExecutor) adaptCompatPayload(auth *cliproxyauth.Auth, payload []byte) []byte {
	if len(payload) == 0 || !e.requiresAnthropicImageContent(auth) {
		return payload
	}
	return rewriteOpenAIImageURLPartsToAnthropicImages(payload)
}

func (e *OpenAICompatExecutor) requiresAnthropicImageContent(auth *cliproxyauth.Auth) bool {
	if auth == nil {
		return false
	}
	baseURL, _ := e.resolveCredentials(auth)
	if strings.Contains(strings.ToLower(strings.TrimSpace(baseURL)), "desktop-llm.skywork.ai") {
		return true
	}
	if compat := e.resolveCompatConfig(auth); compat != nil && strings.EqualFold(strings.TrimSpace(compat.Name), "skywork") {
		return true
	}
	return strings.EqualFold(strings.TrimSpace(auth.Provider), "skywork")
}

func rewriteOpenAIImageURLPartsToAnthropicImages(payload []byte) []byte {
	messages := gjson.GetBytes(payload, "messages")
	if !messages.Exists() || !messages.IsArray() {
		return payload
	}

	out := payload
	messages.ForEach(func(messageKey, message gjson.Result) bool {
		content := message.Get("content")
		if !content.Exists() || !content.IsArray() {
			return true
		}
		content.ForEach(func(contentKey, part gjson.Result) bool {
			if part.Get("type").String() != "image_url" {
				return true
			}
			url := part.Get("image_url.url").String()
			replacement := chatcompletions.ConvertOpenAIImageURLToClaudePart(url)
			if replacement == "" {
				return true
			}
			path := fmt.Sprintf("messages.%s.content.%s", messageKey.String(), contentKey.String())
			if updated, err := sjson.SetRawBytes(out, path, []byte(replacement)); err == nil {
				out = updated
			}
			return true
		})
		return true
	})
	return out
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

// isClaudeFamilyModel returns true if the model name indicates a Claude-family model.
func isClaudeFamilyModel(model string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(model)), "claude-")
}

// skyworkClaude1MModels lists Claude models that support the context-1m beta.
var skyworkClaude1MModels = map[string]bool{
	"claude-opus-4.6":   true,
	"claude-sonnet-4.6": true,
}

// ensureSkyworkClaude1MBeta automatically injects the context-1m beta header for
// Skywork Claude 4.6 requests that don't already have it. This ensures all requests
// (including sub-agents that don't inherit the parent's beta headers) route to
// Skywork's 1M backend cluster, which is more stable than the non-1M cluster.
func ensureSkyworkClaude1MBeta(r *http.Request, auth *cliproxyauth.Auth, model string) {
	if r == nil || !cliproxyauth.IsSkyworkFallbackAuth(auth) {
		return
	}
	if !skyworkClaude1MModels[strings.ToLower(strings.TrimSpace(model))] {
		return
	}
	existing := r.Header.Get("Anthropic-Beta")
	if strings.Contains(existing, "context-1m") {
		return
	}
	if existing == "" {
		r.Header.Set("Anthropic-Beta", "context-1m-2025-08-07")
	} else {
		r.Header.Set("Anthropic-Beta", existing+",context-1m-2025-08-07")
	}
}

// adaptCrossFamilyReasoningEffort adjusts reasoning_effort in the payload when the
// execution model is in a different family than the originally-requested model.
func adaptCrossFamilyReasoningEffort(payload []byte, execModel, requestedModel string) []byte {
	if len(payload) == 0 {
		return payload
	}
	fromFamily := cliproxyauth.SkyworkModelFamily(execModel)     // empty if unknown
	toFamily := cliproxyauth.SkyworkModelFamily(requestedModel)  // empty if unknown
	// Swap: fromFamily is the *requested* model's family, toFamily is the *exec* model's family.
	// We are mapping FROM the requested effort space TO the exec effort space.
	fromFamily, toFamily = toFamily, fromFamily
	if fromFamily == "" || toFamily == "" || fromFamily == toFamily {
		return payload
	}
	effort := strings.ToLower(strings.TrimSpace(gjson.GetBytes(payload, "reasoning_effort").String()))
	if effort == "" {
		return payload
	}
	mapped := cliproxyauth.MapCrossFamilyReasoningEffort(effort, fromFamily, toFamily)
	if mapped == effort {
		return payload
	}
	out, err := sjson.SetBytes(payload, "reasoning_effort", mapped)
	if err != nil {
		return payload
	}
	return out
}

func normalizeOpenAICompatTransportError(err error) error {
	if err == nil {
		return nil
	}
	if _, ok := err.(interface{ StatusCode() int }); ok {
		return err
	}
	if errors.Is(err, context.DeadlineExceeded) || isTimeoutLikeError(err) {
		return statusErr{code: http.StatusGatewayTimeout, msg: "upstream request timed out: " + err.Error()}
	}
	return err
}

func isTimeoutLikeError(err error) bool {
	type timeout interface {
		Timeout() bool
	}
	var timeoutErr timeout
	if errors.As(err, &timeoutErr) && timeoutErr != nil && timeoutErr.Timeout() {
		return true
	}
	msg := strings.ToLower(strings.TrimSpace(err.Error()))
	return strings.Contains(msg, "client.timeout exceeded") ||
		strings.Contains(msg, "while awaiting headers") ||
		strings.Contains(msg, "while reading body")
}
