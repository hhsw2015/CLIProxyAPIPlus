package cliproxy

import (
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/sdk/config"
)

// PoolMetricsSnapshot exposes pool runtime counters through management/monitoring endpoints.
type PoolMetricsSnapshot struct {
	Enabled              bool                   `json:"enabled"`
	Provider             string                 `json:"provider"`
	TargetSize           int                    `json:"target_size"`
	ActiveCount          int                    `json:"active_count"`
	ReserveCount         int                    `json:"reserve_count"`
	LimitCount           int                    `json:"limit_count"`
	PublishedActiveCount int                    `json:"published_active_count"`
	Underfilled          bool                   `json:"underfilled"`
	PromotionsTotal      int64                  `json:"promotions_total"`
	ActiveRemovedTotal   int64                  `json:"active_removed_total"`
	RefreshedTotal       int64                  `json:"refreshed_total"`
	MovedToLimitTotal    int64                  `json:"moved_to_limit_total"`
	DeletedTotal         int64                  `json:"deleted_total"`
	RestoredFromLimit    int64                  `json:"restored_from_limit_total"`
	PublishAddTotal      int64                  `json:"publish_add_total"`
	PublishModifyTotal   int64                  `json:"publish_modify_total"`
	PublishDeleteTotal   int64                  `json:"publish_delete_total"`
	Probes               PoolProbeMetrics       `json:"probes"`
	LastPublishAt        time.Time              `json:"last_publish_at,omitempty"`
	LastEventAt          time.Time              `json:"last_event_at,omitempty"`
}

// PoolProbeMetrics aggregates probe counters by bucket and outcome.
type PoolProbeMetrics struct {
	SuccessTotal int64 `json:"success_total"`
	FailureTotal int64 `json:"failure_total"`
	ActiveTotal  int64 `json:"active_total"`
	ReserveTotal int64 `json:"reserve_total"`
	LimitTotal   int64 `json:"limit_total"`
}

// PoolMetrics tracks observable pool events.
type PoolMetrics struct {
	mu sync.RWMutex

	enabled  bool
	provider string
	target   int

	promotionsTotal    int64
	activeRemovedTotal int64
	refreshedTotal     int64
	movedToLimitTotal  int64
	deletedTotal       int64
	restoredFromLimit  int64
	publishAddTotal    int64
	publishModifyTotal int64
	publishDeleteTotal int64
	probes             PoolProbeMetrics
	lastPublishAt      time.Time
	lastEventAt        time.Time
}

// NewPoolMetrics creates an empty pool metrics tracker.
func NewPoolMetrics(cfg config.PoolManagerConfig) *PoolMetrics {
	return &PoolMetrics{
		enabled:  cfg.Size > 0,
		provider: cfg.Provider,
		target:   cfg.Size,
	}
}

func (m *PoolMetrics) touch(now time.Time) {
	if now.IsZero() {
		now = time.Now()
	}
	m.lastEventAt = now
}

// RecordProbe records a health probe against the given pool bucket.
func (m *PoolMetrics) RecordProbe(bucket PoolState, success bool) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if success {
		m.probes.SuccessTotal++
	} else {
		m.probes.FailureTotal++
	}
	switch bucket {
	case PoolStateActive:
		m.probes.ActiveTotal++
	case PoolStateLimit:
		m.probes.LimitTotal++
	default:
		m.probes.ReserveTotal++
	}
	m.touch(time.Now())
}

// RecordPromotion records a reserve-to-active promotion.
func (m *PoolMetrics) RecordPromotion() {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.promotionsTotal++
	m.touch(time.Now())
}

// RecordActiveRemoval records an active auth being removed from the pool.
func (m *PoolMetrics) RecordActiveRemoval() {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.activeRemovedTotal++
	m.touch(time.Now())
}

// RecordRefreshed records a successful refresh recovery.
func (m *PoolMetrics) RecordRefreshed() {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.refreshedTotal++
	m.touch(time.Now())
}

// RecordMovedToLimit records quota/archive movement.
func (m *PoolMetrics) RecordMovedToLimit() {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.movedToLimitTotal++
	m.touch(time.Now())
}

// RecordDeleted records an auth permanently deleted from the pool.
func (m *PoolMetrics) RecordDeleted() {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.deletedTotal++
	m.touch(time.Now())
}

// RecordRestoredFromLimit records a limit auth being restored for reuse.
func (m *PoolMetrics) RecordRestoredFromLimit() {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.restoredFromLimit++
	m.touch(time.Now())
}

// RecordPublish records runtime publication deltas.
func (m *PoolMetrics) RecordPublish(add, modify, del int) {
	if m == nil {
		return
	}
	now := time.Now()
	m.mu.Lock()
	defer m.mu.Unlock()
	m.publishAddTotal += int64(add)
	m.publishModifyTotal += int64(modify)
	m.publishDeleteTotal += int64(del)
	m.lastPublishAt = now
	m.touch(now)
}

// Snapshot returns an immutable view of current pool counters.
func (m *PoolMetrics) Snapshot(pool PoolSnapshot, publishedActive int) PoolMetricsSnapshot {
	if m == nil {
		return PoolMetricsSnapshot{
			Enabled:              pool.TargetSize > 0,
			Provider:             pool.Provider,
			TargetSize:           pool.TargetSize,
			ActiveCount:          pool.ActiveCount,
			ReserveCount:         pool.ReserveCount,
			LimitCount:           pool.LimitCount,
			PublishedActiveCount: publishedActive,
			Underfilled:          pool.Underfilled,
		}
	}

	m.mu.RLock()
	defer m.mu.RUnlock()

	return PoolMetricsSnapshot{
		Enabled:              m.enabled,
		Provider:             pool.Provider,
		TargetSize:           pool.TargetSize,
		ActiveCount:          pool.ActiveCount,
		ReserveCount:         pool.ReserveCount,
		LimitCount:           pool.LimitCount,
		PublishedActiveCount: publishedActive,
		Underfilled:          pool.Underfilled,
		PromotionsTotal:      m.promotionsTotal,
		ActiveRemovedTotal:   m.activeRemovedTotal,
		RefreshedTotal:       m.refreshedTotal,
		MovedToLimitTotal:    m.movedToLimitTotal,
		DeletedTotal:         m.deletedTotal,
		RestoredFromLimit:    m.restoredFromLimit,
		PublishAddTotal:      m.publishAddTotal,
		PublishModifyTotal:   m.publishModifyTotal,
		PublishDeleteTotal:   m.publishDeleteTotal,
		Probes:               m.probes,
		LastPublishAt:        m.lastPublishAt,
		LastEventAt:          m.lastEventAt,
	}
}
