package executor

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
)

// isConverseModel reports whether a Bedrock model should be served via the
// Bedrock Converse API rather than the Anthropic InvokeModel path. It covers all
// NON-Claude Bedrock families (Nova/Llama/DeepSeek/Mistral/Nemotron/MiniMax/GLM/
// gpt-oss/...). Claude models (claude-*) keep the Anthropic path. This predicate
// only ever runs for bedrock auths, and the served set is controlled by the
// generator, so a broad provider-prefix match is safe here.
func isConverseModel(model string) bool {
	l := strings.ToLower(strings.TrimSpace(model))
	if strings.HasPrefix(l, "claude") || l == "" {
		return false
	}
	for _, p := range []string{
		"amazon.", "nova-", "meta.", "deepseek", "mistral.", "nvidia.",
		"minimax", "zai.", "ai21.", "cohere.", "openai.", "qwen.",
		"writer.", "xai.", "moonshot", "google.", "luma.", "twelvelabs.",
	} {
		if strings.HasPrefix(l, p) {
			return true
		}
	}
	return false
}

func int32Ptr(i int64) *int32       { v := int32(i); return &v }
func float32Ptr(f float64) *float32 { v := float32(f); return &v }

// openAIContentToBlocks converts an OpenAI message "content" (string or array of
// parts) into Bedrock Converse ContentBlocks (text + inline images).
func openAIContentToBlocks(content gjson.Result) []types.ContentBlock {
	var blocks []types.ContentBlock
	switch {
	case content.Type == gjson.String:
		if s := content.String(); s != "" {
			blocks = append(blocks, &types.ContentBlockMemberText{Value: s})
		}
	case content.IsArray():
		content.ForEach(func(_, part gjson.Result) bool {
			switch part.Get("type").String() {
			case "text", "":
				if t := part.Get("text").String(); t != "" {
					blocks = append(blocks, &types.ContentBlockMemberText{Value: t})
				}
			case "image_url":
				if img := dataURLToImageBlock(part.Get("image_url.url").String()); img != nil {
					blocks = append(blocks, img)
				}
			}
			return true
		})
	}
	return blocks
}

// dataURLToImageBlock decodes a data: URL into a Converse image ContentBlock.
// Returns nil for non-data URLs or undecodable data (Converse needs raw bytes).
func dataURLToImageBlock(url string) types.ContentBlock {
	if !strings.HasPrefix(url, "data:") {
		return nil
	}
	comma := strings.Index(url, ",")
	if comma < 0 {
		return nil
	}
	meta, b64 := url[5:comma], url[comma+1:]
	format := types.ImageFormatPng
	switch {
	case strings.Contains(meta, "jpeg"), strings.Contains(meta, "jpg"):
		format = types.ImageFormatJpeg
	case strings.Contains(meta, "gif"):
		format = types.ImageFormatGif
	case strings.Contains(meta, "webp"):
		format = types.ImageFormatWebp
	}
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil || len(raw) == 0 {
		return nil
	}
	return &types.ContentBlockMemberImage{Value: types.ImageBlock{
		Format: format,
		Source: &types.ImageSourceMemberBytes{Value: raw},
	}}
}

