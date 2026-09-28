package registry

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestCodexConfigurationUpdateCapability(t *testing.T) {
	for _, tier := range []struct {
		name    string
		models  func() []*ModelInfo
		capable []string
	}{
		{name: "free", models: GetCodexFreeModels, capable: []string{"gpt-6-luna"}},
		{name: "team", models: GetCodexTeamModels, capable: []string{"gpt-6-astra", "gpt-6-sol", "gpt-6-luna"}},
		{name: "plus", models: GetCodexPlusModels, capable: []string{"gpt-6-astra", "gpt-6-sol", "gpt-6-luna"}},
		{name: "pro", models: GetCodexProModels, capable: []string{"gpt-6-astra", "gpt-6-sol", "gpt-6-luna"}},
	} {
		t.Run(tier.name, func(t *testing.T) {
			byID := make(map[string]*ModelInfo)
			for _, info := range tier.models() {
				if info != nil {
					byID[info.ID] = info
				}
			}
			for _, id := range append([]string{"gpt-5.5"}, tier.capable...) {
				info := byID[id]
				if info == nil {
					t.Errorf("%s is missing", id)
					continue
				}
				want := id != "gpt-5.5"
				if info.SupportConfigurationUpdate != want {
					t.Errorf("%s configuration_update = %v, want %v", id, info.SupportConfigurationUpdate, want)
				}
				if cloned := cloneModelInfo(info); cloned.SupportConfigurationUpdate != want {
					t.Errorf("%s clone configuration_update = %v, want %v", id, cloned.SupportConfigurationUpdate, want)
				}
				raw, errMarshal := json.Marshal(info)
				if errMarshal != nil {
					t.Fatal(errMarshal)
				}
				if strings.Contains(string(raw), "support_configuration_update") || strings.Contains(string(raw), "SupportConfigurationUpdate") {
					t.Errorf("%s exposed configuration_update capability: %s", id, raw)
				}
			}
		})
	}

	for _, tt := range []struct {
		name string
		raw  string
		want bool
	}{
		{name: "true", raw: `{"id":"test","support_configuration_update":true}`, want: true},
		{name: "false", raw: `{"id":"test","support_configuration_update":false}`},
		{name: "absent", raw: `{"id":"test"}`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			info := ModelInfo{SupportConfigurationUpdate: true}
			if errUnmarshal := json.Unmarshal([]byte(tt.raw), &info); errUnmarshal != nil {
				t.Fatal(errUnmarshal)
			}
			if info.SupportConfigurationUpdate != tt.want {
				t.Errorf("capability = %v, want %v", info.SupportConfigurationUpdate, tt.want)
			}
		})
	}
}

func TestGetStaticModelDefinitionsByChannelSupportsGeminiInteractions(t *testing.T) {
	models := GetStaticModelDefinitionsByChannel("gemini-interactions")
	if len(models) == 0 {
		t.Fatal("GetStaticModelDefinitionsByChannel(gemini-interactions) returned no models")
	}
}

func TestGetStaticModelDefinitionsByChannelSupportsKimiAndKimiAI(t *testing.T) {
	for _, channel := range []string{"kimi", "kimi-ai", "kimi.ai", "kimi.com"} {
		models := GetStaticModelDefinitionsByChannel(channel)
		if len(models) == 0 {
			t.Fatalf("GetStaticModelDefinitionsByChannel(%s) returned no models", channel)
		}
	}
}

func TestModelOverrideHeadersFromEmbeddedModels(t *testing.T) {
	const wantUA = "codex-tui/0.144.0 (Mac OS 26.5.1; arm64) iTerm.app/3.6.11 (codex-tui; 0.144.0)"
	got := ModelOverrideHeaders("gpt-5.6-luna")
	if got == nil {
		t.Fatal("ModelOverrideHeaders(gpt-5.6-luna) = nil, want headers")
	}
	if got["user-agent"] != wantUA {
		t.Fatalf("user-agent = %q, want %q", got["user-agent"], wantUA)
	}
	if got := ModelOverrideHeaders("gpt-5.4"); got != nil {
		t.Fatalf("ModelOverrideHeaders(gpt-5.4) = %#v, want nil", got)
	}
}

func TestGeminiVertexModelsUseFlashLiteReleaseID(t *testing.T) {
	const releaseID = "gemini-3.1-flash-lite"
	const previewID = releaseID + "-preview"

	for _, model := range GetGeminiVertexModels() {
		if model == nil {
			continue
		}
		if model.ID == previewID {
			t.Fatalf("Vertex model ID = %q, want release ID %q", model.ID, releaseID)
		}
		if model.ID == releaseID {
			return
		}
	}

	t.Fatalf("Vertex models do not contain %q", releaseID)
}

func TestWithXAIBuiltinsIncludesImage20(t *testing.T) {
	models := WithXAIBuiltins(nil)
	for _, model := range models {
		if model != nil && model.ID == xaiBuiltinImage20ModelID {
			if model.Created != 1786060800 {
				t.Fatalf("created = %d, want 1786060800 (2026-08-07)", model.Created)
			}
			return
		}
	}
	t.Fatalf("expected xAI builtin model %s", xaiBuiltinImage20ModelID)
}

func TestWithXAIBuiltinsIncludesVideo15GAAndPreviewAlias(t *testing.T) {
	models := WithXAIBuiltins(nil)
	foundGA := false
	foundPreviewAlias := false

	for _, model := range models {
		if model == nil {
			continue
		}
		if model.ID == xaiBuiltinVideo15ModelID {
			foundGA = true
		}
		if model.ID == xaiBuiltinVideo15PreviewID {
			foundPreviewAlias = true
		}
	}

	if !foundGA {
		t.Fatalf("expected xAI builtin model %s", xaiBuiltinVideo15ModelID)
	}
	if !foundPreviewAlias {
		t.Fatalf("expected xAI builtin compatibility alias %s", xaiBuiltinVideo15PreviewID)
	}
}

func TestAntigravityWebSearchModelForRequiresRequestedModelCapability(t *testing.T) {
	registryRef := GetGlobalRegistry()
	registryRef.RegisterClient("test-antigravity-websearch-route", "antigravity", []*ModelInfo{
		{ID: "gemini-route-test"},
		{ID: "gemini-web-search-test", SupportsWebSearch: true},
	})
	registryRef.RegisterClient("test-gemini-websearch-route", "gemini", []*ModelInfo{
		{ID: "gemini-cross-provider-route"},
		{ID: "gemini-cross-provider-search", SupportsWebSearch: true},
	})
	t.Cleanup(func() {
		registryRef.UnregisterClient("test-antigravity-websearch-route")
		registryRef.UnregisterClient("test-gemini-websearch-route")
	})

	if got := AntigravityWebSearchModelFor("gemini-route-test"); got != "" {
		t.Fatalf("route model without web search support should not get fallback model, got %q", got)
	}
	if got := AntigravityWebSearchModelFor("gemini-route-test(high)"); got != "" {
		t.Fatalf("suffix route model without web search support should not get fallback model, got %q", got)
	}
	if got := AntigravityWebSearchModelFor("gemini-web-search-test"); got != "gemini-web-search-test" {
		t.Fatalf("AntigravityWebSearchModelFor capable model = %q, want itself", got)
	}
	if got := AntigravityWebSearchModelFor("gemini-cross-provider-route"); got != "" {
		t.Fatalf("cross-provider model should not get Antigravity web search model, got %q", got)
	}
	if got := AntigravityWebSearchModelFor("unknown-model"); got != "" {
		t.Fatalf("unknown model should not get Antigravity web search model, got %q", got)
	}
}
