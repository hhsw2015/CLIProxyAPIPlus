#!/usr/bin/env bash
# End-to-end reusable CPA upgrade: build linux binary + regenerate config,
# scp to VPS, probe on port 8319 with new binary, atomic swap if healthy,
# restart tmux session. Idempotent - safe to re-run.
#
# Env overrides:
#   CPA_DATE   - override --date for gen_llm_config_v2.py (default: today ISO)
#   SKIP_BUILD - skip linux build if already in /tmp/cpa-release
#   SKIP_GEN   - skip regenerating config (use scripts/generated_v2 as-is)
#   VPS_HOST   - default azureuser@4.151.241.30
#   VPS_KEY    - default ~/Downloads/pikapk3219_vps_key.pem
#   PROBE_PORT - default 8319
#   LIVE_PORT  - default 8318
set -euo pipefail

VPS_HOST=${VPS_HOST:-azureuser@4.151.241.30}
VPS_KEY=${VPS_KEY:-$HOME/Downloads/pikapk3219_vps_key.pem}
PROBE_PORT=${PROBE_PORT:-8319}
LIVE_PORT=${LIVE_PORT:-8318}
CPA_DATE=${CPA_DATE:-$(date +%Y-%m-%d)}
REPO_ROOT=$(cd "$(dirname "$0")/.." && pwd)

SSH="ssh -i $VPS_KEY -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null $VPS_HOST"
SCP="scp -i $VPS_KEY -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null"

log() { echo "[$(date +%H:%M:%S)] $*"; }

log "=== 1. build linux binary ==="
if [ "${SKIP_BUILD:-}" = "1" ] && [ -f /tmp/cpa-release/cpa-new-server ]; then
    log "skipping build (SKIP_BUILD=1)"
else
    ( cd "$REPO_ROOT" && bash scripts/build_cpa_linux.sh )
fi
[ -f /tmp/cpa-release/cpa-new-server ] || { log "linux binary missing"; exit 1; }
LOCAL_BIN_SHA=$(shasum -a 256 /tmp/cpa-release/cpa-new-server | head -c 16)
log "local binary sha=$LOCAL_BIN_SHA size=$(du -h /tmp/cpa-release/cpa-new-server | cut -f1)"

log "=== 2. regenerate config (date=$CPA_DATE) ==="
if [ "${SKIP_GEN:-}" = "1" ]; then
    log "skipping config gen (SKIP_GEN=1)"
else
    ( cd "$REPO_ROOT" && python3 scripts/gen_llm_config_v2.py --date "$CPA_DATE" ) | tail -3
fi
CFG=$REPO_ROOT/scripts/generated_v2/cpa-new-config.yaml
[ -f "$CFG" ] || { log "config file missing: $CFG"; exit 1; }
log "config strategy: $(grep '^  strategy:\| strategy: ' "$CFG" | head -1 | tr -s ' ')"

log "=== 3. upload to VPS (skip binary if unchanged) ==="
# Config always uploads (fast). Binary only uploads if its sha differs from
# what's already running on the VPS — a config-only push is ~10x faster and
# skips the binary probe step entirely.
$SCP "$CFG" $VPS_HOST:/home/azureuser/CLIProxyAPIPlus-new/cpa-new-config.yaml.new
LIVE_BIN_SHA=$($SSH 'sha256sum /home/azureuser/CLIProxyAPIPlus-new/cpa-new-server 2>/dev/null' | head -c 16 || echo "")
if [ "$LOCAL_BIN_SHA" = "$LIVE_BIN_SHA" ] && [ -n "$LIVE_BIN_SHA" ]; then
    log "binary unchanged (sha=$LOCAL_BIN_SHA) — config-only upgrade"
    CONFIG_ONLY=1
else
    log "binary changed (local=$LOCAL_BIN_SHA live=$LIVE_BIN_SHA) — uploading"
    $SCP /tmp/cpa-release/cpa-new-server $VPS_HOST:/home/azureuser/CLIProxyAPIPlus-new/cpa-new-server.new
    REMOTE_SHA=$($SSH 'sha256sum /home/azureuser/CLIProxyAPIPlus-new/cpa-new-server.new' | head -c 16)
    log "remote sha=$REMOTE_SHA"
    [ "$LOCAL_BIN_SHA" = "$REMOTE_SHA" ] || { log "SHA mismatch after upload!"; exit 1; }
    $SSH "chmod +x /home/azureuser/CLIProxyAPIPlus-new/cpa-new-server.new"
    CONFIG_ONLY=0
fi

log "=== 4. probe binary+config on VPS:$PROBE_PORT ==="
$SSH bash -s <<REMOTE_EOF
set -euo pipefail
cd /home/azureuser/CLIProxyAPIPlus-new
TS=\$(date +%Y%m%d-%H%M%S)
PROBE_LOG=/tmp/cpa-probe-\$TS.log
CONFIG_ONLY=$CONFIG_ONLY

# The probe validates BOTH new binary (if changed) AND new config. In
# config-only mode we use the currently-running binary against the new
# config to catch config-related startup errors without touching the
# still-good binary.
if [ "\$CONFIG_ONLY" = "1" ]; then
    PROBE_BIN=./cpa-new-server
else
    PROBE_BIN=./cpa-new-server.new
fi

