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

func TestOpenAICompatExecutorExecute_SingularityUsesCookieAuthAndAggregatesSSE(t *testing.T) {
	var gotPath string
	var gotBody []byte
	var gotCookieHeader string
	var gotBillingHeader string
	var gotAuthHeader string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotCookieHeader = r.Header.Get("X-Skywork-Cookies")
		gotBillingHeader = r.Header.Get("X-Skywork-Billing-Source")
		gotAuthHeader = r.Header.Get("Authorization")
		body, _ := io.ReadAll(r.Body)
		gotBody = body
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"id\":\"resp_1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"google/gemini-3-flash-preview\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"OK\"},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":1,\"total_tokens\":6}}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer server.Close()

	executor := NewOpenAICompatExecutor("singularity", &config.Config{})
	auth := &cliproxyauth.Auth{
		Provider: "singularity",
		Attributes: map[string]string{
			"base_url":     server.URL + "/chat/completions",
			"api_key":      "skybot-token",
			"compat_name":  "singularity",
			"provider_key": "singularity",
		},
	}

	resp, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "gemini-3-flash-preview",
		Payload: []byte(`{"model":"gemini-3-flash-preview","messages":[{"role":"user","content":"Reply with OK only."}],"max_tokens":16,"stream":false}`),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai"),
		Stream:       false,
	})
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}

	if gotPath != "/chat/completions" {
		t.Fatalf("path = %q, want %q", gotPath, "/chat/completions")
	}
	if gotCookieHeader != "token=skybot-token" {
		t.Fatalf("X-Skywork-Cookies = %q, want %q", gotCookieHeader, "token=skybot-token")
	}
	if gotBillingHeader != "" {
		t.Fatalf("X-Skywork-Billing-Source should be stripped, got %q", gotBillingHeader)
	}
	if strings.TrimSpace(gotAuthHeader) != "" {
		t.Fatalf("did not expect Authorization header, got %q", gotAuthHeader)
	}
	if got := gjson.GetBytes(gotBody, "stream").Bool(); !got {
		t.Fatalf("expected upstream stream=true, got body %s", string(gotBody))
	}
	if got := gjson.GetBytes(resp.Payload, "choices.0.message.content").String(); got != "OK" {
		t.Fatalf("choices.0.message.content = %q, want %q; payload=%s", got, "OK", string(resp.Payload))
	}
}

func TestOpenAICompatExecutorExecuteStream_SingularityUsesCookieAuth(t *testing.T) {
	var gotPath string
	var gotBody []byte
	var gotCookieHeader string
	var gotBillingHeader string
	var gotAuthHeader string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotCookieHeader = r.Header.Get("X-Skywork-Cookies")
		gotBillingHeader = r.Header.Get("X-Skywork-Billing-Source")
		gotAuthHeader = r.Header.Get("Authorization")
		body, _ := io.ReadAll(r.Body)
		gotBody = body
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"id\":\"resp_1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"google/gemini-3-flash-preview\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"OK\"}}]}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer server.Close()

	executor := NewOpenAICompatExecutor("singularity", &config.Config{})
	auth := &cliproxyauth.Auth{
		Provider: "singularity",
		Attributes: map[string]string{
			"base_url":     server.URL + "/chat/completions",
			"api_key":      "skybot-token",
			"compat_name":  "singularity",
			"provider_key": "singularity",
		},
	}

	result, err := executor.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "gemini-3-flash-preview",
		Payload: []byte(`{"model":"gemini-3-flash-preview","messages":[{"role":"user","content":"Reply with OK only."}],"max_tokens":16,"stream":true}`),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai"),
		Stream:       true,
	})
	if err != nil {
		t.Fatalf("ExecuteStream error: %v", err)
	}
	for range result.Chunks {
	}

	if gotPath != "/chat/completions" {
		t.Fatalf("path = %q, want %q", gotPath, "/chat/completions")
	}
	if gotCookieHeader != "token=skybot-token" {
		t.Fatalf("X-Skywork-Cookies = %q, want %q", gotCookieHeader, "token=skybot-token")
	}
	if gotBillingHeader != "" {
		t.Fatalf("X-Skywork-Billing-Source should be stripped, got %q", gotBillingHeader)
	}
	if strings.TrimSpace(gotAuthHeader) != "" {
		t.Fatalf("did not expect Authorization header, got %q", gotAuthHeader)
	}
	if got := gjson.GetBytes(gotBody, "stream").Bool(); !got {
		t.Fatalf("expected upstream stream=true, got body %s", string(gotBody))
	}
}

func TestAdaptSingularityPayload_ConvertsReasoningEffortToThinking(t *testing.T) {
	// For Claude models, reasoning_effort should be converted to thinking.budget_tokens.
	adapted := adaptSingularityPayload([]byte(`{"model":"claude-opus-4.6","reasoning_effort":"xhigh","stream":false}`), "claude-opus-4.6")

	if got := gjson.GetBytes(adapted, "thinking.type").String(); got != "enabled" {
		t.Fatalf("thinking.type = %q, want %q; body=%s", got, "enabled", string(adapted))
	}
	if got := gjson.GetBytes(adapted, "thinking.budget_tokens").Int(); got != 32768 {
		t.Fatalf("thinking.budget_tokens = %d, want %d; body=%s", got, 32768, string(adapted))
	}
	if gjson.GetBytes(adapted, "reasoning_effort").Exists() {
		t.Fatalf("did not expect reasoning_effort in adapted body: %s", string(adapted))
	}
	if got := gjson.GetBytes(adapted, "stream").Bool(); !got {
		t.Fatalf("expected stream=true in adapted body, got %s", string(adapted))
	}
}

