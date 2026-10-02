#!/usr/bin/env python3
"""models5.com radar — frontier-model + channel-breadth discovery for the maintenance flow.

Pulls models5.com's machine-readable catalog (one fetch of /catalog.json, a
{models, providers} DB, 12h-cached) and diffs it against our config. Three views:

  (default) GAPS      — frontier models (tool_call + released < N days) we have ZERO
                        coverage of  ->  to-PROBE queue.
  --breadth           — models WE cover, with how many providers models5 knows serve
                        them  ->  where to ADD channels for redundancy (dual-metric
                        axis 2: more channels = stability).
  --providers         — providers we don't have, ranked by how many frontier models
                        they serve  ->  channel-acquisition targets.

IMPORTANT scope (why this COMPLEMENTS, not replaces, our probe layer):
  models5 is a CATALOG of ADVERTISED availability, not verified-on-our-keys truth.
  It will claim e.g. Vertex/Azure serve claude-sonnet-5-5 while OUR probe 404s. So
  every hit is a to-PROBE lead, never "wire it" (iron rule: listed != usable,
  chat/endpoint-probe + accept-any guard before wiring). This sharpens the FRONT of
  the loop (what exists, where to look, how broad); the verify end is unchanged.

Usage:
  python3 scripts/models5_radar.py [--days 90] [--breadth] [--providers] [--no-cache] [--json]
Exit code: 0 always (it's a report). Cert on models5 is expired -> we fetch with an
unverified TLS context on purpose (data is current; the cert is just stale).
"""
import argparse, json, re, ssl, sys, time, urllib.request, datetime
from pathlib import Path
import yaml

GEN = Path(__file__).resolve().parent / "generated_v2"
CONFIG = GEN / "cpa-new-config.yaml"
CACHE = GEN / "models5_catalog.json"
CACHE_TTL = 12 * 3600
CATALOG_URL = "https://models5.com/catalog.json"


def _compact(s):
    return re.sub(r"[^a-z0-9]", "", (s or "").lower())


def bare(mid):
    return (mid or "").split("/")[-1].lower()


def fetch_catalog(no_cache=False):
    if not no_cache and CACHE.exists() and (time.time() - CACHE.stat().st_mtime) < CACHE_TTL:
        try:
            return json.loads(CACHE.read_text()), "cache"
        except Exception:  # noqa: BLE001
            pass
    ctx = ssl.create_default_context()
    ctx.check_hostname = False
    ctx.verify_mode = ssl.CERT_NONE  # models5 cert is expired; data is current
    req = urllib.request.Request(CATALOG_URL, headers={"User-Agent": "cpa-models5-radar/1"})
    with urllib.request.urlopen(req, timeout=40, context=ctx) as r:
        data = json.loads(r.read().decode("utf-8", "replace"))
    CACHE.write_text(json.dumps(data, ensure_ascii=False))
    return data, "live"


def config_ids(config_path=None):
    cfg = yaml.safe_load(Path(config_path or CONFIG).read_text(encoding="utf-8"))
    ids = set()
    for sec in ("claude-api-key", "openai-compatibility", "gemini-api-key", "vertex-api-key"):
        for e in (cfg.get(sec) or []):
            for m in (e.get("models") or []):
                if isinstance(m, dict):
                    for f in ("name", "alias"):
                        if m.get(f):
                            ids.add(m[f])
                elif m:
                    ids.add(m)
    return ids


def config_provider_blob(config_path=None):
    """Lowercase text of the config for fuzzy 'do we have this provider' checks."""
    return Path(config_path or CONFIG).read_text(encoding="utf-8").lower()


def is_covered(model_id, compact_ids):
    ck = _compact(bare(model_id))
    if len(ck) < 4:
        return True
    for cid in compact_ids:
        if len(cid) < 4:
            continue
        # covered iff the models5 id equals, or is a substring of, one of our config
        # ids (we have the same-or-more-specific model). Do NOT treat a config id that
        # is a substring of the models5 id as covered -- that is the version BUMP case
        # (config claude-sonnet-5 vs models5 claude-sonnet-5-5), the signal we want.
        if ck == cid or ck in cid:
            return True
    return False


def as_list(x):
    return x if isinstance(x, list) else list(x.values())


