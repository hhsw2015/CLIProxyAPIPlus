#!/usr/bin/env python3
"""Manual vertex ban / config-change watcher.

Run this ad-hoc to answer TWO questions in one shot, with minimum footprint:

  1. Anthropic still banning our SA?       -> 1 Vertex API call
  2. Upstream config changed (Vertex bit)? -> 1 MSE fetch of channels-claude

Design goals (in order): fast, private, safe.
  - Fast: two independent async requests, prints result within seconds.
  - Private: probes ONE model + ONE project (least fingerprint).
            No prompt content, max_tokens=1, cheapest identity-check possible.
  - Safe: never prints SA JSON / private keys / creds. State stored in a
          file next to this script; diff mode shows only structural changes
          (project list, private_key_id) — no key material.

Usage:
  python3 scripts/vertex_watch.py               # check both, print status
  python3 scripts/vertex_watch.py --sa          # only SA ban probe
  python3 scripts/vertex_watch.py --config      # only config diff
  python3 scripts/vertex_watch.py --json        # machine-readable output

Exit codes:
  0 = both unchanged (still banned & config same)
  1 = SA unbanned (good news, act now)
  2 = config changed (vertex bit — inspect + regen)
  3 = both changed
  10+ = internal error

The state file (scripts/vertex_watch_state.json) is gitignored.
"""

from __future__ import annotations

import argparse
import base64
import concurrent.futures
import hashlib
import json
import os
import re
import sys
import time
from pathlib import Path

# Shared Vertex probe primitives — same code path used by gen_llm_config_v2.py
# so both scripts agree on token minting, URL construction, and response
# classification.
sys.path.insert(0, str(Path(__file__).resolve().parent))
from vertex_probe_common import (  # noqa: E402
    classify_probe_result,
    mint_gcp_token,
    probe_vertex_model,
)

REPO_ROOT = Path(__file__).resolve().parent.parent
STATE_FILE = Path(__file__).resolve().parent / "vertex_watch_state.json"
# Shared probe cache with gen_llm_config_v2.py. When watch confirms a project
# is banned/unbanned it stamps ALL (model, project, region) rows in this file
# so the next `gen` run doesn't have to re-probe — a single cheap watch call
# invalidates gen's stale success cache in one shot.
GEN_PROBE_CACHE = Path(__file__).resolve().parent / "probe_state_vertex.json"

# Probe target — kept minimal + realistic. opus-4-7 was working before the ban,
# so a probe against it looks like a normal customer request, not "hunting for
# banned models". max_tokens=1 = ~1 output token = cheapest possible identity
# check the Anthropic-Vertex endpoint responds to.
#
# 2026-08-12 update: ban proved to be per-(project, Anthropic Marketplace SKU)
# not per-SA — cla-01~05 got flagged while cla-06~10 stayed live, even though
# they share the same SA. Single-project probe misdiagnoses SA as fully banned.
#
# Project list is derived dynamically from the upstream vertex-claude config
# (via MSE fetch) so it always matches what CPA would actually use. Env vars
# VERTEX_WATCH_PROJECTS and VERTEX_WATCH_MODELS override for manual scoping.
#
# By default we probe EVERY model declared by upstream, PLUS canary models
# not currently declared (opus-5, sonnet-5, fable-5) — these return 404 today
# but flipping to 200 = new SKU landed and gen should be re-run. Full matrix
# gives an accurate picture of which (project, model) pairs are actually usable.
_CANARY_MODELS = ["claude-opus-4-7", "claude-opus-5", "claude-sonnet-5", "claude-fable-5"]
PROBE_TIMEOUT = int(os.environ.get("VERTEX_WATCH_TIMEOUT", "10"))


def _load_state() -> dict:
    if STATE_FILE.exists():
        try:
            return json.loads(STATE_FILE.read_text())
        except Exception:
            pass
    return {"sa": {}, "config": {}}


