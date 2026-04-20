package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/runtime/executor/helps"
)

func isNovaModel(model string) bool {
	lower := strings.ToLower(model)
	return strings.HasPrefix(lower, "nova-") ||
		strings.Contains(lower, "amazon.nova")
}

// prepareNovaConverseBody converts an OpenAI-style messages body to Bedrock Converse format.
// Converse uses: {"messages": [{"role":"user","content":[{"text":"..."}]}], "inferenceConfig":{...}}
func prepareNovaConverseBody(body []byte, modelID string) []byte {
	messages := gjson.GetBytes(body, "messages")
	if !messages.Exists() {
		return body
	}

	var convoMessages []map[string]any
	messages.ForEach(func(_, msg gjson.Result) bool {
		role := msg.Get("role").String()
		content := msg.Get("content")

		var contentBlocks []map[string]any
		if content.Type == gjson.String {
			contentBlocks = append(contentBlocks, map[string]any{"text": content.String()})
		} else if content.IsArray() {
			content.ForEach(func(_, part gjson.Result) bool {
				switch part.Get("type").String() {
				case "text", "":
					contentBlocks = append(contentBlocks, map[string]any{"text": part.Get("text").String()})
				case "image_url":
					contentBlocks = append(contentBlocks, map[string]any{
						"image": map[string]any{
							"format": "png",
							"source": map[string]any{
								"bytes": part.Get("image_url.url").String(),
							},
						},
					})
				}
				return true
			})
		}

		if len(contentBlocks) > 0 {
			convoMessages = append(convoMessages, map[string]any{
				"role":    role,
				"content": contentBlocks,
			})
		}
		return true
	})

	inferenceConfig := map[string]any{}
	if v := gjson.GetBytes(body, "max_tokens").Int(); v > 0 {
		inferenceConfig["maxTokens"] = v
	}
	if v := gjson.GetBytes(body, "temperature").Float(); v > 0 {
		inferenceConfig["temperature"] = v
	}
	if v := gjson.GetBytes(body, "top_p").Float(); v > 0 {
		inferenceConfig["topP"] = v
	}

	out := map[string]any{
		"modelId":  modelID,
		"messages": convoMessages,
	}
	if len(inferenceConfig) > 0 {
		out["inferenceConfig"] = inferenceConfig
	}

	// System prompt
	if sys := gjson.GetBytes(body, "system"); sys.Exists() {
		var sysParts []map[string]any
		if sys.IsArray() {
			sys.ForEach(func(_, part gjson.Result) bool {
				sysParts = append(sysParts, map[string]any{"text": part.Get("text").String()})
				return true
			})
		} else if sys.Type == gjson.String {
			sysParts = append(sysParts, map[string]any{"text": sys.String()})
		}
		if len(sysParts) > 0 {
			out["system"] = sysParts
		}
	}

	result, _ := json.Marshal(out)
	return result
}

// parseNovaConverseResponse converts Converse API response to OpenAI chat completion format.
func parseNovaConverseResponse(data []byte) []byte {
	output := gjson.GetBytes(data, "output.message.content")
	if !output.Exists() {
		return data
	}

	var text strings.Builder
	output.ForEach(func(_, block gjson.Result) bool {
		if t := block.Get("text").String(); t != "" {
			text.WriteString(t)
		}
		return true
	})

	resp := []byte(`{"choices":[{"message":{"role":"assistant","content":""},"index":0,"finish_reason":"stop"}]}`)
	resp, _ = sjson.SetBytes(resp, "choices.0.message.content", text.String())

	if usage := gjson.GetBytes(data, "usage"); usage.Exists() {
		resp, _ = sjson.SetRawBytes(resp, "usage", []byte(fmt.Sprintf(
			`{"prompt_tokens":%d,"completion_tokens":%d,"total_tokens":%d}`,
			usage.Get("inputTokens").Int(),
			usage.Get("outputTokens").Int(),
			usage.Get("inputTokens").Int()+usage.Get("outputTokens").Int(),
		)))
	}
	return resp
}

// executeBedrockNova handles non-streaming Nova Converse API requests.
func (e *ClaudeExecutor) executeBedrockNova(ctx context.Context, auth *cliproxyauth.Auth, body []byte, baseModel string) (cliproxyexecutor.Response, error) {
	ak, sk, region := bedrockCreds(auth)
	client := e.getBedrockClient(ak, sk, region)
	modelID := e.resolveBedrockModelID(auth, baseModel)

	reporter := helps.NewUsageReporter(ctx, e.Identifier(), baseModel, auth)

	converseBody := prepareNovaConverseBody(body, modelID)

	output, err := client.InvokeModel(ctx, &bedrockruntime.InvokeModelInput{
		ModelId:     &modelID,
		ContentType: stringPtr("application/json"),
		Accept:      stringPtr("application/json"),
		Body:        converseBody,
	})
	if err != nil {
		reporter.PublishFailure(ctx)
		return cliproxyexecutor.Response{}, fmt.Errorf("nova-converse: %w", err)
	}

	result := parseNovaConverseResponse(output.Body)
	reporter.Publish(ctx, helps.ParseOpenAIUsage(result))
	return cliproxyexecutor.Response{Payload: result}, nil
}

// executeBedrockNovaStream handles streaming Nova Converse API requests.
func (e *ClaudeExecutor) executeBedrockNovaStream(ctx context.Context, auth *cliproxyauth.Auth, body []byte, baseModel string) (<-chan cliproxyexecutor.StreamChunk, error) {
	ak, sk, region := bedrockCreds(auth)
	client := e.getBedrockClient(ak, sk, region)
	modelID := e.resolveBedrockModelID(auth, baseModel)

	reporter := helps.NewUsageReporter(ctx, e.Identifier(), baseModel, auth)

	converseBody := prepareNovaConverseBody(body, modelID)

	output, err := client.InvokeModelWithResponseStream(ctx, &bedrockruntime.InvokeModelWithResponseStreamInput{
		ModelId:     &modelID,
		ContentType: stringPtr("application/json"),
		Accept:      stringPtr("application/json"),
		Body:        converseBody,
	})
	if err != nil {
		reporter.PublishFailure(ctx)
		return nil, fmt.Errorf("nova-converse-stream: %w", err)
	}

	out := make(chan cliproxyexecutor.StreamChunk, 32)
	go func() {
		defer close(out)
		defer reporter.PublishFailure(ctx)
		stream := output.GetStream()
		defer stream.Close()

		for event := range stream.Events() {
			switch v := event.(type) {
			case *types.ResponseStreamMemberChunk:
				chunk := parseNovaStreamChunk(v.Value.Bytes)
				if chunk != nil {
					out <- cliproxyexecutor.StreamChunk{Payload: chunk}
				}
			}
		}
		if err := stream.Err(); err != nil {
			out <- cliproxyexecutor.StreamChunk{Err: fmt.Errorf("nova-stream: %w", err)}
		}
	}()
	return out, nil
}

func parseNovaStreamChunk(data []byte) []byte {
	text := gjson.GetBytes(data, "contentBlockDelta.delta.text").String()
	if text == "" {
		text = gjson.GetBytes(data, "delta.text").String()
	}
	if text == "" {
		return nil
	}
	chunk := fmt.Sprintf(`data: {"choices":[{"delta":{"content":"%s"},"index":0}]}`, strings.ReplaceAll(text, `"`, `\"`))
	return []byte(chunk + "\n\n")
}

func stringPtr(s string) *string { return &s }
