#!/usr/bin/env python3
"""Azure fleet hidden-deployment miner.

The SKYROUTER azure dump LAGS newly-deployed models (gpt-6.1-sol was live on 66
resources while the dump listed 0). Azure resources are SPECIALIZED — each hosts a
different model-family subset — so a model can be deployed fleet-wide yet invisible
to both the dump and any small sample.

This probes each candidate DEPLOYMENT NAME (dot form; Azure puts the name in the URL
path) across EVERY azure resource. 404 = not deployed; 200 = live; 400/403 = deployed
(param error / quota provisioning). Then cross-references our generated config to flag
HIDDEN = deployed on azure but not served (via azure) in config = a mining lead.

Usage: python3 scripts/probe_azure_deployments.py [--cands a,b,c] [--workers 30]
                                                   [--out /tmp/azure_probe.json]
Iron rule unchanged: a 200/400/403 = deployment EXISTS; confirm real usability with a
proper request before wiring (this tool's minimal body only proves existence).
"""
import argparse, json, glob, os, sys, urllib.request, urllib.error, ssl
import concurrent.futures as cf
from collections import Counter, defaultdict
from pathlib import Path
import yaml

REPO = Path(__file__).resolve().parents[1]
CONFIG = REPO / "scripts/generated_v2/cpa-new-config.yaml"

# Recent OpenAI frontier deployment names to hunt (dot form). Families are probed
# whole because Azure often deploys a family together (sol/astra/luna/terra/codex).
DEFAULT_CANDS = [
    # gpt-6.1 family (gpt-6.1-sol confirmed live 2026-09-30)
    "gpt-6.1", "gpt-6.1-sol", "gpt-6.1-astra", "gpt-6.1-luna", "gpt-6.1-terra",
    "gpt-6.1-mini", "gpt-6.1-nano", "gpt-6.1-codex", "gpt-6.1-sol-codex", "gpt-6.1-pro",
    # gpt-6 family
    "gpt-6", "gpt-6-astra", "gpt-6-sol", "gpt-6-luna", "gpt-6-terra", "gpt-6-mini",
    "gpt-6-nano", "gpt-6-codex", "gpt-6-sol-codex", "gpt-6-astra-codex", "gpt-6-pro",
    # gpt-5.x recent
    "gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna", "gpt-5.6-sol-codex", "gpt-5.6-codex",
    "gpt-5.5", "gpt-5.5-pro", "gpt-5.4", "gpt-5.3-codex",
    # o-series reasoning
    "o3", "o3-pro", "o3-mini", "o4", "o4-mini", "o4-pro",
    # media / other
    "gpt-image-2", "gpt-image-2.5", "sora-2", "sora-2-pro", "gpt-audio", "gpt-realtime",
]


def azure_resources():
    """All unique azure resources (host -> (base, key, api_version)) from the dump."""
    res = {}
    for f in glob.glob(os.path.expanduser(
            "~/Dev/dvina-2api/artifacts/api-keys/*-nacos/2026-*/SKYROUTER_channels-azure.json")):
        try:
            d = json.load(open(f))
        except Exception:  # noqa: BLE001
            continue
        for ch in (d.get("channels") or []):
            for n in ((ch.get("config") or {}).get("nodes") or []):
                b, k = n.get("base_url"), n.get("api_key")
                ms = n.get("models") or {}
                if not b or not k:
                    continue
                h = b.split("//")[-1].split(".")[0]
                ver = next((v.get("api_version") for v in ms.values()
                            if isinstance(v, dict) and v.get("api_version")), "2025-04-01-preview")
                res.setdefault(h, (b, k, ver))
    return res


def config_served():
    """Model names our config serves, and which are served via an azure host."""
    cfg = yaml.safe_load(CONFIG.read_text())
    served, via_azure = set(), set()
    for e in (cfg.get("openai-compatibility") or []):
        base = (e.get("base-url") or "").lower()
        for m in (e.get("models") or []):
            nm = m.get("name") if isinstance(m, dict) else m
            if not nm:
                continue
            served.add(str(nm))
            if "openai.azure.com" in base:
                via_azure.add(str(nm))
    return served, via_azure


def probe(base, key, dep, ver, timeout=15):
    url = f"{base.rstrip('/')}/openai/deployments/{dep}/chat/completions?api-version={ver}"
    body = json.dumps({"messages": [{"role": "user", "content": "hi"}],
                       "max_completion_tokens": 16}).encode()
    try:
        return urllib.request.urlopen(urllib.request.Request(
            url, data=body, headers={"api-key": key, "Content-Type": "application/json"}),
            timeout=timeout, context=ssl.create_default_context()).status
    except urllib.error.HTTPError as e:
        return e.code
    except Exception:  # noqa: BLE001
        return "ERR"


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--cands", default="", help="comma deployment names; empty = built-in frontier set")
    ap.add_argument("--workers", type=int, default=30)
    ap.add_argument("--out", default="/tmp/azure_probe.json")
    a = ap.parse_args()
    cands = [x for x in a.cands.split(",") if x] or DEFAULT_CANDS
    res = azure_resources()
    served, via_azure = config_served()
    print(f"azure resources: {len(res)} | candidates: {len(cands)}", file=sys.stderr)

    tasks = [(dep, h, b, k, v) for dep in cands for h, (b, k, v) in res.items()]
    hits = defaultdict(lambda: {"live": [], "codes": Counter()})
    with cf.ThreadPoolExecutor(max_workers=a.workers) as pool:
        futs = {pool.submit(probe, b, k, dep, v): (dep, h) for dep, h, b, k, v in tasks}
        for fu in cf.as_completed(futs):
            dep, h = futs[fu]
            c = fu.result()
            hits[dep]["codes"][c] += 1
            if c in (200, 403):  # 200 = live, 403 = deployed + quota provisioning
                hits[dep]["live"].append(h)

    # USABLE = 200 or 403 (deploymentless works, deployed). A 400 ("does not support
    # deploymentless inference" / "operation unsupported") = model RECOGNIZED but NOT
    # deploymentlessly callable (no deployment we can hit) -> NOT a usable lead. 404 =
    # unknown/not-deployed. Only 200/403 count as usable; only those become HIDDEN leads.
    rows = []
    for dep in cands:
        c = hits[dep]["codes"]
        usable = c.get(200, 0) + c.get(403, 0)
        recognized = c.get(400, 0)
        rows.append({
            "dep": dep, "usable": usable, "recognized_400": recognized,
            "in_config": dep in served, "via_azure": dep in via_azure,
            "hidden": usable > 0 and dep not in via_azure,
            "codes": dict(c), "usable_hosts": sorted(hits[dep]["live"])[:5],
        })
    rows.sort(key=lambda r: (r["usable"], r["recognized_400"]), reverse=True)
    Path(a.out).write_text(json.dumps(rows, ensure_ascii=False, indent=2))

    print("\n=== AZURE DEPLOYMENT SCAN (USABLE = 200/403; 400 = recognized-but-not-deploymentless) ===")
    for r in rows:
        if r["usable"] == 0 and r["recognized_400"] == 0:
            continue
        if r["usable"] > 0:
            flag = "🔴HIDDEN(wire)" if r["hidden"] else "✅in-config"
        else:
            flag = "⚠️recognized-only (no usable deployment)"
        print(f"  {r['dep']:20s} usable {r['usable']:>3d}  400 {r['recognized_400']:>3d}  {flag}"
              + (f"  e.g. {','.join(r['usable_hosts'][:3])}" if r["usable_hosts"] else ""))
    print(f"\n  saved -> {a.out}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