def _save_state(state: dict) -> None:
    STATE_FILE.write_text(json.dumps(state, indent=2))


def _sync_gen_probe_cache(per_project: dict[str, dict]) -> dict:
    """Push per-project ban results into gen_llm_config_v2.py's probe cache.

    Ban is project-scoped: if cla-01 returns 498 for opus-4-7 on global, every
    Anthropic model on cla-01 across every region is also blocked (Anthropic
    Marketplace enforces per-project). So we do two things:

      1. Rewrite every existing (model|project|region) row for banned projects
         to keep=false — invalidates gen's stale success cache.
      2. SEED synthetic rows for (banned_project, model, region) combos that
         don't yet have a cache entry, so gen doesn't attempt a live probe
         that would just return 498 after wasting 5s per (model, region, proj).

    For usable projects we only refresh existing rows — we can't seed usable
    rows because a project's Marketplace SKU coverage varies (e.g. opus-5 SKU
    isn't on cla-06~10). Gen's per-model probe is still the authority there.

    Returns a summary dict with counts of rows updated.
    """
    if not GEN_PROBE_CACHE.exists():
        return {"synced": False, "reason": "gen cache file does not exist yet"}
    try:
        cache = json.loads(GEN_PROBE_CACHE.read_text())
    except Exception as e:
        return {"synced": False, "reason": f"cache read failed: {e}"}

    now = time.time()
    updated_banned = 0
    refreshed = 0
    seeded = 0
    banned_set = {proj for proj, pp in per_project.items() if pp.get("banned")}
    usable_set = {proj for proj, pp in per_project.items() if pp.get("usable_models")}

    # 1) Update existing rows.
    existing_keys = set(cache.keys())
    for key, entry in list(cache.items()):
        parts = key.split("|")
        if len(parts) != 3:
            continue
        _model, project, _region = parts
        if project in banned_set:
            entry["keep"] = False
            entry["status"] = 498
            entry["ts"] = now
            entry["error"] = "banned (per-project ban confirmed by vertex_watch)"
            updated_banned += 1
        elif project in usable_set and entry.get("keep") is True:
            entry["ts"] = now
            refreshed += 1

    # 2) Seed synthetic banned rows for (banned_project, model, region) combos
    #    gen's default coverage will probe. Union of models seen in cache plus
    #    the models we already know from per_project — no hardcoded list.
    cached_models: set[str] = set()
    cached_regions: set[str] = set()
    for k in existing_keys:
        parts = k.split("|")
        if len(parts) == 3:
            cached_models.add(parts[0])
            cached_regions.add(parts[2])
    # Also add models from the watch matrix (in case some project has models
    # gen never probed before).
    for pp in per_project.values():
        for lst_key in ("banned_models", "usable_models", "missing_models"):
            for m in pp.get(lst_key) or []:
                cached_models.add(m)
    # Always include the 4 regions gen fans out over.
    cached_regions.update({"global", "us-east5", "europe-west1", "asia-southeast1"})

    for project in banned_set:
        for model in cached_models:
            for region in cached_regions:
                key = f"{model}|{project}|{region}"
                if key in cache:
                    continue
                cache[key] = {
                    "keep": False,
                    "status": 498,
                    "ts": now,
                    "error": "banned (seeded by vertex_watch, per-project SA ban)",
                }
                seeded += 1

    GEN_PROBE_CACHE.write_text(json.dumps(cache, indent=2, sort_keys=True))
    return {
        "synced": True,
        "banned_marked": updated_banned,
        "success_refreshed": refreshed,
        "banned_seeded": seeded,
        "cache_path": str(GEN_PROBE_CACHE),
    }


