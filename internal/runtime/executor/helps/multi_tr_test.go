package helps

import (
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/tidwall/gjson"
)

func TestRTK_MultipleToolResultsAllCompressed(t *testing.T) {
	bigDiff := strings.Repeat("diff --git a/x b/x\nindex abc1234..def5678 100644\n--- a/x\n+++ b/x\n@@ -1,1 +1,1 @@\n-old\n+new\n", 200)
	payload := []byte(`{"messages":[
		{"role":"assistant","content":[
			{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"git diff a"}},
			{"type":"tool_use","id":"t2","name":"Bash","input":{"command":"git diff b"}}
		]},
		{"role":"user","content":[
			{"type":"tool_result","tool_use_id":"t1","content":"` + jsonEscape(bigDiff) + `"},
			{"type":"tool_result","tool_use_id":"t2","content":"` + jsonEscape(bigDiff) + `"}
		]}
	]}`)
	cfg := &config.Config{SDKConfig: config.SDKConfig{RTK: config.RTKConfig{Enabled: true, MinSavingsPct: 5}}}

	out := applyRTKToolResultCompression(cfg, payload)

	r1 := gjson.GetBytes(out, "messages.1.content.0.content").String()
	r2 := gjson.GetBytes(out, "messages.1.content.1.content").String()
	if r1 == bigDiff {
		t.Fatalf("first tool_result not compressed")
	}
	if r2 == bigDiff {
		t.Fatalf("second tool_result not compressed")
	}
	if len(r1) >= len(bigDiff) || len(r2) >= len(bigDiff) {
		t.Fatalf("compressed sizes not smaller: r1=%d r2=%d orig=%d", len(r1), len(r2), len(bigDiff))
	}
}

// Anthropic supports tool_result.content as an array of {type:"text", text:"..."}.
// Verify each text part is independently compressed.
func TestRTK_AnthropicArrayContentCompressed(t *testing.T) {
	bigDiff := strings.Repeat("diff --git a/x b/x\nindex abc..def 100644\n--- a/x\n+++ b/x\n@@ -1,1 +1,1 @@\n-old\n+new\n", 200)
	payload := []byte(`{"messages":[
		{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"git diff"}}]},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":[
			{"type":"text","text":"` + jsonEscape(bigDiff) + `"},
			{"type":"text","text":"short text — under floor, must be untouched"}
		]}]}
	]}`)
	cfg := &config.Config{SDKConfig: config.SDKConfig{RTK: config.RTKConfig{Enabled: true, MinSavingsPct: 5}}}
	out := applyRTKToolResultCompression(cfg, payload)

	first := gjson.GetBytes(out, "messages.1.content.0.content.0.text").String()
	second := gjson.GetBytes(out, "messages.1.content.0.content.1.text").String()
	if first == bigDiff {
		t.Fatalf("array text part not compressed")
	}
	if len(first) >= len(bigDiff) {
		t.Fatalf("compressed first part not smaller: %d >= %d", len(first), len(bigDiff))
	}
	if second != "short text — under floor, must be untouched" {
		t.Fatalf("under-floor second part should not change, got %q", second)
	}
}

// OpenAI Responses supports function_call_output.output as an array of
// {type:"input_text", text:"..."}. Mirror the Anthropic-array test.
func TestRTK_OpenAIArrayOutputCompressed(t *testing.T) {
	bigDiff := strings.Repeat("diff --git a/x b/x\nindex abc..def 100644\n--- a/x\n+++ b/x\n@@ -1,1 +1,1 @@\n-old\n+new\n", 200)
	payload := []byte(`{"input":[
		{"type":"function_call","call_id":"c1","name":"Bash","arguments":"{\"command\":\"git diff\"}"},
		{"type":"function_call_output","call_id":"c1","output":[
			{"type":"input_text","text":"` + jsonEscape(bigDiff) + `"},
			{"type":"input_text","text":"short text — under floor, must be untouched"}
		]}
	]}`)
	cfg := &config.Config{SDKConfig: config.SDKConfig{RTK: config.RTKConfig{Enabled: true, MinSavingsPct: 5}}}
	out := applyRTKToolResultCompression(cfg, payload)

	first := gjson.GetBytes(out, "input.1.output.0.text").String()
	second := gjson.GetBytes(out, "input.1.output.1.text").String()
	if first == bigDiff {
		t.Fatalf("array text part not compressed")
	}
	if len(first) >= len(bigDiff) {
		t.Fatalf("compressed first part not smaller: %d >= %d", len(first), len(bigDiff))
	}
	if second != "short text — under floor, must be untouched" {
		t.Fatalf("under-floor second part should not change, got %q", second)
	}
}

// MinSavingsPct gate: if the filter saves less than the threshold, original
// content must be preserved verbatim.
//
// We construct an output that filterTruncate (150-line cap) shrinks by exactly
// ~5%: 158 lines, no other markers. Truncating to 150 drops 8 lines and adds
// the omitted-marker line — net savings around 5%. We then assert the gate
// rejects at minPct=20 but accepts at minPct=2.
func TestRTK_MinSavingsPctGate(t *testing.T) {
	// Use a `gt` command (matches the registry rule with filterTruncate).
	cmd := "gt log"
	// Each line ~80 chars, 158 lines, total ~12KB → above rtkBytesFloor.
	line := strings.Repeat("x", 80)
	output := ""
	for i := 0; i < 158; i++ {
		output += line + "\n"
	}

	tcs := []struct {
		name      string
		minPct    int
		expectMod bool
	}{
		{"strict_minPct_50_rejects", 50, false},
		{"loose_minPct_2_accepts", 2, true},
	}

	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			payload := []byte(`{"messages":[
				{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"` + cmd + `"}}]},
				{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"` + jsonEscape(output) + `"}]}
			]}`)
			cfg := &config.Config{SDKConfig: config.SDKConfig{RTK: config.RTKConfig{Enabled: true, MinSavingsPct: tc.minPct}}}
			out := applyRTKToolResultCompression(cfg, payload)
			got := gjson.GetBytes(out, "messages.1.content.0.content").String()

			if tc.expectMod {
				if got == output {
					t.Fatalf("expected compression (savings ≥ %d%%), got identical output", tc.minPct)
				}
			} else {
				if got != output {
					t.Fatalf("expected verbatim (savings < %d%% rejected), got len=%d (orig=%d)", tc.minPct, len(got), len(output))
				}
			}
		})
	}
}

// tool_result whose tool_use_id has no matching tool_use entry must be left alone.
func TestRTK_ToolResultWithoutMatchingUse(t *testing.T) {
	bigDiff := strings.Repeat("diff --git a/x b/x\nindex abc..def 100644\n--- a/x\n+++ b/x\n@@ -1,1 +1,1 @@\n-old\n+new\n", 200)
	payload := []byte(`{"messages":[
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"orphan_id","content":"` + jsonEscape(bigDiff) + `"}]}
	]}`)
	cfg := &config.Config{SDKConfig: config.SDKConfig{RTK: config.RTKConfig{Enabled: true, MinSavingsPct: 5}}}
	out := applyRTKToolResultCompression(cfg, payload)
	got := gjson.GetBytes(out, "messages.0.content.0.content").String()
	if got != bigDiff {
		t.Fatalf("orphan tool_result must not be compressed (no command to classify)")
	}
}
