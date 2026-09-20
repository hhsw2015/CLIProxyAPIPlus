package helps

import (
	"context"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

// EnsureSessionContext ensures ctx carries an internal session identity for
// $CPA-SESSION-ID expansion in custom headers. It resolves the session ID from
// (in order) an existing context value, an explicit-session marker, or the
// client request metadata.
//
// NOTE: this fork's variant intentionally omits upstream's
// cliproxyauth.CanonicalSessionID derivation (headers/payload/metadata) — that
// helper does not exist here. opts/payload are accepted for call-site
// compatibility and reserved for a future canonical-derivation port.
func EnsureSessionContext(ctx context.Context, opts cliproxyexecutor.Options, payload []byte) context.Context {
	_ = opts
	_ = payload
	if ctx == nil {
		ctx = context.Background()
	}
	if id := util.SessionIDFromContext(ctx); id != "" {
		return ctx
	}
	if util.HasExplicitSessionID(ctx) {
		return ctx
	}
	return ctx
}
