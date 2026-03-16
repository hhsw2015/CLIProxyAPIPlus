package auth

import (
	"testing"
)

func TestSkyworkFallbackChain_DisabledReturnsOnlyRequested(t *testing.T) {
	// When fallback is disabled, should return only the requested model.
	chain := PlanSkyworkFallbackChain("claude-opus-4.6", nil, false)
	if len(chain) != 1 || chain[0] != "claude-opus-4.6" {
		t.Fatalf("expected [claude-opus-4.6], got %v", chain)
	}
}

func TestSkyworkFallbackChain_RequestedModelAlwaysFirst(t *testing.T) {
	available := []string{"claude-opus-4.6", "gpt-5.4", "claude-sonnet-4.6", "gpt-5.3-codex"}
	chain := PlanSkyworkFallbackChain("gpt-5.4", available, false)
	if len(chain) == 0 || chain[0] != "gpt-5.4" {
		t.Fatalf("expected gpt-5.4 first, got %v", chain)
	}
}

func TestSkyworkFallbackChain_LightRequest_SameFamilyFirst(t *testing.T) {
	available := []string{"claude-opus-4.6", "gpt-5.4", "claude-sonnet-4.6", "gpt-5.3-codex", "gpt-5.2"}
	chain := PlanSkyworkFallbackChain("claude-opus-4.6", available, false)

	// Light request from Claude: should prefer same-family before cross-family.
	// Expected: claude-opus-4.6, claude-sonnet-4.6, gpt-5.4, gpt-5.3-codex, gpt-5.2
	if chain[0] != "claude-opus-4.6" {
		t.Errorf("expected claude-opus-4.6 first, got %s", chain[0])
	}
	// Second should be same-family (claude-sonnet-4.6)
	if chain[1] != "claude-sonnet-4.6" {
		t.Errorf("expected claude-sonnet-4.6 second (same-family), got %s", chain[1])
	}
	// Cross-family should come after all same-family
	foundCrossFamilyIdx := -1
	for i, m := range chain {
		if skyworkModelFamily(m) == "gpt" {
			foundCrossFamilyIdx = i
			break
		}
	}
	if foundCrossFamilyIdx < 2 {
		t.Errorf("expected cross-family models after same-family, first GPT at index %d", foundCrossFamilyIdx)
	}
}

func TestSkyworkFallbackChain_LightRequest_GPTSameFamilyFirst(t *testing.T) {
	available := []string{"claude-opus-4.6", "gpt-5.4", "claude-sonnet-4.6", "gpt-5.3-codex", "gpt-5.2"}
	chain := PlanSkyworkFallbackChain("gpt-5.4", available, false)

	// Light GPT request: same-family T1/T2 first, then cross-family T1/T2. T3 excluded.
	if chain[0] != "gpt-5.4" {
		t.Errorf("expected gpt-5.4 first, got %s", chain[0])
	}
	if chain[1] != "gpt-5.3-codex" {
		t.Errorf("expected gpt-5.3-codex second, got %s", chain[1])
	}
	// Then cross-family
	if chain[2] != "claude-opus-4.6" {
		t.Errorf("expected claude-opus-4.6 third, got %s", chain[2])
	}
	// gpt-5.2 (T3) should NOT be in chain
	for _, m := range chain {
		if m == "gpt-5.2" {
			t.Error("gpt-5.2 (T3) should not be in fallback chain")
		}
	}
}

func TestSkyworkFallbackChain_HeavyRequest_CrossTierFirst(t *testing.T) {
	available := []string{"claude-opus-4.6", "gpt-5.4", "claude-sonnet-4.6", "gpt-5.3-codex"}
	chain := PlanSkyworkFallbackChain("claude-opus-4.6", available, true)

	// Heavy Claude request: same-tier cross-family (gpt-5.4) before same-family lower tier
	if chain[0] != "claude-opus-4.6" {
		t.Errorf("expected claude-opus-4.6 first, got %s", chain[0])
	}
	if chain[1] != "gpt-5.4" {
		t.Errorf("expected gpt-5.4 second (same-tier cross-family), got %s", chain[1])
	}
}

func TestSkyworkFallbackChain_HeavyRequest_GPTCrossTierFirst(t *testing.T) {
	available := []string{"claude-opus-4.6", "gpt-5.4", "claude-sonnet-4.6", "gpt-5.3-codex"}
	chain := PlanSkyworkFallbackChain("gpt-5.4", available, true)

	// Heavy GPT request: same-tier cross-family (claude-opus-4.6) first
	if chain[0] != "gpt-5.4" {
		t.Errorf("expected gpt-5.4 first, got %s", chain[0])
	}
	if chain[1] != "claude-opus-4.6" {
		t.Errorf("expected claude-opus-4.6 second (same-tier cross-family), got %s", chain[1])
	}
}

