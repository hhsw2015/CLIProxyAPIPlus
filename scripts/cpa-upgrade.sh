#!/usr/bin/env bash
# In-place atomic CPA upgrade for the CURRENT (new) VPS: build linux binary +
# regenerate config, scp both to the VPS, probe on port 8319 with the new binary,
# atomic swap if healthy, restart. Idempotent - safe to re-run.
#
# NEW-VPS access (2026-09-22): the VPS is a dynamically-allocated proot container
# reached through a cloudflared access tunnel + sshpass password, HOME=/root. The
# host changes on every allocation, so pass VPS_SSH_HOST after re-allocating.
# (Provisioning a FRESH VPS still uses the release flow: build_cpa_linux.sh ->
#  dvina-2api/tools/cpa_release_upload.sh -> deploy_streamlit_lightweight.sh.
#  THIS script is the fast in-place path for an already-running VPS.)
#
# Env overrides:
#   CPA_DATE      - override --date for gen_llm_config_v2.py (default: today ISO)
#   SKIP_BUILD    - skip linux build if already in /tmp/cpa-release
#   SKIP_GEN      - skip regenerating config (use scripts/generated_v2 as-is)
#   VPS_SSH_HOST  - default ssh.geeker.indevs.in   (CHANGES per allocation)
#   VPS_SSH_PORT  - default 9022
#   VPS_SSH_USER  - default root
#   VPS_SSH_PASS  - default '123qwe!@#'
#   REMOTE_DIR    - default /root/CLIProxyAPIPlus-new
#   REMOTE_AUTH   - default /root/.cli-proxy-api   (also the log path: $REMOTE_AUTH/logs)
#   PROBE_PORT    - default 8319
#   LIVE_PORT     - default 8318
set -euo pipefail

# Single source of truth for the (dynamic) VPS connection = the user's login script
# ~/App/CLIProxyAPIPlus/ssh_cpa.sh. Parse host/port/user/pass from it so re-allocating
# the VPS (just update ssh_cpa.sh) auto-propagates here. Explicit env vars still win.
SSH_CPA=${SSH_CPA:-$HOME/App/CLIProxyAPIPlus/ssh_cpa.sh}
if [ -f "$SSH_CPA" ]; then
    _l=$(tr '\n' ' ' < "$SSH_CPA")
    _pass=$(printf '%s' "$_l" | sed -nE "s/.*sshpass -p '([^']*)'.*/\1/p")
    _uhp=$(printf '%s' "$_l" | grep -oE '[a-z]+@[^ ]+ -p [0-9]+' | head -1)
    _user=$(printf '%s' "$_uhp" | sed -E 's/@.*//')
    _host=$(printf '%s' "$_uhp" | sed -E 's/.*@([^ ]+) .*/\1/')
    _port=$(printf '%s' "$_uhp" | sed -E 's/.* -p ([0-9]+)/\1/')
