package api

import "testing"

// Guards the /v1/embeddings dispatch: DashScope-native embedding ids route to the
// first-party handler, while OpenAI text-embedding-3-* and siliconflow
// qwen3-embedding-8b must stay on the generic path (misrouting = wrong key / 404).
func TestIsDashScopeEmbeddingModel(t *testing.T) {
	cases := map[string]bool{
		"qwen3.7-text-embedding":       true,
		"qwen3.7-text-embedding-flash": true,
		"text-embedding-v4":            true,
		"text-embedding-v3":            true,
		"TEXT-EMBEDDING-V4":            true, // case-insensitive
		// Must NOT match:
		"text-embedding-3-small": false, // OpenAI
		"text-embedding-3-large": false, // OpenAI
		"qwen3-embedding-8b":     false, // siliconflow (embedding, not text-embedding)
		"qwen3-embedding-0.6b":   false,
		"bge-m3":                 false,
		"":                       false,
	}
	for model, want := range cases {
		if got := isDashScopeEmbeddingModel(model); got != want {
			t.Errorf("isDashScopeEmbeddingModel(%q) = %v, want %v", model, got, want)
		}
	}
}

// Guards the /v1/audio/speech dispatch: Qwen TTS / cosyvoice route to the DashScope
// handler; other providers' TTS models do not.
func TestIsDashScopeTTSModel(t *testing.T) {
	cases := map[string]bool{
		"qwen3-tts-flash": true,
		"qwen-tts":        true,
		"cosyvoice-v2":    true,
		"CosyVoice":       true, // case-insensitive
		// Must NOT match:
		"minimax-speech-02-hd": false,
		"azure-tts":            false,
		"gpt-4o-mini-tts":      false,
		"qwen-vl-ocr":          false, // qwen but not tts
		"":                     false,
	}
	for model, want := range cases {
		if got := isDashScopeTTSModel(model); got != want {
			t.Errorf("isDashScopeTTSModel(%q) = %v, want %v", model, got, want)
		}
	}
}
