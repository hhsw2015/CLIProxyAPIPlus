#!/usr/bin/env python3
"""agent_reconcile — make relay-models.json self-maintaining against CPA + models5.

Run by `cpa agent-sync` BEFORE regen. Three automated steps:

1. ENRICH — every relay model's context + efforts(thinking levels) are pulled from the
   models5 catalog (authoritative), so they're never hand-set/guessed/stale.

2. AUTO-ADD (CAND) — when CPA serves a HIGHER-version of the SAME VARIANT already in relay
   (relay has claude-opus-5 -> CPA serves claude-opus-5-5; relay gemini-3.1-pro-preview ->
   gemini-3.5-pro-preview), AND models5 confirms a real chat frontier (in catalog, text
   output, tool_call or reasoning), it's added automatically (wire derived from id, specs
   from models5). Matching is by VARIANT-KEY (id minus version digits) so a *pro* family
   never pulls in *flash*/*lite*/*content-safety* siblings. Brand-new variants/families
   stay advisory (a bigger curation call), never auto-adopted.

3. MISS — relay models NOT served by CPA. "Served" = the UNION of the generated config on
   disk + the live remote + the live local endpoint, so a flaky remote or a stale local
   never false-negatives a model that any authoritative source has. Reported only
   (pending vs genuinely-dropped) — NEVER auto-deleted (drop is a human decision).

Writes relay in place. Prints machine-readable lines the caller renders:
  ENRICH:n · ADDED:a,b · SKIP:… · MISS:… · SERVED:<n from k sources>
"""
import argparse, json, re, sys, urllib.request
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
from enrich_relay import load_catalog, bare, efforts_for  # noqa: E402

REPO = Path(__file__).resolve().parent
GEN_CONFIG = REPO / "generated_v2/cpa-new-config.yaml"

# Never auto-add these (niche / non-chat) even if version/variant math says upgrade.
NICHE = ("content-safety", "guard", "-lightning", "-image", "-edit", "embed", "rerank",
         "transcribe", "-tts", "-asr", "-realtime", "-audio", "-ocr", "-vision", "-moderation")


def catalog_index(cat):
    models = cat.get("models")
    models = models if isinstance(models, list) else list(models.values())
    idx = {}
    for m in models:
        b = bare(m.get("id", ""))
        if not b:
            continue
        lim = m.get("limit")
        cw = (lim.get("context") if isinstance(lim, dict) else lim)
        out = (m.get("modalities") or {}).get("output") or []
        idx[b] = {
            "context": cw,
            "reasoning": bool(m.get("reasoning")),
            "tool_call": bool(m.get("tool_call")),
            "text_out": ("text" in out) if out else True,
            "media": bool(any(x in out for x in ("image", "audio", "video"))),
        }
    return idx


def variant_key(x):
    """id minus version tokens -> groups by VARIANT (sol/astra/pro-preview/flash/ultra).
    claude-opus-5 & claude-opus-5-5 -> 'claude-opus'; gemini-3.1-pro-preview -> 'gemini-pro-preview'
    (distinct from gemini-3.5-flash -> 'gemini-flash')."""
    toks = [t for t in re.split(r"[-.]", bare(x)) if t and not re.match(r"^v?\d+[a-z]*$", t)]
    return "-".join(toks)


def ver(x):
    out = []
    b = bare(x)
    # version = leading contiguous numeric tokens; stop at size(14b/128e) / date/size(>=100)
    for tok in re.findall(r"\d+[a-z]*", b):
        if tok[-1] in "be":
            break
        n = int(re.match(r"\d+", tok).group())
        if n >= 100:
            break
        out.append(n)
        if len(out) >= 3:
            break
    return tuple(out)


def derive_wire(mid):
    b = bare(mid)
    if b.startswith("claude"):
        return "anthropic"
    if re.match(r"(gpt|o)\d", b) and any(k in b for k in ("sol", "astra", "luna", "terra", "codex", "-pro", "-max")):
        return "responses"
    return "chat"


