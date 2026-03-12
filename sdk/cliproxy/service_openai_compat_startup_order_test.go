package cliproxy

import (
	"context"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/sdk/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func TestBootstrapAuthSnapshot_PoolModeRegistersConfigAuthBeforeSlowPoolBootstrap(t *testing.T) {
	skyworkAuth := &coreauth.Auth{
		ID:       "openai-compatibility:skywork:startup-order",
		Provider: "skywork",
		Prefix:   "skywork",
		Attributes: map[string]string{
			"compat_name":  "skywork",
			"provider_key": "skywork",
		},
	}
	rootAuth := &coreauth.Auth{
		ID:       "pool-root-auth",
		Provider: "codex",
		Status:   coreauth.StatusActive,
	}

	service := &Service{
		coreManager: coreauth.NewManager(nil, nil, nil),
		cfg: &config.Config{
			OpenAICompatibility: []config.OpenAICompatibility{{
				Name:   "skywork",
				Prefix: "skywork",
				Models: []config.OpenAICompatibilityModel{{
					Name: "claude-opus-4.5",
				}},
			}},
			PoolManager: config.PoolManagerConfig{
				Size:     1,
				Provider: "codex",
			},
		},
	}

	blockProbe := make(chan struct{})
	originalProbe := poolProbeAuthFunc
	poolProbeAuthFunc = func(ctx context.Context, cfg *config.Config, auth *coreauth.Auth) (*coreauth.Auth, coreauth.Result) {
		<-blockProbe
		return auth, coreauth.Result{
			AuthID:   auth.ID,
			Provider: auth.Provider,
			Model:    "gpt-5",
			Success:  true,
		}
	}
	t.Cleanup(func() { poolProbeAuthFunc = originalProbe })

	reg := GlobalModelRegistry()
	reg.UnregisterClient(skyworkAuth.ID)
	t.Cleanup(func() {
		reg.UnregisterClient(skyworkAuth.ID)
	})

	done := make(chan struct{})
	go func() {
		service.bootstrapAuthSnapshot(context.Background(), &WatcherWrapper{
			snapshotAuths: func() []*coreauth.Auth {
				return []*coreauth.Auth{skyworkAuth.Clone()}
			},
			snapshotRootFileAuths: func() []*coreauth.Auth {
				return []*coreauth.Auth{rootAuth.Clone()}
			},
		})
		close(done)
	}()

	select {
	case <-time.After(150 * time.Millisecond):
		if !reg.ClientSupportsModel(skyworkAuth.ID, "skywork/claude-opus-4.5") {
			t.Fatal("expected config-backed skywork auth to register before slow pool bootstrap completed")
		}
	case <-done:
		t.Fatal("expected bootstrap to remain blocked on slow pool probe during assertion window")
	}

	close(blockProbe)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("bootstrapAuthSnapshot did not finish after releasing pool probe")
	}
}
