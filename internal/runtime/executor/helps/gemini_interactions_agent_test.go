package helps

import (
	"testing"

	"github.com/tidwall/gjson"
)

func TestApplyGeminiInteractionsAgentModePreset(t *testing.T) {
	body := []byte(`{"model":"deep-research-preview-04-2026","input":[{"type":"user_input","content":[{"type":"text","text":"hi"}]}]}`)
	out, bg := ApplyGeminiInteractionsAgentMode(body, "deep-research-preview-04-2026")
	if !bg {
		t.Fatalf("expected background=true for a preset agent model")
	}
	r := gjson.ParseBytes(out)
	if r.Get("model").Exists() {
		t.Fatalf("model field should be removed for agent mode, got %q", r.Get("model").String())
	}
	if got := r.Get("agent").String(); got != "deep-research-preview-04-2026" {
		t.Fatalf("agent = %q, want deep-research-preview-04-2026", got)
	}
	if !r.Get("background").Bool() {
		t.Fatalf("background must be true")
	}
	if got := r.Get("agent_config.type").String(); got != "deep-research" {
		t.Fatalf("agent_config.type = %q, want deep-research", got)
	}
}

func TestApplyGeminiInteractionsAgentModePlainChatUnchanged(t *testing.T) {
	body := []byte(`{"model":"antigravity-preview-latest","input":[{"type":"user_input","content":[{"type":"text","text":"hi"}]}]}`)
	out, bg := ApplyGeminiInteractionsAgentMode(body, "antigravity-preview-latest")
	if bg {
		t.Fatalf("plain chat model must not become a background agent")
	}
	if gjson.GetBytes(out, "background").Exists() {
		t.Fatalf("background must not be set for a plain chat model")
	}
	if gjson.GetBytes(out, "agent").Exists() {
		t.Fatalf("agent must not be set for a plain chat model")
	}
}

func TestApplyGeminiInteractionsAgentModeClientSuppliedConfig(t *testing.T) {
	// A client may drive a dynamic agent by passing agent_config explicitly, even for a
	// model not in the preset map.
	body := []byte(`{"model":"gemini-3-flash","agent_config":{"type":"dynamic"},"input":[]}`)
	out, bg := ApplyGeminiInteractionsAgentMode(body, "gemini-3-flash")
	if !bg {
		t.Fatalf("explicit agent_config should force agent mode")
	}
	r := gjson.ParseBytes(out)
	if got := r.Get("agent").String(); got != "gemini-3-flash" {
		t.Fatalf("agent = %q, want gemini-3-flash", got)
	}
	if got := r.Get("agent_config.type").String(); got != "dynamic" {
		t.Fatalf("agent_config.type = %q, want dynamic (client value preserved)", got)
	}
}

func TestBuildInteractionsStreamFramesFromFinal(t *testing.T) {
	final := []byte(`{"id":"ix_dr","agent":"deep-research-preview-04-2026","status":"completed",` +
		`"steps":[` +
		`{"type":"user_input","content":[{"type":"text","text":"q"}]},` +
		`{"type":"model_output","content":[{"type":"text","text":"# Report"}]}],` +
		`"usage":{"total_input_tokens":10,"total_output_tokens":5,"total_tokens":15}}`)
	frames := BuildInteractionsStreamFramesFromFinal(final)
	if len(frames) < 4 {
		t.Fatalf("expected >=4 frames (created, step.start, step.delta, completed), got %d", len(frames))
	}
	if et := gjson.GetBytes(frames[0], "event_type").String(); et != "interaction.created" {
		t.Fatalf("frame[0] event_type = %q, want interaction.created", et)
	}
	last := frames[len(frames)-1]
	if et := gjson.GetBytes(last, "event_type").String(); et != "interaction.completed" {
		t.Fatalf("last frame event_type = %q, want interaction.completed", et)
	}
	// The report text must appear in a step.delta frame (user_input echo must NOT).
	sawReport := false
	for _, f := range frames {
		if gjson.GetBytes(f, "event_type").String() == "step.delta" {
			if gjson.GetBytes(f, "delta.text").String() == "# Report" {
				sawReport = true
			}
			if gjson.GetBytes(f, "delta.text").String() == "q" {
				t.Fatalf("user_input echo leaked into a step.delta frame")
			}
		}
	}
	if !sawReport {
		t.Fatalf("report text not emitted as a step.delta frame")
	}
}
