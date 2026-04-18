package executor

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/thinking"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
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
	for i := range e.cfg.ClaudeKey {
		ck := &e.cfg.ClaudeKey[i]
		if ck.AWSAccessKeyID == "" {
			continue
		}
		// Match by AK (stored as api_key in attributes).
		if strings.TrimSpace(ck.AWSAccessKeyID) != attrKey {
			continue
		}
		// When multiple entries share the same AK (different regions), also match by region
		// to ensure the ARN corresponds to the correct regional endpoint.
		if attrRegion != "" && strings.TrimSpace(ck.AWSRegion) != "" && strings.TrimSpace(ck.AWSRegion) != attrRegion {
			continue
		}
		for j := range ck.Models {
			m := &ck.Models[j]
			if strings.EqualFold(strings.TrimSpace(m.Name), clientModel) ||
				strings.EqualFold(strings.TrimSpace(m.Alias), clientModel) {
				if mid := strings.TrimSpace(m.ModelID); mid != "" {
					return mid
				}
				return m.Name
			}
		}
	}
	return clientModel
}

// prepareBedrockBody adapts an Anthropic Messages API body for Bedrock:
// removes "model", "stream" and any OpenAI-only fields not supported by Bedrock.
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

// executeBedrock handles non-streaming Bedrock requests.
func (e *ClaudeExecutor) executeBedrock(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (resp cliproxyexecutor.Response, err error) {
	baseModel := thinking.ParseSuffix(req.Model).ModelName

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
	body = prepareBedrockBody(body)

	ak, sk, region := bedrockCreds(auth)
	client := e.getBedrockClient(ak, sk, region)
	modelID := e.resolveBedrockModelID(auth, baseModel)

	output, err := client.InvokeModel(ctx, &bedrockruntime.InvokeModelInput{
		ModelId:     aws.String(modelID),
		ContentType: aws.String("application/json"),
		Accept:      aws.String("application/json"),
		Body:        body,
	})
	if err != nil {
		log.Errorf("bedrock InvokeModel error for model %s (modelID=%s, region=%s): %v", baseModel, modelID, region, err)
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
	baseModel := thinking.ParseSuffix(req.Model).ModelName

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
	body = prepareBedrockBody(body)

	ak, sk, region := bedrockCreds(auth)
	client := e.getBedrockClient(ak, sk, region)
	modelID := e.resolveBedrockModelID(auth, baseModel)

	output, err := client.InvokeModelWithResponseStream(ctx, &bedrockruntime.InvokeModelWithResponseStreamInput{
		ModelId:     aws.String(modelID),
		ContentType: aws.String("application/json"),
		Body:        body,
	})
	if err != nil {
		log.Errorf("bedrock InvokeModelWithResponseStream error for model %s (modelID=%s, region=%s): %v", baseModel, modelID, region, err)
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