def _extract_sa_credentials_b64() -> str:
    """Pull one SA credentials-b64 blob from the generated CPA config.

    We reuse the SA that gen_llm_config_v2.py already embedded — no need to
    duplicate the SA source anywhere. If the config isn't generated yet, we
    fall back to the raw source in the gen script.
    """
    cpa_config = REPO_ROOT / "scripts" / "generated_v2" / "cpa-new-config.yaml"
    if cpa_config.exists():
        cfg = cpa_config.read_text()
        m = re.search(
            r"vertex-sa-global-claude-opus-4-7.*?credentials-b64: \|\n((?:      [A-Za-z0-9+/=]+\n)+)",
            cfg,
            re.DOTALL,
        )
        if m:
            return "".join(line.strip() for line in m.group(1).splitlines())
    raise RuntimeError(
        f"SA credentials not found in {cpa_config}. "
        "Run `python3 scripts/gen_llm_config_v2.py --no-auto` first."
    )


def _mint_token(creds_b64: str) -> str:
    """Thin wrapper — shared with gen_llm_config_v2.py via vertex_probe_common."""
    return mint_gcp_token(creds_b64, timeout=PROBE_TIMEOUT)


def probe_sa_ban(projects: list[str], models: list[str]) -> dict:
    """Probe each (project, model) pair for ban/availability status.

    Returns:
        {
          ok: bool,
          matrix: [{project, model, http, banned, usable, note}, ...],
          per_project: {project: {banned: bool, usable_models: [...], banned_models: [...], missing_models: [...]}},
          usable_projects: [...],   # any model returned 2xx/429 on this project
          banned_projects: [...],   # canary model (first in PROBE_MODELS) returned 498
          fully_banned: bool,
          elapsed_ms: int
        }
    """
    started = time.time()
    if not projects:
        return {
            "ok": False,
            "note": "empty project list",
            "matrix": [],
            "per_project": {},
            "usable_projects": [],
            "banned_projects": [],
            "fully_banned": False,
            "elapsed_ms": 0,
        }
    try:
        creds_b64 = _extract_sa_credentials_b64()
        token = _mint_token(creds_b64)
    except Exception as e:
        return {
            "ok": False,
            "note": f"token error: {type(e).__name__}: {e}",
            "matrix": [],
            "per_project": {},
            "usable_projects": [],
            "banned_projects": [],
            "fully_banned": False,
            "elapsed_ms": int((time.time() - started) * 1000),
        }

    def one(pair: tuple[str, str]) -> dict:
        project, model = pair
        try:
            code, text = probe_vertex_model(project, "global", model, token, PROBE_TIMEOUT)
        except Exception as e:
            return {
                "project": project,
                "model": model,
                "http": 0,
                "banned": None,
                "missing": None,
                "usable": False,
                "note": f"error: {type(e).__name__}",
            }
        status = classify_probe_result(code, text)
        return {
            "project": project,
            "model": model,
            "http": code,
            "banned": status == "banned",
            "missing": status == "missing",
            "usable": status in ("live", "quota"),
            "note": status if status != "error" else f"unexpected: {text[:60]}",
        }

    pairs = [(p, m) for p in projects for m in models]
    with concurrent.futures.ThreadPoolExecutor(max_workers=min(16, len(pairs))) as pool:
        matrix = list(pool.map(one, pairs))

    per_project: dict[str, dict] = {}
    for row in matrix:
        p = row["project"]
        pp = per_project.setdefault(p, {"banned_models": [], "usable_models": [], "missing_models": []})
        if row["banned"]:
            pp["banned_models"].append(row["model"])
        elif row["usable"]:
            pp["usable_models"].append(row["model"])
        elif row["missing"]:
            pp["missing_models"].append(row["model"])

    # A project is "banned" when the canary model (first in `models`)
    # reports 498. Missing SKUs (404) are NOT a ban.
    canary = models[0] if models else None
    banned_projects = []
    for p, pp in per_project.items():
        pp["banned"] = canary in pp["banned_models"]
        if pp["banned"]:
            banned_projects.append(p)
    usable_projects = [p for p, pp in per_project.items() if pp["usable_models"]]

    return {
        "ok": True,
        "matrix": matrix,
        "per_project": per_project,
        "usable_projects": sorted(usable_projects),
        "banned_projects": sorted(banned_projects),
        "fully_banned": len(banned_projects) == len(projects),
        "elapsed_ms": int((time.time() - started) * 1000),
    }


