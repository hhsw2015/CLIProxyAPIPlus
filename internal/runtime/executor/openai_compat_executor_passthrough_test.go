package executor

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestOpenAICompatExecutorExecute_PassthroughsAnthropicBetasAndSpeed(t *testing.T) {
	var gotBody []byte
	var gotBetaHeader string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		gotBody = body
		gotBetaHeader = r.Header.Get("Anthropic-Beta")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl_1","object":"chat.completion","model":"claude-opus-4.5","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	}))
	defer server.Close()

	executor := NewOpenAICompatExecutor("openai-compatibility", &config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"base_url": server.URL + "/v1",
		"api_key":  "test",
	}}

	originalPayload := []byte(`{
		"model":"claude-opus-4.5",
		"max_tokens":64,
		"betas":["context-1m-2025-08-07"],
		"speed":"fast",
		"messages":[{"role":"user","content":"hi"}]
	}`)

	resp, err := executor.Execute(newOpenAICompatGinContext(map[string]string{
		"Anthropic-Beta":  "fast-mode-2026-02-01",
		"X-CPA-CLAUDE-1M": "true",
	}), auth, cliproxyexecutor.Request{
		Model:   "claude-opus-4.5",
		Payload: originalPayload,
	}, cliproxyexecutor.Options{
		SourceFormat:    sdktranslator.FromString("claude"),
		OriginalRequest: originalPayload,
		Stream:          false,
	})
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	if gotBetaHeader != "fast-mode-2026-02-01,context-1m-2025-08-07" {
		t.Fatalf("Anthropic-Beta = %q, want %q", gotBetaHeader, "fast-mode-2026-02-01,context-1m-2025-08-07")
	}
	if got := gjson.GetBytes(gotBody, "speed").String(); got != "fast" {
		t.Fatalf("speed = %q, want %q; body=%s", got, "fast", string(gotBody))
	}
	if gjson.GetBytes(gotBody, "betas").Exists() {
		t.Fatalf("did not expect betas in upstream body: %s", string(gotBody))
	}
	if len(resp.Payload) == 0 {
		t.Fatal("expected translated response payload")
	}
}

func TestOpenAICompatExecutorExecuteStream_PassthroughsAnthropicBetasAndSpeed(t *testing.T) {
	var gotBody []byte
	var gotBetaHeader string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		gotBody = body
		gotBetaHeader = r.Header.Get("Anthropic-Beta")
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"id\":\"resp_1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"claude-opus-4.5\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"}}]}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer server.Close()

	executor := NewOpenAICompatExecutor("openai-compatibility", &config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"base_url": server.URL + "/v1",
		"api_key":  "test",
	}}

	originalPayload := []byte(`{
		"model":"claude-opus-4.5",
		"max_tokens":64,
		"betas":["context-1m-2025-08-07"],
		"speed":"fast",
		"messages":[{"role":"user","content":"hi"}],
		"stream":true
	}`)

	result, err := executor.ExecuteStream(newOpenAICompatGinContext(map[string]string{
		"Anthropic-Beta":  "fast-mode-2026-02-01",
		"X-CPA-CLAUDE-1M": "true",
	}), auth, cliproxyexecutor.Request{
		Model:   "claude-opus-4.5",
		Payload: originalPayload,
	}, cliproxyexecutor.Options{
		SourceFormat:    sdktranslator.FromString("claude"),
		OriginalRequest: originalPayload,
		Stream:          true,
	})
	if err != nil {
		t.Fatalf("ExecuteStream error: %v", err)
	}
	for range result.Chunks {
	}

	if gotBetaHeader != "fast-mode-2026-02-01,context-1m-2025-08-07" {
		t.Fatalf("Anthropic-Beta = %q, want %q", gotBetaHeader, "fast-mode-2026-02-01,context-1m-2025-08-07")
	}
	if got := gjson.GetBytes(gotBody, "speed").String(); got != "fast" {
		t.Fatalf("speed = %q, want %q; body=%s", got, "fast", string(gotBody))
	}
	if gjson.GetBytes(gotBody, "betas").Exists() {
		t.Fatalf("did not expect betas in upstream body: %s", string(gotBody))
	}
}

func newOpenAICompatGinContext(headers map[string]string) context.Context {
	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	ginCtx.Request = req
	return context.WithValue(context.Background(), "gin", ginCtx)
}
