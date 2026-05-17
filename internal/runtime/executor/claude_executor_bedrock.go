package executor

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	kiroclaude "github.com/router-for-me/CLIProxyAPI/v7/internal/translator/kiro/claude"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/thinking"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	log "github.com/sirupsen/logrus"
)

// isBedrockAuth returns true if the auth entry has AWS Bedrock credentials.
func isBedrockAuth(auth *cliproxyauth.Auth) bool {
	return auth != nil && auth.Attributes != nil &&
		strings.TrimSpace(auth.Attributes["aws_access_key_id"]) != ""
}

// bedrockCreds extracts AWS credentials from auth attributes.
func bedrockCreds(auth *cliproxyauth.Auth) (ak, sk, region string) {
	if auth == nil || auth.Attributes == nil {
		return
	}
	ak = strings.TrimSpace(auth.Attributes["aws_access_key_id"])
	sk = strings.TrimSpace(auth.Attributes["aws_secret_access_key"])
	region = strings.TrimSpace(auth.Attributes["aws_region"])
	if region == "" {
		region = "us-east-1"
	}
	return
}

// getBedrockClient returns a cached Bedrock client for the given credentials.
func (e *ClaudeExecutor) getBedrockClient(ak, sk, region string) *bedrockruntime.Client {
	cacheKey := ak + ":" + region
	if v, ok := e.bedrockClients.Load(cacheKey); ok {
		return v.(*bedrockruntime.Client)
	}
	client := bedrockruntime.New(bedrockruntime.Options{
		Region: region,
		Credentials: aws.NewCredentialsCache(
			credentials.NewStaticCredentialsProvider(ak, sk, ""),
		),
	})
	e.bedrockClients.Store(cacheKey, client)
	return client
}

// resolveBedrockModelID looks up the provider-specific model ID from config.
// If a ClaudeModel entry has a ModelID field set, that value is used as the
// Bedrock model identifier; otherwise the model name is used directly.
func (e *ClaudeExecutor) resolveBedrockModelID(auth *cliproxyauth.Auth, clientModel string) string {
	if e.cfg == nil {
		return clientModel
	}
	attrKey := ""
	attrRegion := ""
	if auth != nil && auth.Attributes != nil {
		attrKey = auth.Attributes["api_key"]
		attrRegion = strings.TrimSpace(auth.Attributes["aws_region"])
	}
	// Try exact AK + region match first; if not found, accept dot/dash variants
	// of the model name. Same-AK / same-region uniquely identifies a config entry.
	candidates := []string{strings.ToLower(strings.TrimSpace(clientModel))}
	if alt := flipClaudeOrthography(clientModel); alt != "" && alt != candidates[0] {
		candidates = append(candidates, alt)
	}
	for i := range e.cfg.ClaudeKey {
		ck := &e.cfg.ClaudeKey[i]
		if ck.AWSAccessKeyID == "" {
			continue
		}
		if strings.TrimSpace(ck.AWSAccessKeyID) != attrKey {
			continue
		}
		if attrRegion != "" && strings.TrimSpace(ck.AWSRegion) != "" && strings.TrimSpace(ck.AWSRegion) != attrRegion {
			continue
		}
		for j := range ck.Models {
			m := &ck.Models[j]
			name := strings.ToLower(strings.TrimSpace(m.Name))
			alias := strings.ToLower(strings.TrimSpace(m.Alias))
			for _, want := range candidates {
				if name == want || alias == want {
					if mid := strings.TrimSpace(m.ModelID); mid != "" {
						return mid
					}
					return m.Name
				}
			}
		}
	}
	log.Warnf("[bedrock-modelid] no ARN match: ak=%s region=%s client_model=%s -> falling back to raw name (will likely 400)",
		attrKey, attrRegion, clientModel)
	return clientModel
}

