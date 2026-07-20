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
		out := stripReasoningEffortIfToolsPresent(in)
		if !gjson.GetBytes(out, "reasoning_effort").Exists() {
			t.Fatal("reasoning_effort was stripped despite no tools present")
		}
	})

	t.Run("tools + reasoning_effort strips reasoning_effort", func(t *testing.T) {
		in := []byte(`{"model":"gpt-5.6-sol","reasoning_effort":"xhigh","tools":[{"type":"function","function":{"name":"f"}}],"messages":[]}`)
		out := stripReasoningEffortIfToolsPresent(in)
		if gjson.GetBytes(out, "reasoning_effort").Exists() {
			t.Errorf("reasoning_effort should have been removed, got: %s", out)
		}
		if !gjson.GetBytes(out, "tools").Exists() {
			t.Error("tools was incorrectly removed")
		}
	})

	t.Run("tools + reasoning.effort object with only effort strips whole reasoning", func(t *testing.T) {
		in := []byte(`{"model":"gpt-5.6-sol","reasoning":{"effort":"high"},"tools":[{"type":"function"}]}`)
		out := stripReasoningEffortIfToolsPresent(in)
		if gjson.GetBytes(out, "reasoning").Exists() {
			t.Errorf("reasoning should be removed when it only contained effort, got: %s", out)
		}
	})

	t.Run("tools + reasoning object with other fields keeps siblings", func(t *testing.T) {
		in := []byte(`{"tools":[{"type":"function"}],"reasoning":{"effort":"high","summary":"auto"}}`)
		out := stripReasoningEffortIfToolsPresent(in)
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
		out := stripReasoningEffortIfToolsPresent(in)
		if string(out) != before {
			t.Errorf("unexpected mutation: %s -> %s", before, out)
		}
	})

	t.Run("empty payload", func(t *testing.T) {
		if got := stripReasoningEffortIfToolsPresent(nil); got != nil {
			t.Errorf("nil input should stay nil, got %v", got)
		}
		if got := stripReasoningEffortIfToolsPresent([]byte{}); len(got) != 0 {
			t.Errorf("empty input should stay empty, got %v", got)
		}
	})
}
