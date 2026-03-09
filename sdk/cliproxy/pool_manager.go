package cliproxy

import (
	"sort"
	"strings"
	"sync"

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
