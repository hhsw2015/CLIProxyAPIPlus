package auth

import (
	"context"
	"time"
)

// AuthDisposition describes the final post-processing outcome for a credential
// after MarkResult has classified it (success / cooldown / delete / etc.).
// Fork-only surface kept here so the pool manager can observe transitions
// without depending on the outer cliproxy package.
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

type dispositionSourceContextKey struct{}

// WithDispositionSource tags a request context with a caller-provided source
// label (for example "pool_probe" or "handler"). MarkResult attaches this label
// to the emitted AuthDisposition so pool observers can distinguish probe-driven
// events from regular request outcomes.
func WithDispositionSource(ctx context.Context, source string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, dispositionSourceContextKey{}, source)
}

// DispositionSourceFromContext returns the label previously stored by
// WithDispositionSource, or an empty string when none is present.
func DispositionSourceFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	if v, ok := ctx.Value(dispositionSourceContextKey{}).(string); ok {
		return v
	}
	return ""
}

// OnAuthDisposition implements the disposition hook for callers that embed
// NoopHook. Real Hook implementations may shadow this method to observe events.
func (NoopHook) OnAuthDisposition(context.Context, AuthDisposition) {}