def served_union(local, remote, key):
    """Union of what CPA serves per every authoritative source: the generated config on
    disk (source of truth for the next deploy) + live remote + live local. Robust: a
    flaky remote or a stale local can't false-negative a model another source has."""
    served, srcs = set(), []
    try:
        import yaml
        c = yaml.safe_load(GEN_CONFIG.read_text(encoding="utf-8"))
        n = len(served)
        for sec in ("openai-compatibility", "claude-api-key", "gemini-api-key", "vertex-api-key"):
            for e in (c.get(sec) or []):
                for m in (e.get("models") or []):
                    nm = m.get("name") if isinstance(m, dict) else m
                    if nm:
                        served.add(nm)
        if len(served) > n:
            srcs.append("gen")
    except Exception:  # noqa: BLE001
        pass
    for base, tag in ((remote, "remote"), (local, "local")):
        try:
            # Cloudflare (in front of the remote tunnel) blocks urllib's default UA -> the
            # remote fetch silently failed and a remote-only model false-MISSed. Send a
            # curl-like UA so the live remote is actually reached.
            req = urllib.request.Request(base + "/v1/models",
                                         headers={"Authorization": "Bearer " + key, "User-Agent": "curl/8.4"})
            got = {m["id"] for m in json.load(urllib.request.urlopen(req, timeout=10)).get("data", [])}
            if got:
                served |= got; srcs.append(tag)
        except Exception:  # noqa: BLE001
            pass
    return served, srcs


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--relay", required=True)
    ap.add_argument("--local", required=True)
    ap.add_argument("--remote", required=True)
    ap.add_argument("--key", required=True)
    ap.add_argument("--no-fetch", action="store_true")
    a = ap.parse_args()
    relay = Path(a.relay)
    if not relay.exists():
        print(f"agent_reconcile: relay not found {relay}", file=sys.stderr)
        return 0
    try:
        idx = catalog_index(load_catalog(a.no_fetch))
    except Exception as e:  # noqa: BLE001
        print(f"agent_reconcile: models5 unavailable ({e}); skip", file=sys.stderr)
        idx = {}
    d = json.loads(relay.read_text())
    models = d["models"] if isinstance(d, dict) else d

    # 1. ENRICH specs from models5
    enr = 0
    for m in models:
        if not isinstance(m, dict) or not m.get("id"):
            continue
        sp = idx.get(bare(m["id"]))
        if not sp:
            continue
        if sp["context"] and m.get("context") != sp["context"]:
            m["context"] = sp["context"]; enr += 1
        want = efforts_for(m.get("wire", "chat"), sp["reasoning"])
        if m.get("efforts") != want:
            m["efforts"] = want; enr += 1
    if enr:
        print(f"ENRICH:{enr}")

    served, srcs = served_union(a.local, a.remote, a.key)
    if not served:
        relay.write_text(json.dumps(d, ensure_ascii=False, indent=2) + "\n")
        print("SERVED:none")
        return 0
    print(f"SERVED:{len(served)} from {'+'.join(srcs)}")
    curated = [m["id"] for m in models if isinstance(m, dict) and m.get("id")]
    cset = set(curated)
    vmax = {}                              # variant_key -> highest curated version
    for i in curated:
        vk = variant_key(i); v = ver(i)
        if v and v > vmax.get(vk, ()):
            vmax[vk] = v

    # 2. AUTO-ADD CAND (models5-verified SAME-VARIANT upgrades)
    added, skipped = [], []
    for s in sorted(served):
        if s in cset:
            continue
        vk = variant_key(s); v = ver(s)
        if vk not in vmax or not v or v <= vmax[vk]:
            continue                       # not a higher version of an existing variant
        if any(k in bare(s) for k in NICHE):
            skipped.append(s + "(niche)"); continue
        sp = idx.get(bare(s))
        if not sp:
            skipped.append(s + "(not-in-m5)"); continue
        if sp["media"] or not sp["text_out"]:
            skipped.append(s + "(media)"); continue
        if not (sp["tool_call"] or sp["reasoning"]):
            skipped.append(s + "(non-frontier)"); continue
        wire = derive_wire(s)
        models.append({
            "id": s if "/" in s else bare(s),
            "wire": wire,
            "context": sp["context"] or 200000,
            "efforts": efforts_for(wire, sp["reasoning"]),
        })
        cset.add(s); added.append(s)
    if added:
        print("ADDED:" + ",".join(added))
    if skipped:
        print("SKIP:" + ",".join(skipped[:12]))

    # 3. MISS vs the union (report only; never auto-delete)
    miss = [i for i in curated if i not in served]
    if miss:
        print("MISS:" + ",".join(miss))

    relay.write_text(json.dumps(d, ensure_ascii=False, indent=2) + "\n")
    return 0


if __name__ == "__main__":
    sys.exit(main())
