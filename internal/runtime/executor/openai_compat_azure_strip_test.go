package executor

import (
	"testing"

	"github.com/tidwall/gjson"
)

func TestIsAzureOpenAIBaseURL(t *testing.T) {
	cases := []struct {
		name string
		url  string
		want bool
	}{
		{"empty", "", false},
		{"openai.com", "https://api.openai.com/v1", false},
		{"third-party", "https://api.taijiaicloud.com/v1", false},
		{"azure openai deploy", "https://gpt-scu-sky-03.openai.azure.com/openai/deployments/gpt-5.6-sol", true},
		{"azure openai bare", "https://myres.openai.azure.com", true},
		{"cognitiveservices with /openai", "https://xingm.cognitiveservices.azure.com/openai/deployments/gpt-5", true},
		{"cognitiveservices without /openai", "https://cog.cognitiveservices.azure.com/vision/v1", false},
		{"uppercase mixed", "https://GPT-SCU-SKY-03.OpenAI.Azure.Com/openai/deployments/gpt-5.6-sol", true},
	}
	for _, tc := range cases {
		if got := isAzureOpenAIBaseURL(tc.url); got != tc.want {
			t.Errorf("%s: isAzureOpenAIBaseURL(%q) = %v, want %v", tc.name, tc.url, got, tc.want)
		}
	}
}

func TestStripReasoningEffortIfToolsPresent(t *testing.T) {
	t.Run("no tools = no-op", func(t *testing.T) {
		in := []byte(`{"model":"gpt-5.6-sol","reasoning_effort":"xhigh","messages":[]}`)
		out := stripReasoningEffortIfToolsPresent(in, "")
		if !gjson.GetBytes(out, "reasoning_effort").Exists() {
			t.Fatal("reasoning_effort was stripped despite no tools present")
		}
	})

	t.Run("tools + reasoning_effort strips reasoning_effort", func(t *testing.T) {
		in := []byte(`{"model":"gpt-5.6-sol","reasoning_effort":"xhigh","tools":[{"type":"function","function":{"name":"f"}}],"messages":[]}`)
		out := stripReasoningEffortIfToolsPresent(in, "")
		if gjson.GetBytes(out, "reasoning_effort").Exists() {
			t.Errorf("reasoning_effort should have been removed, got: %s", out)
		}
		if !gjson.GetBytes(out, "tools").Exists() {
			t.Error("tools was incorrectly removed")
		}
	})

	t.Run("tools + reasoning.effort object with only effort strips whole reasoning", func(t *testing.T) {
		in := []byte(`{"model":"gpt-5.6-sol","reasoning":{"effort":"high"},"tools":[{"type":"function"}]}`)
		out := stripReasoningEffortIfToolsPresent(in, "")
		if gjson.GetBytes(out, "reasoning").Exists() {
			t.Errorf("reasoning should be removed when it only contained effort, got: %s", out)
		}
	})

	t.Run("tools + reasoning object with other fields keeps siblings", func(t *testing.T) {
		in := []byte(`{"tools":[{"type":"function"}],"reasoning":{"effort":"high","summary":"auto"}}`)
		out := stripReasoningEffortIfToolsPresent(in, "")
		if gjson.GetBytes(out, "reasoning.effort").Exists() {
			t.Error("reasoning.effort should have been removed")
		}
		if !gjson.GetBytes(out, "reasoning.summary").Exists() {
			t.Errorf("reasoning.summary should be preserved, got: %s", out)
		}
	})

	t.Run("tools without any reasoning field = no-op", func(t *testing.T) {
		in := []byte(`{"model":"gpt-5.6-sol","tools":[{"type":"function"}],"messages":[]}`)
		before := string(in)
		out := stripReasoningEffortIfToolsPresent(in, "")
		if string(out) != before {
			t.Errorf("unexpected mutation: %s -> %s", before, out)
		}
	})

	// gpt-6 must force reasoning_effort to "none" (deleting it is not enough:
	// gpt-6 defaults to a non-none effort upstream and rejects tools then).
	t.Run("gpt-6 via base URL forces reasoning_effort=none (had explicit effort)", func(t *testing.T) {
		in := []byte(`{"reasoning_effort":"high","tools":[{"type":"function"}]}`)
		out := stripReasoningEffortIfToolsPresent(in, "https://x.openai.azure.com/openai/deployments/gpt-6-astra/chat/completions")
		if v := gjson.GetBytes(out, "reasoning_effort"); v.String() != "none" {
			t.Errorf("reasoning_effort = %q, want \"none\"; body: %s", v.String(), out)
		}
	})

	t.Run("gpt-6 forces reasoning_effort=none when field absent", func(t *testing.T) {
		in := []byte(`{"model":"gpt-6-astra","tools":[{"type":"function"}],"messages":[]}`)
		out := stripReasoningEffortIfToolsPresent(in, "")
		if v := gjson.GetBytes(out, "reasoning_effort"); v.String() != "none" {
			t.Errorf("reasoning_effort = %q, want \"none\"; body: %s", v.String(), out)
		}
	})

	t.Run("gpt-6 no tools = no-op even with effort", func(t *testing.T) {
		in := []byte(`{"model":"gpt-6-astra","reasoning_effort":"high","messages":[]}`)
		out := stripReasoningEffortIfToolsPresent(in, "https://x.openai.azure.com/openai/deployments/gpt-6-astra/chat/completions")
		if v := gjson.GetBytes(out, "reasoning_effort"); v.String() != "high" {
			t.Errorf("reasoning_effort should be untouched without tools, got %q", v.String())
		}
	})

	t.Run("codex internal passthrough stripped from responses input", func(t *testing.T) {
		in := []byte(`{"model":"gpt-6-astra","input":[{"role":"user","content":"hi"},{"role":"assistant","content":"ok","internal_chat_message_metadata_passthrough":{"content_item_kinds":["text"]}}]}`)
		out := stripCodexInternalResponsesFields(in)
		if gjson.GetBytes(out, "input.1.internal_chat_message_metadata_passthrough").Exists() {
			t.Errorf("passthrough field should be stripped, got: %s", out)
		}
		if gjson.GetBytes(out, "input.0.content").String() != "hi" || gjson.GetBytes(out, "input.1.content").String() != "ok" {
			t.Errorf("conversation content must be preserved, got: %s", out)
		}
	})

	t.Run("no input array = no-op for codex strip", func(t *testing.T) {
		in := []byte(`{"model":"x","messages":[{"role":"user","content":"hi"}]}`)
		before := string(in)
		if got := string(stripCodexInternalResponsesFields(in)); got != before {
			t.Errorf("unexpected mutation: %s", got)
		}
	})

	t.Run("empty payload", func(t *testing.T) {
		if got := stripReasoningEffortIfToolsPresent(nil, ""); got != nil {
			t.Errorf("nil input should stay nil, got %v", got)
		}
		if got := stripReasoningEffortIfToolsPresent([]byte{}, ""); len(got) != 0 {
			t.Errorf("empty input should stay empty, got %v", got)
		}
	})
}

