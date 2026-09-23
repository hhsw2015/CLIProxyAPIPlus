#!/usr/bin/env bash
# Mac 本地模式原子升级: build mac binary + regenerate config,
# 原子 swap 到 ~/App/CLIProxyAPIPlus, restart tmux session.
# 向后兼容 VPS 模式(保留 cpa-upgrade.sh 不变)。
set -euo pipefail

LOCAL_APP_DIR=${LOCAL_APP_DIR:-~/App/CLIProxyAPIPlus}
LOCAL_APP_DIR=$(eval echo "$LOCAL_APP_DIR")  # expand ~
PROBE_PORT=${PROBE_PORT:-8319}
LIVE_PORT=${LIVE_PORT:-8318}
CPA_DATE=${CPA_DATE:-$(date +%Y-%m-%d)}
REPO_ROOT=$(cd "$(dirname "$0")/.." && pwd)

log() { echo "[$(date +%H:%M:%S)] $*"; }

log "=== 1. build mac arm64 binary ==="
if [ "${SKIP_BUILD:-}" = "1" ] && [ -f /tmp/cpa-mac-arm64 ]; then
    log "skipping build (SKIP_BUILD=1)"
else
    ( cd "$REPO_ROOT" && GOOS=darwin GOARCH=arm64 go build -o /tmp/cpa-mac-arm64 ./cmd/server )
fi
[ -f /tmp/cpa-mac-arm64 ] || { log "mac binary missing"; exit 1; }
LOCAL_BIN_SHA=$(shasum -a 256 /tmp/cpa-mac-arm64 | head -c 16)
log "local binary sha=$LOCAL_BIN_SHA size=$(du -h /tmp/cpa-mac-arm64 | cut -f1)"

log "=== 2. regenerate config (date=$CPA_DATE) ==="
if [ "${SKIP_GEN:-}" = "1" ]; then
    log "skipping config gen (SKIP_GEN=1)"
else
    if command -v uv >/dev/null 2>&1; then
        ( cd "$REPO_ROOT" && CPA_LOCAL=1 uv run --with pyjwt --with cryptography --with requests --python 3.12 python3 scripts/gen_llm_config_v2.py --date "$CPA_DATE" ) | tail -3
    else
        ( cd "$REPO_ROOT" && CPA_LOCAL=1 python3 scripts/gen_llm_config_v2.py --date "$CPA_DATE" ) | tail -3
    fi
fi
CFG=$REPO_ROOT/scripts/generated_v2/cpa-new-config.yaml
[ -f "$CFG" ] || { log "config file missing: $CFG"; exit 1; }

# 2b. DROP-GATE (同 VPS)
if [ "${SKIP_GEN:-}" != "1" ]; then
    DIFF_JSON=$REPO_ROOT/scripts/generated_v2/cpa-config-diff.json
    if [ -f "$DIFF_JSON" ]; then
        DROPPED=$(python3 -c "import json,sys; d=json.load(open('$DIFF_JSON')); m=d.get('models_removed') or []; print(len(m)); print('\n'.join('    - '+x for x in m[:40]), file=sys.stderr)" 2>/tmp/cpa-dropped.txt)
        if [ "${DROPPED:-0}" -gt 0 ]; then
            log "⚠️  DROP-GATE: this regen REMOVED $DROPPED client model(s):"
            cat /tmp/cpa-dropped.txt >&2
            if [ "${ALLOW_MODEL_DROP:-}" = "1" ]; then
                log "⚠️  ALLOW_MODEL_DROP=1 set — proceeding."
            else
                log "🛑 ABORT: refusing to deploy a config that drops models. Re-run with ALLOW_MODEL_DROP=1 if verified."
                exit 1
            fi
        fi
    fi
fi

log "=== 3. stage to $LOCAL_APP_DIR ==="
mkdir -p "$LOCAL_APP_DIR"
cp /tmp/cpa-mac-arm64 "$LOCAL_APP_DIR/cpa-new-server.new"
chmod +x "$LOCAL_APP_DIR/cpa-new-server.new"
cp "$CFG" "$LOCAL_APP_DIR/cpa-new-config.yaml.new"
log "staged binary + config"

log "=== 4. place binary + config (no probe, no restart — you start it) ==="
cd "$LOCAL_APP_DIR"
TS=$(date +%Y%m%d-%H%M%S)
cp cpa-new-server "cpa-new-server.bak.$TS" 2>/dev/null || true
cp cpa-new-config.yaml "cpa-new-config.yaml.bak.$TS" 2>/dev/null || true
ls -1t cpa-new-server.bak.* 2>/dev/null | tail -n +3 | xargs rm -f 2>/dev/null || true
ls -1t cpa-new-config.yaml.bak.* 2>/dev/null | tail -n +3 | xargs rm -f 2>/dev/null || true
mv cpa-new-server.new cpa-new-server
chmod +x cpa-new-server
mv cpa-new-config.yaml.new cpa-new-config.yaml
log "placed binary + config (backup .$TS). NOT started — launch it yourself:"
log "  cd $LOCAL_APP_DIR && ./cpa-new-server -config cpa-new-config.yaml"
log "=== LOCAL PLACE OK ==="
