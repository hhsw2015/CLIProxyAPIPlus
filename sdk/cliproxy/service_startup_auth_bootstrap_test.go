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

func TestServiceRunBootstrapsPoolStartupFromRootCandidatesOnly(t *testing.T) {
	activeAuthID := "pool-active-auth"
	reserveAuthID := "pool-reserve-auth"
	limitAuthID := "limit/pool-limit-auth.json"

	reg := GlobalModelRegistry()
	for _, id := range []string{activeAuthID, reserveAuthID, limitAuthID} {
		reg.UnregisterClient(id)
	}
	t.Cleanup(func() {
		for _, id := range []string{activeAuthID, reserveAuthID, limitAuthID} {
			reg.UnregisterClient(id)
		}
	})

	cfg := &config.Config{
		Host:    "127.0.0.1",
		Port:    0,
		AuthDir: filepath.Join(t.TempDir(), "auth"),
		SDKConfig: config.SDKConfig{
			APIKeys: []string{"test-key"},
		},
		PoolManager: config.PoolManagerConfig{
			Size:     1,
			Provider: "codex",
		},
	}

	activeAuth := &coreauth.Auth{
		ID:       activeAuthID,
		Provider: "codex",
		Status:   coreauth.StatusActive,
		Attributes: map[string]string{
			"api_key":  "active-key",
			"base_url": "https://example.com/v1",
		},
	}
	reserveAuth := &coreauth.Auth{
		ID:       reserveAuthID,
		Provider: "codex",
		Status:   coreauth.StatusActive,
		Attributes: map[string]string{
			"api_key":  "reserve-key",
			"base_url": "https://example.com/v1",
		},
	}
	limitAuth := &coreauth.Auth{
		ID:       limitAuthID,
		Provider: "codex",
		Status:   coreauth.StatusActive,
		Attributes: map[string]string{
			"api_key":  "limit-key",
			"base_url": "https://example.com/v1",
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
				snapshotAuths: func() []*coreauth.Auth { return nil },
				snapshotRootFileAuths: func() []*coreauth.Auth {
					return []*coreauth.Auth{reserveAuth.Clone(), activeAuth.Clone()}
				},
				snapshotLimitAuths: func() []*coreauth.Auth {
					return []*coreauth.Auth{limitAuth.Clone()}
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
		if service.poolManager != nil && service.poolManager.Snapshot().ActiveCount == 1 {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}

	if service.poolManager == nil {
		cancel()
		<-done
		t.Fatal("expected pool manager to be initialized")
	}

	snapshot := service.poolManager.Snapshot()
	if snapshot.ActiveCount != 1 || snapshot.ActiveIDs[0] != activeAuthID {
		cancel()
		<-done
		t.Fatalf("unexpected active snapshot: %+v", snapshot)
	}
	if snapshot.ReserveCount != 1 || snapshot.ReserveIDs[0] != reserveAuthID {
		cancel()
		<-done
		t.Fatalf("unexpected reserve snapshot: %+v", snapshot)
	}
	if snapshot.LimitCount != 1 || snapshot.LimitIDs[0] != limitAuthID {
		cancel()
		<-done
		t.Fatalf("unexpected limit snapshot: %+v", snapshot)
	}
	if len(service.publishedActive) != 1 {
		cancel()
		<-done
		t.Fatalf("publishedActive len = %d, want 1", len(service.publishedActive))
	}
	if _, ok := service.publishedActive[activeAuthID]; !ok {
		cancel()
		<-done
		t.Fatalf("expected active auth %s to be published", activeAuthID)
	}

	if !reg.ClientSupportsModel(activeAuthID, "gpt-5") {
		cancel()
		<-done
		t.Fatalf("expected active auth %s to be published to runtime", activeAuthID)
	}
	if reg.ClientSupportsModel(reserveAuthID, "gpt-5") {
		cancel()
		<-done
		t.Fatalf("did not expect reserve auth %s to be published to runtime", reserveAuthID)
	}
	if reg.ClientSupportsModel(limitAuthID, "gpt-5") {
		cancel()
		<-done
		t.Fatalf("did not expect limit auth %s to be published to runtime", limitAuthID)
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
}

func TestHandleAuthDisposition_RemovesActiveAndPromotesReserve(t *testing.T) {
	activeAuthID := "pool-active-auth-disposition"
	reserveAuthID := "pool-reserve-auth-disposition"

	reg := GlobalModelRegistry()
	for _, id := range []string{activeAuthID, reserveAuthID} {
		reg.UnregisterClient(id)
	}
	t.Cleanup(func() {
		for _, id := range []string{activeAuthID, reserveAuthID} {
			reg.UnregisterClient(id)
		}
	})

	service := &Service{
		cfg:         &config.Config{},
		poolManager: NewPoolManager(config.PoolManagerConfig{Size: 1, Provider: "codex"}),
		poolCandidates: map[string]*coreauth.Auth{
			activeAuthID: {
				ID:       activeAuthID,
				Provider: "codex",
				Status:   coreauth.StatusActive,
				Attributes: map[string]string{
					"api_key":  "active-key",
					"base_url": "https://example.com/v1",
				},
			},
			reserveAuthID: {
				ID:       reserveAuthID,
				Provider: "codex",
				Status:   coreauth.StatusActive,
				Attributes: map[string]string{
					"api_key":  "reserve-key",
					"base_url": "https://example.com/v1",
				},
			},
		},
		coreManager: coreauth.NewManager(nil, nil, nil),
	}
	service.poolManager.SetActive(PoolMember{AuthID: activeAuthID, Provider: "codex"})
	service.poolManager.SetReserve(PoolMember{AuthID: reserveAuthID, Provider: "codex"})
	service.syncPoolActiveToRuntime(context.Background())

	if !reg.ClientSupportsModel(activeAuthID, "gpt-5") {
		t.Fatal("expected initial active auth to be published")
	}
	if reg.ClientSupportsModel(reserveAuthID, "gpt-5") {
		t.Fatal("did not expect reserve auth to be published before promotion")
	}

	service.handleAuthDisposition(context.Background(), coreauth.AuthDisposition{
		AuthID:       activeAuthID,
		Provider:     "codex",
		Healthy:      false,
		PoolEligible: false,
		Deleted:      true,
		Source:       "request",
	})

	snapshot := service.poolManager.Snapshot()
	if snapshot.ActiveCount != 1 || snapshot.ActiveIDs[0] != reserveAuthID {
		t.Fatalf("unexpected active snapshot after disposition: %+v", snapshot)
	}
	if reg.ClientSupportsModel(activeAuthID, "gpt-5") {
		t.Fatal("expected deleted active auth to be removed from runtime")
	}
	if !reg.ClientSupportsModel(reserveAuthID, "gpt-5") {
		t.Fatal("expected reserve auth to be promoted into runtime")
	}
}
