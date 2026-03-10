package cliproxy

import "time"

// PoolState represents the current membership state of an auth inside the pool manager.
type PoolState string

const (
	PoolStateActive   PoolState = "active"
	PoolStateReserve  PoolState = "reserve"
	PoolStateLowQuota PoolState = "low_quota"
	PoolStateLimit    PoolState = "limit"
)

// PoolMember stores the runtime pool tracking information for an auth.
type PoolMember struct {
	AuthID               string
	Provider             string
	PoolState            PoolState
	RemainingPercent     int
	LastSelectedAt       time.Time
	LastSuccessAt        time.Time
	LastProbeAt          time.Time
	NextProbeAt          time.Time
	LastQuotaProbeAt     time.Time
	NextQuotaProbeAt     time.Time
	ProtectedUntil       time.Time
	InFlightCount        int
	ConsecutiveFailures  int
	LastProbeReason      string
	LastQuotaProbeReason string
}

// AuthDisposition describes the final post-processing outcome for a credential.
type AuthDisposition struct {
	AuthID         string
	Provider       string
	Model          string
	Healthy        bool
	PoolEligible   bool
	Deleted        bool
	MovedToLimit   bool
	Refreshed      bool
	QuotaExceeded  bool
	NextRetryAfter time.Time
	NextRecoverAt  time.Time
	Source         string
}

// PoolSnapshot is a read-only snapshot used by tests and diagnostics.
type PoolSnapshot struct {
	TargetSize    int
	Provider      string
	ActiveIDs     []string
	ReserveIDs    []string
	LowQuotaIDs   []string
	LimitIDs      []string
	Underfilled   bool
	ActiveCount   int
	ReserveCount  int
	LowQuotaCount int
	LimitCount    int
}
