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
	pm.SetActive(PoolMember{AuthID: "a-2"})

	previous := map[string]time.Time{
		"a-2": time.Now(),
		"a-3": time.Now(),
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