fi
VPS_SSH_HOST=${VPS_SSH_HOST:-${_host:-ssh.geeker.indevs.in}}
VPS_SSH_PORT=${VPS_SSH_PORT:-${_port:-9022}}
VPS_SSH_USER=${VPS_SSH_USER:-${_user:-root}}
VPS_SSH_PASS=${VPS_SSH_PASS:-${_pass:-123qwe!@#}}
REMOTE_DIR=${REMOTE_DIR:-/root/CLIProxyAPIPlus-new}
REMOTE_AUTH=${REMOTE_AUTH:-/root/.cli-proxy-api}
PROBE_PORT=${PROBE_PORT:-8319}
LIVE_PORT=${LIVE_PORT:-8318}
CPA_DATE=${CPA_DATE:-$(date +%Y-%m-%d)}
REPO_ROOT=$(cd "$(dirname "$0")/.." && pwd)

_SSH_OPTS=(-o "ProxyCommand=cloudflared access ssh --hostname %h"
           -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null
           -o ConnectTimeout=25 -o ServerAliveInterval=15)
_ssh() { sshpass -p "$VPS_SSH_PASS" ssh "${_SSH_OPTS[@]}" "$VPS_SSH_USER@$VPS_SSH_HOST" -p "$VPS_SSH_PORT" "$@"; }
_scp() { sshpass -p "$VPS_SSH_PASS" scp "${_SSH_OPTS[@]}" -P "$VPS_SSH_PORT" "$1" "$VPS_SSH_USER@$VPS_SSH_HOST:$2"; }

log() { echo "[$(date +%H:%M:%S)] $*"; }

command -v sshpass >/dev/null || { log "sshpass not found (brew install hudochenkov/sshpass/sshpass)"; exit 1; }
command -v cloudflared >/dev/null || { log "cloudflared not found"; exit 1; }
log "target: $VPS_SSH_USER@$VPS_SSH_HOST:$VPS_SSH_PORT  dir=$REMOTE_DIR"

log "=== 0. connectivity check ==="
_ssh 'echo ok' >/dev/null 2>&1 || { log "cannot reach VPS (host changed? re-allocate then set VPS_SSH_HOST=...)"; exit 1; }

log "=== 1. build linux binary ==="
if [ "${SKIP_BUILD:-}" = "1" ] && [ -f /tmp/cpa-release/cpa-new-server ]; then
    log "skipping build (SKIP_BUILD=1)"
else
    ( cd "$REPO_ROOT" && TARGETS="${TARGETS:-linux}" bash scripts/build_cpa_linux.sh )
fi
[ -f /tmp/cpa-release/cpa-new-server ] || { log "linux binary missing"; exit 1; }
LOCAL_BIN_SHA=$(shasum -a 256 /tmp/cpa-release/cpa-new-server | head -c 16)
log "local binary sha=$LOCAL_BIN_SHA size=$(du -h /tmp/cpa-release/cpa-new-server | cut -f1)"

log "=== 2. regenerate config (date=$CPA_DATE) ==="
if [ "${SKIP_GEN:-}" = "1" ]; then
    log "skipping config gen (SKIP_GEN=1)"
else
    if command -v uv >/dev/null 2>&1; then
        ( cd "$REPO_ROOT" && uv run --with pyjwt --with cryptography --with requests --python 3.12 python3 scripts/gen_llm_config_v2.py --date "$CPA_DATE" ) | tail -3
    else
        ( cd "$REPO_ROOT" && python3 scripts/gen_llm_config_v2.py --date "$CPA_DATE" ) | tail -3
    fi
fi
CFG=$REPO_ROOT/scripts/generated_v2/cpa-new-config.yaml
[ -f "$CFG" ] || { log "config file missing: $CFG"; exit 1; }

# === 2b. DROP-GATE: never deploy a regen that silently lost usable models ===
if [ "${SKIP_GEN:-}" != "1" ]; then
    DIFF_JSON=$REPO_ROOT/scripts/generated_v2/cpa-config-diff.json
    if [ -f "$DIFF_JSON" ]; then
        DROPPED=$(python3 -c "import json,sys; d=json.load(open('$DIFF_JSON')); m=d.get('models_removed') or []; print(len(m)); print('\n'.join('    - '+x for x in m[:40]), file=sys.stderr)" 2>/tmp/cpa-dropped.txt)
        if [ "${DROPPED:-0}" -gt 0 ]; then
            log "⚠️  DROP-GATE: this regen REMOVED $DROPPED client model(s):"
            cat /tmp/cpa-dropped.txt >&2
            if [ "${ALLOW_MODEL_DROP:-}" = "1" ]; then
                log "⚠️  ALLOW_MODEL_DROP=1 set — proceeding despite the drop (acknowledged)."
            else
                log "🛑 ABORT: refusing to deploy a config that drops models. Probe each on the VPS;"
                log "   if truly dead/intentional re-run with ALLOW_MODEL_DROP=1, else wire it back + regen."
                exit 1
            fi
        fi
    fi
fi

# === 2c. adapt config for this VPS (proot /root) ===
# Only rewrite auth-dir (= the log path, $auth/logs). Keep proxy-url as the generator
# default socks5://127.0.0.1:1080 — the VPS provides that socks5 and CPA egresses
# through it (do NOT force direct). Done here so the probe runs the VPS's exact config.
CFG_VPS=$REPO_ROOT/scripts/generated_v2/cpa-new-config-vps.yaml
sed -e "s|^auth-dir: .*|auth-dir: $REMOTE_AUTH|" "$CFG" > "$CFG_VPS"
log "config adapted -> auth-dir: $REMOTE_AUTH (proxy-url kept as socks5://127.0.0.1:1080)"

log "=== 3. upload to VPS (config always; binary only if sha differs) ==="
_ssh "mkdir -p $REMOTE_DIR $REMOTE_AUTH"
_scp "$CFG_VPS" "$REMOTE_DIR/cpa-new-config.yaml.new"
LIVE_BIN_SHA=$(_ssh "sha256sum $REMOTE_DIR/cpa-new-server 2>/dev/null" | head -c 16 || echo "")
if [ "$LOCAL_BIN_SHA" = "$LIVE_BIN_SHA" ] && [ -n "$LIVE_BIN_SHA" ]; then
    log "binary unchanged (sha=$LOCAL_BIN_SHA) — config-only upgrade"
    CONFIG_ONLY=1
else
    log "binary changed (local=$LOCAL_BIN_SHA live=$LIVE_BIN_SHA) — uploading (~105M over the tunnel)"
    _scp /tmp/cpa-release/cpa-new-server "$REMOTE_DIR/cpa-new-server.new"
    REMOTE_SHA=$(_ssh "sha256sum $REMOTE_DIR/cpa-new-server.new" | head -c 16)
    [ "$LOCAL_BIN_SHA" = "$REMOTE_SHA" ] || { log "SHA mismatch after upload!"; exit 1; }
    _ssh "chmod +x $REMOTE_DIR/cpa-new-server.new"
    CONFIG_ONLY=0
fi

log "=== 4. probe binary+config on VPS:$PROBE_PORT ==="
_ssh "REMOTE_DIR=$REMOTE_DIR PROBE_PORT=$PROBE_PORT LIVE_PORT=$LIVE_PORT CONFIG_ONLY=$CONFIG_ONLY bash -s" <<'REMOTE_EOF'
set -euo pipefail
cd "$REMOTE_DIR"
TS=$(date +%Y%m%d-%H%M%S); PROBE_LOG=/tmp/cpa-probe-$TS.log
if [ "$CONFIG_ONLY" = "1" ]; then PROBE_BIN=./cpa-new-server; else PROBE_BIN=./cpa-new-server.new; fi
pkill -f "cpa-new-server.*probe" 2>/dev/null || true
sleep 1
( ss -tln 2>/dev/null || netstat -tln 2>/dev/null ) | grep -q ":$PROBE_PORT " && { echo "port $PROBE_PORT busy"; exit 1; }
sed "s/^port: $LIVE_PORT/port: $PROBE_PORT/" cpa-new-config.yaml.new > cpa-new-config-probe.yaml
JEM=/usr/lib/x86_64-linux-gnu/libjemalloc.so.2; ENV=""; [ -f "$JEM" ] && ENV="LD_PRELOAD=$JEM "
nohup env $ENV $PROBE_BIN -config cpa-new-config-probe.yaml >"$PROBE_LOG" 2>&1 &
PROBE_PID=$!; echo "probe pid=$PROBE_PID log=$PROBE_LOG"
OK=0
for i in $(seq 1 30); do
    sleep 1
    kill -0 "$PROBE_PID" 2>/dev/null || { echo "probe died early (i=$i)"; break; }
    if ( ss -tln 2>/dev/null || netstat -tln 2>/dev/null ) | grep -q ":$PROBE_PORT "; then OK=1; echo "probe listening after ${i}s"; break; fi
done
if [ "$OK" != "1" ]; then echo "=== probe log tail ==="; tail -60 "$PROBE_LOG"; kill "$PROBE_PID" 2>/dev/null || true; rm -f cpa-new-config-probe.yaml; exit 2; fi
CODE=$(curl -s -o /dev/null -w "%{http_code}" --max-time 5 "http://127.0.0.1:$PROBE_PORT/v1/models" || echo "000")
if [ "$CODE" != "401" ] && [ "$CODE" != "200" ]; then echo "probe /v1/models unexpected: $CODE"; tail -40 "$PROBE_LOG"; kill "$PROBE_PID" 2>/dev/null || true; rm -f cpa-new-config-probe.yaml; exit 3; fi
echo "probe /v1/models=$CODE (healthy)"
kill "$PROBE_PID" 2>/dev/null || true; for i in 1 2 3 4 5; do kill -0 "$PROBE_PID" 2>/dev/null || break; sleep 1; done; kill -9 "$PROBE_PID" 2>/dev/null || true
rm -f cpa-new-config-probe.yaml
ls -1t /tmp/cpa-probe-*.log 2>/dev/null | tail -n +2 | xargs -r rm -f
echo "probe cleaned up"
REMOTE_EOF

log "=== 5. atomic swap + restart on VPS ==="
_ssh "REMOTE_DIR=$REMOTE_DIR REMOTE_AUTH=$REMOTE_AUTH LIVE_PORT=$LIVE_PORT CONFIG_ONLY=$CONFIG_ONLY bash -s" <<'REMOTE_EOF'
set -euo pipefail
cd "$REMOTE_DIR"
TS=$(date +%Y%m%d-%H%M%S)
# keep <=2 rolling backups; clear per-request logs (can hit GBs, no one reads them)
ls -1t cpa-new-server.bak.* 2>/dev/null | tail -n +3 | xargs -r rm -f
ls -1t cpa-new-config.bak.yaml.* 2>/dev/null | tail -n +3 | xargs -r rm -f
find "$REMOTE_AUTH/logs" -mindepth 1 -delete 2>/dev/null || true
mkdir -p "$REMOTE_AUTH/logs"
cp cpa-new-config.yaml "cpa-new-config.bak.yaml.$TS" 2>/dev/null || true
mv cpa-new-config.yaml.new cpa-new-config.yaml
if [ "$CONFIG_ONLY" = "0" ]; then
    cp cpa-new-server "cpa-new-server.bak.$TS" 2>/dev/null || true
    mv cpa-new-server.new cpa-new-server
    chmod +x cpa-new-server
    echo "swapped binary + config (backup .$TS)"
else
    echo "swapped config only (backup .$TS)"
fi
# RESTART: the running CPA may NOT be inside a 'cpa-new' tmux session (proot deploy
# orphans it), so a tmux kill-session alone leaves the old process holding the port.
# pkill the actual binary first, then (re)create the tmux session.
tmux kill-session -t cpa-new 2>/dev/null || true
pkill -f "cpa-new-server -config cpa-new-config.yaml" 2>/dev/null || true
sleep 2
pkill -9 -f "cpa-new-server -config cpa-new-config.yaml" 2>/dev/null || true
JEM=/usr/lib/x86_64-linux-gnu/libjemalloc.so.2; ENV=""; [ -f "$JEM" ] && ENV="LD_PRELOAD=$JEM "
tmux new-session -d -s cpa-new "cd $REMOTE_DIR && env $ENV ./cpa-new-server -config cpa-new-config.yaml 2>&1 | tee cpa-new.log"
echo "recreated cpa-new session"
for i in $(seq 1 30); do
    sleep 1
    if ( ss -tln 2>/dev/null || netstat -tln 2>/dev/null ) | grep -q ":$LIVE_PORT "; then
        LIVE_CODE=$(curl -s -o /dev/null -w "%{http_code}" --max-time 5 "http://127.0.0.1:$LIVE_PORT/v1/models" || echo "000")
        echo "live up after ${i}s, /v1/models=$LIVE_CODE"; exit 0
    fi
done
echo "LIVE PORT $LIVE_PORT DID NOT COME UP"; tail -60 cpa-new.log; exit 4
REMOTE_EOF

log "=== UPGRADE OK ==="
