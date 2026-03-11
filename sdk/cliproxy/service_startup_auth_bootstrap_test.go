package cliproxy

import (
	"bytes"
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	internalusage "github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/watcher"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	coreusage "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/usage"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/config"
	log "github.com/sirupsen/logrus"
)

type captureAuthStore struct {
	mu       sync.Mutex
	lastAuth *coreauth.Auth
	saveCnt  int
}

type serviceStartupTestExecutor struct {
	provider string
}

func (e serviceStartupTestExecutor) Identifier() string { return e.provider }

func (e serviceStartupTestExecutor) Execute(context.Context, *coreauth.Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{Payload: []byte(`{"ok":true}`)}, nil
}

func (e serviceStartupTestExecutor) ExecuteStream(context.Context, *coreauth.Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	return nil, nil
}

func (e serviceStartupTestExecutor) Refresh(_ context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	return auth, nil
}

func (e serviceStartupTestExecutor) CountTokens(context.Context, *coreauth.Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}

func (e serviceStartupTestExecutor) HttpRequest(context.Context, *coreauth.Auth, *http.Request) (*http.Response, error) {
	return nil, nil
}

func (s *captureAuthStore) List(context.Context) ([]*coreauth.Auth, error) { return nil, nil }

func (s *captureAuthStore) Save(_ context.Context, auth *coreauth.Auth) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.saveCnt++
	if auth != nil {
		s.lastAuth = auth.Clone()
		return auth.ID, nil
	}
	return "", nil
}

func (s *captureAuthStore) Delete(context.Context, string) error { return nil }

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