func TestRenameMaxTokensForReasoningModels(t *testing.T) {
	t.Run("renames max_tokens to max_completion_tokens", func(t *testing.T) {
		in := []byte(`{"max_tokens":100,"model":"gpt-5.6-sol"}`)
		out := renameMaxTokensForReasoningModels(in)
		if gjson.GetBytes(out, "max_tokens").Exists() {
			t.Errorf("max_tokens should be removed, got: %s", out)
		}
		if v := gjson.GetBytes(out, "max_completion_tokens"); !v.Exists() || v.Int() != 100 {
			t.Errorf("max_completion_tokens = %v, want 100", v)
		}
	})

	t.Run("keeps max_completion_tokens if already present and drops legacy", func(t *testing.T) {
		in := []byte(`{"max_tokens":50,"max_completion_tokens":200}`)
		out := renameMaxTokensForReasoningModels(in)
		if gjson.GetBytes(out, "max_tokens").Exists() {
			t.Errorf("max_tokens should be removed, got: %s", out)
		}
		if v := gjson.GetBytes(out, "max_completion_tokens"); v.Int() != 200 {
			t.Errorf("max_completion_tokens = %v, want 200", v)
		}
	})

	t.Run("no max_tokens = no-op", func(t *testing.T) {
		in := []byte(`{"messages":[]}`)
		before := string(in)
		out := renameMaxTokensForReasoningModels(in)
		if string(out) != before {
			t.Errorf("mutation without max_tokens: %s -> %s", before, out)
		}
	})

	t.Run("empty payload", func(t *testing.T) {
		if got := renameMaxTokensForReasoningModels(nil); got != nil {
			t.Errorf("nil input should stay nil, got %v", got)
		}
		if got := renameMaxTokensForReasoningModels([]byte{}); len(got) != 0 {
			t.Errorf("empty input should stay empty, got %v", got)
		}
	})
}
