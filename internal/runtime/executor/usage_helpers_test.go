package executor

import (
	"testing"

	coreusage "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/usage"
)

func TestParseOpenAIUsageChatCompletions(t *testing.T) {
	data := []byte(`{"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3,"prompt_tokens_details":{"cached_tokens":4},"completion_tokens_details":{"reasoning_tokens":5}}}`)
	detail := parseOpenAIUsage(data)
	if detail.InputTokens != 1 {
		t.Fatalf("input tokens = %d, want %d", detail.InputTokens, 1)
	}
	if detail.OutputTokens != 2 {
		t.Fatalf("output tokens = %d, want %d", detail.OutputTokens, 2)
	}
	if detail.TotalTokens != 3 {
		t.Fatalf("total tokens = %d, want %d", detail.TotalTokens, 3)
	}
	if detail.CachedTokens != 4 {
		t.Fatalf("cached tokens = %d, want %d", detail.CachedTokens, 4)
	}
	if detail.ReasoningTokens != 5 {
		t.Fatalf("reasoning tokens = %d, want %d", detail.ReasoningTokens, 5)
	}
}

func TestParseOpenAIUsageResponses(t *testing.T) {
	data := []byte(`{"usage":{"input_tokens":10,"output_tokens":20,"total_tokens":30,"input_tokens_details":{"cached_tokens":7},"output_tokens_details":{"reasoning_tokens":9}}}`)
	detail := parseOpenAIUsage(data)
	if detail.InputTokens != 10 {
		t.Fatalf("input tokens = %d, want %d", detail.InputTokens, 10)
	}
	if detail.OutputTokens != 20 {
		t.Fatalf("output tokens = %d, want %d", detail.OutputTokens, 20)
	}
	if detail.TotalTokens != 30 {
		t.Fatalf("total tokens = %d, want %d", detail.TotalTokens, 30)
	}
	if detail.CachedTokens != 7 {
		t.Fatalf("cached tokens = %d, want %d", detail.CachedTokens, 7)
	}
	if detail.ReasoningTokens != 9 {
		t.Fatalf("reasoning tokens = %d, want %d", detail.ReasoningTokens, 9)
	}
}

func TestParseClaudeStreamUsage_TopLevelUsage(t *testing.T) {
	line := []byte(`data: {"type":"message_delta","usage":{"input_tokens":302,"output_tokens":311,"cache_read_input_tokens":151396}}`)
	detail, ok := parseClaudeStreamUsage(line)
	if !ok {
		t.Fatal("expected top-level Claude stream usage to be parsed")
	}
	if detail.InputTokens != 302 {
		t.Fatalf("input tokens = %d, want %d", detail.InputTokens, 302)
	}
	if detail.OutputTokens != 311 {
		t.Fatalf("output tokens = %d, want %d", detail.OutputTokens, 311)
	}
	if detail.CachedTokens != 151396 {
		t.Fatalf("cached tokens = %d, want %d", detail.CachedTokens, 151396)
	}
	if detail.TotalTokens != 613 {
		t.Fatalf("total tokens = %d, want %d", detail.TotalTokens, 613)
	}
}

func TestParseClaudeStreamUsage_MessageUsage(t *testing.T) {
	line := []byte(`data: {"type":"message_start","message":{"id":"msg_1","usage":{"input_tokens":302,"cache_read_input_tokens":150087}}}`)
	detail, ok := parseClaudeStreamUsage(line)
	if !ok {
		t.Fatal("expected nested Claude message.usage to be parsed")
	}
	if detail.InputTokens != 302 {
		t.Fatalf("input tokens = %d, want %d", detail.InputTokens, 302)
	}
	if detail.OutputTokens != 0 {
		t.Fatalf("output tokens = %d, want %d", detail.OutputTokens, 0)
	}
	if detail.CachedTokens != 150087 {
		t.Fatalf("cached tokens = %d, want %d", detail.CachedTokens, 150087)
	}
	if detail.TotalTokens != 302 {
		t.Fatalf("total tokens = %d, want %d", detail.TotalTokens, 302)
	}
}

func TestPreferRicherUsageDetail(t *testing.T) {
	current := coreusage.Detail{InputTokens: 302, CachedTokens: 150087, TotalTokens: 302}
	candidate := coreusage.Detail{OutputTokens: 311, TotalTokens: 311}
	got := preferRicherUsageDetail(current, candidate)
	if got.InputTokens != 302 {
		t.Fatalf("InputTokens = %d, want 302", got.InputTokens)
	}
	if got.OutputTokens != 311 {
		t.Fatalf("OutputTokens = %d, want 311", got.OutputTokens)
	}
	if got.CachedTokens != 150087 {
		t.Fatalf("CachedTokens = %d, want 150087", got.CachedTokens)
	}
	if got.TotalTokens != 613 {
		t.Fatalf("TotalTokens = %d, want 613", got.TotalTokens)
	}
}