// flipClaudeOrthography swaps "-X.Y" <-> "-X-Y" in the version segment of a
// Claude model name (claude-opus-4.6 <-> claude-opus-4-6) so a registered name
// in either orthography can resolve to its config entry. Empty when no match.
func flipClaudeOrthography(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	// Match -X.Y or -X-Y just before optional -YYYYMMDD snapshot suffix.
	re := claudeVersionRe
	m := re.FindStringSubmatch(s)
	if m == nil {
		return ""
	}
	sep := "-"
	if m[2] == "-" {
		sep = "."
	}
	return strings.ToLower(s[:len(s)-len(m[0])] + "-" + m[1] + sep + m[3] + m[4])
}

var claudeVersionRe = regexp.MustCompile(`(?i)-(\d+)([-\.])(\d+)((?:-\d{8})?)$`)

// prepareBedrockBody adapts an Anthropic Messages API body for Bedrock:
// removes "model", "stream" and any OpenAI-only fields not supported by Bedrock.
// bedrockSupportedToolTypes lists the tool type tags that Bedrock inference
// profiles currently accept. Any tool whose "type" field is not in this set
// will cause a 400 ValidationException. Updated from the actual Bedrock error
// messages as new tool types ship.
var bedrockSupportedToolTypes = map[string]struct{}{
	"bash_20250124":                    {},
	"custom":                           {},
	"memory_20250818":                  {},
	"text_editor_20250124":             {},
	"text_editor_20250429":             {},
	"text_editor_20250728":             {},
	"tool_search_tool_bm25":            {},
	"tool_search_tool_bm25_20251119":   {},
	"tool_search_tool_regex":           {},
	"tool_search_tool_regex_20251119":  {},
	"computer_20250124":                {},
	"mcp":                              {},
	// web_search_20250305 deliberately absent — Bedrock rejects it.
}

// bodyHasUnsupportedBedrockTools returns true if the request body contains
// tools whose type tag Bedrock doesn't recognize. Callers should skip the
// Bedrock path entirely (return a typed error so the conductor falls through
// to Anthropic-API providers that support the full tool catalog).
func (e *ClaudeExecutor) bodyHasUnsupportedBedrockTools(body []byte) (bool, string) {
	tools := gjson.GetBytes(body, "tools")
	if !tools.IsArray() {
		return false, ""
	}
	for _, tool := range tools.Array() {
		tt := tool.Get("type").String()
		if tt == "" {
			continue
		}
		// When web-search interception is enabled, web_search tools will be
		// handled by CPA before reaching Bedrock — not unsupported.
		if e.cfg != nil && e.cfg.WebSearch.Enabled && strings.HasPrefix(tt, "web_search") {
			continue
		}
		if _, ok := bedrockSupportedToolTypes[tt]; !ok {
			return true, tt
		}
	}
	return false, ""
}

// handleBedrockWebSearchStream intercepts web_search tool requests: executes
// the search via the configured provider (TinyFish/Bing), injects results back
// into the conversation, strips the web_search tool definition, then re-enters
// executeStreamBedrock with the modified payload (HasWebSearchTool will return
// false after ReplaceWebSearchToolDescription).
func (e *ClaudeExecutor) handleBedrockWebSearchStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	query := kiroclaude.ExtractSearchQuery(req.Payload)
	if query == "" {
		query = "latest information"
	}

	log.Infof("[web-search] intercepting for bedrock, query=%q", query)

	results, err := dispatchWebSearch(ctx, e.cfg, query)
	if err != nil {
		log.Warnf("[web-search] dispatch failed: %v, proceeding without search results", err)
		// Strip web_search tool and proceed — model won't have search results
		// but at least won't 400 on Bedrock.
		req.Payload, _ = kiroclaude.ReplaceWebSearchToolDescription(req.Payload)
		return e.executeStreamBedrock(ctx, auth, req, opts)
	}

	toolUseID := "srvtoolu_websearch_bedrock"
	modifiedPayload, err := kiroclaude.InjectToolResultsClaude(req.Payload, toolUseID, query, results)
	if err != nil {
		log.Warnf("[web-search] inject failed: %v, stripping tool", err)
		req.Payload, _ = kiroclaude.ReplaceWebSearchToolDescription(req.Payload)
		return e.executeStreamBedrock(ctx, auth, req, opts)
	}

	modifiedPayload, _ = kiroclaude.ReplaceWebSearchToolDescription(modifiedPayload)
	req.Payload = modifiedPayload
	return e.executeStreamBedrock(ctx, auth, req, opts)
}

