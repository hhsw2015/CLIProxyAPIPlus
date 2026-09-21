package executor

import (
	"testing"

	"github.com/tidwall/gjson"
)

// TestRebuildMidSystemMessagesToTopLevel covers the fable-5-1 fix: text-bearing
// mid-array system turns must be hoisted to top-level system (they violate the
// placement rule), while directive-only turns (empty content + output_config,
// legal at any position) must be left in place so the directive isn't dropped.
func TestRebuildMidSystemMessagesToTopLevel(t *testing.T) {
	payload := []byte(`{
		"system":[{"type":"text","text":"base"}],
		"messages":[
			{"role":"user","content":[{"type":"text","text":"hi"}]},
			{"role":"system","content":[{"type":"text","text":"steer"}]},
			{"role":"system","content":[],"output_config":{"effort":"high"}},
			{"role":"user","content":[{"type":"text","text":"go"}]}
		]
	}`)

	out := rebuildMidSystemMessagesToTopLevel(payload)

	// Text-bearing system turn hoisted onto the existing top-level system.
	sys := gjson.GetBytes(out, "system")
	if n := len(sys.Array()); n != 2 {
		t.Fatalf("system parts = %d, want 2 (base + steer): %s", n, sys.Raw)
	}
	if sys.Array()[1].Get("text").String() != "steer" {
		t.Fatalf("hoisted text = %q, want steer", sys.Array()[1].Get("text").String())
	}

	// Messages: user, directive-only system (kept in place), user.
	msgs := gjson.GetBytes(out, "messages").Array()
	if len(msgs) != 3 {
		t.Fatalf("messages = %d, want 3 (text-system hoisted, directive kept): %s", len(msgs), out)
	}
	if msgs[1].Get("role").String() != "system" || !msgs[1].Get("output_config").Exists() {
		t.Fatalf("directive-only turn not preserved in place: %s", msgs[1].Raw)
	}
	for _, m := range msgs {
		if m.Get("role").String() == "system" && len(claudeSystemTextParts(m.Get("content"))) > 0 {
			t.Fatalf("a text-bearing system turn survived in messages: %s", m.Raw)
		}
	}
}
