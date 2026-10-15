#!/usr/bin/env bash
# magpie_mode.sh local|cloud|status — flip the CPA backend BEHIND the magpie gateway.
#
# ONE-SIDED expose, so BARE model ids follow the switch too (not just groups):
#   active provider  = full chat catalog exposed (serves bare ids, e.g. Codex's gpt-6-astra)
#   standby provider = only the group-anchor models (which must NOT include any bare-accessed id)
# Why: magpie's resolveIn() pins a bare id that TWO providers expose to the first entry (ignores
# mode); a bare id only ONE provider exposes routes there. So a bare-accessed model (gpt-6-astra)
# exposed on the ACTIVE side only will follow local<->cloud. Groups route via their picked
# provider/model (expose-independent) with both providers ON, so they switch via group pick.
# `group auto off` (magpie >=0.1.607) keeps the one overlap from spawning auto-<model> groups.
#
# Why group pick (not provider off/on): off degrades a group's member -> magpie reverts the agent
# configs it manages. Both ON + re-pointing the group is transparent (Codex keeps working).
set -euo pipefail
M=${MAGPIE_BIN:-/Applications/magpie.app/Contents/MacOS/magpie}
[ -x "$M" ] || { echo "magpie binary not found: $M" >&2; exit 1; }

# Reachability of the cloud backend. A DEAD cloud (headroom 530 / unreachable) kept ON makes
# magpie poll its models.url forever and spin the daemon (~155% CPU — 2026-10-15 incident). So
# when cloud is the STANDBY (mode=local) and down, we turn it OFF instead of ON.
CLOUD_URL="https://headroom.geeker.indevs.in"
cloud_up() {
  local tok; tok=$("$HOME/.commandcode/relay-token.sh" 2>/dev/null || true)
  [ -n "$tok" ] || return 1
  [ "$(curl -s -m 6 -o /dev/null -w '%{http_code}' "$CLOUD_URL/v1/models" -H "Authorization: Bearer $tok" 2>/dev/null || true)" = 200 ]
}

