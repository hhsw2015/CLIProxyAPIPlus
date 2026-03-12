package executor

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	kiroclaude "github.com/router-for-me/CLIProxyAPI/v6/internal/translator/kiro/claude"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
)

func TestOpenAICompatExecutorExecuteStream_InterceptsPureWebSearchForSkywork(t *testing.T) {
	var upstreamCalls int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls++
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"id\":\"resp_1\",\"model\":\"claude-opus-4.5\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"search complete\"}}]}\n\n"))
		_, _ = w.Write([]byte("data: {\"id\":\"resp_1\",\"model\":\"claude-opus-4.5\",\"choices\":[],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":3},\"finish_reason\":\"stop\"}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer upstream.Close()

	originalSearch := openAICompatWebSearchFunc
	openAICompatWebSearchFunc = func(ctx context.Context, client *http.Client, query string) (*kiroclaude.WebSearchResults, error) {
		return &kiroclaude.WebSearchResults{
			Results: []kiroclaude.WebSearchResult{
				{
					Title: "Search Result One",
					URL:   "https://example.com/one",
				},
				{
					Title: "Search Result Two",
					URL:   "https://example.com/two",
				},
			},
		}, nil
	}
	t.Cleanup(func() { openAICompatWebSearchFunc = originalSearch })

	executor := NewOpenAICompatExecutor("skywork", &config.Config{
		OpenAICompatibility: []config.OpenAICompatibility{{
			Name:    "skywork",
			BaseURL: upstream.URL + "/v1",
			Models:  []config.OpenAICompatibilityModel{{Name: "claude-opus-4.5"}},
		}},
	})
	auth := &cliproxyauth.Auth{
		ID:       "skywork-auth",
		Provider: "skywork",
		Attributes: map[string]string{
			"base_url":     upstream.URL + "/v1",
			"compat_name":  "skywork",
			"provider_key": "skywork",
		},
	}
	payload := []byte(`{"model":"skywork/claude-opus-4.5","max_tokens":128,"stream":true,"tools":[{"type":"web_search_20250305","name":"web_search"}],"messages":[{"role":"user","content":[{"type":"text","text":"Perform a web search for the query: stripe confirm automation"}]}]}`)

	result, err := executor.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "skywork/claude-opus-4.5",
		Payload: payload,
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("claude"),
		Stream:       true,
	})
	if err != nil {
		t.Fatalf("ExecuteStream error: %v", err)
	}

	var joined strings.Builder
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("unexpected chunk error: %v", chunk.Err)
		}
		joined.Write(chunk.Payload)
	}

	got := joined.String()
	if upstreamCalls != 1 {
		t.Fatalf("expected one upstream call after injecting search results, got %d", upstreamCalls)
	}
	if !strings.Contains(got, `"type":"server_tool_use"`) {
		t.Fatalf("expected server_tool_use event, got %q", got)
	}
	if !strings.Contains(got, `"type":"web_search_tool_result"`) {
		t.Fatalf("expected web_search_tool_result event, got %q", got)
	}
	if !strings.Contains(got, `https://example.com/one`) || !strings.Contains(got, `Search Result Two`) {
		t.Fatalf("expected injected search results, got %q", got)
	}
	if !strings.Contains(got, "search complete") {
		t.Fatalf("expected final model answer, got %q", got)
	}
	if !strings.Contains(got, `"type":"message_stop"`) {
		t.Fatalf("expected message_stop event, got %q", got)
	}
}

