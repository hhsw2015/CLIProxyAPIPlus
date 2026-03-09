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
	mu                       sync.RWMutex
	size                     int
	provider                 string
	lowQuotaThresholdPercent int
	active                   map[string]*PoolMember
	reserve                  map[string]*PoolMember
	lowQuota                 map[string]*PoolMember
	limit                    map[string]*PoolMember
	lastSeen                 map[string]*PoolMember
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
	lowQuotaThreshold := cfg.LowQuotaThresholdPercent
	if lowQuotaThreshold <= 0 {
		lowQuotaThreshold = 20
	}
	if lowQuotaThreshold > 100 {
		lowQuotaThreshold = 100
	}
	return &PoolManager{
		size:                     cfg.Size,
		provider:                 provider,
		lowQuotaThresholdPercent: lowQuotaThreshold,
		active:                   make(map[string]*PoolMember),
		reserve:                  make(map[string]*PoolMember),
		lowQuota:                 make(map[string]*PoolMember),
		limit:                    make(map[string]*PoolMember),
		lastSeen:                 make(map[string]*PoolMember),
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

// ReserveTargetSize returns the configured warm-reserve target size.
func (p *PoolManager) ReserveTargetSize() int {
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

// LowQuotaThresholdPercent returns the configured low-quota remaining-percent threshold.
func (p *PoolManager) LowQuotaThresholdPercent() int {
	if p == nil {
		return 0
	}
	return p.lowQuotaThresholdPercent
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

// SetLowQuota inserts or updates a low-quota pool member.
func (p *PoolManager) SetLowQuota(member PoolMember) {
	p.setMember(member, PoolStateLowQuota)
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
	delete(p.lowQuota, member.AuthID)
	delete(p.limit, member.AuthID)
	cloned := member
	p.lastSeen[member.AuthID] = &cloned
	switch state {
	case PoolStateLimit:
		p.limit[member.AuthID] = &cloned
	case PoolStateLowQuota:
		p.lowQuota[member.AuthID] = &cloned
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
	delete(p.lowQuota, authID)
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

// LowQuotaIDs returns low-quota auth identifiers in deterministic order.
func (p *PoolManager) LowQuotaIDs() []string {
	return p.idsForState(PoolStateLowQuota)
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
	case PoolStateLowQuota:
		source = p.lowQuota
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
	lowQuota := p.LowQuotaIDs()
	limit := p.LimitIDs()
	target := p.TargetSize()
	return PoolSnapshot{
		TargetSize:    target,
		Provider:      p.Provider(),
		ActiveIDs:     active,
		ReserveIDs:    reserve,
		LowQuotaIDs:   lowQuota,
		LimitIDs:      limit,
		Underfilled:   target > 0 && len(active) < target,
		ActiveCount:   len(active),
		ReserveCount:  len(reserve),
		LowQuotaCount: len(lowQuota),
		LimitCount:    len(limit),
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

// HasTrackedState reports whether the auth is currently assigned to any explicit pool bucket.
func (p *PoolManager) HasTrackedState(authID string) bool {
	if p == nil {
		return false
	}
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return false
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	if _, ok := p.active[authID]; ok {
		return true
	}
	if _, ok := p.reserve[authID]; ok {
		return true
	}
	if _, ok := p.lowQuota[authID]; ok {
		return true
	}
	if _, ok := p.limit[authID]; ok {
		return true
	}
	return false
}

// ActiveFallbackIDs returns active auth IDs that are at or below the low-quota threshold,
// ordered from lowest remaining percent to highest.
func (p *PoolManager) ActiveFallbackIDs(thresholdPercent int) []string {
	if p == nil || thresholdPercent < 0 {
		return nil
	}
	p.mu.RLock()
	defer p.mu.RUnlock()

	type candidate struct {
		id               string
		remainingPercent int
	}
	candidates := make([]candidate, 0, len(p.active))
	for id, member := range p.active {
		if member == nil {
			continue
		}
		if member.RemainingPercent < 0 {
			continue
		}
		if member.RemainingPercent > thresholdPercent {
			continue
		}
		candidates = append(candidates, candidate{id: id, remainingPercent: member.RemainingPercent})
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].remainingPercent == candidates[j].remainingPercent {
			return candidates[i].id < candidates[j].id
		}
		return candidates[i].remainingPercent < candidates[j].remainingPercent
	})
	ids := make([]string, 0, len(candidates))
	for _, item := range candidates {
		ids = append(ids, item.id)
	}
	return ids
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

	for id, member := range p.active {
		publishedAt, ok := previous[id]
		if !ok {
			added = append(added, id)
			continue
		}
		if poolMemberChangedAfter(member, publishedAt) {
			modified = append(modified, id)
		}
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

func poolMemberChangedAfter(member *PoolMember, publishedAt time.Time) bool {
	if member == nil {
		return false
	}
	if publishedAt.IsZero() {
		return true
	}
	for _, ts := range []time.Time{member.LastSelectedAt, member.LastSuccessAt, member.LastProbeAt, member.NextProbeAt} {
		if !ts.IsZero() && ts.After(publishedAt) {
			return true
		}
	}
	return false
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

// BeginUse marks an auth as selected for a live request and protects it from background probing.
func (p *PoolManager) BeginUse(authID string, now time.Time, lease time.Duration) {
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
	member.LastSelectedAt = now
	member.InFlightCount++
	if lease > 0 {
		protectedUntil := now.Add(lease)
		if protectedUntil.After(member.ProtectedUntil) {
			member.ProtectedUntil = protectedUntil
		}
	}
	if active := p.active[authID]; active != nil {
		*active = *member
	}
	if reserve := p.reserve[authID]; reserve != nil {
		*reserve = *member
	}
	if lowQuota := p.lowQuota[authID]; lowQuota != nil {
		*lowQuota = *member
	}
	if limit := p.limit[authID]; limit != nil {
		*limit = *member
	}
}

// RecordRequest records a completed real request outcome and releases any in-flight protection.
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
	member := p.lastSeen[authID]
	if member == nil {
		return
	}
	if member.InFlightCount > 0 {
		member.InFlightCount--
	}
	if success {
		member.LastSuccessAt = now
		member.ConsecutiveFailures = 0
		member.LastProbeReason = ""
	}
	if active := p.active[authID]; active != nil {
		*active = *member
	}
	if reserve := p.reserve[authID]; reserve != nil {
		*reserve = *member
	}
	if lowQuota := p.lowQuota[authID]; lowQuota != nil {
		*lowQuota = *member
	}
	if limit := p.limit[authID]; limit != nil {
		*limit = *member
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
		if member.InFlightCount > 0 {
			continue
		}
		if !member.ProtectedUntil.IsZero() && member.ProtectedUntil.After(now) {
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
		if member.InFlightCount > 0 {
			continue
		}
		if !member.ProtectedUntil.IsZero() && member.ProtectedUntil.After(now) {
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
		if member.InFlightCount > 0 {
			continue
		}
		if !member.ProtectedUntil.IsZero() && member.ProtectedUntil.After(now) {
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
