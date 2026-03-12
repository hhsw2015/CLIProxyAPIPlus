package cliproxy

import (
	"testing"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func TestRegisterModelsForAuth_OpenAICompatAddsImplicitClaudeHyphenAliases(t *testing.T) {
	svc := &Service{
		cfg: &internalconfig.Config{
			OpenAICompatibility: []internalconfig.OpenAICompatibility{{
				Name:   "skywork",
				Prefix: "skywork",
				Models: []internalconfig.OpenAICompatibilityModel{{
					Name: "claude-opus-4.5",
				}},
			}},
		},
	}

	auth := &coreauth.Auth{
		ID:       "openai-compat-skywork-register-test",
		Provider: "skywork",
		Prefix:   "skywork",
		Attributes: map[string]string{
			"compat_name":  "skywork",
			"provider_key": "skywork",
		},
	}

	reg := GlobalModelRegistry()
	reg.UnregisterClient(auth.ID)
	t.Cleanup(func() {
		reg.UnregisterClient(auth.ID)
	})

	svc.registerModelsForAuth(auth)

	for _, modelID := range []string{
		"claude-opus-4.5",
		"claude-opus-4-5",
		"skywork/claude-opus-4.5",
		"skywork/claude-opus-4-5",
	} {
		if !reg.ClientSupportsModel(auth.ID, modelID) {
			t.Fatalf("expected %q to be registered for %s", modelID, auth.ID)
		}
	}
}
