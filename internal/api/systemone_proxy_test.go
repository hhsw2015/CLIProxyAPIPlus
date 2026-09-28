package api

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

// TestSystemOneChannelsSiliconFlowKev verifies the SiliconFlow Kev channel is
// discovered by base-url host, ordered BEFORE the TypeSafe catch-all, and that its
// model rewrite normalizes the kev family to Kev-4B while staying disjoint from jev.
func TestSystemOneChannelsSiliconFlowKev(t *testing.T) {
	s := &Server{cfg: &config.Config{
		OpenAICompatibility: []config.OpenAICompatibility{
			{
				Name:    "silicon-direct",
				BaseURL: "https://api.siliconflow.cn/v1",
				APIKeyEntries: []config.OpenAICompatibilityAPIKey{
					{APIKey: "sk-silicon"},
				},
			},
			{
				// Duplicate SiliconFlow entry sharing the same key: must be deduped.
				Name:    "siliconflow-kev-systemone",
				BaseURL: "https://api.siliconflow.cn/v1",
				APIKeyEntries: []config.OpenAICompatibilityAPIKey{
					{APIKey: "sk-silicon"},
				},
			},
			{
				Name:    "typesafe-systemone",
				BaseURL: "https://api.typesafe.ai/v1",
				APIKeyEntries: []config.OpenAICompatibilityAPIKey{
					{APIKey: "sk-typesafe"},
				},
			},
		},
	}}

	channels := s.systemOneChannels()

	var sfIdx, tsIdx = -1, -1
	for i, ch := range channels {
		switch ch.name {
		case "siliconflow":
			sfIdx = i
			if ch.endpoint != "https://api.siliconflow.cn/v1/systemone" {
				t.Fatalf("siliconflow endpoint = %q", ch.endpoint)
			}
			if len(ch.keys) != 1 { // deduped across the two entries
				t.Fatalf("siliconflow keys = %v, want 1 (deduped)", ch.keys)
			}
			// kev family normalizes to Kev-4B; jev is not served here.
			for _, in := range []string{"Kev-4B", "kev-4b", "kev-latest", "kev"} {
				if out, ok := ch.rewriteModel(in); !ok || out != "Kev-4B" {
					t.Fatalf("rewriteModel(%q) = (%q,%v), want (Kev-4B,true)", in, out, ok)
				}
			}
			if _, ok := ch.rewriteModel("jev-1.13"); ok {
				t.Fatalf("siliconflow must not serve jev-1.13")
			}
		case "typesafe":
			tsIdx = i
		}
	}
	if sfIdx < 0 {
		t.Fatal("siliconflow channel not discovered")
	}
	if tsIdx < 0 {
		t.Fatal("typesafe channel not present")
	}
	// The TypeSafe channel is a catch-all (accepts any id); siliconflow must come
	// first so a kev request is not swallowed by it.
	if sfIdx > tsIdx {
		t.Fatalf("siliconflow (idx %d) must precede typesafe catch-all (idx %d)", sfIdx, tsIdx)
	}
}