func TestSkyworkFallbackChain_FiltersByAvailableModels(t *testing.T) {
	// Only claude-opus-4.6 and gpt-5.4 are available (both T1) — chain should include both.
	available := []string{"claude-opus-4.6", "gpt-5.4"}
	chain := PlanSkyworkFallbackChain("claude-opus-4.6", available, false)

	if len(chain) != 2 {
		t.Fatalf("expected 2 models, got %d: %v", len(chain), chain)
	}
	if chain[0] != "claude-opus-4.6" || chain[1] != "gpt-5.4" {
		t.Errorf("expected [claude-opus-4.6, gpt-5.4], got %v", chain)
	}
}

func TestSkyworkFallbackChain_UnknownModelReturnsOnlyRequested(t *testing.T) {
	available := []string{"claude-opus-4.6", "gpt-5.4", "unknown-model-xyz"}
	chain := PlanSkyworkFallbackChain("unknown-model-xyz", available, false)

	// Unknown model not in capability table: return only that model
	if len(chain) != 1 || chain[0] != "unknown-model-xyz" {
		t.Fatalf("expected [unknown-model-xyz], got %v", chain)
	}
}

func TestSkyworkFallbackChain_NilAvailableReturnsOnlyRequested(t *testing.T) {
	chain := PlanSkyworkFallbackChain("claude-opus-4.6", nil, false)
	if len(chain) != 1 || chain[0] != "claude-opus-4.6" {
		t.Fatalf("expected [claude-opus-4.6], got %v", chain)
	}
}

func TestSkyworkFallbackChain_AllModelsIncluded(t *testing.T) {
	available := []string{"claude-opus-4.6", "gpt-5.4", "claude-sonnet-4.6", "gpt-5.3-codex",
		"claude-opus-4.5", "claude-sonnet-4.5", "gpt-5.2"}
	chain := PlanSkyworkFallbackChain("claude-opus-4.6", available, false)

	// Only T1+T2 models should appear: claude-opus-4.6, gpt-5.4, claude-sonnet-4.6, gpt-5.3-codex
	if len(chain) != 4 {
		t.Fatalf("expected 4 models (T1+T2 only), got %d: %v", len(chain), chain)
	}
	// No duplicates
	seen := make(map[string]bool)
	for _, m := range chain {
		if seen[m] {
			t.Errorf("duplicate model: %s", m)
		}
		seen[m] = true
	}
	// No T3 models
	for _, m := range chain {
		cap := skyworkModelIndex[m]
		if cap.Tier > 2 {
			t.Errorf("T3 model %s should not be in fallback chain", m)
		}
	}
}

func TestCrossFamilyReasoningEffort_ClaudeToGPT(t *testing.T) {
	tests := []struct {
		input, expected string
	}{
		{"high", "xhigh"},
		{"medium", "high"},
		{"low", "medium"},
		{"none", "none"},
	}
	for _, tt := range tests {
		got := MapCrossFamilyReasoningEffort(tt.input, "claude", "gpt")
		if got != tt.expected {
			t.Errorf("claude→gpt %q: expected %q, got %q", tt.input, tt.expected, got)
		}
	}
}

func TestCrossFamilyReasoningEffort_GPTToClaude(t *testing.T) {
	tests := []struct {
		input, expected string
	}{
		{"xhigh", "high"},
		{"high", "high"},
		{"medium", "medium"},
		{"low", "low"},
		{"none", "none"},
	}
	for _, tt := range tests {
		got := MapCrossFamilyReasoningEffort(tt.input, "gpt", "claude")
		if got != tt.expected {
			t.Errorf("gpt→claude %q: expected %q, got %q", tt.input, tt.expected, got)
		}
	}
}

func TestCrossFamilyReasoningEffort_SameFamily_NoChange(t *testing.T) {
	got := MapCrossFamilyReasoningEffort("high", "claude", "claude")
	if got != "high" {
		t.Errorf("same family should not change: expected high, got %s", got)
	}
}

func TestIsHeavyRequest(t *testing.T) {
	small := make([]byte, 50*1024) // 50KB
	if IsHeavySkyworkRequest(small) {
		t.Error("50KB should not be heavy")
	}

	large := make([]byte, 200*1024) // 200KB
	if !IsHeavySkyworkRequest(large) {
		t.Error("200KB should be heavy")
	}
}

func TestSkyworkModelFamily(t *testing.T) {
	if skyworkModelFamily("claude-opus-4.6") != "claude" {
		t.Error("expected claude family")
	}
	if skyworkModelFamily("gpt-5.4") != "gpt" {
		t.Error("expected gpt family")
	}
	if skyworkModelFamily("unknown") != "" {
		t.Error("expected empty family for unknown model")
	}
}
