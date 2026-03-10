package cliproxy

import (
	"context"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/config"
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