mode="${1:-status}"
case "$mode" in
  local|cloud)
    tgt="cpa-$mode"          # active provider id (cpa-local / cpa-cloud)
    other=$([ "$mode" = local ] && echo cpa-cloud || echo cpa-local)
    SELF_DIR="$(cd "$(dirname "${BASH_SOURCE[0]:-$0}")" && pwd)"
    # Fast path: skip the expensive full re-expose when already in $mode (e.g. start.sh re-running
    # local every launch). This setup routes by ONE-SIDED expose, so the PRIMARY mode signal is the
    # provider expose state (active=$tgt has the full catalog, standby=$other has anchors-only ~1),
    # NOT groups — there are usually 0 switchable groups, so the old group-only check never fired and
    # every run paid the ~12s re-expose. Check expose first; groups second (only if any exist).
    _pv=$("$M" providers 2>/dev/null || true)
    _tn=$(printf '%s\n' "$_pv" | grep -E "[[:space:]]${tgt}[[:space:]]" | grep -oE '[0-9]+ of [0-9]+ models' | grep -oE '^[0-9]+' | head -1 || true)
    _cn=$(printf '%s\n' "$_pv" | grep -E "[[:space:]]${other}[[:space:]]" | grep -oE '[0-9]+ of [0-9]+ models' | grep -oE '^[0-9]+' | head -1 || true)
    _expose_ok=0
    if [ -n "${_tn:-}" ] && [ -n "${_cn:-}" ] && [ "$_tn" -gt 1 ] && [ "$_cn" -le 1 ]; then _expose_ok=1; fi
    _groups_ok=1
    _sw=$("$M" groups 2>/dev/null | grep -E 'cpa-local/|cpa-cloud/' | grep -v 'group/auto-' || true)
    if [ -n "$_sw" ]; then
      _tot=$(printf '%s\n' "$_sw" | grep -c . || true)
      _gon=$(printf '%s\n' "$_sw" | grep -c "● ${tgt}/" || true)
      [ "$_tot" -gt 0 ] && [ "$_tot" = "$_gon" ] || _groups_ok=0
    fi
    if [ "$_expose_ok" = 1 ] && [ "$_groups_ok" = 1 ]; then
      echo "magpie mode: $(echo "$mode" | tr a-z A-Z) already active (expose $tgt=$_tn / $other=$_cn; groups ok) — skipped re-expose"
      exit 0
    fi
    # Both backends normally ON so groups never degrade (group pick, never provider off/on). BUT
    # keep cpa-cloud ON only if it's the active target OR reachable: a dead cloud kept ON makes
    # magpie poll its models.url and spin the daemon. If it's the standby (mode=local) and down,
    # turn it OFF (with 0 switchable groups — the usual case — nothing degrades).
    "$M" provider on cpa-local  >/dev/null 2>&1 || true
    if [ "$mode" = cloud ] || cloud_up; then
      "$M" provider on cpa-cloud  >/dev/null 2>&1 || true
    else
      "$M" provider off cpa-cloud >/dev/null 2>&1 || true
      echo "  cpa-cloud UNREACHABLE -> kept OFF (avoids magpie polling the dead backend / CPU spin)"
    fi
    # Anchor (group-member) models from magpie_sync. gpt-6-astra is deliberately NOT among them
    # (Codex uses the BARE id), so it won't be exposed on the standby -> stays one-sided -> follows.
    # STANDBY keeps ONE throwaway model exposed (effectively nothing), so NO agent-accessed bare id
    # sits on both sides -> each bare id has exactly ONE exposer = the ACTIVE side = follows the
    # switch (resolveIn routes a bare id to the sole provider that exposes it). Active gets the full
    # chat catalog below. (group pick still runs for any legacy groups; harmless.)
    "$M" provider models "$other" qwen1.5-0.5b-chat >/dev/null 2>&1 || true
    _ids=$(python3 - "$mode" "$SELF_DIR" <<'PY'
import sys
mode, d = sys.argv[1], sys.argv[2]
sys.path.insert(0, d)
try:
    import magpie_sync as m
    base = m.LOCAL if mode == "local" else m.CLOUD
    print(" ".join(i for i in m.fetch_ids(base) if m.ischat(i)))
except Exception:
    print("")
PY
)
    if [ -n "$_ids" ]; then
      "$M" provider models "$tgt" $_ids >/dev/null 2>&1 || true
      echo "  active $tgt: $(printf '%s' "$_ids" | wc -w | tr -d ' ') models (full); standby $other: anchors only"
    else
      echo "  WARN: catalog fetch for $tgt failed (backend down?); picker unchanged, group-pick still applied"
    fi
    # Re-point every switchable [cpa-local,cpa-cloud] group to $tgt (identified by MEMBERS, not name;
    # excluding magpie's implicit auto-* groups).
    n=0
    while IFS= read -r line; do
      case "$line" in *group/auto-*) continue;; esac
      printf '%s' "$line" | grep -q 'cpa-local/' && printf '%s' "$line" | grep -q 'cpa-cloud/' || continue
      g=$(printf '%s' "$line" | grep -oE 'group/[A-Za-z0-9._-]+' | head -1 | sed 's#group/##')
      member=$(printf '%s' "$line" | grep -oE "${tgt}/[A-Za-z0-9._/-]+" | head -1)
      [ -n "$g" ] && [ -n "$member" ] || continue
      "$M" group pick "$g" "$member" >/dev/null 2>&1 && { echo "  $g -> $member"; n=$((n+1)); }
    done < <("$M" groups 2>/dev/null | grep -E 'cpa-local/|cpa-cloud/')
    echo "magpie mode: $(echo "$mode" | tr a-z A-Z)  (one-sided: $tgt full / $other anchors; $n group(s) picked to $tgt)"
    ;;
  status)
    "$M" groups 2>/dev/null | grep -E 'cpa-local/|cpa-cloud/' | grep -v 'group/auto-' \
      || echo "(no switchable cpa groups yet — run magpie_sync.py)"
    ;;
  *)
    echo "usage: magpie_mode.sh local|cloud|status" >&2; exit 1 ;;
esac
