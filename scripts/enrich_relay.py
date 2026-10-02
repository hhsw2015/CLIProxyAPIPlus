#!/usr/bin/env python3
"""Auto-enrich relay-models.json from the models5 catalog — so context windows and
thinking/effort levels never have to be hand-set/guessed again (they went stale:
grok-4.7 was 256K but is really 500K; gpt-sol family was 400K but is 1.05M).

For every relay model found in the models5 catalog (matched by bare id):
  - context  <- models5 limit.context (authoritative window)
  - efforts  <- derived from models5 `reasoning` (models5 has no explicit tier list):
        reasoning=false            -> []                              (no thinking)
        reasoning=true, wire in
          (responses, anthropic)   -> [low, medium, high, xhigh, max] (5-tier flagships)
        reasoning=true, wire=chat  -> [low, medium, high]             (CPA's chat tiers)
Models NOT in models5 (same-day/CN releases, aliases) keep their existing values.

Run standalone to refresh, or let `cpa agent-sync` call it automatically before gen.
Usage: python3 scripts/enrich_relay.py [--relay PATH] [--no-fetch] [--dry-run]
Exit 0 always (report). Prints one line per change.
"""
import argparse, json, os, ssl, sys, time, urllib.request
from pathlib import Path

REPO = Path(__file__).resolve().parent
CACHE = REPO / "generated_v2/models5_catalog.json"
CACHE_TTL = 12 * 3600
CATALOG_URL = "https://models5.com/catalog.json"
DEFAULT_RELAY = Path(os.path.expanduser("~/Dev/CLIProxyAPIPlus/scripts/relay-models.json"))

TIER5 = ["low", "medium", "high", "xhigh", "max"]
TIER3 = ["low", "medium", "high"]


def bare(x):
    return str(x).split("/")[-1].lower()


def load_catalog(no_fetch=False):
    if CACHE.exists() and (no_fetch or time.time() - CACHE.stat().st_mtime < CACHE_TTL):
        try:
            return json.loads(CACHE.read_text())
        except Exception:  # noqa: BLE001
            pass
    ctx = ssl.create_default_context()
    ctx.check_hostname = False
    ctx.verify_mode = ssl.CERT_NONE  # models5 cert is expired; data is current
    req = urllib.request.Request(CATALOG_URL, headers={"User-Agent": "cpa-enrich/1"})
    data = json.loads(urllib.request.urlopen(req, timeout=40, context=ctx).read().decode("utf-8", "replace"))
    CACHE.write_text(json.dumps(data, ensure_ascii=False))
    return data


def catalog_specs(cat):
    """bare id -> {context, reasoning}."""
    models = cat.get("models")
    models = models if isinstance(models, list) else list(models.values())
    out = {}
    for m in models:
        b = bare(m.get("id", ""))
        if not b:
            continue
        lim = m.get("limit")
        cw = (lim.get("context") if isinstance(lim, dict) else lim)
        out[b] = {"context": cw, "reasoning": bool(m.get("reasoning"))}
    return out


def efforts_for(wire, reasoning):
    if not reasoning:
        return []
    return TIER5 if wire in ("responses", "anthropic") else TIER3


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--relay", default=str(DEFAULT_RELAY))
    ap.add_argument("--no-fetch", action="store_true")
    ap.add_argument("--dry-run", action="store_true")
    a = ap.parse_args()
    relay_path = Path(a.relay)
    if not relay_path.exists():
        print(f"[enrich-relay] relay not found: {relay_path}", file=sys.stderr)
        return 0
    try:
        specs = catalog_specs(load_catalog(a.no_fetch))
    except Exception as e:  # noqa: BLE001
        print(f"[enrich-relay] models5 catalog unavailable ({e}); skipping enrichment", file=sys.stderr)
        return 0
    d = json.loads(relay_path.read_text())
    models = d["models"] if isinstance(d, dict) else d
    changed = 0
    missing = []
    for m in models:
        if not isinstance(m, dict) or not m.get("id"):
            continue
        b = bare(m["id"])
        sp = specs.get(b)
        if not sp:
            missing.append(m["id"])
            continue
        # context
        if sp["context"] and m.get("context") != sp["context"]:
            print(f"  {m['id']:34s} context {m.get('context')} -> {sp['context']}")
            m["context"] = sp["context"]; changed += 1
        # efforts (from reasoning + wire)
        want = efforts_for(m.get("wire", "chat"), sp["reasoning"])
        if m.get("efforts") != want:
            print(f"  {m['id']:34s} efforts {m.get('efforts')} -> {want}  (reasoning={sp['reasoning']})")
            m["efforts"] = want; changed += 1
    if changed and not a.dry_run:
        relay_path.write_text(json.dumps(d, ensure_ascii=False, indent=2) + "\n")
    verb = "would change" if a.dry_run else "changed"
    print(f"[enrich-relay] {verb} {changed} field(s); {len(missing)} model(s) not in models5 (kept): "
          + ", ".join(missing[:8]) + (" …" if len(missing) > 8 else ""))
    return 0


if __name__ == "__main__":
    sys.exit(main())