def _mse_fetch_channels_claude() -> dict | None:
    """Fetch ONLY channels-claude via the existing MSE OpenAPI helper.

    Reuses tools/export_gpt_proxy_daily.py's _mse_get_config so we don't
    duplicate auth logic. Returns parsed JSON or None on failure.
    """
    dvina = Path.home() / "Dev/dvina-2api"
    if not dvina.exists():
        return None
    sys.path.insert(0, str(dvina))
    try:
        from tools.export_gpt_proxy_daily import _mse_get_config  # type: ignore
    except Exception as e:
        print(f"[warn] cannot import MSE helper: {e}", file=sys.stderr)
        return None
    try:
        raw = _mse_get_config("channels-claude", "SKYROUTER_TEST")
        return json.loads(raw) if isinstance(raw, str) else raw
    except Exception as e:
        print(f"[warn] MSE fetch failed: {e}", file=sys.stderr)
        return None
    finally:
        sys.path.pop(0)


def summarize_vertex_channel(payload: dict) -> dict:
    """Extract just the Vertex-relevant fingerprints — no key material."""
    if not payload:
        return {}
    channels = payload.get("channels", [])
    vertex = next((c for c in channels if c.get("name") == "vertex-claude"), None)
    if not vertex:
        return {"present": False}

    cfg = vertex.get("config", {})
    # Decode credentials just enough to extract public identity fields —
    # never store the private key.
    sa_id = {}
    b64 = cfg.get("vertex_credentials_b64")
    if b64:
        try:
            sa = json.loads(base64.b64decode(b64))
            sa_id = {
                "client_email": sa.get("client_email"),
                "project_id": sa.get("project_id"),
                "private_key_id": sa.get("private_key_id"),  # public fingerprint
            }
        except Exception:
            sa_id = {"error": "decode_failed"}

    # Project list + model coverage (structure only)
    nodes = cfg.get("nodes", []) or []
    projects = sorted(
        {
            n.get("project") or n.get("project_id") or n.get("ProjectId") or ""
            for n in nodes
            if isinstance(n, dict)
        }
    )
    projects = [p for p in projects if p]
    regions = sorted({n.get("region", "") for n in nodes if isinstance(n, dict) and n.get("region")})
    models = set()
    for n in nodes:
        if isinstance(n, dict):
            m = n.get("models", {})
            if isinstance(m, dict):
                models.update(m.keys())
    return {
        "present": True,
        "sa": sa_id,
        "projects": projects,
        "regions": regions,
        "models": sorted(models),
        "node_count": len(nodes),
    }


def probe_config_change(state: dict) -> dict:
    """Fetch upstream config, hash the Vertex-relevant slice, diff vs state."""
    started = time.time()
    payload = _mse_fetch_channels_claude()
    if payload is None:
        return {"ok": False, "note": "fetch_failed", "elapsed_ms": int((time.time() - started) * 1000)}
    summary = summarize_vertex_channel(payload)
    canonical = json.dumps(summary, sort_keys=True)
    h = hashlib.sha256(canonical.encode()).hexdigest()[:16]
    last = state.get("config", {})
    changed = last.get("hash") and last.get("hash") != h
    return {
        "ok": True,
        "hash": h,
        "summary": summary,
        "changed": bool(changed),
        "prev_summary": last.get("summary", {}),
        "elapsed_ms": int((time.time() - started) * 1000),
    }


