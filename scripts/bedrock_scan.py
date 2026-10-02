#!/usr/bin/env python3
"""Bedrock non-Claude discovery — Converse-probe EVERY foundation model with BOTH the
bare id AND the us. cross-region inference profile.

Why this exists: the earlier manual enumeration (maintenance-log ⑲) tested only BARE
model-ids. Cross-region models reject on-demand bare invocation with
`ValidationException: Invocation ... with on-demand throughput isn't supported` and only
work via the `us.`/`eu.`/`apac.` inference profile -> the bare-only scan FALSE-NEGATIVED
every profile-only model (grok-4.7, qwen3-coder-next, kimi-k2.5, glm-4.7, gpt-oss-20b,
llama4-scout, ...). This tool tries both forms so nothing is missed.

Output: the ready-to-paste BEDROCK_CONVERSE_MODELS entries for the models NOT already
wired. Dict shape = {bare_key: working_value}:
  - KEY  = the BARE bedrock id (its provider prefix must be in isConverseModel so the
           bedrock executor routes the client request to Converse).
  - VALUE = the form that actually returned 200 (bare if it worked, else the us. profile).

Iron rule: Bedrock Converse returns the real model (no house-default), so a 200 here is
genuinely usable -- but claude/* is served via the Anthropic /v1/messages path, not here.

Usage: python3 scripts/bedrock_scan.py [--region us-east-1] [--workers 12] [--json]
Creds are read from the generated config's ClaudeAws (AKIAU6GDVU...) entry.
"""
import argparse, json, re, sys
import concurrent.futures as cf
from pathlib import Path

REPO = Path(__file__).resolve().parents[1]
CONFIG = REPO / "scripts/generated_v2/cpa-new-config.yaml"

def wired_values():
    """Read the currently-wired VALUEs straight from gen_llm_config_v2.py's
    BEDROCK_CONVERSE_MODELS (region-keyed) so the NEW filter never goes stale."""
    import re as _re
    gen = (REPO / "scripts/gen_llm_config_v2.py").read_text(encoding="utf-8")
    blk = gen[gen.find("BEDROCK_CONVERSE_MODELS = {"): gen.find("def generate_bedrock")]
    return {v for _, v in _re.findall(r'"([^"]+)":\s*"([^"]+)"', blk)}


WIRED_VALUES = wired_values()


def bedrock_accounts():
    """YAML-parse the config and return DISTINCT accounts {ak: (sk, [regions])}. Usability
    is per-account (entitlement differs even when ListFoundationModels is identical), so
    every account must be scanned -- not just one. Fields can be interleaved with
    rebuild-mid-system-message etc., so YAML-parse instead of grepping fields independently."""
    import yaml
    cfg = yaml.safe_load(CONFIG.read_text(encoding="utf-8"))
    accts = {}
    for sec in (cfg or {}).values():
        if not isinstance(sec, list):
            continue
        for e in sec:
            if isinstance(e, dict) and e.get("aws-access-key-id") and e.get("aws-secret-access-key"):
                ak = e["aws-access-key-id"]
                a = accts.setdefault(ak, [str(e["aws-secret-access-key"]), set()])
                a[1].add(e.get("aws-region", "us-east-1"))
    if not accts:
        print("[bedrock-scan] no AWS creds in config", file=sys.stderr); sys.exit(1)
    return accts


def scan_account(boto3, ak, sk, region, workers):
    """Return {bare_key: working_value} the account can actually Converse in `region`."""
    bc = boto3.client("bedrock", region_name=region, aws_access_key_id=ak, aws_secret_access_key=sk)
    rt = boto3.client("bedrock-runtime", region_name=region, aws_access_key_id=ak, aws_secret_access_key=sk)
    try:
        ids = sorted({m["modelId"] for m in bc.list_foundation_models().get("modelSummaries", [])})
    except Exception as e:  # noqa: BLE001
        print(f"    [{region}] list ERR {str(e).split(':')[-1][:50]}", file=sys.stderr); return {}
    cand = [i for i in ids if not i.lower().startswith("claude") and "anthropic.claude" not in i.lower()]

    import time as _t

    def converse_ok(mid):
        # Retry on throttling: parallel probing hits ThrottlingException, which is NOT a
        # real "unusable" -- a single try false-negatived grok-4.7 (verified 200) once.
        for attempt in range(3):
            try:
                rt.converse(modelId=mid, messages=[{"role": "user", "content": [{"text": "OK"}]}],
                            inferenceConfig={"maxTokens": 8})
                return True
            except Exception as e:  # noqa: BLE001
                s = str(e)
                if ("Throttl" in s or "TooManyRequests" in s or "rate" in s.lower()) and attempt < 2:
                    _t.sleep(2 * (attempt + 1))
                    continue
                return False
        return False

    def resolve(base):
        bare = base[3:] if base.startswith(("us.", "eu.", "apac.")) else base
        us = bare if base.startswith(("us.", "eu.", "apac.")) else "us." + bare
        if converse_ok(bare):
            return (bare, bare)
        if converse_ok(us):
            return (bare, us)
        return None

    bases = sorted({(i[3:] if i.startswith(("us.", "eu.", "apac.")) else i) for i in cand})
    out = {}
    with cf.ThreadPoolExecutor(max_workers=workers) as ex:
        for r in ex.map(resolve, bases):
            if r:
                out[r[0]] = r[1]
    return out


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--region", default=None, help="scan only this region (default: each account's configured regions + us-east-1/us-west-2)")
    ap.add_argument("--workers", type=int, default=12)
    ap.add_argument("--all", action="store_true", help="show already-wired too")
    ap.add_argument("--json", action="store_true")
    a = ap.parse_args()
    try:
        import boto3
    except ImportError:
        print("[bedrock-scan] pip install boto3", file=sys.stderr); return 1
    accts = bedrock_accounts()
    union = {}          # bare_key -> working_value (usable on ANY account)
    per_acct = {}       # ak -> {key: value}
    for ak, (sk, regions) in accts.items():
        regs = [a.region] if a.region else sorted(set(regions) | {"us-east-1", "us-west-2"})
        acc = {}
        for region in regs:
            got = scan_account(boto3, ak, sk, region, a.workers)
            acc.update(got)
            if got:
                # first region with hits is enough (catalog is region-stable); keep scanning
                # only adds cross-region-only models, cheap enough to continue.
                pass
        per_acct[ak] = acc
        for k, v in acc.items():
            union.setdefault(k, v)
        print(f"[{ak[:12]}...] usable non-Claude: {len(acc)}", file=sys.stderr)

    new = union if a.all else {k: v for k, v in union.items() if v not in WIRED_VALUES}
    if a.json:
        print(json.dumps({"per_account": {k: len(v) for k, v in per_acct.items()},
                          "union": union, "new": new}, indent=2))
        return 0
    print(f"=== BEDROCK non-Claude scan: {len(accts)} accounts -> union {len(union)} usable, {len(new)} NEW ===")
    for ak, acc in per_acct.items():
        extra = sorted(set(acc) - {k for a2, v2 in per_acct.items() if a2 != ak for k in v2})
        print(f"  {ak[:12]}...: {len(acc)} usable" + (f"  (unique to it: {extra[:6]})" if extra else ""))
    print("# NEW (union, not yet wired) -> paste into BEDROCK_CONVERSE_MODELS:")
    for k in sorted(new):
        print(f'    "{k}": "{new[k]}",')
    return 0


if __name__ == "__main__":
    sys.exit(main())
