package cliproxy

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/config"
	log "github.com/sirupsen/logrus"
)

func testCodexAuthWithRemaining(id string, remainingPercent int) *coreauth.Auth {
	return &coreauth.Auth{
		ID:       id,
		Provider: "codex",
		Metadata: map[string]any{
			poolQuotaWeeklyRemainingPercentKey: remainingPercent,
		},
	}
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func TestPlaceProbedAuth_ReplacesLowQualityActiveWhenReserveBetter(t *testing.T) {
	service := &Service{
		poolManager:    NewPoolManager(config.PoolManagerConfig{Size: 2, Provider: "codex"}),
		poolCandidates: map[string]*coreauth.Auth{},
	}

	lowActive := testCodexAuthWithRemaining("a-low", 30)
	highActive := testCodexAuthWithRemaining("a-high", 40)
	bestReserve := testCodexAuthWithRemaining("r-best", 90)
	worstReserve := testCodexAuthWithRemaining("r-worst", 10)

	for _, auth := range []*coreauth.Auth{lowActive, highActive, bestReserve, worstReserve} {
		service.poolCandidates[auth.ID] = auth.Clone()
	}

	service.setPoolMemberState(service.poolMemberForAuth(lowActive, PoolStateActive), PoolStateActive, "seed")
	service.setPoolMemberState(service.poolMemberForAuth(highActive, PoolStateActive), PoolStateActive, "seed")
	service.setPoolMemberState(service.poolMemberForAuth(bestReserve, PoolStateReserve), PoolStateReserve, "seed")
	service.setPoolMemberState(service.poolMemberForAuth(worstReserve, PoolStateReserve), PoolStateReserve, "seed")

	snapshot := service.poolManager.Snapshot()
	if snapshot.ActiveCount != 2 || snapshot.ReserveCount != 2 {
		t.Fatalf("unexpected snapshot before probe: %+v", snapshot)
	}

	placed := service.placeProbedAuth(context.Background(), bestReserve, true)
	if !placed {
		t.Fatalf("expected placeProbedAuth to upgrade bestReserve into active, got placed=%t", placed)
	}

	if !service.poolManager.IsActive(bestReserve.ID) {
		t.Fatalf("expected %s to be active after replacement", bestReserve.ID)
	}
	if service.poolManager.IsActive(lowActive.ID) {
		t.Fatalf("expected %s to be removed from active after replacement", lowActive.ID)
	}
	if !service.poolManager.IsActive(highActive.ID) {
		t.Fatalf("expected %s to remain active after replacement", highActive.ID)
	}

	reserveIDs := service.poolManager.ReserveIDs()
	if !containsString(reserveIDs, lowActive.ID) {
		t.Fatalf("expected %s to be moved to reserve after replacement, got reserve=%v", lowActive.ID, reserveIDs)
	}
	if !containsString(reserveIDs, worstReserve.ID) {
		t.Fatalf("expected %s to remain in reserve after replacement, got reserve=%v", worstReserve.ID, reserveIDs)
	}
	if containsString(reserveIDs, bestReserve.ID) {
		t.Fatalf("expected %s to be removed from reserve after replacement, got reserve=%v", bestReserve.ID, reserveIDs)
	}
}

func TestRunActiveProbeCycle_UpdatesActiveRemainingPercent(t *testing.T) {
	originalProbe := poolProbeAuthFunc
	poolProbeAuthFunc = func(ctx context.Context, cfg *config.Config, auth *coreauth.Auth) (*coreauth.Auth, coreauth.Result) {
		if auth == nil {
			return nil, coreauth.Result{Provider: "codex", Success: false}
		}
		auth = auth.Clone()
		if auth.Metadata == nil {
			auth.Metadata = make(map[string]any)
		}
		auth.Metadata[poolQuotaWeeklyRemainingPercentKey] = 10
		return auth, coreauth.Result{
			AuthID:   auth.ID,
			Provider: "codex",
			Model:    "gpt-5",
			Success:  true,
		}
	}
	t.Cleanup(func() { poolProbeAuthFunc = originalProbe })

	cfg := &config.Config{
		PoolManager: config.PoolManagerConfig{
			Size:                          1,
			Provider:                      "codex",
			ActiveIdleScanIntervalSeconds: 60,
			LowQuotaThresholdPercent:      20,
		},
	}

	coreManager := coreauth.NewManager(nil, &coreauth.RoundRobinSelector{}, nil)
	coreManager.SetConfig(cfg)
	auth := testCodexAuthWithRemaining("a-1", 50)
	auth.Status = coreauth.StatusActive
	if _, err := coreManager.Register(coreauth.WithSkipPersist(context.Background()), auth); err != nil {
		t.Fatalf("register core auth: %v", err)
	}

	service := &Service{
		cfg:            cfg,
		coreManager:    coreManager,
		poolManager:    NewPoolManager(cfg.PoolManager),
		poolCandidates: map[string]*coreauth.Auth{auth.ID: auth.Clone()},
	}
	service.setPoolMemberState(service.poolMemberForAuth(auth, PoolStateActive), PoolStateActive, "seed")

	now := time.Unix(1_700_000_000, 0).UTC()
	service.runActiveProbeCycle(context.Background(), now)

	member, ok := service.poolManager.LastSeenMember(auth.ID)
	if !ok {
		t.Fatalf("expected pool member for %s", auth.ID)
	}
	if member.RemainingPercent != 10 {
		t.Fatalf("RemainingPercent=%d, want 10", member.RemainingPercent)
	}
}

func TestRunActiveQuotaRefreshCycle_ProbesEvenWhenLastSuccessFresh(t *testing.T) {
	originalProbe := poolProbeAuthFunc
	probeCalls := 0
	poolProbeAuthFunc = func(ctx context.Context, cfg *config.Config, auth *coreauth.Auth) (*coreauth.Auth, coreauth.Result) {
		probeCalls++
		if auth == nil {
			return nil, coreauth.Result{Provider: "codex", Success: false}
		}
		auth = auth.Clone()
		if auth.Metadata == nil {
			auth.Metadata = make(map[string]any)
		}
		auth.Metadata[poolQuotaWeeklyRemainingPercentKey] = 10
		return auth, coreauth.Result{
			AuthID:   auth.ID,
			Provider: "codex",
			Model:    "gpt-5",
			Success:  true,
		}
	}
	t.Cleanup(func() { poolProbeAuthFunc = originalProbe })

	cfg := &config.Config{
		PoolManager: config.PoolManagerConfig{
			Size:                              1,
			Provider:                          "codex",
			ActiveIdleScanIntervalSeconds:     1800,
			LowQuotaThresholdPercent:          20,
			ActiveQuotaRefreshIntervalSeconds: 60,
			ActiveQuotaRefreshSampleSize:      1,
		},
	}

	coreManager := coreauth.NewManager(nil, &coreauth.RoundRobinSelector{}, nil)
	coreManager.SetConfig(cfg)
	auth := testCodexAuthWithRemaining("a-1", 50)
	auth.Status = coreauth.StatusActive
	if _, err := coreManager.Register(coreauth.WithSkipPersist(context.Background()), auth); err != nil {
		t.Fatalf("register core auth: %v", err)
	}

	service := &Service{
		cfg:            cfg,
		coreManager:    coreManager,
		poolManager:    NewPoolManager(cfg.PoolManager),
		poolCandidates: map[string]*coreauth.Auth{auth.ID: auth.Clone()},
	}
	service.setPoolMemberState(service.poolMemberForAuth(auth, PoolStateActive), PoolStateActive, "seed")

	now := time.Unix(1_700_000_100, 0).UTC()
	service.poolManager.RecordRequest(auth.ID, true, now)

	service.runActiveQuotaRefreshCycle(context.Background(), now)

	member, ok := service.poolManager.LastSeenMember(auth.ID)
	if !ok {
		t.Fatalf("expected pool member for %s", auth.ID)
	}
	if member.RemainingPercent != 10 {
		t.Fatalf("RemainingPercent=%d, want 10", member.RemainingPercent)
	}
	if probeCalls != 1 {
		t.Fatalf("probeCalls=%d, want 1", probeCalls)
	}

	service.runActiveQuotaRefreshCycle(context.Background(), now.Add(10*time.Second))
	if probeCalls != 1 {
		t.Fatalf("second refresh should be throttled, probeCalls=%d, want 1", probeCalls)
	}
}

func TestSyncPoolActiveToRuntime_TrimsOverflowActiveToTarget(t *testing.T) {
	service := &Service{
		poolManager:    NewPoolManager(config.PoolManagerConfig{Size: 2, Provider: "codex", LowQuotaThresholdPercent: 20}),
		poolCandidates: map[string]*coreauth.Auth{},
	}

	low := testCodexAuthWithRemaining("a-low", 30)
	mid := testCodexAuthWithRemaining("a-mid", 50)
	high := testCodexAuthWithRemaining("a-high", 80)
	for _, auth := range []*coreauth.Auth{low, mid, high} {
		service.poolCandidates[auth.ID] = auth.Clone()
		service.setPoolMemberState(service.poolMemberForAuth(auth, PoolStateActive), PoolStateActive, "seed")
	}

	service.syncPoolActiveToRuntime(context.Background())

	snapshot := service.poolManager.Snapshot()
	if snapshot.ActiveCount != 2 {
		t.Fatalf("ActiveCount=%d, want 2", snapshot.ActiveCount)
	}
	if service.poolManager.IsActive(low.ID) {
		t.Fatalf("expected %s to be trimmed from active", low.ID)
	}
	if !service.poolManager.IsActive(mid.ID) || !service.poolManager.IsActive(high.ID) {
		t.Fatalf("expected higher-quota auths to remain active, snapshot=%+v", snapshot)
	}
	reserveIDs := service.poolManager.ReserveIDs()
	if !containsString(reserveIDs, low.ID) {
		t.Fatalf("expected %s to move to reserve, got reserve=%v", low.ID, reserveIDs)
	}
}

func TestRunActiveQuotaRefreshCycle_ReplacesDegradedActiveWithBetterReserve(t *testing.T) {
	originalProbe := poolProbeAuthFunc
	poolProbeAuthFunc = func(ctx context.Context, cfg *config.Config, auth *coreauth.Auth) (*coreauth.Auth, coreauth.Result) {
		if auth == nil {
			return nil, coreauth.Result{Provider: "codex", Success: false}
		}
		auth = auth.Clone()
		if auth.Metadata == nil {
			auth.Metadata = make(map[string]any)
		}
		switch auth.ID {
		case "a-1":
			auth.Metadata[poolQuotaWeeklyRemainingPercentKey] = 35
		case "r-1":
			auth.Metadata[poolQuotaWeeklyRemainingPercentKey] = 80
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
		PoolManager: config.PoolManagerConfig{
			Size:                              1,
			Provider:                          "codex",
			LowQuotaThresholdPercent:          20,
			ActiveQuotaRefreshIntervalSeconds: 60,
			ActiveQuotaRefreshSampleSize:      1,
		},
	}

	coreManager := coreauth.NewManager(nil, &coreauth.RoundRobinSelector{}, nil)
	coreManager.SetConfig(cfg)

	activeAuth := testCodexAuthWithRemaining("a-1", 70)
	activeAuth.Status = coreauth.StatusActive
	reserveAuth := testCodexAuthWithRemaining("r-1", 80)
	reserveAuth.Status = coreauth.StatusActive
	for _, auth := range []*coreauth.Auth{activeAuth, reserveAuth} {
		if _, err := coreManager.Register(coreauth.WithSkipPersist(context.Background()), auth); err != nil {
			t.Fatalf("register auth %s: %v", auth.ID, err)
		}
	}

	service := &Service{
		cfg:         cfg,
		coreManager: coreManager,
		poolManager: NewPoolManager(cfg.PoolManager),
		poolCandidates: map[string]*coreauth.Auth{
			activeAuth.ID:  activeAuth.Clone(),
			reserveAuth.ID: reserveAuth.Clone(),
		},
	}
	service.setPoolMemberState(service.poolMemberForAuth(activeAuth, PoolStateActive), PoolStateActive, "seed")
	service.setPoolMemberState(service.poolMemberForAuth(reserveAuth, PoolStateReserve), PoolStateReserve, "seed")

	now := time.Unix(1_700_000_200, 0).UTC()
	service.runActiveQuotaRefreshCycle(context.Background(), now)

	if !service.poolManager.IsActive(reserveAuth.ID) {
		t.Fatalf("expected reserve auth %s to replace degraded active", reserveAuth.ID)
	}
	if service.poolManager.IsActive(activeAuth.ID) {
		t.Fatalf("expected degraded active auth %s to leave active", activeAuth.ID)
	}
	reserveIDs := service.poolManager.ReserveIDs()
	if !containsString(reserveIDs, activeAuth.ID) {
		t.Fatalf("expected degraded active auth %s to move to reserve, got reserve=%v", activeAuth.ID, reserveIDs)
	}
}

func TestRunPoolRebalanceNow_PrefersHighQuotaColdCandidatesForReserve(t *testing.T) {
	originalProbe := poolProbeAuthFunc
	poolProbeAuthFunc = func(ctx context.Context, cfg *config.Config, auth *coreauth.Auth) (*coreauth.Auth, coreauth.Result) {
		if auth == nil {
			return nil, coreauth.Result{Provider: "codex", Success: false}
		}
		auth = auth.Clone()
		return auth, coreauth.Result{
			AuthID:   auth.ID,
			Provider: "codex",
			Model:    "gpt-5",
			Success:  true,
		}
	}
	t.Cleanup(func() { poolProbeAuthFunc = originalProbe })

	cfg := &config.Config{
		PoolManager: config.PoolManagerConfig{
			Size:                     1,
			Provider:                 "codex",
			ReserveSampleSize:        1,
			LowQuotaThresholdPercent: 20,
		},
	}

	activeAuth := testCodexAuthWithRemaining("a-1", 70)
	activeAuth.Status = coreauth.StatusActive
	low1 := testCodexAuthWithRemaining("c-low-1", 0)
	low1.Status = coreauth.StatusActive
	low2 := testCodexAuthWithRemaining("c-low-2", 5)
	low2.Status = coreauth.StatusActive
	high := testCodexAuthWithRemaining("c-high", 80)
	high.Status = coreauth.StatusActive

	service := &Service{
		cfg:         cfg,
		poolManager: NewPoolManager(cfg.PoolManager),
		poolMetrics: NewPoolMetrics(cfg.PoolManager),
		poolCandidates: map[string]*coreauth.Auth{
			activeAuth.ID: activeAuth.Clone(),
			low1.ID:       low1.Clone(),
			low2.ID:       low2.Clone(),
			high.ID:       high.Clone(),
		},
	}
	service.poolManager.SetActive(service.poolMemberForAuth(activeAuth, PoolStateActive))
	service.poolCandidateOrder = []string{low1.ID, low2.ID, high.ID}

	service.runPoolRebalanceNow(context.Background())

	snapshot := service.poolManager.Snapshot()
	if snapshot.ReserveCount != 1 {
		t.Fatalf("expected reserve to be filled, got %+v", snapshot)
	}
	if !containsString(snapshot.ActiveIDs, high.ID) && !containsString(snapshot.ReserveIDs, high.ID) {
		t.Fatalf("expected highest quota cold candidate to be pulled into the warm set, got %+v", snapshot)
	}
}

func TestSyncPoolActiveToRuntime_SkipsModifyOnlyPublish(t *testing.T) {
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

	auth := testCodexAuthWithRemaining("a-1", 70)
	auth.Status = coreauth.StatusActive

	service := &Service{
		poolManager: NewPoolManager(config.PoolManagerConfig{Size: 1, Provider: "codex"}),
		poolCandidates: map[string]*coreauth.Auth{
			auth.ID: auth.Clone(),
		},
		publishedActive: map[string]time.Time{},
	}
	service.setPoolMemberState(service.poolMemberForAuth(auth, PoolStateActive), PoolStateActive, "seed")

	service.syncPoolActiveToRuntime(context.Background())
	buf.Reset()

	member, ok := service.poolManager.LastSeenMember(auth.ID)
	if !ok {
		t.Fatalf("expected active member %s", auth.ID)
	}
	member.LastProbeAt = time.Now().Add(time.Second)
	service.setPoolMemberState(member, PoolStateActive, "probe_ok")

	service.syncPoolActiveToRuntime(context.Background())

	logOutput := buf.String()
	if strings.Contains(logOutput, "pool-publish: completed add=0 modify=1 delete=0") {
		t.Fatalf("expected modify-only sync to be skipped, got %q", logOutput)
	}
}

func TestFillWarmReserveFromColdCandidates_CapsReserveOnlyBurst(t *testing.T) {
	originalProbe := poolProbeAuthFunc
	probeCalls := 0
	poolProbeAuthFunc = func(ctx context.Context, cfg *config.Config, auth *coreauth.Auth) (*coreauth.Auth, coreauth.Result) {
		probeCalls++
		if auth == nil {
			return nil, coreauth.Result{Provider: "codex", Success: false}
		}
		auth = auth.Clone()
		return auth, coreauth.Result{
			AuthID:   auth.ID,
			Provider: "codex",
			Model:    "gpt-5",
			Success:  true,
		}
	}
	t.Cleanup(func() { poolProbeAuthFunc = originalProbe })

	cfg := &config.Config{
		PoolManager: config.PoolManagerConfig{
			Size:                     1,
			Provider:                 "codex",
			ReserveSampleSize:        1,
			LowQuotaThresholdPercent: 20,
		},
	}

	activeAuth := testCodexAuthWithRemaining("a-1", 70)
	activeAuth.Status = coreauth.StatusActive
	low1 := testCodexAuthWithRemaining("c-low-1", 0)
	low1.Status = coreauth.StatusActive
	low2 := testCodexAuthWithRemaining("c-low-2", 5)
	low2.Status = coreauth.StatusActive

	service := &Service{
		cfg:         cfg,
		poolManager: NewPoolManager(cfg.PoolManager),
		poolMetrics: NewPoolMetrics(cfg.PoolManager),
		poolCandidates: map[string]*coreauth.Auth{
			activeAuth.ID: activeAuth.Clone(),
			low1.ID:       low1.Clone(),
			low2.ID:       low2.Clone(),
		},
	}
	service.poolManager.SetActive(service.poolMemberForAuth(activeAuth, PoolStateActive))
	service.poolCandidateOrder = []string{low1.ID, low2.ID}

	service.fillWarmReserveFromColdCandidates(context.Background())

	if probeCalls != 1 {
		t.Fatalf("expected reserve-only refill to honor configured sample size, probeCalls=%d", probeCalls)
	}
}

func TestPlaceProbedAuth_DoesNotMakeFreshActiveImmediatelyProbeDue(t *testing.T) {
	cfg := &config.Config{
		PoolManager: config.PoolManagerConfig{
			Size:                          1,
			Provider:                      "codex",
			ActiveIdleScanIntervalSeconds: 1800,
			LowQuotaThresholdPercent:      20,
		},
	}

	auth := testCodexAuthWithRemaining("a-1", 70)
	auth.Status = coreauth.StatusActive

	service := &Service{
		cfg:            cfg,
		poolManager:    NewPoolManager(cfg.PoolManager),
		poolCandidates: map[string]*coreauth.Auth{auth.ID: auth.Clone()},
	}

	placed := service.placeProbedAuth(context.Background(), auth, true)
	if !placed {
		t.Fatal("expected auth to be placed into active")
	}

	due := service.poolManager.DueActiveProbeIDs(time.Now(), 30*time.Minute)
	if containsString(due, auth.ID) {
		t.Fatalf("expected freshly placed active auth to avoid immediate reprobe, due=%v", due)
	}
}