def _print_human(sa: dict, cfg: dict, sync: dict) -> None:
    now = time.strftime("%Y-%m-%d %H:%M:%S")
    print(f"=== vertex-watch @ {now} ===")

    # SA probe (per-(project, model) matrix)
    if sa.get("ok"):
        total_projects = len(sa["per_project"])
        usable = sa["usable_projects"]
        banned = sa["banned_projects"]
        if not banned:
            print(f"✅ SA probe: all {total_projects} projects live [{sa['elapsed_ms']}ms]")
        elif not usable:
            print(f"🚫 SA probe: all {total_projects} projects BANNED [{sa['elapsed_ms']}ms]")
        else:
            print(f"⚠️  SA probe: {len(usable)}/{total_projects} projects live, {len(banned)} banned [{sa['elapsed_ms']}ms]")

        # Per-project detail: show which models are usable/banned/missing.
        for project in sorted(sa["per_project"].keys()):
            pp = sa["per_project"][project]
            if pp.get("banned"):
                marker = "🚫"
                status = f"BANNED ({len(pp['banned_models'])} models blocked)"
            elif pp["usable_models"]:
                marker = "✅"
                status = f"live ({len(pp['usable_models'])} models)"
            else:
                marker = "❓"
                status = "no usable models"
            print(f"   {marker} {project:6} {status}")
            if pp["usable_models"]:
                print(f"      usable:  {sorted(pp['usable_models'])}")
            if pp["missing_models"]:
                print(f"      missing: {sorted(pp['missing_models'])}")
            if pp["banned_models"] and not pp.get("banned"):
                # Partial ban — rare but flag it.
                print(f"      banned:  {sorted(pp['banned_models'])}")
    else:
        print(f"⚠️  SA probe failed: {sa.get('note')}")

    # Config diff
    if cfg.get("ok"):
        s = cfg["summary"]
        if not s.get("present"):
            print("⚠️  Upstream config: vertex-claude channel MISSING")
        else:
            print(f"📄 Upstream config: hash={cfg['hash']} projects={len(s['projects'])} regions={s.get('regions', [])} models={len(s['models'])} [{cfg['elapsed_ms']}ms]")
            print(f"   SA identity: {s['sa'].get('client_email','?')} pkid={s['sa'].get('private_key_id','?')[:16]}...")
            print(f"   projects: {s['projects']}")
            print(f"   models:   {s['models']}")
        if cfg["changed"]:
            print("🔔 CONFIG CHANGED since last run — diff:")
            prev = cfg.get("prev_summary") or {}
            curr = s
            for key in ("sa", "projects", "models", "regions"):
                if prev.get(key) != curr.get(key):
                    print(f"   [{key}] before={prev.get(key)!r}")
                    print(f"   [{key}] after ={curr.get(key)!r}")
    else:
        print(f"⚠️  Config fetch failed: {cfg.get('note')}")

    # Gen-cache sync summary
    if sync.get("synced"):
        b, r, s = sync["banned_marked"], sync["success_refreshed"], sync.get("banned_seeded", 0)
        if b or r or s:
            print(f"💾 Synced gen probe cache: {b} banned marked, {s} banned seeded, {r} success refreshed")
        else:
            print(f"💾 Gen probe cache unchanged")
    elif sync:
        print(f"💾 Gen cache sync skipped: {sync.get('reason')}")


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--sa", action="store_true", help="only SA ban probe")
    ap.add_argument("--config", action="store_true", help="only config diff")
    ap.add_argument("--json", action="store_true", help="machine-readable output")
    ap.add_argument(
        "--recheck",
        action="store_true",
        help=(
            "Fast mode: probe ONLY (project, model) pairs previously flagged "
            "as banned/missing/error in state.json. Use for quick recovery "
            "checks — skips full matrix, dozens of ms per pair."
        ),
    )
    ap.add_argument(
        "--ban-only",
        action="store_true",
        help=(
            "Fastest recovery check: probe only the canary model on projects "
            "that were banned last time (skip missing-SKU rechecks and skip "
            "config fetch). Use to quickly see if Anthropic unbanned cla-01~05."
        ),
    )
    args = ap.parse_args()

    do_sa = args.sa or args.ban_only or not (args.sa or args.config or args.ban_only)
    do_config = args.config or (not (args.sa or args.config or args.ban_only))
    # --ban-only implies --recheck-shape (narrow to canary on prev-banned)
    # and skips config fetch to save the ~1.5s MSE round-trip.

    state = _load_state()
    sa_result: dict = {}
    cfg_result: dict = {}

    # We must fetch config first — the SA probe needs the project list from it.
    # Env var VERTEX_WATCH_PROJECTS override, otherwise take from upstream config.
    if do_config or do_sa:
        cfg_result = probe_config_change(state)

    if do_sa:
        # Projects: env override → upstream config → empty.
        override = os.environ.get("VERTEX_WATCH_PROJECTS", "").strip()
        if override:
            projects = [p.strip() for p in override.split(",") if p.strip()]
        elif cfg_result.get("ok") and cfg_result["summary"].get("projects"):
            projects = list(cfg_result["summary"]["projects"])
        else:
            projects = []

        # Models: env override → (upstream-declared ∪ known canaries) → canaries only.
        # We put the canary (opus-4-7) first — it's the one probe_sa_ban uses to
        # decide ban status. The rest give a full availability matrix.
        model_override = os.environ.get("VERTEX_WATCH_MODELS", "").strip()
        if model_override:
            models = [m.strip() for m in model_override.split(",") if m.strip()]
        else:
            declared = list(cfg_result.get("summary", {}).get("models", []) or [])
            # Ensure canary is first; then upstream-declared models; then extra
            # canaries (opus-5/sonnet-5/fable-5) which may 404 today but flip to
            # 200 the moment a new SKU is enabled.
            seen = set()
            models = []
            for m in [_CANARY_MODELS[0]] + declared + _CANARY_MODELS[1:]:
                if m and m not in seen:
                    seen.add(m)
                    models.append(m)

        # --recheck / --ban-only: fast-recovery probe.
        # Ban is project-scoped, so for a previously-banned project we only need
        # to re-probe the CANARY (first model) — if it flips 498→200, the whole
        # project recovered. --ban-only stops there; --recheck also probes
        # previously-missing (project, model) pairs. Both read the banned set
        # DYNAMICALLY from state.json — nothing is hardcoded, so if next week
        # a different subset gets banned/unbanned this still tracks it.
        # Prior-good rows are carried forward from state so per_project detail
        # stays complete.
        recheck_pairs: list[tuple[str, str]] | None = None
        prior_matrix: dict[tuple[str, str], dict] = {}
        if args.recheck or args.ban_only:
            prev_pp = state.get("sa", {}).get("per_project", {}) or {}
            pairs_to_recheck: list[tuple[str, str]] = []
            canary = models[0]
            for proj, pp in prev_pp.items():
                if proj not in projects:
                    continue
                if pp.get("banned"):
                    # One canary probe is enough to detect unban.
                    pairs_to_recheck.append((proj, canary))
                    # NB: don't carry forward banned rows — a project unbans
                    # atomically, so if canary flips we'll re-run gen probes.
                else:
                    if args.ban_only:
                        # Skip missing-SKU rechecks in ban-only mode; just keep
                        # the previously-usable snapshot for full detail.
                        for m in pp.get("usable_models") or []:
                            prior_matrix[(proj, m)] = {
                                "project": proj, "model": m, "http": 200,
                                "banned": False, "missing": False, "usable": True,
                                "note": "cached: was live",
                            }
                        continue
                    # --recheck: also probe previously-missing pairs so we can
                    # detect SKU activation (e.g. opus-5 landing on cla-06~10).
                    for m in pp.get("missing_models") or []:
                        if m in models:
                            pairs_to_recheck.append((proj, m))
                    for m in pp.get("usable_models") or []:
                        prior_matrix[(proj, m)] = {
                            "project": proj, "model": m, "http": 200,
                            "banned": False, "missing": False, "usable": True,
                            "note": "cached: was live",
                        }
            if pairs_to_recheck:
                recheck_pairs = pairs_to_recheck

        if recheck_pairs is not None:
            # Feed just the bad pairs; probe_sa_ban expects (projects, models)
            # so pass unique projects + unique models spanning the bad set —
            # then filter its output down to the pairs we actually asked for.
            r_projects = sorted({p for p, _ in recheck_pairs})
            r_models = sorted({m for _, m in recheck_pairs})
            sa_result = probe_sa_ban(r_projects, r_models)
            # Restrict matrix to the recheck set
            wanted = set(recheck_pairs)
            if sa_result.get("ok"):
                sa_result["matrix"] = [
                    row for row in sa_result["matrix"]
                    if (row["project"], row["model"]) in wanted
                ]
                # Merge prior-good rows back into per_project detail
                # so downstream code still sees full picture.
                merged: dict[str, dict] = {}
                for row in list(prior_matrix.values()) + sa_result["matrix"]:
                    p = row["project"]
                    pp = merged.setdefault(
                        p,
                        {
                            "banned_models": [],
                            "usable_models": [],
                            "missing_models": [],
                        },
                    )
                    if row["banned"]:
                        pp["banned_models"].append(row["model"])
                    elif row["usable"]:
                        pp["usable_models"].append(row["model"])
                    elif row["missing"]:
                        pp["missing_models"].append(row["model"])
                canary = models[0]
                banned_projects = []
                for p, pp in merged.items():
                    pp["banned"] = canary in pp["banned_models"]
                    if pp["banned"]:
                        banned_projects.append(p)
                sa_result["per_project"] = merged
                sa_result["banned_projects"] = sorted(banned_projects)
                sa_result["usable_projects"] = sorted(
                    p for p, pp in merged.items() if pp["usable_models"]
                )
        else:
            sa_result = probe_sa_ban(projects, models)

    # Persist
    sync_summary: dict = {}
    if do_sa and sa_result.get("ok"):
        prev = state.get("sa", {})
        new = {
            "usable_projects": sa_result["usable_projects"],
            "banned_projects": sa_result["banned_projects"],
            "per_project": sa_result["per_project"],
            "last_check": time.time(),
        }
        prev_banned = sorted(prev.get("banned_projects", []) or [])
        if prev_banned != sorted(new["banned_projects"]):
            new["last_change"] = time.time()
        else:
            new["last_change"] = prev.get("last_change")
        state["sa"] = new
        # Propagate ban/usable info into gen_llm_config_v2.py's shared probe
        # cache so a subsequent `gen` run doesn't need to re-probe.
        sync_summary = _sync_gen_probe_cache(sa_result["per_project"])
    if do_config and cfg_result.get("ok"):
        state["config"] = {
            "hash": cfg_result["hash"],
            "summary": cfg_result["summary"],
            "last_check": time.time(),
        }
    _save_state(state)

    if args.json:
        print(json.dumps({"sa": sa_result, "config": cfg_result, "sync": sync_summary}, indent=2, default=str))
    else:
        _print_human(sa_result, cfg_result, sync_summary)

    # Exit codes signal action-worthy transitions.
    exit_code = 0
    if do_sa and sa_result.get("ok"):
        # Success signal: SA was fully banned last time and now any project works.
        prev = state.get("sa", {}).get("banned_projects", [])
        prev_all_banned = bool(prev) and len(prev) == len(sa_result.get("usable_projects", []) + sa_result.get("banned_projects", []))
        if prev_all_banned and sa_result.get("usable_projects"):
            exit_code |= 1  # some project unbanned
        elif not sa_result.get("banned_projects"):
            # All live — treat as "no action needed", exit 0.
            pass
    if do_config and cfg_result.get("changed"):
        exit_code |= 2  # config drift
    return exit_code


if __name__ == "__main__":
    sys.exit(main())
