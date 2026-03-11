package cliproxy

import (
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/sdk/config"
)

func TestNewPoolManager_DefaultsProvider(t *testing.T) {
	pm := NewPoolManager(config.PoolManagerConfig{Size: 100})

	if !pm.Enabled() {
		t.Fatal("expected pool manager to be enabled")
	}
	if pm.TargetSize() != 100 {
		t.Fatalf("TargetSize() = %d, want 100", pm.TargetSize())
	}
	if pm.Provider() != "codex" {
		t.Fatalf("Provider() = %q, want %q", pm.Provider(), "codex")
	}
	if pm.LowQuotaThresholdPercent() != 20 {
		t.Fatalf("LowQuotaThresholdPercent() = %d, want 20", pm.LowQuotaThresholdPercent())
	}
}

func TestPoolManager_SetActiveAndRemove(t *testing.T) {
	pm := NewPoolManager(config.PoolManagerConfig{Size: 2, Provider: "codex"})

	pm.SetActive(PoolMember{AuthID: "a-1", Provider: "codex"})
	pm.SetActive(PoolMember{AuthID: "a-2", Provider: "codex"})

	snapshot := pm.Snapshot()
	if snapshot.ActiveCount != 2 {
		t.Fatalf("ActiveCount = %d, want 2", snapshot.ActiveCount)
	}
	if snapshot.Underfilled {
		t.Fatal("expected pool not to be underfilled")
	}

	pm.Remove("a-1")

	snapshot = pm.Snapshot()
	if snapshot.ActiveCount != 1 {
		t.Fatalf("ActiveCount = %d, want 1", snapshot.ActiveCount)
	}
	if !snapshot.Underfilled {
		t.Fatal("expected pool to be underfilled after removal")
	}
	if got := snapshot.ActiveIDs; len(got) != 1 || got[0] != "a-2" {
		t.Fatalf("ActiveIDs = %#v, want [a-2]", got)
	}
}