def provider_model_ids(p):
    out = []
    m = p.get("models") or {}
    items = m.items() if isinstance(m, dict) else enumerate(m)
    for k, v in items:
        if isinstance(v, dict):
            out.append(v.get("id") or (k if isinstance(k, str) else v.get("name", "")))
        elif isinstance(k, str):
            out.append(k)
        elif isinstance(v, str):
            out.append(v)
    return [x for x in out if x]


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--days", type=int, default=90, help="frontier window: models released within N days")
    ap.add_argument("--breadth", action="store_true", help="show provider-count universe for models we cover")
    ap.add_argument("--providers", action="store_true", help="rank providers we don't have by frontier coverage")
    ap.add_argument("--no-cache", action="store_true")
    ap.add_argument("--config", default=None)
    ap.add_argument("--json", action="store_true")
    a = ap.parse_args()

    try:
        cat, src = fetch_catalog(a.no_cache)
    except Exception as e:  # noqa: BLE001
        print(f"[models5-radar] catalog unreachable: {e}", file=sys.stderr)
        return 0

    models = as_list(cat.get("models") or [])
    provs = as_list(cat.get("providers") or [])
    cids = config_ids(a.config)
    compact_ids = {_compact(i) for i in cids}
    blob = config_provider_blob(a.config)

    # model bare-id -> set(provider names) and provider -> set(bare model ids)
    model2provs, prov_models = {}, {}
    for p in provs:
        pname = p.get("name") or p.get("id") or "?"
        mids = {bare(x) for x in provider_model_ids(p)}
        prov_models[pname] = (p, mids)
        for b in mids:
            model2provs.setdefault(b, set()).add(pname)

    cutoff = datetime.date.today() - datetime.timedelta(days=a.days)

    def rel_date(m):
        rd = m.get("release_date") or m.get("last_updated") or ""
        try:
            return datetime.date.fromisoformat(str(rd)[:10])
        except Exception:  # noqa: BLE001
            return None

    # ---- (1) GAPS: recent tool_call models we don't cover ----
    gaps = []
    for m in models:
        mid = m.get("id") or ""
        if not mid or is_covered(mid, compact_ids):
            continue
        if not m.get("tool_call"):
            continue
        d = rel_date(m)
        if not d or d < cutoff:
            continue
        gaps.append({
            "model": mid, "date": str(d),
            "weights": "open" if m.get("open_weights") else "closed",
            "providers": len(model2provs.get(bare(mid), [])),
            "context": (m.get("limit") or {}).get("context") if isinstance(m.get("limit"), dict) else m.get("limit"),
            "reasoning": bool(m.get("reasoning")),
            "serves": sorted(model2provs.get(bare(mid), []))[:8],
        })
    gaps.sort(key=lambda c: (c["date"], c["providers"]), reverse=True)

    gen_at = datetime.datetime.now().strftime("%Y-%m-%d %H:%M")
    if a.json:
        print(json.dumps({"generated_at": gen_at, "source": src, "gaps": gaps}, ensure_ascii=False, indent=2))
        return 0

    print("=" * 68)
    print(f"MODELS5 RADAR  (catalog={src}, {len(models)} models / {len(provs)} providers, {gen_at})")
    print("=" * 68)
    print(f"  frontier window <{a.days}d | GAPS (tool_call, we have 0 coverage): {len(gaps)}")
    if not gaps:
        print("  quiet: no recent tool_call frontier model we lack.")
    else:
        print("  🆕 to-PROBE (catalog says exists; listed!=usable, probe before wiring):")
        for c in gaps:
            print(f"     - {c['model']:44s} {c['date']} {c['weights']:6s} "
                  f"{c['providers']:>2d}prov  ctx={c['context']}")
            if c["serves"]:
                print(f"         serves: {', '.join(c['serves'])}")

    # ---- (2) BREADTH: provider universe for models we cover ----
    if a.breadth:
        print("\n  --- BREADTH (models we cover; models5 provider universe = add-channel pool) ---")
        rows = []
        for b, ps in model2provs.items():
            if is_covered(b, compact_ids) and len(ps) >= 5:
                rows.append((len(ps), b, sorted(ps)))
        rows.sort(reverse=True)
        for n, b, ps in rows[:30]:
            print(f"     {b:34s} {n:>3d} providers: {', '.join(ps[:10])}{'…' if n > 10 else ''}")

    # ---- (3) PROVIDERS we don't have, ranked by frontier coverage ----
    if a.providers:
        frontier_bare = {bare(m.get('id', '')) for m in models
                         if m.get('tool_call') and (rel_date(m) or datetime.date.min) >= cutoff}
        rows = []
        for pname, (p, mids) in prov_models.items():
            pid = (p.get("id") or "").lower()
            have = pid and pid in blob or _compact(pname) in _compact(blob)
            if have:
                continue
            fcount = len(mids & frontier_bare)
            rows.append((fcount, len(mids), pname, p.get("id", ""), (p.get("env") or [None])[0]))
        rows.sort(reverse=True)
        print("\n  --- PROVIDERS we don't have, ranked by frontier coverage (acquire targets) ---")
        for fc, tot, pname, pid, env in rows[:25]:
            print(f"     {pname:26s} id={pid:24s} frontier={fc:>2d} total={tot:>4d} env={env}")

    return 0


if __name__ == "__main__":
    sys.exit(main())
