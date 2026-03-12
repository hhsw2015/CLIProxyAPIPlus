package executor

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestOpenAICompatExecutorExecuteStream_SkyworkRewritesImageURLParts(t *testing.T) {
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		gotBody = body
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"id\":\"resp_1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"claude-opus-4.5\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"}}]}\n\n"))
		_, _ = w.Write([]byte("data: {\"id\":\"resp_1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"claude-opus-4.5\",\"choices\":[],\"finish_reason\":\"stop\"}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer server.Close()

	executor := NewOpenAICompatExecutor("skywork", &config.Config{
		OpenAICompatibility: []config.OpenAICompatibility{{
			Name:    "skywork",
			BaseURL: server.URL + "/v1",
			Models:  []config.OpenAICompatibilityModel{{Name: "claude-opus-4.5"}},
		}},
	})

	auth := &cliproxyauth.Auth{
		ID:       "skywork-auth",
		Provider: "skywork",
		Attributes: map[string]string{
			"base_url":     server.URL + "/v1",
			"compat_name":  "skywork",
			"provider_key": "skywork",
		},
	}

	payload := []byte(`{"model":"claude-opus-4.5","max_tokens":64,"messages":[{"role":"user","content":[{"type":"text","text":"what is in this image?"},{"type":"image_url","image_url":{"url":"data:image/png;base64,abc"}}]}],"stream":true}`)
	result, err := executor.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "claude-opus-4.5",
		Payload: payload,
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai"),
		Stream:       true,
	})
	if err != nil {
		t.Fatalf("ExecuteStream error: %v", err)
	}
	for range result.Chunks {
	}

	if gjson.GetBytes(gotBody, "messages.0.content.1.type").String() != "image" {
		t.Fatalf("expected skywork image part type=image, got body %s", string(gotBody))
	}
	if gjson.GetBytes(gotBody, "messages.0.content.1.source.type").String() != "base64" {
		t.Fatalf("expected skywork image source.type=base64, got body %s", string(gotBody))
	}
	if gjson.GetBytes(gotBody, "messages.0.content.1.source.media_type").String() != "image/png" {
		t.Fatalf("expected skywork image media_type=image/png, got body %s", string(gotBody))
	}
	if gjson.GetBytes(gotBody, "messages.0.content.1.source.data").String() != "abc" {
		t.Fatalf("expected skywork image data=abc, got body %s", string(gotBody))
	}
	if strings.Contains(string(gotBody), "\"image_url\"") {
		t.Fatalf("did not expect image_url in rewritten skywork body: %s", string(gotBody))
	}
}

func TestOpenAICompatExecutorExecuteStream_GenericProviderKeepsImageURLParts(t *testing.T) {
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		gotBody = body
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"id\":\"resp_1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"gpt-4o\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"}}]}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer server.Close()

	executor := NewOpenAICompatExecutor("generic", &config.Config{
		OpenAICompatibility: []config.OpenAICompatibility{{
			Name:    "generic",
			BaseURL: server.URL + "/v1",
			Models:  []config.OpenAICompatibilityModel{{Name: "gpt-4o"}},
		}},
	})

	auth := &cliproxyauth.Auth{
		ID:       "generic-auth",
		Provider: "generic",
		Attributes: map[string]string{
			"base_url":     server.URL + "/v1",
			"compat_name":  "generic",
			"provider_key": "generic",
		},
	}

	payload := []byte(`{"model":"gpt-4o","max_tokens":64,"messages":[{"role":"user","content":[{"type":"text","text":"what is in this image?"},{"type":"image_url","image_url":{"url":"data:image/png;base64,abc"}}]}],"stream":true}`)
	result, err := executor.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "gpt-4o",
		Payload: payload,
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai"),
		Stream:       true,
	})
	if err != nil {
		t.Fatalf("ExecuteStream error: %v", err)
	}
	for range result.Chunks {
	}

	if gjson.GetBytes(gotBody, "messages.0.content.1.type").String() != "image_url" {
		t.Fatalf("expected generic provider to keep image_url body, got %s", string(gotBody))
	}
}

func TestOpenAICompatExecutorExecuteStream_SSEJSONErrorBodyReturnsStreamError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"invalid_request_error","message":"operation error Bedrock Runtime: InvokeModelWithResponseStream, https response error StatusCode: 400, RequestID: req_1, ValidationException: tools.0 unsupported"}}`))
	}))
	defer server.Close()

	executor := NewOpenAICompatExecutor("skywork", &config.Config{
		OpenAICompatibility: []config.OpenAICompatibility{{
			Name:    "skywork",
			BaseURL: server.URL + "/v1",
			Models:  []config.OpenAICompatibilityModel{{Name: "claude-opus-4.5"}},
		}},
	})

	auth := &cliproxyauth.Auth{
		ID:       "skywork-auth",
		Provider: "skywork",
		Attributes: map[string]string{
			"base_url":     server.URL + "/v1",
			"compat_name":  "skywork",
			"provider_key": "skywork",
		},
	}

	result, err := executor.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "claude-opus-4.5",
		Payload: []byte(`{"model":"claude-opus-4.5","stream":true}`),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai"),
		Stream:       true,
	})
	if err != nil {
		t.Fatalf("ExecuteStream error: %v", err)
	}

	chunk, ok := <-result.Chunks
	if !ok {
		t.Fatal("expected stream error chunk, got closed channel")
	}
	if chunk.Err == nil {
		t.Fatalf("expected stream error chunk, got payload=%q", string(chunk.Payload))
	}
	if !strings.Contains(chunk.Err.Error(), "tools.0 unsupported") {
		t.Fatalf("expected upstream validation error message, got %v", chunk.Err)
	}
	if se, ok := chunk.Err.(interface{ StatusCode() int }); !ok || se.StatusCode() != http.StatusBadRequest {
		t.Fatalf("expected BadRequest status on stream error, got %#v", chunk.Err)
	}
}

func TestParseOpenAICompatStreamError_StripsLiteralEscapedNewlines(t *testing.T) {
	err := parseOpenAICompatStreamError([]byte("{\"type\":\"error\",\"error\":{\"type\":\"invalid_request_error\",\"message\":\"operation error Bedrock Runtime: InvokeModelWithResponseStream, https response error StatusCode: 400, RequestID: req_1, ValidationException: tools.0 unsupported\"}}\\n\\n"))
	if err == nil {
		t.Fatal("expected parsed error, got nil")
	}
	if !strings.Contains(err.Error(), "tools.0 unsupported") {
		t.Fatalf("expected parsed upstream message, got %v", err)
	}
	if se, ok := err.(interface{ StatusCode() int }); !ok || se.StatusCode() != http.StatusBadRequest {
		t.Fatalf("expected BadRequest status, got %#v", err)
	}
}