func TestAdaptSingularityPayload_GPTKeepsReasoningEffort(t *testing.T) {
	// For GPT models, reasoning_effort should NOT be converted — kept as-is.
	adapted := adaptSingularityPayload([]byte(`{"model":"gpt-5.4","reasoning_effort":"xhigh","stream":false}`), "gpt-5.4")

	if gjson.GetBytes(adapted, "thinking.type").Exists() {
		t.Fatalf("did not expect thinking.type for GPT model: %s", string(adapted))
	}
	if got := gjson.GetBytes(adapted, "reasoning_effort").String(); got != "xhigh" {
		t.Fatalf("reasoning_effort = %q, want %q; body=%s", got, "xhigh", string(adapted))
	}
	if got := gjson.GetBytes(adapted, "stream").Bool(); !got {
		t.Fatalf("expected stream=true in adapted body, got %s", string(adapted))
	}
}

func TestOpenAICompatExecutorExecute_SingularityWrappedQuotaErrorReturnsFailure(t *testing.T) {
	wrappedErr := parseOpenAICompatWrappedErrorPayload([]byte(`{"code":429,"message":"skywork_router_limit","data":{"error_message":"Your current usage quota has reached its limit. Please try again later."},"trace_id":""}`))
	if wrappedErr == nil {
		t.Fatal("expected parseOpenAICompatWrappedErrorPayload to detect wrapped quota error")
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":429,"message":"skywork_router_limit","data":{"error_message":"Your current usage quota has reached its limit. Please try again later."},"trace_id":""}`))
	}))
	defer server.Close()

	executor := NewOpenAICompatExecutor("singularity", &config.Config{})
	auth := &cliproxyauth.Auth{
		Provider: "singularity",
		Attributes: map[string]string{
			"base_url":     server.URL + "/chat/completions",
			"api_key":      "skybot-token",
			"compat_name":  "singularity",
			"provider_key": "singularity",
		},
	}

	resp, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "gemini-3-flash-preview",
		Payload: []byte(`{"model":"gemini-3-flash-preview","messages":[{"role":"user","content":"Reply with OK only."}],"max_tokens":16,"stream":false}`),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai"),
		Stream:       false,
	})
	if err == nil {
		t.Fatalf("expected wrapped quota error, got nil; payload=%s", string(resp.Payload))
	}
	if !strings.Contains(err.Error(), "quota") {
		t.Fatalf("expected quota error message, got %v", err)
	}
	if se, ok := err.(interface{ StatusCode() int }); !ok || se.StatusCode() != http.StatusTooManyRequests {
		t.Fatalf("expected TooManyRequests status, got %#v", err)
	}
}

func TestOpenAICompatExecutorExecuteStream_SingularityJSONWrappedErrorReturnsFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":400000,"code_msg":"Authorization验证错误"}`))
	}))
	defer server.Close()

	executor := NewOpenAICompatExecutor("singularity", &config.Config{})
	auth := &cliproxyauth.Auth{
		Provider: "singularity",
		Attributes: map[string]string{
			"base_url":     server.URL + "/chat/completions",
			"api_key":      "skybot-token",
			"compat_name":  "singularity",
			"provider_key": "singularity",
		},
	}

	_, err := executor.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "gemini-3-flash-preview",
		Payload: []byte(`{"model":"gemini-3-flash-preview","messages":[{"role":"user","content":"Reply with OK only."}],"max_tokens":16,"stream":true}`),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai"),
		Stream:       true,
	})
	if err == nil {
		t.Fatal("expected wrapped auth error, got nil")
	}
	if !strings.Contains(err.Error(), "Authorization") && !strings.Contains(err.Error(), "验证") {
		t.Fatalf("expected auth error message, got %v", err)
	}
	if se, ok := err.(interface{ StatusCode() int }); !ok || se.StatusCode() != http.StatusUnauthorized {
		t.Fatalf("expected Unauthorized status, got %#v", err)
	}
}

func TestOpenAICompatExecutorExecute_SingularityCompactUnsupported(t *testing.T) {
	executor := NewOpenAICompatExecutor("singularity", &config.Config{})
	auth := &cliproxyauth.Auth{
		Provider: "singularity",
		Attributes: map[string]string{
			"base_url":     "https://example.com/chat/completions",
			"api_key":      "skybot-token",
			"compat_name":  "singularity",
			"provider_key": "singularity",
		},
	}

	_, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "gemini-3-flash-preview",
		Payload: []byte(`{"model":"gemini-3-flash-preview","input":[{"role":"user","content":"Reply with OK only."}]}`),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai-response"),
		Alt:          "responses/compact",
		Stream:       false,
	})
	if err == nil {
		t.Fatal("expected compact unsupported error, got nil")
	}
	if !strings.Contains(err.Error(), "responses/compact") {
		t.Fatalf("expected responses/compact error message, got %v", err)
	}
	if se, ok := err.(interface{ StatusCode() int }); !ok || se.StatusCode() != http.StatusBadRequest {
		t.Fatalf("expected BadRequest status, got %#v", err)
	}
}