# Clean any leftover probe
pkill -f "cpa-new-server.*probe" 2>/dev/null || true
sleep 1
ss -tln 2>/dev/null | grep -q ":$PROBE_PORT " && { echo "port $PROBE_PORT busy"; exit 1; }

sed "s/^port: $LIVE_PORT/port: $PROBE_PORT/" cpa-new-config.yaml.new > cpa-new-config-probe.yaml
nohup \$PROBE_BIN -config cpa-new-config-probe.yaml >"\$PROBE_LOG" 2>&1 &
PROBE_PID=\$!
echo "probe pid=\$PROBE_PID log=\$PROBE_LOG"

OK=0
for i in \$(seq 1 30); do
    sleep 1
    if ! kill -0 "\$PROBE_PID" 2>/dev/null; then
        echo "probe died early (i=\$i)"
        break
    fi
    if ss -tln 2>/dev/null | grep -q ":$PROBE_PORT "; then
        OK=1
        echo "probe listening after \${i}s"
        break
    fi
done

if [ "\$OK" != "1" ]; then
    echo "=== probe log tail ==="
    tail -60 "\$PROBE_LOG"
    kill "\$PROBE_PID" 2>/dev/null || true
    rm -f cpa-new-config-probe.yaml
    exit 2
fi

CODE=\$(curl -s -o /dev/null -w "%{http_code}" --max-time 5 "http://127.0.0.1:$PROBE_PORT/v1/models" || echo "000")
if [ "\$CODE" != "401" ] && [ "\$CODE" != "200" ]; then
    echo "probe /v1/models unexpected status: \$CODE"
    tail -40 "\$PROBE_LOG"
    kill "\$PROBE_PID" 2>/dev/null || true
    rm -f cpa-new-config-probe.yaml
    exit 3
fi
echo "probe /v1/models=\$CODE (healthy)"

kill "\$PROBE_PID" 2>/dev/null || true
for i in 1 2 3 4 5; do
    kill -0 "\$PROBE_PID" 2>/dev/null || break
    sleep 1
done
kill -9 "\$PROBE_PID" 2>/dev/null || true
rm -f cpa-new-config-probe.yaml
# Retain only the most recent probe log for post-mortem; drop older ones.
ls -1t /tmp/cpa-probe-*.log 2>/dev/null | tail -n +2 | xargs -r rm -f
echo "probe cleaned up"
REMOTE_EOF

log "=== 5. atomic swap + restart on VPS ==="
$SSH bash -s <<REMOTE_EOF
set -euo pipefail
cd /home/azureuser/CLIProxyAPIPlus-new
TS=\$(date +%Y%m%d-%H%M%S)
CONFIG_ONLY=$CONFIG_ONLY

# Prune old artifacts BEFORE swap so we always keep <=2 rolling backups
# even if the swap fails partway. Also clear per-request logs (they can hit
# 1-2 GB in a day and no one reads them after a session ends).
ls -1t cpa-new-server.bak.* 2>/dev/null | tail -n +3 | xargs -r rm -f
ls -1t cpa-new-config.bak.yaml.* 2>/dev/null | tail -n +3 | xargs -r rm -f
rm -rf ~/.cli-proxy-api/logs/
mkdir -p ~/.cli-proxy-api/logs/
echo "cleaned old backups + per-request logs"

cp cpa-new-config.yaml cpa-new-config.bak.yaml.\$TS
mv cpa-new-config.yaml.new cpa-new-config-staged.yaml
mv cpa-new-config-staged.yaml cpa-new-config.yaml
if [ "\$CONFIG_ONLY" = "0" ]; then
    cp cpa-new-server cpa-new-server.bak.\$TS
    mv cpa-new-server.new cpa-new-server.staged
    mv cpa-new-server.staged cpa-new-server
    chmod +x cpa-new-server
    echo "swapped binary + config (backup suffix .\$TS)"
else
    echo "swapped config only (backup suffix .\$TS)"
fi

# Restart tmux — minimum-gap swap. tmux kill-session is scoped to cpa-new;
# no other sessions (e.g. headroom) are affected. The kill+new-session are
# issued back-to-back with no sleeps in between so port :$LIVE_PORT is only
# unbound for ~500ms — the SO_REUSEADDR bind on start-up handles the rest.
# True zero-downtime would require SO_REUSEPORT + coordinated handoff or a
# reverse proxy; CPA has neither, so this is the practical minimum.
tmux kill-session -t cpa-new 2>/dev/null || echo "cpa-new session not present (will create)"
tmux new-session -d -s cpa-new "cd /home/azureuser/CLIProxyAPIPlus-new && ./cpa-new-server -config cpa-new-config.yaml 2>&1 | tee cpa-new.log"
echo "recreated cpa-new session"

# Wait live
for i in \$(seq 1 30); do
    sleep 1
    if ss -tln 2>/dev/null | grep -q ":$LIVE_PORT "; then
        LIVE_CODE=\$(curl -s -o /dev/null -w "%{http_code}" --max-time 5 "http://127.0.0.1:$LIVE_PORT/v1/models" || echo "000")
        echo "live up after \${i}s, /v1/models=\$LIVE_CODE"
        exit 0
    fi
done
echo "LIVE PORT $LIVE_PORT DID NOT COME UP"
tail -60 cpa-new.log
exit 4
REMOTE_EOF

log "=== UPGRADE OK ==="
