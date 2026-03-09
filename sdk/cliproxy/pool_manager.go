package cliproxy

import (
	"math/rand"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/sdk/config"
)

// PoolManager maintains the runtime active/reserve/limit auth partitioning.
// V1 stores this state in memory only.
type PoolManager struct {
	mu       sync.RWMutex
	size     int
	provider string
	active   map[string]*PoolMember
	reserve  map[string]*PoolMember
	limit    map[string]*PoolMember
	lastSeen map[string]*PoolMember
}

var poolShuffleStringsFunc = func(values []string) {
	rand.Shuffle(len(values), func(i, j int) {
		values[i], values[j] = values[j], values[i]
	})
}

// NewPoolManager creates a new pool manager from config.
func NewPoolManager(cfg config.PoolManagerConfig) *PoolManager {
	provider := strings.ToLower(strings.TrimSpace(cfg.Provider))
	if provider == "" {
		provider = "codex"
	}
	return &PoolManager{
		size:     cfg.Size,
		provider: provider,
		active:   make(map[string]*PoolMember),
		reserve:  make(map[string]*PoolMember),
		limit:    make(map[string]*PoolMember),
		lastSeen: make(map[string]*PoolMember),
	}
}

// Enabled reports whether pool mode is enabled.
func (p *PoolManager) Enabled() bool {
	return p != nil && p.size > 0
}

// TargetSize returns the configured active pool size.
func (p *PoolManager) TargetSize() int {
	if p == nil {
		return 0
	}
	return p.size
}

// Provider returns the provider handled by the pool manager.
func (p *PoolManager) Provider() string {
	if p == nil {
		return ""
	}
	return p.provider
}

// SetActive inserts or updates an active pool member.
func (p *PoolManager) SetActive(member PoolMember) {
	p.setMember(member, PoolStateActive)
}

// SetReserve inserts or updates a reserve pool member.
func (p *PoolManager) SetReserve(member PoolMember) {
	p.setMember(member, PoolStateReserve)
}

// SetLimit inserts or updates a limit pool member.
func (p *PoolManager) SetLimit(member PoolMember) {
	p.setMember(member, PoolStateLimit)
}

func (p *PoolManager) setMember(member PoolMember, state PoolState) {
	if p == nil || strings.TrimSpace(member.AuthID) == "" {
		return
	}
	member.AuthID = strings.TrimSpace(member.AuthID)
	if provider := strings.ToLower(strings.TrimSpace(member.Provider)); provider != "" {
		member.Provider = provider
	} else if p.provider != "" {
		member.Provider = p.provider
	}
	member.PoolState = state

	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.active, member.AuthID)
	delete(p.reserve, member.AuthID)
	delete(p.limit, member.AuthID)
	cloned := member
	p.lastSeen[member.AuthID] = &cloned
	switch state {
	case PoolStateLimit:
		p.limit[member.AuthID] = &cloned
	case PoolStateReserve:
		p.reserve[member.AuthID] = &cloned
	default:
		p.active[member.AuthID] = &cloned
	}
}

// Remove deletes a member from all pool sets.
func (p *PoolManager) Remove(authID string) {
	if p == nil {
		return
	}
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.active, authID)
	delete(p.reserve, authID)
	delete(p.limit, authID)
	delete(p.lastSeen, authID)
}

// ActiveIDs returns the active auth identifiers in deterministic order.
func (p *PoolManager) ActiveIDs() []string {
	return p.idsForState(PoolStateActive)
}

// ReserveIDs returns reserve auth identifiers in deterministic order.
func (p *PoolManager) ReserveIDs() []string {
	return p.idsForState(PoolStateReserve)
}

// LimitIDs returns limit auth identifiers in deterministic order.
func (p *PoolManager) LimitIDs() []string {
	return p.idsForState(PoolStateLimit)
}

