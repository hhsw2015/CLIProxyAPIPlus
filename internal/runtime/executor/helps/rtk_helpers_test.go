package helps

import (
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/tidwall/gjson"
)

func TestRTK_AnthropicToolResultCompressed(t *testing.T) {
	bigGitDiff := strings.Repeat("diff --git a/x b/x\nindex abc1234..def5678 100644\n--- a/x\n+++ b/x\n@@ -1,1 +1,1 @@\n-old\n+new\n", 200)
	payload := []byte(`{"messages":[
		{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"Bash","input":{"command":"git diff HEAD"}}]},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"` + jsonEscape(bigGitDiff) + `"}]}
	]}`)
	cfg := &config.Config{SDKConfig: config.SDKConfig{RTK: config.RTKConfig{Enabled: true, MinSavingsPct: 5}}}

	out := applyRTKToolResultCompression(cfg, payload)

	got := gjson.GetBytes(out, "messages.1.content.0.content").String()
	if got == bigGitDiff {
		t.Fatalf("expected git diff to be compressed; got identical output")
	}
	if len(got) >= len(bigGitDiff) {
		t.Fatalf("compressed output not smaller: %d >= %d", len(got), len(bigGitDiff))
	}
}

func TestRTK_DisabledLeavesPayloadUntouched(t *testing.T) {
	bigGitDiff := strings.Repeat("diff --git a/x b/x\n@@ -1,1 +1,1 @@\n-old\n+new\n", 30)
	payload := []byte(`{"messages":[
		{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"Bash","input":{"command":"git diff"}}]},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"` + jsonEscape(bigGitDiff) + `"}]}
	]}`)
	cfg := &config.Config{}
	out := applyRTKToolResultCompression(cfg, payload)
	if string(out) != string(payload) {
		t.Fatalf("disabled RTK must not modify payload")
	}
}

func TestRTK_SkipsErrorToolResult(t *testing.T) {
	bigOutput := strings.Repeat("error trace line\n", 100)
	payload := []byte(`{"messages":[
		{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"Bash","input":{"command":"git diff"}}]},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","is_error":true,"content":"` + jsonEscape(bigOutput) + `"}]}
	]}`)
	cfg := &config.Config{SDKConfig: config.SDKConfig{RTK: config.RTKConfig{Enabled: true}}}
	out := applyRTKToolResultCompression(cfg, payload)
	got := gjson.GetBytes(out, "messages.1.content.0.content").String()
	if got != bigOutput {
		t.Fatalf("error tool_result must be preserved verbatim")
	}
}

func TestRTK_OpenAIResponsesFunctionCallOutput(t *testing.T) {
	bigGitDiff := strings.Repeat("diff --git a/x b/x\nindex abc1234..def5678 100644\n--- a/x\n+++ b/x\n@@ -1,1 +1,1 @@\n-old\n+new\n", 200)
	payload := []byte(`{"input":[
		{"type":"function_call","call_id":"c1","name":"Bash","arguments":"{\"command\":\"git diff\"}"},
		{"type":"function_call_output","call_id":"c1","output":"` + jsonEscape(bigGitDiff) + `"}
	]}`)
	cfg := &config.Config{SDKConfig: config.SDKConfig{RTK: config.RTKConfig{Enabled: true, MinSavingsPct: 5}}}
	out := applyRTKToolResultCompression(cfg, payload)
	got := gjson.GetBytes(out, "input.1.output").String()
	if got == bigGitDiff {
		t.Fatalf("expected function_call_output to be compressed")
	}
	if len(got) >= len(bigGitDiff) {
		t.Fatalf("compressed output not smaller: %d >= %d", len(got), len(bigGitDiff))
	}
}

func TestRTK_UnclassifiedCommandUnchanged(t *testing.T) {
	body := strings.Repeat("the quick brown fox jumps over the lazy dog\n", 30)
	payload := []byte(`{"messages":[
		{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"Bash","input":{"command":"some-internal-tool --weird-args"}}]},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"` + jsonEscape(body) + `"}]}
	]}`)
	cfg := &config.Config{SDKConfig: config.SDKConfig{RTK: config.RTKConfig{Enabled: true}}}
	out := applyRTKToolResultCompression(cfg, payload)
	got := gjson.GetBytes(out, "messages.1.content.0.content").String()
	if got != body {
		t.Fatalf("unclassified command must not trigger compression")
	}
}

func jsonEscape(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`, "\r", `\r`, "\t", `\t`)
	return r.Replace(s)
}