func prepareBedrockBody(body []byte) []byte {
	body, _ = sjson.DeleteBytes(body, "model")
	body, _ = sjson.DeleteBytes(body, "stream")
	// context_management is an OpenAI Responses API field; Bedrock rejects it.
	body, _ = sjson.DeleteBytes(body, "context_management")
	// response_format and parallel_tool_calls may survive the OpenAI→Claude
	// translation layer; Bedrock returns 400 ValidationException for both.
	// Confirmed present in GPT Proxy's ModifyClaudeParams (IDA 0xd95ed4, 0xd95f14).
	body, _ = sjson.DeleteBytes(body, "response_format")
	body, _ = sjson.DeleteBytes(body, "parallel_tool_calls")
	// betas is an Anthropic API concept; Bedrock rejects unknown beta flags.
	body, _ = sjson.DeleteBytes(body, "betas")
	// Also strip anthropic_beta (alternative field name used by some clients).
	body, _ = sjson.DeleteBytes(body, "anthropic_beta")
	// Force Bedrock-specific anthropic_version. Client-supplied values may contain
	// beta identifiers that the Bedrock model version doesn't support.
	body, _ = sjson.SetBytes(body, "anthropic_version", "bedrock-2023-05-31")
	return body
}

// bedrockThinkingStripCache tracks sessions that have encountered invalid
// thinking signatures. Once a session hits this error, subsequent requests
// for that session strip thinking blocks proactively for a bounded window.
//
// Stripping thinking blocks is safe for correctness (model reasons fresh
// each turn) but discards Anthropic's signed-thinking cache hint, costing
// extra latency + tokens. The TTL prevents the mark from persisting forever
// after a transient route to a thinking-incompatible upstream.
var bedrockThinkingStripCache sync.Map // session-id -> time.Time (markedAt)

const thinkingStripTTL = 10 * time.Minute

// thinkingStripCacheKey returns a stable cache key for the strip mark.
// Prefers the conductor-propagated session id (extracted from the request body
// or headers, shared across calls in the same client chat session); falls back
// to the Anthropic execution-session id, then to the per-request id so even an
// anonymous request still benefits from the inline strip shortcut. The TTL
// bounds any over-pessimism.
func thinkingStripCacheKey(ctx context.Context) string {
	if sid := cliproxyauth.ExecutorSessionIDFromContext(ctx); sid != "" {
		return "sess:" + sid
	}
	if sid := handlers.ExecutionSessionIDFromContext(ctx); sid != "" {
		return "sess:" + sid
	}
	if rid := logging.GetRequestID(ctx); rid != "" {
		return "req:" + rid
	}
	return ""
}

func bedrockSessionID(ctx context.Context) string {
	return thinkingStripCacheKey(ctx)
}

func shouldStripThinkingForSession(ctx context.Context) bool {
	sid := bedrockSessionID(ctx)
	if sid == "" {
		return false
	}
	v, found := bedrockThinkingStripCache.Load(sid)
	if !found {
		return false
	}
	markedAt, ok := v.(time.Time)
	if !ok {
		bedrockThinkingStripCache.Delete(sid)
		return false
	}
	if time.Since(markedAt) > thinkingStripTTL {
		bedrockThinkingStripCache.Delete(sid)
		return false
	}
	return true
}

func markSessionNeedsThinkingStrip(ctx context.Context) {
	sid := bedrockSessionID(ctx)
	if sid == "" {
		log.Warn("[thinking-strip] error matched but session-id empty, cannot mark for next request")
		return
	}
	bedrockThinkingStripCache.Store(sid, time.Now())
	log.Infof("[thinking-strip] session %s marked (TTL %s), next request will strip thinking blocks proactively", sid, thinkingStripTTL)
}