func (p *PoolManager) idsForState(state PoolState) []string {
	if p == nil {
		return nil
	}
	p.mu.RLock()
	defer p.mu.RUnlock()

	var source map[string]*PoolMember
	switch state {
	case PoolStateLimit:
		source = p.limit
	case PoolStateReserve:
		source = p.reserve
	default:
		source = p.active
	}

	ids := make([]string, 0, len(source))
	for id := range source {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// Snapshot returns the current pool snapshot.
func (p *PoolManager) Snapshot() PoolSnapshot {
	if p == nil {
		return PoolSnapshot{}
	}
	active := p.ActiveIDs()
	reserve := p.ReserveIDs()
	limit := p.LimitIDs()
	target := p.TargetSize()
	return PoolSnapshot{
		TargetSize:   target,
		Provider:     p.Provider(),
		ActiveIDs:    active,
		ReserveIDs:   reserve,
		LimitIDs:     limit,
		Underfilled:  target > 0 && len(active) < target,
		ActiveCount:  len(active),
		ReserveCount: len(reserve),
		LimitCount:   len(limit),
	}
}

// PromoteNextReserve promotes the lexicographically first reserve auth into active.
func (p *PoolManager) PromoteNextReserve() (PoolMember, bool) {
	if p == nil {
		return PoolMember{}, false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.reserve) == 0 {
		return PoolMember{}, false
	}
	ids := make([]string, 0, len(p.reserve))
	for id := range p.reserve {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	member := p.reserve[ids[0]]
	if member == nil {
		return PoolMember{}, false
	}
	cloned := *member
	cloned.PoolState = PoolStateActive
	p.active[cloned.AuthID] = &cloned
	delete(p.reserve, cloned.AuthID)
	p.lastSeen[cloned.AuthID] = &cloned
	return cloned, true
}

// IsActive reports whether the given auth ID is part of the active pool.
func (p *PoolManager) IsActive(authID string) bool {
	if p == nil {
		return false
	}
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return false
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	_, ok := p.active[authID]
	return ok
}

// ActiveDiff reports the add/modify/delete delta between the previous published active set
// and the current active set. The first return value contains auth IDs newly active, the second
// contains auth IDs that remain active but should be treated as modified, and the third contains
// auth IDs removed from active.
func (p *PoolManager) ActiveDiff(previous map[string]time.Time) ([]string, []string, []string) {
	if p == nil {
		return nil, nil, nil
	}
	p.mu.RLock()
	defer p.mu.RUnlock()

	if previous == nil {
		previous = make(map[string]time.Time)
	}

	added := make([]string, 0, len(p.active))
	modified := make([]string, 0, len(p.active))
	removed := make([]string, 0, len(previous))

	for id := range p.active {
		if _, ok := previous[id]; !ok {
			added = append(added, id)
			continue
		}
		modified = append(modified, id)
	}
	for id := range previous {
		if _, ok := p.active[id]; !ok {
			removed = append(removed, id)
		}
	}

	sort.Strings(added)
	sort.Strings(modified)
	sort.Strings(removed)
	return added, modified, removed
}

// LastSeenMember returns the latest known pool member state for a given auth ID.
func (p *PoolManager) LastSeenMember(authID string) (PoolMember, bool) {
	if p == nil {
		return PoolMember{}, false
	}
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return PoolMember{}, false
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	member, ok := p.lastSeen[authID]
	if !ok || member == nil {
		return PoolMember{}, false
	}
	return *member, true
}

// RecordRequest records a real request outcome for an active auth.
func (p *PoolManager) RecordRequest(authID string, success bool, now time.Time) {
	if p == nil {
		return
	}
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	member := p.active[authID]
	if member == nil {
		return
	}
	member.LastSelectedAt = now
	if success {
		member.LastSuccessAt = now
		member.ConsecutiveFailures = 0
		member.LastProbeReason = ""
	}
}

// MarkProbe records a probe result and schedules the next probe time.
func (p *PoolManager) MarkProbe(authID string, now time.Time, next time.Time, success bool, reason string) {
	if p == nil {
		return
	}
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	member := p.lastSeen[authID]
	if member == nil {
		return
	}
	member.LastProbeAt = now
	member.NextProbeAt = next
	member.LastProbeReason = strings.TrimSpace(reason)
	if success {
		member.ConsecutiveFailures = 0
		member.LastSuccessAt = now
	} else {
		member.ConsecutiveFailures++
	}
	if active := p.active[authID]; active != nil {
		*active = *member
	}
	if reserve := p.reserve[authID]; reserve != nil {
		*reserve = *member
	}
	if limit := p.limit[authID]; limit != nil {
		*limit = *member
	}
}

// DueActiveProbeIDs returns active auth IDs due for idle health probing.
func (p *PoolManager) DueActiveProbeIDs(now time.Time, interval time.Duration) []string {
	if p == nil || interval <= 0 {
		return nil
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	ids := make([]string, 0, len(p.active))
	for id, member := range p.active {
		if member == nil {
			continue
		}
		if !member.NextProbeAt.IsZero() && member.NextProbeAt.After(now) {
			continue
		}
		if !member.LastSuccessAt.IsZero() && member.LastSuccessAt.Add(interval).After(now) {
			continue
		}
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// DueReserveProbeIDs returns up to sampleSize reserve auth IDs that are due for background probing.
func (p *PoolManager) DueReserveProbeIDs(now time.Time, interval time.Duration, sampleSize int) []string {
	if p == nil || interval <= 0 || sampleSize <= 0 {
		return nil
	}
	p.mu.RLock()
	ids := make([]string, 0, len(p.reserve))
	for id, member := range p.reserve {
		if member == nil {
			continue
		}
		if !member.NextProbeAt.IsZero() && member.NextProbeAt.After(now) {
			continue
		}
		ids = append(ids, id)
	}
	p.mu.RUnlock()

	if len(ids) == 0 {
		return nil
	}
	sort.Strings(ids)
	poolShuffleStringsFunc(ids)
	if len(ids) > sampleSize {
		ids = ids[:sampleSize]
	}
	sort.Strings(ids)
	return ids
}

// DueLimitProbeIDs returns limit auth IDs due for recovery probing.
func (p *PoolManager) DueLimitProbeIDs(now time.Time, interval time.Duration) []string {
	if p == nil || interval <= 0 {
		return nil
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	ids := make([]string, 0, len(p.limit))
	for id, member := range p.limit {
		if member == nil {
			continue
		}
		if !member.NextProbeAt.IsZero() && member.NextProbeAt.After(now) {
			continue
		}
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}
