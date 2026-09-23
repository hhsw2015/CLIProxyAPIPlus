#!/usr/bin/env bash
# One-command automated CPA deploy. Build once, place everywhere:
#   1. build linux + mac binaries (scripts/build_cpa_linux.sh)
#   2. Mac  -> ~/App/CLIProxyAPIPlus   (local config: proxy :10808 / proxy-pool OFF /
#             auth-dir ~/.cli-proxy-api). PLACED ONLY, not started — you launch it.
#   3. Linux -> GitHub release (gibunxi4201/kube-node-diag@v2.0) = single source of
#             truth, + update GH_ASSET_CPA in _relay.env.
#   4. VPS  -> atomic upgrade (VPS downloads the binary FROM GitHub + config uploaded +
#             probe :8319 + atomic swap + restart tmux cpa-new) IF the VPS is reachable.
#             Unreachable (the VPS may simply not be started) = GRACEFUL SKIP, not an error.
#
# Steps 1-3 always run (no VPS needed). Step 4 self-skips when the VPS is off.
# Idempotent. Env passthrough: CPA_DATE, SKIP_GEN, ALLOW_MODEL_DROP, VPS_SSH_HOST, ...
set -uo pipefail
REPO_ROOT=$(cd "$(dirname "$0")/.." && pwd)
cd "$REPO_ROOT"
log() { echo "[deploy-all $(date +%H:%M:%S)] $*"; }

log "=== 1. build linux + mac ==="
TARGETS="linux mac" bash scripts/build_cpa_linux.sh || { log "build FAILED"; exit 1; }
[ -f /tmp/cpa-release/cpa-new-server ] || { log "linux binary missing"; exit 1; }
[ -f /tmp/cpa-release/cpa-new-server-mac-arm64 ] || { log "mac binary missing"; exit 1; }
# cpa-upgrade-local.sh (SKIP_BUILD) expects the mac binary at /tmp/cpa-mac-arm64
cp /tmp/cpa-release/cpa-new-server-mac-arm64 /tmp/cpa-mac-arm64

log "=== 2. Mac local placement (place only, no restart) ==="
SKIP_BUILD=1 bash scripts/cpa-upgrade-local.sh || { log "Mac placement FAILED"; exit 1; }

# cpa-upgrade.sh: uploads the linux binary to the GH release (source of truth), then
# — if the VPS is reachable — runs the full atomic upgrade; otherwise it exits 0 after
# updating GH (VPS off is not an error). SKIP_BUILD=1 reuses the binary built in step 1.
log "=== 3+4. Linux -> GH release, then VPS atomic upgrade (graceful skip if VPS off) ==="
SKIP_BUILD=1 bash scripts/cpa-upgrade.sh || { log "GH upload / VPS upgrade FAILED"; exit 1; }

log "=== deploy-all done ==="
