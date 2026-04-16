package registry

import "testing"

func TestGetCodexFreeModels_BackfillsPreTierSplitModels(t *testing.T) {
	original := getModels()

	modelsCatalogStore.mu.Lock()
	modelsCatalogStore.data = &staticModelsJSON{
		CodexFree: []*ModelInfo{
			{ID: "gpt-5"},
			{ID: "gpt-5.2-codex"},
		},
		CodexPlus: []*ModelInfo{
			{ID: "gpt-5.3-codex"},
			{ID: "gpt-5.4"},
		},
		CodexTeam: []*ModelInfo{
			{ID: "gpt-5.3-codex"},
			{ID: "gpt-5.4"},
		},
		CodexPro: []*ModelInfo{
			{ID: "gpt-5.3-codex"},
			{ID: "gpt-5.4"},
		},
	}
	modelsCatalogStore.mu.Unlock()
	t.Cleanup(func() {
		modelsCatalogStore.mu.Lock()
		modelsCatalogStore.data = original
		modelsCatalogStore.mu.Unlock()
	})

	models := GetCodexFreeModels()
	if len(models) != 4 {
		t.Fatalf("expected 4 free models after backfill, got %d", len(models))
	}

	got := make([]string, 0, len(models))
	seen := make(map[string]struct{}, len(models))
	for _, model := range models {
		if model == nil {
			t.Fatal("expected model entry, got nil")
		}
		got = append(got, model.ID)
		if _, ok := seen[model.ID]; ok {
			t.Fatalf("expected unique model IDs, got duplicate %q", model.ID)
		}
		seen[model.ID] = struct{}{}
	}

	want := []string{"gpt-5", "gpt-5.2-codex", "gpt-5.3-codex", "gpt-5.4"}
	for i, id := range want {
		if got[i] != id {
			t.Fatalf("expected model %d to be %q, got %q", i, id, got[i])
		}
	}
}

func TestGitHubCopilotClaudeModelsSupportMessages(t *testing.T) {
	models := GetGitHubCopilotModels()
	required := map[string]bool{
		"claude-haiku-4.5":  false,
		"claude-opus-4.1":   false,
		"claude-opus-4.5":   false,
		"claude-opus-4.6":   false,
		"claude-sonnet-4":   false,
		"claude-sonnet-4.5": false,
		"claude-sonnet-4.6": false,
	}

	for _, model := range models {
		if _, ok := required[model.ID]; !ok {
			continue
		}
		required[model.ID] = true
		if !containsString(model.SupportedEndpoints, "/chat/completions") {
			t.Fatalf("model %q supported endpoints = %v, missing /chat/completions", model.ID, model.SupportedEndpoints)
		}
		if !containsString(model.SupportedEndpoints, "/messages") {
			t.Fatalf("model %q supported endpoints = %v, missing /messages", model.ID, model.SupportedEndpoints)
		}
	}

	for modelID, found := range required {
		if !found {
			t.Fatalf("expected GitHub Copilot model %q in definitions", modelID)
		}
	}
}

func containsString(items []string, want string) bool {
	for _, item := range items {
		if item == want {
			return true
		}
	}
	return false
}
