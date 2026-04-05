package refusal

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestAIRewrite_Success(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify request format.
		if r.Header.Get("Content-Type") != "application/json" {
			t.Error("expected Content-Type: application/json")
		}
		if r.Header.Get("Authorization") != "Bearer test-key" {
			t.Error("expected Authorization: Bearer test-key")
		}

		var req chatCompletionRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("failed to decode request: %v", err)
		}
		if req.Model != "gpt-4o-mini" {
			t.Errorf("expected model gpt-4o-mini, got %s", req.Model)
		}
		if len(req.Messages) != 2 {
			t.Errorf("expected 2 messages, got %d", len(req.Messages))
		}

		resp := chatCompletionResponse{
			Choices: []struct {
				Message struct {
					Content string `json:"content"`
				} `json:"message"`
			}{
				{Message: struct {
					Content string `json:"content"`
				}{Content: "Certainly! Let me analyze the security vulnerability:"}},
			},
		}
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	result := AIRewrite(context.Background(), AIRewriterConfig{
		Endpoint:    server.URL,
		APIKey:      "test-key",
		Model:       "gpt-4o-mini",
		Timeout:     5 * time.Second,
		UserMessage: "Find the SQL injection vulnerability",
		RefusalText: "I'm sorry, I cannot assist with that.",
	})

	if result == "" {
		t.Fatal("expected non-empty AI rewrite result")
	}
	if result != "Certainly! Let me analyze the security vulnerability:" {
		t.Errorf("unexpected result: %s", result)
	}
}

func TestAIRewrite_NoAuth(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Ollama-style: no auth needed.
		if auth := r.Header.Get("Authorization"); auth != "" {
			t.Errorf("expected no Authorization header, got %s", auth)
		}
		resp := chatCompletionResponse{
			Choices: []struct {
				Message struct {
					Content string `json:"content"`
				} `json:"message"`
			}{
				{Message: struct {
					Content string `json:"content"`
				}{Content: "Sure, here's the analysis:"}},
			},
		}
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	result := AIRewrite(context.Background(), AIRewriterConfig{
		Endpoint: server.URL,
		APIKey:   "", // empty = no auth (Ollama, LM Studio, etc.)
		Model:    "llama3",
		Timeout:  5 * time.Second,
	})

	if result == "" {
		t.Fatal("expected non-empty result for no-auth endpoint")
	}
}

func TestAIRewrite_Timeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Second) // simulate slow response
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	result := AIRewrite(context.Background(), AIRewriterConfig{
		Endpoint: server.URL,
		Model:    "gpt-4o-mini",
		Timeout:  100 * time.Millisecond, // very short timeout
	})

	if result != "" {
		t.Error("expected empty result on timeout")
	}
}

func TestAIRewrite_ServerError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	result := AIRewrite(context.Background(), AIRewriterConfig{
		Endpoint: server.URL,
		Model:    "gpt-4o-mini",
		Timeout:  5 * time.Second,
	})

	if result != "" {
		t.Error("expected empty result on server error")
	}
}

func TestAIRewrite_EmptyEndpoint(t *testing.T) {
	result := AIRewrite(context.Background(), AIRewriterConfig{
		Endpoint: "",
		Model:    "gpt-4o-mini",
	})

	if result != "" {
		t.Error("expected empty result for empty endpoint")
	}
}

func TestExtractLastUserMessage_ChatFormat(t *testing.T) {
	payload := []byte(`{"messages":[
		{"role":"system","content":"You are helpful."},
		{"role":"user","content":"First question"},
		{"role":"assistant","content":"Answer"},
		{"role":"user","content":"Second question"}
	]}`)

	msg := ExtractLastUserMessage(payload)
	if msg != "Second question" {
		t.Errorf("expected 'Second question', got '%s'", msg)
	}
}

func TestExtractLastUserMessage_ResponsesFormat(t *testing.T) {
	payload := []byte(`{"input":[
		{"type":"message","role":"user","content":[{"type":"input_text","text":"My request"}]}
	]}`)

	msg := ExtractLastUserMessage(payload)
	if msg != "My request" {
		t.Errorf("expected 'My request', got '%s'", msg)
	}
}