// isThinkingErrorMessage scans a raw upstream error string for the same
// symptoms isThinkingSignatureError handles. Used by non-Bedrock executors
// (claude direct, openai-compat cookie-pool) that surface upstream 4xx as
// statusErr.msg rather than as a typed AWS error.
func isThinkingErrorMessage(msg string) bool {
	if msg == "" {
		return false
	}
	hasThinking := strings.Contains(msg, "thinking") || strings.Contains(msg, "redacted_thinking")
	if !hasThinking {
		return false
	}
	return strings.Contains(msg, "signature") ||
		strings.Contains(msg, "Invalid `data`") ||
		strings.Contains(msg, "Invalid data") ||
		strings.Contains(msg, "Field required")
}

// isThinkingSignatureError returns true if the Bedrock error is about a
// thinking / redacted_thinking block in the conversation history. Covers:
//   - "Invalid signature in thinking block"     (cross-provider signature mismatch)
//   - "Field required: signature"               (signature missing on a thinking block)
//   - "Invalid `data` in `redacted_thinking`"   (cross-provider redacted_thinking blob)
//
// In all three cases, stripping thinking-type blocks from history and
// retrying is safe -- previous-turn thinking outputs don't affect the
// model's reasoning for the current turn.
func isThinkingSignatureError(err error) bool {
	if err == nil {
		return false
	}
	return isThinkingErrorMessage(err.Error())
}

// stripThinkingBlocksFromHistory removes thinking-type content blocks from
// assistant messages in the conversation history. The last assistant message
// is preserved as-is since it may be a prefill or the current turn.
// stripThinkingBlocksFromHistory removes thinking and redacted_thinking
// content blocks from EVERY message in the conversation, regardless of role.
// Cross-provider sessions can stash these blocks anywhere (assistant messages,
// tool_result content arrays, even system-injected wrappers); Bedrock only
// validates structure, not provenance, so removing them everywhere is safe
// and matches headroom's behavior.
func stripThinkingBlocksFromHistory(body []byte) []byte {
	messages := gjson.GetBytes(body, "messages")
	if !messages.IsArray() {
		return body
	}
	arr := messages.Array()
	if len(arr) == 0 {
		return body
	}
	stripped := 0
	for i, msg := range arr {
		content := msg.Get("content")
		if !content.IsArray() {
			continue
		}
		blocks := content.Array()
		var kept []gjson.Result
		for _, block := range blocks {
			bt := block.Get("type").String()
			if bt == "thinking" || bt == "redacted_thinking" {
				stripped++
				continue
			}
			kept = append(kept, block)
		}
		if len(kept) == len(blocks) {
			continue
		}
		if len(kept) == 0 {
			kept = append(kept, gjson.Parse(`{"type":"text","text":""}`))
		}
		raw := []byte("[")
		for j, k := range kept {
			if j > 0 {
				raw = append(raw, ',')
			}
			raw = append(raw, []byte(k.Raw)...)
		}
		raw = append(raw, ']')
		path := fmt.Sprintf("messages.%d.content", i)
		body, _ = sjson.SetRawBytes(body, path, raw)
	}
	if stripped > 0 {
		log.Infof("[thinking-strip] removed %d thinking/redacted_thinking blocks from history", stripped)
	}
	return body
}