func TestOpenAICompatExecutorExecuteStream_InterceptedWebSearchSupportsRefinedSearchLoop(t *testing.T) {
	var upstreamCalls int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls++
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		switch upstreamCalls {
		case 1:
			_, _ = w.Write([]byte("data: {\"id\":\"resp_1\",\"model\":\"claude-opus-4.5\",\"choices\":[{\"index\":1,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_web_1\",\"type\":\"function\",\"function\":{\"name\":\"web_search\",\"arguments\":\"\"}}]}}]}\n\n"))
			_, _ = w.Write([]byte("data: {\"id\":\"resp_1\",\"model\":\"claude-opus-4.5\",\"choices\":[{\"index\":1,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"\",\"type\":\"function\",\"function\":{\"name\":\"\",\"arguments\":\"{\\\"query\\\":\\\"second query\\\"}\"}}]}}]}\n\n"))
			_, _ = w.Write([]byte("data: {\"id\":\"resp_1\",\"model\":\"claude-opus-4.5\",\"choices\":[],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":3},\"finish_reason\":\"tool_use\"}\n\n"))
			_, _ = w.Write([]byte("data: [DONE]\n\n"))
		default:
			_, _ = w.Write([]byte("data: {\"id\":\"resp_2\",\"model\":\"claude-opus-4.5\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"final answer\"}}]}\n\n"))
			_, _ = w.Write([]byte("data: {\"id\":\"resp_2\",\"model\":\"claude-opus-4.5\",\"choices\":[],\"usage\":{\"prompt_tokens\":12,\"completion_tokens\":4},\"finish_reason\":\"stop\"}\n\n"))
			_, _ = w.Write([]byte("data: [DONE]\n\n"))
		}
	}))
	defer upstream.Close()

	searchQueries := make([]string, 0, 2)
	originalSearch := openAICompatWebSearchFunc
	openAICompatWebSearchFunc = func(ctx context.Context, client *http.Client, query string) (*kiroclaude.WebSearchResults, error) {
		searchQueries = append(searchQueries, query)
		return &kiroclaude.WebSearchResults{
			Results: []kiroclaude.WebSearchResult{{
				Title: "Result for " + query,
				URL:   "https://example.com/" + strings.ReplaceAll(query, " ", "-"),
			}},
		}, nil
	}
	t.Cleanup(func() { openAICompatWebSearchFunc = originalSearch })

	executor := NewOpenAICompatExecutor("skywork", &config.Config{
		OpenAICompatibility: []config.OpenAICompatibility{{
			Name:    "skywork",
			BaseURL: upstream.URL + "/v1",
			Models:  []config.OpenAICompatibilityModel{{Name: "claude-opus-4.5"}},
		}},
	})
	auth := &cliproxyauth.Auth{
		ID:       "skywork-auth",
		Provider: "skywork",
		Attributes: map[string]string{
			"base_url":     upstream.URL + "/v1",
			"compat_name":  "skywork",
			"provider_key": "skywork",
		},
	}
	payload := []byte(`{"model":"skywork/claude-opus-4.5","max_tokens":128,"stream":true,"tools":[{"type":"web_search_20250305","name":"web_search"}],"messages":[{"role":"user","content":[{"type":"text","text":"Perform a web search for the query: first query"}]}]}`)

	result, err := executor.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "skywork/claude-opus-4.5",
		Payload: payload,
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("claude"),
		Stream:       true,
	})
	if err != nil {
		t.Fatalf("ExecuteStream error: %v", err)
	}

	var joined strings.Builder
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("unexpected chunk error: %v", chunk.Err)
		}
		joined.Write(chunk.Payload)
	}

	got := joined.String()
	if upstreamCalls != 2 {
		t.Fatalf("expected 2 upstream calls (re-search loop), got %d", upstreamCalls)
	}
	if len(searchQueries) != 2 || searchQueries[0] != "first query" || searchQueries[1] != "second query" {
		t.Fatalf("unexpected search queries: %v", searchQueries)
	}
	if strings.Count(got, `"type":"server_tool_use"`) != 2 {
		t.Fatalf("expected two server_tool_use events, got %q", got)
	}
	if strings.Contains(got, `"type":"tool_use"`) && strings.Contains(got, `"name":"web_search"`) {
		t.Fatalf("expected intercepted web_search tool_use to be filtered, got %q", got)
	}
	if !strings.Contains(got, "Result for first query") || !strings.Contains(got, "Result for second query") {
		t.Fatalf("expected both search result sets in output, got %q", got)
	}
	if !strings.Contains(got, "final answer") {
		t.Fatalf("expected final model answer in output, got %q", got)
	}
}
