package auth

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

// mockFallbackExecutor simulates an executor that fails for specific models.
type mockFallbackExecutor struct {
	failModels map[string]bool    // models that should fail
	calls      []string           // records which models were attempted
	provider   string
}

func (m *mockFallbackExecutor) Identifier() string { return m.provider }
func (m *mockFallbackExecutor) PrepareRequest(_ *http.Request, _ *Auth) error { return nil }
func (m *mockFallbackExecutor) HttpRequest(_ context.Context, _ *Auth, _ *http.Request) (*http.Response, error) {
	return nil, nil
}
func (m *mockFallbackExecutor) Refresh(_ context.Context, _ *Auth) (*Auth, error) { return nil, nil }
func (m *mockFallbackExecutor) CountTokens(_ context.Context, _ *Auth, req cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}

func (m *mockFallbackExecutor) Execute(_ context.Context, _ *Auth, req cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	m.calls = append(m.calls, req.Model)
	if m.failModels[req.Model] {
		return cliproxyexecutor.Response{}, &mockStatusError{code: 504, msg: "timeout for " + req.Model}
	}
	return cliproxyexecutor.Response{
		Payload: []byte(fmt.Sprintf(`{"model":"%s","choices":[{"message":{"content":"ok"}}]}`, req.Model)),
	}, nil
}

func (m *mockFallbackExecutor) ExecuteStream(_ context.Context, _ *Auth, req cliproxyexecutor.Request, _ cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	m.calls = append(m.calls, req.Model)
	if m.failModels[req.Model] {
		return nil, &mockStatusError{code: 504, msg: "timeout for " + req.Model}
	}
	ch := make(chan cliproxyexecutor.StreamChunk, 1)
	ch <- cliproxyexecutor.StreamChunk{Payload: []byte(fmt.Sprintf(`data: {"model":"%s"}`, req.Model))}
	close(ch)
	return &cliproxyexecutor.StreamResult{Chunks: ch}, nil
}

type mockStatusError struct {
	code int
	msg  string
}

func (e *mockStatusError) Error() string            { return e.msg }
func (e *mockStatusError) StatusCode() int           { return e.code }
func (e *mockStatusError) RetryAfter() *time.Duration { return nil }

func TestSkyworkFallback_ClaudeFailsToGPT(t *testing.T) {
	// Scenario: claude-opus-4.6 fails with 504, should fall back to gpt-5.4.
	executor := &mockFallbackExecutor{
		failModels: map[string]bool{"claude-opus-4.6": true},
		provider:   "skywork",
	}

	chain := PlanSkyworkFallbackChain("claude-opus-4.6",
		[]string{"claude-opus-4.6", "gpt-5.4", "claude-sonnet-4.6"}, true)

	// Simulate the conductor's model pool iteration.
	var successModel string
	for _, model := range chain {
		resp, err := executor.Execute(context.Background(), nil,
			cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{})
		if err == nil {
			successModel = model
			_ = resp
			break
		}
	}

	if successModel != "gpt-5.4" {
		t.Fatalf("expected fallback to gpt-5.4, got %s", successModel)
	}
	if len(executor.calls) != 2 {
		t.Fatalf("expected 2 calls (claude-opus-4.6 fail, gpt-5.4 success), got %d: %v",
			len(executor.calls), executor.calls)
	}
	if executor.calls[0] != "claude-opus-4.6" || executor.calls[1] != "gpt-5.4" {
		t.Errorf("expected calls [claude-opus-4.6, gpt-5.4], got %v", executor.calls)
	}
}

func TestSkyworkFallback_GPTFailsToClaude(t *testing.T) {
	// Scenario: gpt-5.4 and gpt-5.3-codex both fail, should fall back to claude-opus-4.6.
	executor := &mockFallbackExecutor{
		failModels: map[string]bool{"gpt-5.4": true, "gpt-5.3-codex": true, "gpt-5.2": true},
		provider:   "skywork",
	}

	chain := PlanSkyworkFallbackChain("gpt-5.4",
		[]string{"claude-opus-4.6", "gpt-5.4", "claude-sonnet-4.6", "gpt-5.3-codex", "gpt-5.2"}, false)

	var successModel string
	for _, model := range chain {
		_, err := executor.Execute(context.Background(), nil,
			cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{})
		if err == nil {
			successModel = model
			break
		}
	}

	if successModel != "claude-opus-4.6" {
		t.Fatalf("expected fallback to claude-opus-4.6, got %s", successModel)
	}
	// Light request: gpt-5.4 → gpt-5.3-codex → gpt-5.2 → claude-opus-4.6
	if len(executor.calls) != 4 {
		t.Fatalf("expected 4 calls, got %d: %v", len(executor.calls), executor.calls)
	}
}