// buildConverseInput translates an OpenAI chat-completions body into a Bedrock
// ConverseInput. System messages go to System[]; user/assistant to Messages[].
func buildConverseInput(body []byte, modelID string) *bedrockruntime.ConverseInput {
	in := &bedrockruntime.ConverseInput{ModelId: aws.String(modelID)}
	var sys []types.SystemContentBlock
	var msgs []types.Message

	gjson.GetBytes(body, "messages").ForEach(func(_, m gjson.Result) bool {
		role := m.Get("role").String()
		blocks := openAIContentToBlocks(m.Get("content"))
		if role == "system" || role == "developer" {
			for _, b := range blocks {
				if tb, ok := b.(*types.ContentBlockMemberText); ok {
					sys = append(sys, &types.SystemContentBlockMemberText{Value: tb.Value})
				}
			}
			return true
		}
		convRole := types.ConversationRoleUser
		if role == "assistant" {
			convRole = types.ConversationRoleAssistant
		}
		if len(blocks) == 0 {
			blocks = []types.ContentBlock{&types.ContentBlockMemberText{Value: "..."}}
		}
		msgs = append(msgs, types.Message{Role: convRole, Content: blocks})
		return true
	})

	// Anthropic-style top-level "system" as a fallback source.
	if s := gjson.GetBytes(body, "system"); s.Exists() && len(sys) == 0 {
		if s.Type == gjson.String && s.String() != "" {
			sys = append(sys, &types.SystemContentBlockMemberText{Value: s.String()})
		} else if s.IsArray() {
			s.ForEach(func(_, p gjson.Result) bool {
				if t := p.Get("text").String(); t != "" {
					sys = append(sys, &types.SystemContentBlockMemberText{Value: t})
				}
				return true
			})
		}
	}

	in.Messages = msgs
	if len(sys) > 0 {
		in.System = sys
	}

	inf := &types.InferenceConfiguration{}
	mt := gjson.GetBytes(body, "max_tokens").Int()
	if mt <= 0 {
		mt = gjson.GetBytes(body, "max_completion_tokens").Int()
	}
	if mt <= 0 {
		mt = 4096 // OpenAI clients often omit max_tokens; Converse defaults can be tiny.
	}
	inf.MaxTokens = int32Ptr(mt)
	if r := gjson.GetBytes(body, "temperature"); r.Exists() {
		inf.Temperature = float32Ptr(r.Float())
	}
	if r := gjson.GetBytes(body, "top_p"); r.Exists() {
		inf.TopP = float32Ptr(r.Float())
	}
	in.InferenceConfig = inf
	return in
}

// converseStopToFinish maps a Bedrock StopReason to an OpenAI finish_reason.
func converseStopToFinish(sr types.StopReason) string {
	switch sr {
	case types.StopReasonMaxTokens:
		return "length"
	case types.StopReasonToolUse:
		return "tool_calls"
	default:
		return "stop"
	}
}

// converseToOpenAI builds an OpenAI chat-completions response from a ConverseOutput.
func converseToOpenAI(out *bedrockruntime.ConverseOutput, model string) []byte {
	var text strings.Builder
	if msg, ok := out.Output.(*types.ConverseOutputMemberMessage); ok {
		for _, block := range msg.Value.Content {
			if tb, ok := block.(*types.ContentBlockMemberText); ok {
				text.WriteString(tb.Value)
			}
		}
	}
	resp := []byte(`{"object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":""},"finish_reason":"stop"}]}`)
	resp, _ = sjson.SetBytes(resp, "model", model)
	resp, _ = sjson.SetBytes(resp, "choices.0.message.content", text.String())
	resp, _ = sjson.SetBytes(resp, "choices.0.finish_reason", converseStopToFinish(out.StopReason))
	if out.Usage != nil {
		in := int64(aws.ToInt32(out.Usage.InputTokens))
		o := int64(aws.ToInt32(out.Usage.OutputTokens))
		resp, _ = sjson.SetRawBytes(resp, "usage", []byte(fmt.Sprintf(
			`{"prompt_tokens":%d,"completion_tokens":%d,"total_tokens":%d}`, in, o, in+o)))
	}
	return resp
}

// executeBedrockConverse handles non-streaming non-Claude Bedrock models via the
// typed Converse API (SigV4 through the cached bedrockruntime.Client).
func (e *ClaudeExecutor) executeBedrockConverse(ctx context.Context, auth *cliproxyauth.Auth, body []byte, baseModel string) (cliproxyexecutor.Response, error) {
	ak, sk, region := bedrockCreds(auth)
	client := e.getBedrockClient(ak, sk, region)
	modelID := e.resolveBedrockModelID(auth, baseModel)
	reporter := helps.NewUsageReporter(ctx, e.Identifier(), baseModel, auth)

	out, err := client.Converse(ctx, buildConverseInput(body, modelID))
	if err != nil {
		reporter.PublishFailure(ctx)
		return cliproxyexecutor.Response{}, statusErr{code: bedrockErrorHTTPStatus(err), msg: fmt.Sprintf("bedrock converse: %v", err)}
	}
	result := converseToOpenAI(out, baseModel)
	reporter.Publish(ctx, helps.ParseOpenAIUsage(result))
	return cliproxyexecutor.Response{Payload: result}, nil
}

