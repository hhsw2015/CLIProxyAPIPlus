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

mode="${1:-status}"
case "$mode" in
  local|cloud)
    tgt="cpa-$mode"          # active provider id (cpa-local / cpa-cloud)
    other=$([ "$mode" = local ] && echo cpa-cloud || echo cpa-local)
    SELF_DIR="$(cd "$(dirname "${BASH_SOURCE[0]:-$0}")" && pwd)"
    # Fast path: if every switchable group is already ● on $tgt we're in the mode. The one-sided
    # expose only changes on an actual switch, so an in-mode re-run can skip the expensive re-expose.
    _sw=$("$M" groups 2>/dev/null | grep -E 'cpa-local/|cpa-cloud/' | grep -v 'group/auto-' || true)
    if [ -n "$_sw" ]; then
      _tot=$(printf '%s\n' "$_sw" | grep -c . || true)
      _on=$(printf '%s\n' "$_sw" | grep -c "● ${tgt}/" || true)
      if [ "$_tot" -gt 0 ] && [ "$_tot" = "$_on" ]; then
        echo "magpie mode: $(echo "$mode" | tr a-z A-Z) already active ($_tot group(s) on $tgt)"
        exit 0
      fi
    fi
    # Both backends ON at all times so groups never degrade (group pick, never provider off/on).
    "$M" provider on cpa-local  >/dev/null 2>&1 || true
    "$M" provider on cpa-cloud  >/dev/null 2>&1 || true
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