func TestSkyworkFallback_AllModelsFail(t *testing.T) {
	// Scenario: all models fail. Should exhaust chain and return error.
	executor := &mockFallbackExecutor{
		failModels: map[string]bool{
			"claude-opus-4.6":   true,
			"gpt-5.4":           true,
			"claude-sonnet-4.6": true,
		},
		provider: "skywork",
	}

	chain := PlanSkyworkFallbackChain("claude-opus-4.6",
		[]string{"claude-opus-4.6", "gpt-5.4", "claude-sonnet-4.6"}, true)

	var lastErr error
	for _, model := range chain {
		_, err := executor.Execute(context.Background(), nil,
			cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{})
		if err == nil {
			t.Fatal("expected all models to fail")
		}
		lastErr = err
	}

	if lastErr == nil {
		t.Fatal("expected error after all models fail")
	}
	if len(executor.calls) != 3 {
		t.Fatalf("expected 3 calls (all failed), got %d: %v", len(executor.calls), executor.calls)
	}
}

func TestSkyworkFallback_CascadeDownToWeakest(t *testing.T) {
	// Scenario: everything except gpt-5.2 fails. Should cascade all the way down.
	executor := &mockFallbackExecutor{
		failModels: map[string]bool{
			"claude-opus-4.6":   true,
			"gpt-5.4":           true,
			"claude-sonnet-4.6": true,
			"gpt-5.3-codex":     true,
			"claude-opus-4.5":   true,
			"claude-sonnet-4.5": true,
		},
		provider: "skywork",
	}

	available := []string{
		"claude-opus-4.6", "gpt-5.4", "claude-sonnet-4.6", "gpt-5.3-codex",
		"claude-opus-4.5", "claude-sonnet-4.5", "gpt-5.2",
	}
	chain := PlanSkyworkFallbackChain("claude-opus-4.6", available, true)

	var successModel string
	for _, model := range chain {
		_, err := executor.Execute(context.Background(), nil,
			cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{})
		if err == nil {
			successModel = model
			break
		}
	}

	if successModel != "gpt-5.2" {
		t.Fatalf("expected fallback all the way to gpt-5.2, got %s", successModel)
	}
	if len(executor.calls) != 7 {
		t.Fatalf("expected 7 calls, got %d: %v", len(executor.calls), executor.calls)
	}
}

func TestSkyworkFallback_ReasoningEffortMappedCorrectly(t *testing.T) {
	// Verify reasoning effort mapping works in the fallback chain context.
	// Original: claude-opus-4.6 with effort "high" → fallback to gpt-5.4 should become "xhigh".
	originalEffort := "high"
	requestedModel := "claude-opus-4.6"

	chain := PlanSkyworkFallbackChain(requestedModel,
		[]string{"claude-opus-4.6", "gpt-5.4", "claude-sonnet-4.6"}, true)

	for _, execModel := range chain {
		fromFamily := SkyworkModelFamily(requestedModel)
		toFamily := SkyworkModelFamily(execModel)
		mapped := MapCrossFamilyReasoningEffort(originalEffort, fromFamily, toFamily)

		switch execModel {
		case "claude-opus-4.6":
			if mapped != "high" {
				t.Errorf("%s: expected effort 'high' (same family), got '%s'", execModel, mapped)
			}
		case "gpt-5.4":
			if mapped != "xhigh" {
				t.Errorf("%s: expected effort 'xhigh' (claude high → gpt xhigh), got '%s'", execModel, mapped)
			}
		case "claude-sonnet-4.6":
			if mapped != "high" {
				t.Errorf("%s: expected effort 'high' (same family), got '%s'", execModel, mapped)
			}
		}
	}
}

func TestSkyworkFallback_StreamingCascade(t *testing.T) {
	// Verify fallback works in streaming mode too.
	executor := &mockFallbackExecutor{
		failModels: map[string]bool{"claude-opus-4.6": true},
		provider:   "skywork",
	}

	chain := PlanSkyworkFallbackChain("claude-opus-4.6",
		[]string{"claude-opus-4.6", "gpt-5.4"}, false)

	var successModel string
	for _, model := range chain {
		result, err := executor.ExecuteStream(context.Background(), nil,
			cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{})
		if err == nil && result != nil {
			// Drain to verify it works.
			for chunk := range result.Chunks {
				if strings.Contains(string(chunk.Payload), model) {
					successModel = model
				}
			}
			break
		}
	}

	if successModel != "gpt-5.4" {
		t.Fatalf("expected streaming fallback to gpt-5.4, got %s", successModel)
	}
}

func TestSkyworkFallback_GeminiNotIncluded(t *testing.T) {
	// Gemini models should NOT appear in the fallback chain even if available.
	available := []string{"claude-opus-4.6", "gpt-5.4", "gemini-3-flash-preview", "gemini-3.1-pro-preview"}
	chain := PlanSkyworkFallbackChain("claude-opus-4.6", available, false)

	for _, m := range chain {
		if strings.HasPrefix(m, "gemini") {
			t.Errorf("gemini model should not be in fallback chain: %s", m)
		}
	}
	// Should only have claude-opus-4.6 and gpt-5.4.
	if len(chain) != 2 {
		t.Fatalf("expected 2 models, got %d: %v", len(chain), chain)
	}
}