func TestServiceRunSeedsLoadedCoreAuthModelsWithoutWatcherSnapshot(t *testing.T) {
	authID := "loaded-codex-startup"
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
	}

	coreManager := coreauth.NewManager(nil, &coreauth.RoundRobinSelector{}, nil)
	coreManager.RegisterExecutor(serviceStartupTestExecutor{provider: "codex"})
	auth := &coreauth.Auth{
		ID:       authID,
		Provider: "codex",
		Status:   coreauth.StatusActive,
		Metadata: map[string]any{
			"type":          "codex",
			"access_token":  "access-token",
			"refresh_token": "refresh-token",
			"expired":       time.Now().Add(10 * 24 * time.Hour).Format(time.RFC3339),
		},
	}
	if _, err := coreManager.Register(coreauth.WithSkipPersist(context.Background()), auth); err != nil {
		t.Fatalf("preload core auth: %v", err)
	}

	tempDir := t.TempDir()
	configPath := filepath.Join(tempDir, "config.yaml")
	if err := os.WriteFile(configPath, []byte("port: 0\n"), 0o600); err != nil {
		t.Fatalf("write temp config: %v", err)
	}

	service, err := NewBuilder().
		WithConfig(cfg).
		WithConfigPath(configPath).
		WithCoreAuthManager(coreManager).
		WithWatcherFactory(func(configPath, authDir string, reload func(*config.Config)) (*WatcherWrapper, error) {
			return &WatcherWrapper{
				start: func(ctx context.Context) error { return nil },
				stop:  func() error { return nil },
				setConfig: func(cfg *config.Config) {
					_ = cfg
				},
				snapshotAuths: func() []*coreauth.Auth { return nil },
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
		if reg.ClientSupportsModel(authID, "gpt-5-codex") {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}

	if !reg.ClientSupportsModel(authID, "gpt-5-codex") {
		cancel()
		select {
		case errRun := <-done:
			if errRun != nil && errRun != context.Canceled {
				t.Fatalf("service run returned error: %v", errRun)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("service did not stop after cancel")
		}
		t.Fatal("expected loaded core auth to be registered in model registry")
	}

	if _, errExecute := coreManager.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: "gpt-5-codex"}, cliproxyexecutor.Options{}); errExecute != nil {
		cancel()
		select {
		case errRun := <-done:
			if errRun != nil && errRun != context.Canceled {
				t.Fatalf("service run returned error: %v", errRun)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("service did not stop after cancel")
		}
		t.Fatalf("expected scheduler to route loaded core auth after startup: %v", errExecute)
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

func TestServiceRunBootstrapsPoolStartupFromRootCandidatesOnly(t *testing.T) {
	activeAuthID := "pool-active-auth"
	reserveAuthID := "pool-reserve-auth"
	limitAuthID := "limit/pool-limit-auth.json"

	originalProbe := poolProbeAuthFunc
	poolProbeAuthFunc = func(ctx context.Context, cfg *config.Config, auth *coreauth.Auth) (*coreauth.Auth, coreauth.Result) {
		return auth, coreauth.Result{
			AuthID:   auth.ID,
			Provider: "codex",
			Model:    "gpt-5",
			Success:  true,
		}
	}
	t.Cleanup(func() { poolProbeAuthFunc = originalProbe })

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
					return []*coreauth.Auth{activeAuth.Clone(), reserveAuth.Clone()}
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
		if service.poolManager != nil {
			snapshot := service.poolManager.Snapshot()
			if snapshot.ActiveCount == 1 && snapshot.LimitCount == 1 {
				break
			}
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

func TestBootstrapPoolSnapshot_IndexesColdCandidatesWithoutRetainingHotCopies(t *testing.T) {
	authDir := t.TempDir()
	activePath := filepath.Join(authDir, "pool-active-auth.json")
	reservePath := filepath.Join(authDir, "pool-reserve-auth.json")
	limitDir := filepath.Join(authDir, "limit")
	if err := os.MkdirAll(limitDir, 0o755); err != nil {
		t.Fatalf("mkdir limit dir: %v", err)
	}
	limitPath := filepath.Join(limitDir, "pool-limit-auth.json")

	for path, content := range map[string]string{
		activePath:  `{"type":"codex","email":"active@example.com","api_key":"active","base_url":"https://example.com/v1"}`,
		reservePath: `{"type":"codex","email":"reserve@example.com","api_key":"reserve","base_url":"https://example.com/v1"}`,
		limitPath:   `{"type":"codex","email":"limit@example.com","api_key":"limit","base_url":"https://example.com/v1"}`,
	} {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatalf("write auth file %s: %v", path, err)
		}
	}

	activeAuth := &coreauth.Auth{
		ID:       "pool-active-auth.json",
		Provider: "codex",
		Status:   coreauth.StatusActive,
		Attributes: map[string]string{
			"path": activePath,
		},
		Metadata: map[string]any{
			poolQuotaWeeklyRemainingPercentKey: 95,
		},
	}
	reserveAuth := &coreauth.Auth{
		ID:       "pool-reserve-auth.json",
		Provider: "codex",
		Status:   coreauth.StatusActive,
		Attributes: map[string]string{
			"path": reservePath,
		},
		Metadata: map[string]any{
			poolQuotaWeeklyRemainingPercentKey: 80,
		},
	}
	limitAuth := &coreauth.Auth{
		ID:       "limit/pool-limit-auth.json",
		Provider: "codex",
		Status:   coreauth.StatusActive,
		Attributes: map[string]string{
			"path": limitPath,
		},
	}

	originalProbe := poolProbeAuthFunc
	poolProbeAuthFunc = func(ctx context.Context, cfg *config.Config, auth *coreauth.Auth) (*coreauth.Auth, coreauth.Result) {
		return auth, coreauth.Result{
			AuthID:   auth.ID,
			Provider: "codex",
			Model:    "gpt-5",
			Success:  true,
		}
	}
	t.Cleanup(func() { poolProbeAuthFunc = originalProbe })

	service := &Service{
		cfg: &config.Config{
			AuthDir: authDir,
			PoolManager: config.PoolManagerConfig{
				Size:     1,
				Provider: "codex",
			},
		},
	}

	service.bootstrapPoolSnapshot(context.Background(), &WatcherWrapper{
		snapshotRootFileAuths: func() []*coreauth.Auth {
			return []*coreauth.Auth{activeAuth.Clone(), reserveAuth.Clone()}
		},
		snapshotLimitAuths: func() []*coreauth.Auth {
			return []*coreauth.Auth{limitAuth.Clone()}
		},
	})

	if len(service.poolCandidateIndex) != 3 {
		t.Fatalf("poolCandidateIndex len = %d, want 3", len(service.poolCandidateIndex))
	}
	if len(service.poolCandidates) != 1 {
		t.Fatalf("poolCandidates len = %d, want 1 hot auth", len(service.poolCandidates))
	}
	if _, ok := service.poolCandidates[activeAuth.ID]; !ok {
		t.Fatalf("expected active auth %s to be kept hot", activeAuth.ID)
	}
	if _, ok := service.poolCandidates[reserveAuth.ID]; ok {
		t.Fatalf("did not expect cold reserve auth %s to stay hot", reserveAuth.ID)
	}
	if _, ok := service.poolCandidates[limitAuth.ID]; ok {
		t.Fatalf("did not expect limit auth %s to stay hot", limitAuth.ID)
	}
	if got := service.poolCandidate(reserveAuth.ID); got == nil || got.ID != reserveAuth.ID {
		t.Fatalf("expected lazy load for reserve auth %s, got %+v", reserveAuth.ID, got)
	}
}

func TestServiceRunBootstrapsPoolStartupSkipsUnhealthyRootCandidate(t *testing.T) {
	firstAuthID := "pool-first-unhealthy"
	secondAuthID := "pool-second-healthy"

	reg := GlobalModelRegistry()
	for _, id := range []string{firstAuthID, secondAuthID} {
		reg.UnregisterClient(id)
	}
	t.Cleanup(func() {
		for _, id := range []string{firstAuthID, secondAuthID} {
			reg.UnregisterClient(id)
		}
	})

	originalProbe := poolProbeAuthFunc
	poolProbeAuthFunc = func(ctx context.Context, cfg *config.Config, auth *coreauth.Auth) (*coreauth.Auth, coreauth.Result) {
		if auth != nil && auth.ID == firstAuthID {
			return auth, coreauth.Result{
				AuthID:   firstAuthID,
				Provider: "codex",
				Success:  false,
				Error:    &coreauth.Error{HTTPStatus: 429, Code: "quota_exceeded", Message: "quota exceeded"},
			}
		}
		return auth, coreauth.Result{
			AuthID:   auth.ID,
			Provider: "codex",
			Success:  true,
		}
	}
	t.Cleanup(func() { poolProbeAuthFunc = originalProbe })

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

	firstAuth := &coreauth.Auth{
		ID:       firstAuthID,
		Provider: "codex",
		Status:   coreauth.StatusActive,
		Attributes: map[string]string{
			"api_key":  "first-key",
			"base_url": "https://example.com/v1",
		},
	}
	secondAuth := &coreauth.Auth{
		ID:       secondAuthID,
		Provider: "codex",
		Status:   coreauth.StatusActive,
		Attributes: map[string]string{
			"api_key":  "second-key",
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
					return []*coreauth.Auth{firstAuth.Clone(), secondAuth.Clone()}
				},
				snapshotLimitAuths: func() []*coreauth.Auth { return nil },
				setUpdateQueue:     func(queue chan<- watcher.AuthUpdate) { _ = queue },
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
		if service.poolManager != nil {
			snapshot := service.poolManager.Snapshot()
			if snapshot.ActiveCount == 1 && snapshot.LimitCount == 1 {
				break
			}
		}
		time.Sleep(25 * time.Millisecond)
	}

	snapshot := service.poolManager.Snapshot()
	if snapshot.ActiveCount != 1 || snapshot.ActiveIDs[0] != secondAuthID {
		cancel()
		<-done
		t.Fatalf("unexpected active snapshot: %+v", snapshot)
	}
	if snapshot.LimitCount != 1 || snapshot.LimitIDs[0] != firstAuthID {
		cancel()
		<-done
		t.Fatalf("expected first auth to be moved to limit in pool state, got %+v", snapshot)
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

func TestServiceRunBootstrapsPoolStartupSkipsLowQuotaRootCandidate(t *testing.T) {
	lowQuotaAuthID := "pool-low-quota-root"
	healthyAuthID := "pool-healthy-root"

	originalProbe := poolProbeAuthFunc
	poolProbeAuthFunc = func(ctx context.Context, cfg *config.Config, auth *coreauth.Auth) (*coreauth.Auth, coreauth.Result) {
		if auth == nil {
			return nil, coreauth.Result{Provider: "codex", Success: false}
		}
		auth = auth.Clone()
		if auth.ID == lowQuotaAuthID {
			if auth.Metadata == nil {
				auth.Metadata = make(map[string]any)
			}
			auth.Metadata[poolQuotaWeeklyRemainingPercentKey] = 15
		}
		return auth, coreauth.Result{
			AuthID:   auth.ID,
			Provider: "codex",
			Model:    "gpt-5",
			Success:  true,
		}
	}
	t.Cleanup(func() { poolProbeAuthFunc = originalProbe })

	cfg := &config.Config{
		Host:    "127.0.0.1",
		Port:    0,
		AuthDir: filepath.Join(t.TempDir(), "auth"),
		SDKConfig: config.SDKConfig{
			APIKeys: []string{"test-key"},
		},
		PoolManager: config.PoolManagerConfig{
			Size:                     1,
			Provider:                 "codex",
			LowQuotaThresholdPercent: 20,
		},
	}

	lowQuotaAuth := &coreauth.Auth{ID: lowQuotaAuthID, Provider: "codex", Status: coreauth.StatusActive, Attributes: map[string]string{"api_key": "low-key", "base_url": "https://example.com/v1"}}
	healthyAuth := &coreauth.Auth{ID: healthyAuthID, Provider: "codex", Status: coreauth.StatusActive, Attributes: map[string]string{"api_key": "healthy-key", "base_url": "https://example.com/v1"}}

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
				start:         func(ctx context.Context) error { return nil },
				stop:          func() error { return nil },
				setConfig:     func(cfg *config.Config) { _ = cfg },
				snapshotAuths: func() []*coreauth.Auth { return nil },
				snapshotRootFileAuths: func() []*coreauth.Auth {
					return []*coreauth.Auth{lowQuotaAuth.Clone(), healthyAuth.Clone()}
				},
				snapshotLimitAuths: func() []*coreauth.Auth { return nil },
				setUpdateQueue:     func(queue chan<- watcher.AuthUpdate) { _ = queue },
			}, nil
		}).
		Build()
	if err != nil {
		t.Fatalf("build service: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- service.Run(ctx) }()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if service.poolManager != nil {
			snapshot := service.poolManager.Snapshot()
			if snapshot.ActiveCount == 1 && snapshot.ActiveIDs[0] == healthyAuthID && snapshot.LowQuotaCount == 1 && snapshot.LowQuotaIDs[0] == lowQuotaAuthID {
				break
			}
		}
		time.Sleep(25 * time.Millisecond)
	}

	snapshot := service.poolManager.Snapshot()
	if snapshot.ActiveCount != 1 || snapshot.ActiveIDs[0] != healthyAuthID {
		cancel()
		<-done
		t.Fatalf("unexpected active snapshot: %+v", snapshot)
	}
	if snapshot.LowQuotaCount != 1 || snapshot.LowQuotaIDs[0] != lowQuotaAuthID {
		cancel()
		<-done
		t.Fatalf("expected low quota auth to be isolated, got %+v", snapshot)
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

func TestServiceRunBootstrapsPoolStartupAllowsLowQuotaFallbackActiveWhenNoHealthyFound(t *testing.T) {
	lowQuotaAuthID := "pool-low-quota-fallback-root"

	originalProbe := poolProbeAuthFunc
	poolProbeAuthFunc = func(ctx context.Context, cfg *config.Config, auth *coreauth.Auth) (*coreauth.Auth, coreauth.Result) {
		if auth == nil {
			return nil, coreauth.Result{Provider: "codex", Success: false}
		}
		auth = auth.Clone()
		if auth.Metadata == nil {
			auth.Metadata = make(map[string]any)
		}
		auth.Metadata[poolQuotaWeeklyRemainingPercentKey] = 0
		return auth, coreauth.Result{
			AuthID:   auth.ID,
			Provider: "codex",
			Model:    "gpt-5",
			Success:  true,
		}
	}
	t.Cleanup(func() { poolProbeAuthFunc = originalProbe })

	cfg := &config.Config{
		Host:    "127.0.0.1",
		Port:    0,
		AuthDir: filepath.Join(t.TempDir(), "auth"),
		SDKConfig: config.SDKConfig{
			APIKeys: []string{"test-key"},
		},
		PoolManager: config.PoolManagerConfig{
			Size:                     1,
			Provider:                 "codex",
			LowQuotaThresholdPercent: 20,
		},
	}

	lowQuotaAuth := &coreauth.Auth{
		ID:       lowQuotaAuthID,
		Provider: "codex",
		Status:   coreauth.StatusActive,
		Attributes: map[string]string{
			"api_key":  "low-key",
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
				start:         func(ctx context.Context) error { return nil },
				stop:          func() error { return nil },
				setConfig:     func(cfg *config.Config) { _ = cfg },
				snapshotAuths: func() []*coreauth.Auth { return nil },
				snapshotRootFileAuths: func() []*coreauth.Auth {
					return []*coreauth.Auth{lowQuotaAuth.Clone()}
				},
				snapshotLimitAuths: func() []*coreauth.Auth { return nil },
				setUpdateQueue:     func(queue chan<- watcher.AuthUpdate) { _ = queue },
			}, nil
		}).
		Build()
	if err != nil {
		t.Fatalf("build service: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- service.Run(ctx) }()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if service.poolManager != nil && service.poolManager.Snapshot().ActiveCount == 1 {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}

	snapshot := service.poolManager.Snapshot()
	if snapshot.ActiveCount != 1 || snapshot.ActiveIDs[0] != lowQuotaAuthID {
		cancel()
		<-done
		t.Fatalf("expected low quota auth to serve as active fallback, got %+v", snapshot)
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

func TestHandleAuthDisposition_RemovesActiveAndPromotesHealthyReserve(t *testing.T) {
	activeAuthID := "pool-active-auth-disposition"
	firstReserveAuthID := "pool-reserve-auth-disposition-bad"
	secondReserveAuthID := "pool-reserve-auth-disposition-good"

	reg := GlobalModelRegistry()
	for _, id := range []string{activeAuthID, firstReserveAuthID, secondReserveAuthID} {
		reg.UnregisterClient(id)
	}
	t.Cleanup(func() {
		for _, id := range []string{activeAuthID, firstReserveAuthID, secondReserveAuthID} {
			reg.UnregisterClient(id)
		}
	})

	originalProbe := poolProbeAuthFunc
	poolProbeAuthFunc = func(ctx context.Context, cfg *config.Config, auth *coreauth.Auth) (*coreauth.Auth, coreauth.Result) {
		switch auth.ID {
		case firstReserveAuthID:
			return auth, coreauth.Result{
				AuthID:   auth.ID,
				Provider: "codex",
				Success:  false,
				Error:    &coreauth.Error{HTTPStatus: 429, Code: "quota_exceeded", Message: "quota exceeded"},
			}
		case secondReserveAuthID:
			return auth, coreauth.Result{
				AuthID:   auth.ID,
				Provider: "codex",
				Success:  true,
			}
		default:
			return auth, coreauth.Result{
				AuthID:   auth.ID,
				Provider: "codex",
				Success:  true,
			}
		}
	}
	t.Cleanup(func() { poolProbeAuthFunc = originalProbe })

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
			firstReserveAuthID: {
				ID:       firstReserveAuthID,
				Provider: "codex",
				Status:   coreauth.StatusActive,
				Attributes: map[string]string{
					"api_key":  "reserve-bad-key",
					"base_url": "https://example.com/v1",
				},
			},
			secondReserveAuthID: {
				ID:       secondReserveAuthID,
				Provider: "codex",
				Status:   coreauth.StatusActive,
				Attributes: map[string]string{
					"api_key":  "reserve-good-key",
					"base_url": "https://example.com/v1",
				},
			},
		},
		coreManager: coreauth.NewManager(nil, nil, nil),
	}
	service.poolManager.SetActive(PoolMember{AuthID: activeAuthID, Provider: "codex"})
	service.poolManager.SetReserve(PoolMember{AuthID: firstReserveAuthID, Provider: "codex"})
	service.poolManager.SetReserve(PoolMember{AuthID: secondReserveAuthID, Provider: "codex"})
	service.syncPoolActiveToRuntime(context.Background())

	if !reg.ClientSupportsModel(activeAuthID, "gpt-5") {
		t.Fatal("expected initial active auth to be published")
	}
	if reg.ClientSupportsModel(firstReserveAuthID, "gpt-5") {
		t.Fatal("did not expect first reserve auth to be published before promotion")
	}
	if reg.ClientSupportsModel(secondReserveAuthID, "gpt-5") {
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
	if snapshot.ActiveCount != 1 || snapshot.ActiveIDs[0] != secondReserveAuthID {
		t.Fatalf("unexpected active snapshot after disposition: %+v", snapshot)
	}
	if reg.ClientSupportsModel(activeAuthID, "gpt-5") {
		t.Fatal("expected deleted active auth to be removed from runtime")
	}
	if !reg.ClientSupportsModel(secondReserveAuthID, "gpt-5") {
		t.Fatal("expected healthy reserve auth to be promoted into runtime")
	}
	if reg.ClientSupportsModel(firstReserveAuthID, "gpt-5") {
		t.Fatal("did not expect unhealthy reserve auth to be promoted into runtime")
	}
	if snapshot.LimitCount != 1 || snapshot.LimitIDs[0] != firstReserveAuthID {
		t.Fatalf("expected unhealthy reserve auth to move to limit state, got %+v", snapshot)
	}
}

func TestRunActiveProbeCycle_RemovesQuotaAuthFromActive(t *testing.T) {
	activeAuthID := "pool-active-probe"
	reserveAuthID := "pool-reserve-probe"

	reg := GlobalModelRegistry()
	for _, id := range []string{activeAuthID, reserveAuthID} {
		reg.UnregisterClient(id)
	}
	t.Cleanup(func() {
		for _, id := range []string{activeAuthID, reserveAuthID} {
			reg.UnregisterClient(id)
		}
	})

	originalProbe := poolProbeAuthFunc
	poolProbeAuthFunc = func(ctx context.Context, cfg *config.Config, auth *coreauth.Auth) (*coreauth.Auth, coreauth.Result) {
		switch auth.ID {
		case activeAuthID:
			return auth, coreauth.Result{
				AuthID:   auth.ID,
				Provider: "codex",
				Model:    "gpt-5",
				Success:  false,
				Error:    &coreauth.Error{HTTPStatus: 429, Code: "quota_exceeded", Message: "quota exceeded"},
			}
		case reserveAuthID:
			return auth, coreauth.Result{
				AuthID:   auth.ID,
				Provider: "codex",
				Model:    "gpt-5",
				Success:  true,
			}
		default:
			return auth, coreauth.Result{
				AuthID:   auth.ID,
				Provider: "codex",
				Model:    "gpt-5",
				Success:  true,
			}
		}
	}
	t.Cleanup(func() { poolProbeAuthFunc = originalProbe })

	service := &Service{
		cfg: &config.Config{
			PoolManager: config.PoolManagerConfig{
				Size:                          1,
				Provider:                      "codex",
				ActiveIdleScanIntervalSeconds: 1800,
			},
		},
		poolManager: NewPoolManager(config.PoolManagerConfig{
			Size:                          1,
			Provider:                      "codex",
			ActiveIdleScanIntervalSeconds: 1800,
		}),
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
		coreManager: coreauth.NewManager(nil, nil, &serviceAuthHook{}),
	}
	service.coreManager.SetHook(&serviceAuthHook{service: service})
	service.poolManager.SetActive(PoolMember{AuthID: activeAuthID, Provider: "codex"})
	service.poolManager.SetReserve(PoolMember{AuthID: reserveAuthID, Provider: "codex"})
	service.syncPoolActiveToRuntime(context.Background())

	service.runActiveProbeCycle(context.Background(), time.Now().Add(2*time.Hour))

	snapshot := service.poolManager.Snapshot()
	if snapshot.ActiveCount != 1 || snapshot.ActiveIDs[0] != reserveAuthID {
		t.Fatalf("unexpected active snapshot after probe cycle: %+v", snapshot)
	}
	if snapshot.LimitCount != 1 || snapshot.LimitIDs[0] != activeAuthID {
		t.Fatalf("expected original active auth to move to limit, got %+v", snapshot)
	}
	if !reg.ClientSupportsModel(reserveAuthID, "gpt-5") {
		t.Fatal("expected reserve auth to be promoted to runtime after active probe failure")
	}
}

func TestHandleAuthDisposition_RebalancesAsynchronously(t *testing.T) {
	activeAuthID := "pool-active-async"
	reserveAuthID := "pool-reserve-async"

	originalProbe := poolProbeAuthFunc
	blockProbe := make(chan struct{})
	poolProbeAuthFunc = func(ctx context.Context, cfg *config.Config, auth *coreauth.Auth) (*coreauth.Auth, coreauth.Result) {
		if auth != nil && auth.ID == reserveAuthID {
			<-blockProbe
			return auth, coreauth.Result{
				AuthID:   auth.ID,
				Provider: "codex",
				Model:    "gpt-5",
				Success:  true,
			}
		}
		return auth, coreauth.Result{
			AuthID:   auth.ID,
			Provider: "codex",
			Model:    "gpt-5",
			Success:  true,
		}
	}
	t.Cleanup(func() { poolProbeAuthFunc = originalProbe })

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
		poolMetrics: NewPoolMetrics(config.PoolManagerConfig{Size: 1, Provider: "codex"}),
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

	workerCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	service.startPoolRebalanceWorker(workerCtx)

	start := time.Now()
	service.handleAuthDisposition(context.Background(), coreauth.AuthDisposition{
		AuthID:       activeAuthID,
		Provider:     "codex",
		Healthy:      false,
		PoolEligible: false,
		Deleted:      true,
		Source:       "request",
	})
	if time.Since(start) > 150*time.Millisecond {
		t.Fatalf("handleAuthDisposition blocked on rebalance")
	}

	snapshot := service.poolManager.Snapshot()
	if snapshot.ActiveCount != 0 {
		t.Fatalf("expected active pool to be empty before async rebalance completes, got %+v", snapshot)
	}
	if reg.ClientSupportsModel(activeAuthID, "gpt-5") {
		t.Fatal("expected removed auth to be unpublished immediately")
	}
	if reg.ClientSupportsModel(reserveAuthID, "gpt-5") {
		t.Fatal("did not expect reserve auth to be promoted before async worker finishes")
	}

	close(blockProbe)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if reg.ClientSupportsModel(reserveAuthID, "gpt-5") {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("expected async rebalance worker to promote reserve auth")
}

func TestRunReserveProbeCycle_ArchivesQuotaReserveToLimitState(t *testing.T) {
	reserveAuthID := "pool-reserve-probe-quota"

	originalProbe := poolProbeAuthFunc
	poolProbeAuthFunc = func(ctx context.Context, cfg *config.Config, auth *coreauth.Auth) (*coreauth.Auth, coreauth.Result) {
		return auth, coreauth.Result{
			AuthID:   auth.ID,
			Provider: "codex",
			Model:    "gpt-5",
			Success:  false,
			Error:    &coreauth.Error{HTTPStatus: 429, Code: "quota_exceeded", Message: "quota exceeded"},
		}
	}
	t.Cleanup(func() { poolProbeAuthFunc = originalProbe })

	originalShuffle := poolShuffleStringsFunc
	poolShuffleStringsFunc = func(values []string) {}
	t.Cleanup(func() { poolShuffleStringsFunc = originalShuffle })

	service := &Service{
		cfg: &config.Config{
			PoolManager: config.PoolManagerConfig{
				Size:                       1,
				Provider:                   "codex",
				ReserveScanIntervalSeconds: 300,
				ReserveSampleSize:          5,
			},
		},
		poolManager: NewPoolManager(config.PoolManagerConfig{
			Size:                       1,
			Provider:                   "codex",
			ReserveScanIntervalSeconds: 300,
			ReserveSampleSize:          5,
		}),
		poolCandidates: map[string]*coreauth.Auth{
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
	}
	service.poolManager.SetReserve(PoolMember{AuthID: reserveAuthID, Provider: "codex"})

	service.runReserveProbeCycle(context.Background(), time.Now().Add(time.Hour))

	snapshot := service.poolManager.Snapshot()
	if snapshot.ReserveCount != 0 {
		t.Fatalf("expected reserve to be emptied, got %+v", snapshot)
	}
	if snapshot.LimitCount != 1 || snapshot.LimitIDs[0] != reserveAuthID {
		t.Fatalf("expected reserve auth to move to limit, got %+v", snapshot)
	}
}

func TestRunReserveProbeCycle_MovesLowQuotaReserveToLowQuotaState(t *testing.T) {
	reserveAuthID := "pool-reserve-probe-low-quota"

	originalProbe := poolProbeAuthFunc
	poolProbeAuthFunc = func(ctx context.Context, cfg *config.Config, auth *coreauth.Auth) (*coreauth.Auth, coreauth.Result) {
		auth = auth.Clone()
		if auth.Metadata == nil {
			auth.Metadata = make(map[string]any)
		}
		auth.Metadata[poolQuotaWeeklyRemainingPercentKey] = 12
		return auth, coreauth.Result{
			AuthID:   auth.ID,
			Provider: "codex",
			Model:    "gpt-5",
			Success:  true,
		}
	}
	t.Cleanup(func() { poolProbeAuthFunc = originalProbe })

	originalShuffle := poolShuffleStringsFunc
	poolShuffleStringsFunc = func(values []string) {}
	t.Cleanup(func() { poolShuffleStringsFunc = originalShuffle })

	service := &Service{
		cfg: &config.Config{
			PoolManager: config.PoolManagerConfig{
				Size:                       1,
				Provider:                   "codex",
				ReserveScanIntervalSeconds: 300,
				ReserveSampleSize:          5,
				LowQuotaThresholdPercent:   20,
			},
		},
		poolManager: NewPoolManager(config.PoolManagerConfig{
			Size:                       1,
			Provider:                   "codex",
			ReserveScanIntervalSeconds: 300,
			ReserveSampleSize:          5,
			LowQuotaThresholdPercent:   20,
		}),
		poolCandidates: map[string]*coreauth.Auth{
			reserveAuthID: {
				ID:         reserveAuthID,
				Provider:   "codex",
				Status:     coreauth.StatusActive,
				Attributes: map[string]string{"api_key": "reserve-key", "base_url": "https://example.com/v1"},
			},
		},
	}
	service.poolManager.SetReserve(PoolMember{AuthID: reserveAuthID, Provider: "codex"})

	service.runReserveProbeCycle(context.Background(), time.Now().Add(time.Hour))

	snapshot := service.poolManager.Snapshot()
	if snapshot.ReserveCount != 0 {
		t.Fatalf("expected reserve to be emptied, got %+v", snapshot)
	}
	if snapshot.ActiveCount != 1 || snapshot.ActiveIDs[0] != reserveAuthID {
		t.Fatalf("expected low quota reserve auth to serve as active fallback while underfilled, got %+v", snapshot)
	}
}

func TestRunLimitProbeCycle_RestoresHealthyLimitToReserve(t *testing.T) {
	limitAuthID := "pool-limit-probe-restore"

	originalProbe := poolProbeAuthFunc
	poolProbeAuthFunc = func(ctx context.Context, cfg *config.Config, auth *coreauth.Auth) (*coreauth.Auth, coreauth.Result) {
		return auth, coreauth.Result{
			AuthID:   auth.ID,
			Provider: "codex",
			Model:    "gpt-5",
			Success:  true,
		}
	}
	t.Cleanup(func() { poolProbeAuthFunc = originalProbe })

	service := &Service{
		cfg: &config.Config{
			PoolManager: config.PoolManagerConfig{
				Size:                       1,
				Provider:                   "codex",
				LimitScanIntervalSeconds:   3600,
				ReserveScanIntervalSeconds: 300,
			},
		},
		poolManager: NewPoolManager(config.PoolManagerConfig{
			Size:                       1,
			Provider:                   "codex",
			LimitScanIntervalSeconds:   3600,
			ReserveScanIntervalSeconds: 300,
		}),
		poolCandidates: map[string]*coreauth.Auth{
			limitAuthID: {
				ID:       limitAuthID,
				Provider: "codex",
				Status:   coreauth.StatusActive,
				Attributes: map[string]string{
					"api_key":  "limit-key",
					"base_url": "https://example.com/v1",
				},
			},
		},
	}
	service.poolManager.SetLimit(PoolMember{AuthID: limitAuthID, Provider: "codex"})

	service.runLimitProbeCycle(context.Background(), time.Now().Add(2*time.Hour))

	snapshot := service.poolManager.Snapshot()
	if snapshot.LimitCount != 0 {
		t.Fatalf("expected limit to be emptied, got %+v", snapshot)
	}
	if snapshot.ReserveCount != 0 {
		t.Fatalf("expected restored auth to be promoted into active because pool is underfilled, got %+v", snapshot)
	}
	if snapshot.ActiveCount != 1 || snapshot.ActiveIDs[0] != limitAuthID {
		t.Fatalf("expected restored limit auth to end up active after refill, got %+v", snapshot)
	}
}

func TestRunActiveProbeCycle_DemotesLowQuotaActive(t *testing.T) {
	activeAuthID := "pool-active-low-quota"
	reserveAuthID := "pool-reserve-after-low-quota"

	reg := GlobalModelRegistry()
	for _, id := range []string{activeAuthID, reserveAuthID} {
		reg.UnregisterClient(id)
	}
	t.Cleanup(func() {
		for _, id := range []string{activeAuthID, reserveAuthID} {
			reg.UnregisterClient(id)
		}
	})

	originalProbe := poolProbeAuthFunc
	poolProbeAuthFunc = func(ctx context.Context, cfg *config.Config, auth *coreauth.Auth) (*coreauth.Auth, coreauth.Result) {
		auth = auth.Clone()
		if auth.ID == activeAuthID {
			if auth.Metadata == nil {
				auth.Metadata = make(map[string]any)
			}
			auth.Metadata[poolQuotaWeeklyRemainingPercentKey] = 18
			return auth, coreauth.Result{AuthID: auth.ID, Provider: "codex", Model: "gpt-5", Success: true}
		}
		return auth, coreauth.Result{AuthID: auth.ID, Provider: "codex", Model: "gpt-5", Success: true}
	}
	t.Cleanup(func() { poolProbeAuthFunc = originalProbe })

	service := &Service{
		cfg: &config.Config{
			PoolManager: config.PoolManagerConfig{
				Size:                          1,
				Provider:                      "codex",
				ActiveIdleScanIntervalSeconds: 1800,
				LowQuotaThresholdPercent:      20,
			},
		},
		poolManager: NewPoolManager(config.PoolManagerConfig{
			Size:                          1,
			Provider:                      "codex",
			ActiveIdleScanIntervalSeconds: 1800,
			LowQuotaThresholdPercent:      20,
		}),
		poolMetrics: NewPoolMetrics(config.PoolManagerConfig{Size: 1, Provider: "codex"}),
		poolCandidates: map[string]*coreauth.Auth{
			activeAuthID:  {ID: activeAuthID, Provider: "codex", Status: coreauth.StatusActive, Attributes: map[string]string{"api_key": "active-key", "base_url": "https://example.com/v1"}},
			reserveAuthID: {ID: reserveAuthID, Provider: "codex", Status: coreauth.StatusActive, Attributes: map[string]string{"api_key": "reserve-key", "base_url": "https://example.com/v1"}},
		},
		coreManager: coreauth.NewManager(nil, nil, &serviceAuthHook{}),
	}
	service.coreManager.SetHook(&serviceAuthHook{service: service})
	service.poolManager.SetActive(PoolMember{AuthID: activeAuthID, Provider: "codex"})
	service.poolManager.SetReserve(PoolMember{AuthID: reserveAuthID, Provider: "codex"})
	service.syncPoolActiveToRuntime(context.Background())
	service.startPoolRebalanceWorker(context.Background())

	service.runActiveProbeCycle(context.Background(), time.Now().Add(2*time.Hour))

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		snapshot := service.poolManager.Snapshot()
		if snapshot.ActiveCount == 1 && snapshot.ActiveIDs[0] == reserveAuthID {
			if snapshot.LowQuotaCount == 1 && snapshot.LowQuotaIDs[0] == activeAuthID {
				metrics := service.poolMetrics.Snapshot(snapshot, len(service.publishedActive))
				if metrics.ActiveRemovedTotal != 1 {
					t.Fatalf("expected active removal metric to increment, got %+v", metrics)
				}
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("expected low quota active to be demoted and reserve promoted, got %+v", service.poolManager.Snapshot())
}

func TestRunActiveProbeCycle_KeepsLowQuotaFallbackWhenNoReserveAvailable(t *testing.T) {
	activeAuthID := "pool-active-low-quota-keep"

	originalProbe := poolProbeAuthFunc
	poolProbeAuthFunc = func(ctx context.Context, cfg *config.Config, auth *coreauth.Auth) (*coreauth.Auth, coreauth.Result) {
		auth = auth.Clone()
		if auth.Metadata == nil {
			auth.Metadata = make(map[string]any)
		}
		auth.Metadata[poolQuotaWeeklyRemainingPercentKey] = 10
		return auth, coreauth.Result{AuthID: auth.ID, Provider: "codex", Model: "gpt-5", Success: true}
	}
	t.Cleanup(func() { poolProbeAuthFunc = originalProbe })

	service := &Service{
		cfg: &config.Config{
			PoolManager: config.PoolManagerConfig{
				Size:                          1,
				Provider:                      "codex",
				ActiveIdleScanIntervalSeconds: 1800,
				LowQuotaThresholdPercent:      20,
			},
		},
		poolManager: NewPoolManager(config.PoolManagerConfig{
			Size:                          1,
			Provider:                      "codex",
			ActiveIdleScanIntervalSeconds: 1800,
			LowQuotaThresholdPercent:      20,
		}),
		poolMetrics: NewPoolMetrics(config.PoolManagerConfig{Size: 1, Provider: "codex"}),
		poolCandidates: map[string]*coreauth.Auth{
			activeAuthID: {ID: activeAuthID, Provider: "codex", Status: coreauth.StatusActive, Attributes: map[string]string{"api_key": "active-key", "base_url": "https://example.com/v1"}},
		},
		coreManager: coreauth.NewManager(nil, nil, &serviceAuthHook{}),
	}
	service.coreManager.SetHook(&serviceAuthHook{service: service})
	service.poolManager.SetActive(PoolMember{AuthID: activeAuthID, Provider: "codex"})
	service.syncPoolActiveToRuntime(context.Background())

	service.runActiveProbeCycle(context.Background(), time.Now().Add(2*time.Hour))

	snapshot := service.poolManager.Snapshot()
	if snapshot.ActiveCount != 1 || snapshot.ActiveIDs[0] != activeAuthID {
		t.Fatalf("expected low quota fallback active to be kept when no reserve exists, got %+v", snapshot)
	}
	if snapshot.LowQuotaCount != 0 {
		t.Fatalf("expected no low_quota demotion without replacement, got %+v", snapshot)
	}
}

func TestRunPoolRebalanceNow_ReplacesFallbackActiveAndCapsWarmReserve(t *testing.T) {
	fallbackActiveID := "pool-fallback-active"
	healthyActiveID := "pool-healthy-upgrade"
	healthyReserveID := "pool-healthy-reserve"
	extraHealthyID := "pool-extra-healthy"

	originalProbe := poolProbeAuthFunc
	poolProbeAuthFunc = func(ctx context.Context, cfg *config.Config, auth *coreauth.Auth) (*coreauth.Auth, coreauth.Result) {
		auth = auth.Clone()
		if auth.Metadata == nil {
			auth.Metadata = make(map[string]any)
		}
		switch auth.ID {
		case fallbackActiveID:
			auth.Metadata[poolQuotaWeeklyRemainingPercentKey] = 5
		case healthyActiveID:
			auth.Metadata[poolQuotaWeeklyRemainingPercentKey] = 90
		case healthyReserveID:
			auth.Metadata[poolQuotaWeeklyRemainingPercentKey] = 80
		case extraHealthyID:
			auth.Metadata[poolQuotaWeeklyRemainingPercentKey] = 70
		}
		return auth, coreauth.Result{AuthID: auth.ID, Provider: "codex", Model: "gpt-5", Success: true}
	}
	t.Cleanup(func() { poolProbeAuthFunc = originalProbe })

	service := &Service{
		cfg: &config.Config{
			PoolManager: config.PoolManagerConfig{
				Size:                     1,
				Provider:                 "codex",
				ReserveSampleSize:        2,
				LowQuotaThresholdPercent: 20,
			},
		},
		poolManager: NewPoolManager(config.PoolManagerConfig{
			Size:                     1,
			Provider:                 "codex",
			ReserveSampleSize:        2,
			LowQuotaThresholdPercent: 20,
		}),
		poolMetrics: NewPoolMetrics(config.PoolManagerConfig{Size: 1, Provider: "codex"}),
		poolCandidates: map[string]*coreauth.Auth{
			fallbackActiveID: {ID: fallbackActiveID, Provider: "codex", Status: coreauth.StatusActive, Attributes: map[string]string{"api_key": "a", "base_url": "https://example.com/v1"}},
			healthyActiveID:  {ID: healthyActiveID, Provider: "codex", Status: coreauth.StatusActive, Attributes: map[string]string{"api_key": "b", "base_url": "https://example.com/v1"}},
			healthyReserveID: {ID: healthyReserveID, Provider: "codex", Status: coreauth.StatusActive, Attributes: map[string]string{"api_key": "c", "base_url": "https://example.com/v1"}},
			extraHealthyID:   {ID: extraHealthyID, Provider: "codex", Status: coreauth.StatusActive, Attributes: map[string]string{"api_key": "d", "base_url": "https://example.com/v1"}},
		},
		coreManager: coreauth.NewManager(nil, nil, &serviceAuthHook{}),
	}
	service.coreManager.SetHook(&serviceAuthHook{service: service})
	service.poolCandidateOrder = []string{fallbackActiveID, healthyActiveID, healthyReserveID, extraHealthyID}
	service.poolManager.SetActive(PoolMember{AuthID: fallbackActiveID, Provider: "codex", RemainingPercent: 5})

	service.runPoolRebalanceNow(context.Background())

	snapshot := service.poolManager.Snapshot()
	if snapshot.ActiveCount != 1 {
		t.Fatalf("expected exactly one active auth after rebalance, got %+v", snapshot)
	}
	if snapshot.ActiveIDs[0] == fallbackActiveID {
		t.Fatalf("expected fallback active to be replaced, got %+v", snapshot)
	}
	if snapshot.ReserveCount != 1 {
		t.Fatalf("expected warm reserve to keep a single healthy buffer, got %+v", snapshot)
	}
	if snapshot.ReserveIDs[0] == fallbackActiveID {
		t.Fatalf("expected reserve to contain a healthy auth, got %+v", snapshot)
	}
	if snapshot.LowQuotaCount != 1 || snapshot.LowQuotaIDs[0] != fallbackActiveID {
		t.Fatalf("expected fallback active to be demoted to low_quota, got %+v", snapshot)
	}
	if snapshot.ReserveCount > snapshot.TargetSize {
		t.Fatalf("expected reserve to be capped to target size, got %+v", snapshot)
	}
}

func TestRunActiveProbeCycle_DoesNotPersistPoolQuotaMetadata(t *testing.T) {
	activeAuthID := "pool-active-persist-clean"
	store := &captureAuthStore{}

	originalProbe := poolProbeAuthFunc
	poolProbeAuthFunc = func(ctx context.Context, cfg *config.Config, auth *coreauth.Auth) (*coreauth.Auth, coreauth.Result) {
		auth = auth.Clone()
		if auth.Metadata == nil {
			auth.Metadata = make(map[string]any)
		}
		auth.Metadata[poolQuotaWeeklyRemainingPercentKey] = 60
		auth.Metadata[poolQuotaWeeklyUsedPercentKey] = 40
		auth.Metadata[poolQuotaPlanTypeKey] = "Free"
		auth.Metadata[poolQuotaWeeklyResetAtKey] = time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339)
		return auth, coreauth.Result{AuthID: auth.ID, Provider: "codex", Model: "gpt-5", Success: true}
	}
	t.Cleanup(func() { poolProbeAuthFunc = originalProbe })

	manager := coreauth.NewManager(store, nil, &serviceAuthHook{})
	auth := &coreauth.Auth{
		ID:       activeAuthID,
		Provider: "codex",
		Status:   coreauth.StatusActive,
		Metadata: map[string]any{"type": "codex", "email": "demo@example.com"},
		Attributes: map[string]string{
			"api_key":  "active-key",
			"base_url": "https://example.com/v1",
		},
	}
	if _, err := manager.Register(coreauth.WithSkipPersist(context.Background()), auth); err != nil {
		t.Fatalf("register auth: %v", err)
	}

	service := &Service{
		cfg: &config.Config{
			PoolManager: config.PoolManagerConfig{
				Size:                          1,
				Provider:                      "codex",
				ActiveIdleScanIntervalSeconds: 1800,
				LowQuotaThresholdPercent:      20,
			},
		},
		poolManager: NewPoolManager(config.PoolManagerConfig{
			Size:                          1,
			Provider:                      "codex",
			ActiveIdleScanIntervalSeconds: 1800,
			LowQuotaThresholdPercent:      20,
		}),
		poolMetrics:    NewPoolMetrics(config.PoolManagerConfig{Size: 1, Provider: "codex"}),
		poolCandidates: map[string]*coreauth.Auth{activeAuthID: auth.Clone()},
		coreManager:    manager,
	}
	service.coreManager.SetHook(&serviceAuthHook{service: service})
	service.poolManager.SetActive(PoolMember{AuthID: activeAuthID, Provider: "codex"})
	service.syncPoolActiveToRuntime(context.Background())

	service.runActiveProbeCycle(context.Background(), time.Now().Add(2*time.Hour))

	if store.saveCnt != 0 {
		t.Fatalf("expected active probe update to skip persistence, got saveCnt=%d lastAuth=%+v", store.saveCnt, store.lastAuth)
	}
}

func TestSyncPoolActiveToRuntime_LogsPoolPublish(t *testing.T) {
	activeAuthID := "pool-log-active-auth"

	reg := GlobalModelRegistry()
	reg.UnregisterClient(activeAuthID)
	t.Cleanup(func() {
		reg.UnregisterClient(activeAuthID)
	})

	var buf bytes.Buffer
	oldOutput := log.StandardLogger().Out
	oldFormatter := log.StandardLogger().Formatter
	oldLevel := log.GetLevel()
	log.SetOutput(&buf)
	log.SetFormatter(&log.TextFormatter{DisableTimestamp: true, DisableColors: true})
	log.SetLevel(log.InfoLevel)
	t.Cleanup(func() {
		log.SetOutput(oldOutput)
		log.SetFormatter(oldFormatter)
		log.SetLevel(oldLevel)
	})

	service := &Service{
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
		},
		coreManager: coreauth.NewManager(nil, nil, nil),
	}
	service.poolManager.SetActive(PoolMember{AuthID: activeAuthID, Provider: "codex"})

	service.syncPoolActiveToRuntime(context.Background())

	logOutput := buf.String()
	if !strings.Contains(logOutput, "pool-publish: add auth="+activeAuthID+" provider=codex") {
		t.Fatalf("expected per-auth pool publish log, got %q", logOutput)
	}
	if !strings.Contains(logOutput, "pool-publish: completed add=1 modify=0 delete=0") {
		t.Fatalf("expected pool publish summary log, got %q", logOutput)
	}
}

func TestRunPoolEvalCycle_LogsWindowDeltas(t *testing.T) {
	var buf bytes.Buffer
	oldOutput := log.StandardLogger().Out
	oldFormatter := log.StandardLogger().Formatter
	oldLevel := log.GetLevel()
	log.SetOutput(&buf)
	log.SetFormatter(&log.TextFormatter{DisableTimestamp: true, DisableColors: true})
	log.SetLevel(log.InfoLevel)
	t.Cleanup(func() {
		log.SetOutput(oldOutput)
		log.SetFormatter(oldFormatter)
		log.SetLevel(oldLevel)
	})

	stats := internalusage.NewRequestStatistics()
	service := &Service{
		usageStats:      stats,
		poolManager:     NewPoolManager(config.PoolManagerConfig{Size: 2, Provider: "codex"}),
		poolMetrics:     NewPoolMetrics(config.PoolManagerConfig{Size: 2, Provider: "codex"}),
		publishedActive: map[string]time.Time{},
	}
	service.poolManager.SetActive(PoolMember{AuthID: "a-1", Provider: "codex"})
	service.poolManager.SetReserve(PoolMember{AuthID: "r-1", Provider: "codex"})
	service.poolManager.SetLimit(PoolMember{AuthID: "l-1", Provider: "codex"})

	start := time.Unix(1_700_000_000, 0).UTC()
	service.runPoolEvalCycle(start)
	if !strings.Contains(buf.String(), "pool-eval: baseline interval=5m0s total_requests=0 success=0 failure=0 success_rate=0.00% active_size=1 reserve_size=1 low_quota_size=0 limit_size=1") {
		t.Fatalf("expected baseline log on first cycle, got %q", buf.String())
	}

	stats.Record(context.Background(), coreusage.Record{Provider: "codex", Model: "gpt-5", RequestedAt: start.Add(time.Minute)})
	stats.Record(context.Background(), coreusage.Record{Provider: "codex", Model: "gpt-5", RequestedAt: start.Add(2 * time.Minute)})
	stats.Record(context.Background(), coreusage.Record{Provider: "codex", Model: "gpt-5", RequestedAt: start.Add(3 * time.Minute), Failed: true})
	service.poolMetrics.RecordPromotion()
	service.poolMetrics.RecordActiveRemoval()
	service.poolMetrics.RecordMovedToLimit()
	service.poolMetrics.RecordRefreshed()

	service.runPoolEvalCycle(start.Add(5 * time.Minute))

	logOutput := buf.String()
	if !strings.Contains(logOutput, "pool-eval: window=5m0s total_requests=3 success=2 failure=1 success_rate=66.67% active_size=1 reserve_size=1 low_quota_size=0 limit_size=1") {
		t.Fatalf("expected request delta log, got %q", logOutput)
	}
	if !strings.Contains(logOutput, "pool-eval: window=5m0s active_removed=1 promoted=1 refreshed=1 moved_to_limit=1 deleted=0 restored=0") {
		t.Fatalf("expected churn delta log, got %q", logOutput)
	}
}

func TestLogPoolEvaluation_IncludesCandidateCounts(t *testing.T) {
	var buf bytes.Buffer
	oldOutput := log.StandardLogger().Out
	oldFormatter := log.StandardLogger().Formatter
	oldLevel := log.GetLevel()
	log.SetOutput(&buf)
	log.SetFormatter(&log.TextFormatter{DisableTimestamp: true, DisableColors: true})
	log.SetLevel(log.InfoLevel)
	t.Cleanup(func() {
		log.SetOutput(oldOutput)
		log.SetFormatter(oldFormatter)
		log.SetLevel(oldLevel)
	})

	service := &Service{
		poolManager:     NewPoolManager(config.PoolManagerConfig{Size: 2, Provider: "codex"}),
		poolMetrics:     NewPoolMetrics(config.PoolManagerConfig{Size: 2, Provider: "codex"}),
		publishedActive: map[string]time.Time{},
		poolCandidates: map[string]*coreauth.Auth{
			"a-1": {ID: "a-1", Provider: "codex"},
			"r-1": {ID: "r-1", Provider: "codex"},
			"c-1": {ID: "c-1", Provider: "codex"},
		},
	}
	service.poolManager.SetActive(PoolMember{AuthID: "a-1", Provider: "codex"})
	service.poolManager.SetReserve(PoolMember{AuthID: "r-1", Provider: "codex"})

	service.logPoolEvaluation()

	logOutput := buf.String()
	if !strings.Contains(logOutput, "candidate_size=3") {
		t.Fatalf("expected candidate_size in pool-eval log, got %q", logOutput)
	}
	if !strings.Contains(logOutput, "cold_candidate_size=1") {
		t.Fatalf("expected cold_candidate_size in pool-eval log, got %q", logOutput)
	}
}

func TestSetPoolMemberState_LogsTransitions(t *testing.T) {
	var buf bytes.Buffer
	oldOutput := log.StandardLogger().Out
	oldFormatter := log.StandardLogger().Formatter
	oldLevel := log.GetLevel()
	log.SetOutput(&buf)
	log.SetFormatter(&log.TextFormatter{DisableTimestamp: true, DisableColors: true})
	log.SetLevel(log.InfoLevel)
	t.Cleanup(func() {
		log.SetOutput(oldOutput)
		log.SetFormatter(oldFormatter)
		log.SetLevel(oldLevel)
	})

	service := &Service{
		poolManager: NewPoolManager(config.PoolManagerConfig{Size: 1, Provider: "codex"}),
		poolCandidates: map[string]*coreauth.Auth{
			"cold-1": {ID: "cold-1", Provider: "codex"},
			"gone-1": {ID: "gone-1", Provider: "codex"},
		},
	}

	service.setPoolMemberState(PoolMember{AuthID: "cold-1", Provider: "codex", RemainingPercent: 88}, PoolStateReserve, "probe_ok")
	service.setPoolMemberState(PoolMember{AuthID: "gone-1", Provider: "codex"}, PoolStateActive, "startup_fill")
	service.removePoolMember("gone-1", "codex", "deleted")

	logOutput := buf.String()
	if !strings.Contains(logOutput, "pool-transition: auth=cold-1 provider=codex from=cold to=reserve reason=probe_ok remaining_percent=88") {
		t.Fatalf("expected cold->reserve transition log, got %q", logOutput)
	}
	if !strings.Contains(logOutput, "pool-transition: auth=gone-1 provider=codex from=active to=deleted reason=deleted") {
		t.Fatalf("expected active->deleted transition log, got %q", logOutput)
	}
}

func TestLogPoolUnderfilled_TracksDuration(t *testing.T) {
	var buf bytes.Buffer
	oldOutput := log.StandardLogger().Out
	oldFormatter := log.StandardLogger().Formatter
	oldLevel := log.GetLevel()
	log.SetOutput(&buf)
	log.SetFormatter(&log.TextFormatter{DisableTimestamp: true, DisableColors: true})
	log.SetLevel(log.WarnLevel)
	t.Cleanup(func() {
		log.SetOutput(oldOutput)
		log.SetFormatter(oldFormatter)
		log.SetLevel(oldLevel)
	})

	service := &Service{
		poolManager: NewPoolManager(config.PoolManagerConfig{Size: 2, Provider: "codex"}),
	}
	service.poolManager.SetActive(PoolMember{AuthID: "a-1", Provider: "codex"})

	start := time.Unix(1_700_000_100, 0).UTC()
	service.logPoolUnderfilled(start, true)
	service.logPoolUnderfilled(start.Add(2*time.Minute+15*time.Second), true)

	logOutput := buf.String()
	if !strings.Contains(logOutput, "pool-manager: underfilled active target=2 actual=1 reserve_exhausted=true underfilled_for=0s") {
		t.Fatalf("expected initial underfilled log, got %q", logOutput)
	}
	if !strings.Contains(logOutput, "pool-manager: underfilled active target=2 actual=1 reserve_exhausted=true underfilled_for=2m15s") {
		t.Fatalf("expected rolling underfilled duration log, got %q", logOutput)
	}
}

func TestClearPoolUnderfilled_LogsRecoveryDuration(t *testing.T) {
	var buf bytes.Buffer
	oldOutput := log.StandardLogger().Out
	oldFormatter := log.StandardLogger().Formatter
	oldLevel := log.GetLevel()
	log.SetOutput(&buf)
	log.SetFormatter(&log.TextFormatter{DisableTimestamp: true, DisableColors: true})
	log.SetLevel(log.InfoLevel)
	t.Cleanup(func() {
		log.SetOutput(oldOutput)
		log.SetFormatter(oldFormatter)
		log.SetLevel(oldLevel)
	})

	service := &Service{
		poolManager: NewPoolManager(config.PoolManagerConfig{Size: 2, Provider: "codex"}),
	}
	service.poolManager.SetActive(PoolMember{AuthID: "a-1", Provider: "codex"})

	start := time.Unix(1_700_000_200, 0).UTC()
	service.logPoolUnderfilled(start, true)
	service.poolManager.SetActive(PoolMember{AuthID: "a-2", Provider: "codex"})
	service.clearPoolUnderfilled(start.Add(90 * time.Second))

	logOutput := buf.String()
	if !strings.Contains(logOutput, "pool-manager: active recovered target=2 actual=2 underfilled_for=1m30s") {
		t.Fatalf("expected recovery log with duration, got %q", logOutput)
	}
}

func TestRunPoolEvalCycle_WarnsAndRecoversForLowSuccessRate(t *testing.T) {
	var buf bytes.Buffer
	oldOutput := log.StandardLogger().Out
	oldFormatter := log.StandardLogger().Formatter
	oldLevel := log.GetLevel()
	log.SetOutput(&buf)
	log.SetFormatter(&log.TextFormatter{DisableTimestamp: true, DisableColors: true})
	log.SetLevel(log.InfoLevel)
	t.Cleanup(func() {
		log.SetOutput(oldOutput)
		log.SetFormatter(oldFormatter)
		log.SetLevel(oldLevel)
	})

	stats := internalusage.NewRequestStatistics()
	service := &Service{
		usageStats:      stats,
		poolManager:     NewPoolManager(config.PoolManagerConfig{Size: 2, Provider: "codex"}),
		poolMetrics:     NewPoolMetrics(config.PoolManagerConfig{Size: 2, Provider: "codex"}),
		publishedActive: map[string]time.Time{},
	}
	service.poolManager.SetActive(PoolMember{AuthID: "a-1", Provider: "codex"})

	start := time.Unix(1_700_000_300, 0).UTC()
	service.runPoolEvalCycle(start)

	for i := 1; i <= 3; i++ {
		windowStart := start.Add(time.Duration(i) * 5 * time.Minute)
		stats.Record(context.Background(), coreusage.Record{Provider: "codex", Model: "gpt-5", RequestedAt: windowStart.Add(-4 * time.Minute)})
		stats.Record(context.Background(), coreusage.Record{Provider: "codex", Model: "gpt-5", RequestedAt: windowStart.Add(-3 * time.Minute), Failed: true})
		service.runPoolEvalCycle(windowStart)
	}

	logOutput := buf.String()
	if !strings.Contains(logOutput, "pool-eval: low_success_rate warning threshold=80.00% consecutive_windows=3 current_rate=50.00% total_requests=2 failure=1") {
		t.Fatalf("expected low success warning log after third window, got %q", logOutput)
	}

	stats.Record(context.Background(), coreusage.Record{Provider: "codex", Model: "gpt-5", RequestedAt: start.Add(16 * time.Minute)})
	stats.Record(context.Background(), coreusage.Record{Provider: "codex", Model: "gpt-5", RequestedAt: start.Add(17 * time.Minute)})
	service.runPoolEvalCycle(start.Add(20 * time.Minute))

	logOutput = buf.String()
	if !strings.Contains(logOutput, "pool-eval: success_rate recovered threshold=80.00% previous_streak=3 current_rate=100.00% total_requests=2") {
		t.Fatalf("expected low success recovery log, got %q", logOutput)
	}
}