// executeBedrock handles non-streaming Bedrock requests.
func (e *ClaudeExecutor) executeBedrock(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (resp cliproxyexecutor.Response, err error) {
	if unsupported, tag := e.bodyHasUnsupportedBedrockTools(req.Payload); unsupported {
		return resp, statusErr{code: http.StatusBadRequest, msg: fmt.Sprintf("bedrock: unsupported tool type %q, skipping", tag)}
	}
	baseModel := thinking.ParseSuffix(req.Model).ModelName
	if isNovaModel(baseModel) {
		return e.executeBedrockNova(ctx, auth, req.Payload, baseModel)
	}

	reporter := newUsageReporter(ctx, e.Identifier(), baseModel, auth)
	defer reporter.trackFailure(ctx, &err)

	from := opts.SourceFormat
	to := sdktranslator.FromString("claude")
	body := sdktranslator.TranslateRequest(from, to, baseModel, req.Payload, false)
	body, err = thinking.ApplyThinking(body, req.Model, from.String(), to.String(), e.Identifier())
	if err != nil {
		return resp, err
	}
	requestedModel := payloadRequestedModel(opts, req.Model)
	originalPayloadSource := req.Payload
	if len(opts.OriginalRequest) > 0 {
		originalPayloadSource = opts.OriginalRequest
	}
	originalTranslated := sdktranslator.TranslateRequest(from, to, baseModel, originalPayloadSource, false)
	body = applyPayloadConfigWithRoot(e.cfg, baseModel, to.String(), "", body, originalTranslated, requestedModel)
	body = normalizeThinkingForAdaptiveModels(body, baseModel)
	body = prepareBedrockBody(body)
	if shouldStripThinkingForSession(ctx) {
		body = stripThinkingBlocksFromHistory(body)
	}

	ak, sk, region := bedrockCreds(auth)
	client := e.getBedrockClient(ak, sk, region)
	modelID := e.resolveBedrockModelID(auth, baseModel)

	output, err := client.InvokeModel(ctx, &bedrockruntime.InvokeModelInput{
		ModelId:     aws.String(modelID),
		ContentType: aws.String("application/json"),
		Accept:      aws.String("application/json"),
		Body:        body,
	})
	if err != nil && isThinkingSignatureError(err) {
		markSessionNeedsThinkingStrip(ctx)
		body = stripThinkingBlocksFromHistory(body)
		output, err = client.InvokeModel(ctx, &bedrockruntime.InvokeModelInput{
			ModelId:     aws.String(modelID),
			ContentType: aws.String("application/json"),
			Accept:      aws.String("application/json"),
			Body:        body,
		})
	}
	if err != nil {
		helps.LogWithRequestID(ctx).Errorf("bedrock InvokeModel error model=%s modelID=%s region=%s body_bytes=%d: %v",
			baseModel, modelID, region, len(body), err)
		return resp, statusErr{code: http.StatusBadGateway, msg: fmt.Sprintf("bedrock invoke error: %v", err)}
	}

	resp.Headers = http.Header{"Content-Type": {"application/json"}}
	resp.Payload = output.Body

	// Publish usage from the non-streaming Bedrock response.
	reporter.publish(ctx, helps.ParseClaudeUsage(output.Body))

	return resp, nil
}

// executeStreamBedrock handles streaming Bedrock requests, bridging
// AWS EventStream events to SSE format on the StreamChunk channel.
func (e *ClaudeExecutor) executeStreamBedrock(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (_ *cliproxyexecutor.StreamResult, err error) {
	// Web search interception: if the request contains web_search tools and
	// gateway-level search is enabled, handle via CPA's search dispatch
	// so the tools array sent to Bedrock won't include web_search.
	if e.cfg != nil && e.cfg.WebSearch.Enabled && kiroclaude.HasWebSearchTool(req.Payload) {
		return e.handleBedrockWebSearchStream(ctx, auth, req, opts)
	}

	baseModel := thinking.ParseSuffix(req.Model).ModelName
	if isNovaModel(baseModel) {
		ch, err := e.executeBedrockNovaStream(ctx, auth, req.Payload, baseModel)
		if err != nil {
			return nil, err
		}
		return &cliproxyexecutor.StreamResult{Chunks: ch}, nil
	}

	reporter := newUsageReporter(ctx, e.Identifier(), baseModel, auth)
	defer reporter.trackFailure(ctx, &err)

	from := opts.SourceFormat
	to := sdktranslator.FromString("claude")
	body := sdktranslator.TranslateRequest(from, to, baseModel, req.Payload, true)
	body, err = thinking.ApplyThinking(body, req.Model, from.String(), to.String(), e.Identifier())
	if err != nil {
		return nil, err
	}
	requestedModel := payloadRequestedModel(opts, req.Model)
	originalPayloadSource := req.Payload
	if len(opts.OriginalRequest) > 0 {
		originalPayloadSource = opts.OriginalRequest
	}
	originalTranslated := sdktranslator.TranslateRequest(from, to, baseModel, originalPayloadSource, true)
	body = applyPayloadConfigWithRoot(e.cfg, baseModel, to.String(), "", body, originalTranslated, requestedModel)
	body = normalizeThinkingForAdaptiveModels(body, baseModel)
	body = prepareBedrockBody(body)
	if shouldStripThinkingForSession(ctx) {
		body = stripThinkingBlocksFromHistory(body)
	}

	ak, sk, region := bedrockCreds(auth)
	client := e.getBedrockClient(ak, sk, region)
	modelID := e.resolveBedrockModelID(auth, baseModel)

	output, err := client.InvokeModelWithResponseStream(ctx, &bedrockruntime.InvokeModelWithResponseStreamInput{
		ModelId:     aws.String(modelID),
		ContentType: aws.String("application/json"),
		Body:        body,
	})
	if err != nil && isThinkingSignatureError(err) {
		markSessionNeedsThinkingStrip(ctx)
		body = stripThinkingBlocksFromHistory(body)
		output, err = client.InvokeModelWithResponseStream(ctx, &bedrockruntime.InvokeModelWithResponseStreamInput{
			ModelId:     aws.String(modelID),
			ContentType: aws.String("application/json"),
			Body:        body,
		})
	}
	if err != nil {
		helps.LogWithRequestID(ctx).Errorf("bedrock InvokeModelWithResponseStream error model=%s modelID=%s region=%s body_bytes=%d: %v",
			baseModel, modelID, region, len(body), err)
		return nil, statusErr{code: http.StatusBadGateway, msg: fmt.Sprintf("bedrock stream error: %v", err)}
	}

	stream := output.GetStream()
	out := make(chan cliproxyexecutor.StreamChunk)

	go func() {
		defer close(out)
		defer stream.Close()

		for event := range stream.Events() {
			chunk, ok := event.(*types.ResponseStreamMemberChunk)
			if !ok {
				continue
			}
			jsonBytes := chunk.Value.Bytes

			// Log and extract usage from the chunk.
			sseDataLine := bytes.Join([][]byte{[]byte("data: "), jsonBytes}, nil)
			appendAPIResponseChunk(ctx, e.cfg, sseDataLine)
			if detail, ok := helps.ParseClaudeStreamUsage(sseDataLine); ok {
				reporter.publish(ctx, detail)
			}
			// Detect rate_limit_error or throttling embedded in stream events.
			if errType := gjson.GetBytes(jsonBytes, "error.type").String(); errType == "rate_limit_error" {
				msg := gjson.GetBytes(jsonBytes, "error.message").String()
				if msg == "" {
					msg = "rate limited (detected in Bedrock stream)"
				}
				out <- cliproxyexecutor.StreamChunk{Err: statusErr{code: 429, msg: msg}}
				return
			}

			// Re-wrap Bedrock JSON as SSE: event type + data + blank line separator.
			eventType := gjson.GetBytes(jsonBytes, "type").String()
			if eventType != "" {
				out <- cliproxyexecutor.StreamChunk{Payload: []byte("event: " + eventType + "\n")}
			}
			dataLine := make([]byte, 0, len(jsonBytes)+7)
			dataLine = append(dataLine, "data: "...)
			dataLine = append(dataLine, jsonBytes...)
			dataLine = append(dataLine, '\n')
			out <- cliproxyexecutor.StreamChunk{Payload: dataLine}
			out <- cliproxyexecutor.StreamChunk{Payload: []byte("\n")}
		}

		if streamErr := stream.Err(); streamErr != nil {
			if !shouldIgnoreClaudeStreamScannerError(streamErr) {
				log.Errorf("bedrock stream error: %v", streamErr)
				out <- cliproxyexecutor.StreamChunk{Err: streamErr}
			}
		}
		reporter.ensurePublished(ctx)
	}()

	return &cliproxyexecutor.StreamResult{
		Headers: http.Header{"Content-Type": {"text/event-stream"}},
		Chunks:  out,
	}, nil
}