// executeBedrockConverseStream handles streaming non-Claude Bedrock models via
// ConverseStream, emitting OpenAI chat-completion SSE chunks.
func (e *ClaudeExecutor) executeBedrockConverseStream(ctx context.Context, auth *cliproxyauth.Auth, body []byte, baseModel string) (<-chan cliproxyexecutor.StreamChunk, error) {
	ak, sk, region := bedrockCreds(auth)
	client := e.getBedrockClient(ak, sk, region)
	modelID := e.resolveBedrockModelID(auth, baseModel)
	reporter := helps.NewUsageReporter(ctx, e.Identifier(), baseModel, auth)

	ci := buildConverseInput(body, modelID)
	output, err := client.ConverseStream(ctx, &bedrockruntime.ConverseStreamInput{
		ModelId:         ci.ModelId,
		Messages:        ci.Messages,
		System:          ci.System,
		InferenceConfig: ci.InferenceConfig,
	})
	if err != nil {
		reporter.PublishFailure(ctx)
		return nil, statusErr{code: bedrockErrorHTTPStatus(err), msg: fmt.Sprintf("bedrock converse-stream: %v", err)}
	}

	out := make(chan cliproxyexecutor.StreamChunk, 32)
	go func() {
		defer close(out)
		stream := output.GetStream()
		defer func() { _ = stream.Close() }()
		var pt, ct int64
		finish := "stop"
		// The API handler frames each chunk as "data: %s\n\n" and emits the
		// terminal "data: [DONE]" itself on channel close, so emit RAW JSON here.
		emit := func(b []byte) bool {
			select {
			case out <- cliproxyexecutor.StreamChunk{Payload: b}:
				return true
			case <-ctx.Done():
				return false
			}
		}
		for event := range stream.Events() {
			switch v := event.(type) {
			case *types.ConverseStreamOutputMemberContentBlockDelta:
				if td, ok := v.Value.Delta.(*types.ContentBlockDeltaMemberText); ok && td.Value != "" {
					chunk := []byte(`{"object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":""}}]}`)
					chunk, _ = sjson.SetBytes(chunk, "model", baseModel)
					chunk, _ = sjson.SetBytes(chunk, "choices.0.delta.content", td.Value)
					if !emit(chunk) {
						return
					}
				}
			case *types.ConverseStreamOutputMemberMessageStop:
				finish = converseStopToFinish(v.Value.StopReason)
			case *types.ConverseStreamOutputMemberMetadata:
				if v.Value.Usage != nil {
					pt = int64(aws.ToInt32(v.Value.Usage.InputTokens))
					ct = int64(aws.ToInt32(v.Value.Usage.OutputTokens))
				}
			}
		}
		if errStream := stream.Err(); errStream != nil {
			reporter.PublishFailure(ctx)
			out <- cliproxyexecutor.StreamChunk{Err: statusErr{code: bedrockErrorHTTPStatus(errStream), msg: errStream.Error()}}
			return
		}
		// Final chunk with finish_reason + usage, then [DONE].
		final := []byte(`{"object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`)
		final, _ = sjson.SetBytes(final, "model", baseModel)
		final, _ = sjson.SetBytes(final, "choices.0.finish_reason", finish)
		usageJSON := fmt.Sprintf(`{"prompt_tokens":%d,"completion_tokens":%d,"total_tokens":%d}`, pt, ct, pt+ct)
		final, _ = sjson.SetRawBytes(final, "usage", []byte(usageJSON))
		_ = emit(final)
		reporter.Publish(ctx, helps.ParseOpenAIUsage([]byte(`{"usage":`+usageJSON+`}`)))
		// Do NOT emit [DONE] — the API handler writes it on channel close.
	}()
	return out, nil
}
