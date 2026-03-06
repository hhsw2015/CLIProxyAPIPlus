package cliproxy

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/watcher"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/config"
)

func TestServiceRunBootstrapsSnapshotAuthsWithoutWatcherQueue(t *testing.T) {
	authID := "skywork-startup-bootstrap"
	reg := GlobalModelRegistry()
	reg.UnregisterClient(authID)
	t.Cleanup(func() {
		reg.UnregisterClient(authID)
	})

	cfg := &config.Config{
		Host:    "127.0.0.1",
		Port:    0,
		AuthDir: filepath.Join(t.TempDir(), "auth"),
		SDKConfig: config.SDKConfig{
			APIKeys: []string{"test-key"},
		},
		OpenAICompatibility: []config.OpenAICompatibility{
			{
				Name:    "skywork",
				Prefix:  "skywork",
				BaseURL: "https://desktop-llm.skywork.ai/skycowork_llm/v1/proxy",
				Models: []config.OpenAICompatibilityModel{
					{Name: "claude-opus-4.5", Alias: "claude-opus-4.5"},
				},
			},
		},
	}

	auth := &coreauth.Auth{
		ID:       authID,
		Provider: "skywork",
		Prefix:   "skywork",
		Status:   coreauth.StatusActive,
		Attributes: map[string]string{
			"compat_name":  "skywork",
			"provider_key": "skywork",
			"base_url":     "https://desktop-llm.skywork.ai/skycowork_llm/v1/proxy",
		},
	}

	tempDir := t.TempDir()
	configPath := filepath.Join(tempDir, "config.yaml")
	if err := os.WriteFile(configPath, []byte("port: 0\n"), 0o600); err != nil {
		t.Fatalf("write temp config: %v", err)
	}

	service, err := NewBuilder().
		WithConfig(cfg).
		WithConfigPath(configPath).
		WithWatcherFactory(func(configPath, authDir string, reload func(*config.Config)) (*WatcherWrapper, error) {
			return &WatcherWrapper{
				start: func(ctx context.Context) error { return nil },
				stop:  func() error { return nil },
				setConfig: func(cfg *config.Config) {
					_ = cfg
				},
				snapshotAuths: func() []*coreauth.Auth {
					return []*coreauth.Auth{auth.Clone()}
				},
				setUpdateQueue: func(queue chan<- watcher.AuthUpdate) {
					_ = queue
				},
			}, nil
		}).
		Build()
	if err != nil {
		t.Fatalf("build service: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- service.Run(ctx)
	}()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if reg.ClientSupportsModel(authID, "skywork/claude-opus-4.5") {
			cancel()
			select {
			case errRun := <-done:
				if errRun != nil && errRun != context.Canceled {
					t.Fatalf("service run returned error: %v", errRun)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("service did not stop after cancel")
			}
			return
		}
		time.Sleep(25 * time.Millisecond)
	}

	cancel()
	select {
	case errRun := <-done:
		if errRun != nil && errRun != context.Canceled {
			t.Fatalf("service run returned error: %v", errRun)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("service did not stop after cancel")
	}
	t.Fatal("expected startup auth snapshot to be registered in model registry")
}