func TestPoolManager_SetReserveDeterministicOrdering(t *testing.T) {
	pm := NewPoolManager(config.PoolManagerConfig{Size: 1, Provider: "codex"})

	pm.SetReserve(PoolMember{AuthID: "r-2"})
	pm.SetReserve(PoolMember{AuthID: "r-1"})
	pm.SetReserve(PoolMember{AuthID: "r-3"})

	got := pm.ReserveIDs()
	want := []string{"r-1", "r-2", "r-3"}
	if len(got) != len(want) {
		t.Fatalf("ReserveIDs() len = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("ReserveIDs()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestPoolManager_MoveBetweenStates(t *testing.T) {
	pm := NewPoolManager(config.PoolManagerConfig{Size: 1, Provider: "codex"})
	member := PoolMember{
		AuthID:         "shared-auth",
		LastSelectedAt: time.Now(),
	}

	pm.SetReserve(member)
	if got := pm.ReserveIDs(); len(got) != 1 || got[0] != "shared-auth" {
		t.Fatalf("ReserveIDs() = %#v, want [shared-auth]", got)
	}

	pm.SetLimit(member)
	if got := pm.ReserveIDs(); len(got) != 0 {
		t.Fatalf("ReserveIDs() = %#v, want []", got)
	}
	if got := pm.LimitIDs(); len(got) != 1 || got[0] != "shared-auth" {
		t.Fatalf("LimitIDs() = %#v, want [shared-auth]", got)
	}

	pm.SetActive(member)
	snapshot := pm.Snapshot()
	if got := snapshot.ActiveIDs; len(got) != 1 || got[0] != "shared-auth" {
		t.Fatalf("ActiveIDs = %#v, want [shared-auth]", got)
	}
	if got := snapshot.LimitIDs; len(got) != 0 {
		t.Fatalf("LimitIDs = %#v, want []", got)
	}
}

func TestPoolManager_ActiveDiff(t *testing.T) {
	pm := NewPoolManager(config.PoolManagerConfig{Size: 2, Provider: "codex"})
	pm.SetActive(PoolMember{AuthID: "a-1"})
	previousAt := time.Now()
	pm.SetActive(PoolMember{AuthID: "a-2", LastProbeAt: previousAt.Add(time.Second)})

	previous := map[string]time.Time{
		"a-2": previousAt,
		"a-3": previousAt,
	}

	added, modified, removed := pm.ActiveDiff(previous)

	if len(added) != 1 || added[0] != "a-1" {
		t.Fatalf("added = %#v, want [a-1]", added)
	}
	if len(modified) != 1 || modified[0] != "a-2" {
		t.Fatalf("modified = %#v, want [a-2]", modified)
	}
	if len(removed) != 1 || removed[0] != "a-3" {
		t.Fatalf("removed = %#v, want [a-3]", removed)
	}
}

func TestPoolManager_LastSeenMember(t *testing.T) {
	pm := NewPoolManager(config.PoolManagerConfig{Size: 1, Provider: "codex"})
	pm.SetReserve(PoolMember{AuthID: "r-1", Provider: "codex"})

	member, ok := pm.LastSeenMember("r-1")
	if !ok {
		t.Fatal("expected LastSeenMember to find r-1")
	}
	if member.AuthID != "r-1" || member.PoolState != PoolStateReserve {
		t.Fatalf("LastSeenMember = %+v, want AuthID=r-1 PoolState=reserve", member)
	}
}

func TestPoolManager_PromoteNextReserve(t *testing.T) {
	pm := NewPoolManager(config.PoolManagerConfig{Size: 2, Provider: "codex"})
	pm.SetReserve(PoolMember{AuthID: "r-2"})
	pm.SetReserve(PoolMember{AuthID: "r-1"})

	member, ok := pm.PromoteNextReserve()
	if !ok {
		t.Fatal("expected PromoteNextReserve to succeed")
	}
	if member.AuthID != "r-1" || member.PoolState != PoolStateActive {
		t.Fatalf("PromoteNextReserve() = %+v, want AuthID=r-1 PoolState=active", member)
	}
	if !pm.IsActive("r-1") {
		t.Fatal("expected r-1 to be active after promotion")
	}
	if got := pm.ReserveIDs(); len(got) != 1 || got[0] != "r-2" {
		t.Fatalf("ReserveIDs() = %#v, want [r-2]", got)
	}
}

func TestPoolManager_DueActiveProbeIDsSkipsFreshSuccess(t *testing.T) {
	pm := NewPoolManager(config.PoolManagerConfig{Size: 2, Provider: "codex"})
	now := time.Now()

	pm.SetActive(PoolMember{AuthID: "a-1", LastSuccessAt: now})
	pm.SetActive(PoolMember{AuthID: "a-2"})

	got := pm.DueActiveProbeIDs(now.Add(5*time.Minute), 30*time.Minute)
	if len(got) != 1 || got[0] != "a-2" {
		t.Fatalf("DueActiveProbeIDs() = %#v, want [a-2]", got)
	}
}

func TestPoolManager_DueActiveProbeIDsSkipsInFlight(t *testing.T) {
	pm := NewPoolManager(config.PoolManagerConfig{Size: 1, Provider: "codex"})
	now := time.Unix(1_700_000_000, 0).UTC()
	pm.SetActive(PoolMember{AuthID: "a-1", Provider: "codex"})
	pm.BeginUse("a-1", now, 30*time.Second)

	got := pm.DueActiveProbeIDs(now.Add(time.Hour), time.Minute)
	if len(got) != 0 {
		t.Fatalf("DueActiveProbeIDs = %#v, want nil while in-flight", got)
	}
}

func TestPoolManager_MarkProbeUpdatesTimestamps(t *testing.T) {
	pm := NewPoolManager(config.PoolManagerConfig{Size: 1, Provider: "codex"})
	pm.SetActive(PoolMember{AuthID: "a-1"})
	now := time.Now()
	next := now.Add(30 * time.Minute)

	pm.MarkProbe("a-1", now, next, true, "")

	member, ok := pm.LastSeenMember("a-1")
	if !ok {
		t.Fatal("expected LastSeenMember to find a-1")
	}
	if member.LastProbeAt.IsZero() || member.NextProbeAt.IsZero() {
		t.Fatalf("expected probe timestamps to be populated, got %+v", member)
	}
	if member.ConsecutiveFailures != 0 {
		t.Fatalf("expected ConsecutiveFailures=0, got %d", member.ConsecutiveFailures)
	}
}

func TestPoolManager_DueReserveProbeIDsRespectsSampleSize(t *testing.T) {
	pm := NewPoolManager(config.PoolManagerConfig{Size: 1, Provider: "codex"})
	for _, id := range []string{"r-1", "r-2", "r-3"} {
		pm.SetReserve(PoolMember{AuthID: id})
	}

	originalShuffle := poolShuffleStringsFunc
	poolShuffleStringsFunc = func(values []string) {}
	defer func() { poolShuffleStringsFunc = originalShuffle }()

	got := pm.DueReserveProbeIDs(time.Now(), 5*time.Minute, 2)
	if len(got) != 2 {
		t.Fatalf("DueReserveProbeIDs() len = %d, want 2", len(got))
	}
	if got[0] != "r-1" || got[1] != "r-2" {
		t.Fatalf("DueReserveProbeIDs() = %#v, want [r-1 r-2]", got)
	}
}

func TestPoolManager_DueLimitProbeIDs(t *testing.T) {
	pm := NewPoolManager(config.PoolManagerConfig{Size: 1, Provider: "codex"})
	now := time.Now()
	pm.SetLimit(PoolMember{AuthID: "l-1"})
	pm.SetLimit(PoolMember{AuthID: "l-2", NextProbeAt: now.Add(time.Hour)})

	got := pm.DueLimitProbeIDs(now.Add(5*time.Minute), 24*time.Hour)
	if len(got) != 1 || got[0] != "l-1" {
		t.Fatalf("DueLimitProbeIDs() = %#v, want [l-1]", got)
	}
}

func TestPoolManagerRatioThresholdHelpers(t *testing.T) {
	cfg := config.PoolManagerConfig{
		Size:                          200,
		ReserveRefillLowRatio:         0.35,
		ReserveRefillHighRatio:        0.85,
		ColdBatchLoadRatio:            0.10,
		ActiveQuotaRefreshSampleRatio: 0.12,
	}

	if got := reserveRefillLowWatermark(cfg, 100); got != 35 {
		t.Fatalf("reserveRefillLowWatermark() = %d, want 35", got)
	}
	if got := reserveRefillHighWatermark(cfg, 100); got != 85 {
		t.Fatalf("reserveRefillHighWatermark() = %d, want 85", got)
	}
	if got := coldBatchLoadSize(cfg, 200); got != 20 {
		t.Fatalf("coldBatchLoadSize() = %d, want 20", got)
	}
	if got := activeQuotaRefreshSampleSize(cfg, 100); got != 12 {
		t.Fatalf("activeQuotaRefreshSampleSize() = %d, want 12", got)
	}
}

func TestPoolManagerRatioThresholdHelpersClampMinimums(t *testing.T) {
	cfg := config.PoolManagerConfig{
		Size:                          3,
		ReserveRefillLowRatio:         0.01,
		ReserveRefillHighRatio:        0.02,
		ColdBatchLoadRatio:            0.01,
		ActiveQuotaRefreshSampleRatio: 0.01,
	}

	if got := reserveRefillLowWatermark(cfg, 3); got != 1 {
		t.Fatalf("reserveRefillLowWatermark() = %d, want 1", got)
	}
	if got := reserveRefillHighWatermark(cfg, 3); got != 1 {
		t.Fatalf("reserveRefillHighWatermark() = %d, want 1", got)
	}
	if got := coldBatchLoadSize(cfg, 3); got != 1 {
		t.Fatalf("coldBatchLoadSize() = %d, want 1", got)
	}
	if got := activeQuotaRefreshSampleSize(cfg, 3); got != 1 {
		t.Fatalf("activeQuotaRefreshSampleSize() = %d, want 1", got)
	}
}
