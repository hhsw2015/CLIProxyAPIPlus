#!/usr/bin/env python3
"""vertex_scan.py — dedicated new-model detector for the Vertex AI Claude channel.

Why ID-probing (not catalog listing): our service account only has predict rights
on the consumer projects; Vertex ListPublisherModels needs serviceusage on a
quota project (403), and the SA home project has the Agent Platform API disabled.
So we enumerate by probing candidate publisher-model IDs with a 1-token
:rawPredict and reading the verdict:

  200  usable            -> NEW if not already in our generated config
  429  usable (throttled) -> treated as usable (auth works)
  403  data-sharing gate  -> exists on Vertex but the project must opt into
                             publisher data-sharing (e.g. claude-fable-5-1)
  404  not on Vertex yet  -> Anthropic hasn't published this id to the publisher
  498  project BANNED     -> Anthropic usage-policy block on that project

Candidates = models already in config (baseline) + an auto forward-window per
family + a curated UPCOMING list. Exit codes drive the cpa dispatcher's AGENT-HINT:
  10 = new-usable and/or newly-unblocked(403) models found (agent should wire)
  0  = scan clean, nothing new
  1  = setup error (no creds / no token)

Usage: python3 vertex_scan.py [--gemini] [--regions] [--json]
"""
import argparse, base64, json, re, sys, urllib.request, ssl
from pathlib import Path
from concurrent.futures import ThreadPoolExecutor

REPO = Path(__file__).resolve().parent.parent
CFG = REPO / "scripts" / "generated_v2" / "cpa-new-config.yaml"
sys.path.insert(0, str(REPO / "scripts"))

# Curated frontier ids to always probe even if config/forward-window miss them.
# Keep in sync with docs/upcoming-models-watchlist. Cheap (1 probe each).
UPCOMING_CLAUDE = [
    "claude-opus-5", "claude-opus-5-1", "claude-opus-5-2", "claude-opus-5-5",
    "claude-opus-6",
    "claude-sonnet-5-1", "claude-sonnet-5-2", "claude-sonnet-5-5", "claude-sonnet-6",
    "claude-haiku-4-6", "claude-haiku-5", "claude-haiku-5-1",
    "claude-fable-5-1", "claude-fable-6",
]
# Gemini (google publisher, native generateContent). Empirically 2.5-pro/flash +
# 3.8-flash serve on our SA with NO enablement; 3.x pro/flash ids drift, so probe
# a wide window and report the 200s.
UPCOMING_GEMINI = [
    "gemini-2.5-pro", "gemini-2.5-flash", "gemini-2.5-flash-lite",
    "gemini-3-pro", "gemini-3-flash", "gemini-3.1-pro", "gemini-3.1-flash",
    "gemini-3.5-flash", "gemini-3.5-flash-lite", "gemini-3.6-flash",
    "gemini-3.7-flash", "gemini-3.8-flash", "gemini-3.8-flash-lite",
    "gemini-3.8-live", "gemini-3.5-transcribe", "gemini-flash-latest",
    "gemini-pro-latest", "gemini-embedding-2",
    # Pro/preview tier — the -preview suffix is required (bare 3.x pro 404s).
    # Empirically gemini-3.1-pro-preview(+customtools) and gemini-3-flash-preview serve.
    "gemini-3.1-pro-preview", "gemini-3.1-pro-preview-customtools", "gemini-3-flash-preview",
    "gemini-3.5-pro-preview", "gemini-3.8-pro-preview", "gemini-4-pro-preview",
    # Image-generation models (nano-banana family, via generateContent → image parts).
    # Reachable now; Imagen is retired so these are the image path.
    "gemini-3-pro-image", "gemini-2.5-flash-image", "gemini-3.1-flash-image",
]
# MaaS open/partner models (OpenAI-compat endpoints/openapi/chat/completions).
# Empirically these serve on our SA with NO per-project enablement (Llama is the
# exception: needs license acceptance -> 404 until enabled). Publisher prefix +
# -maas suffix are part of the model string.
MAAS_CANDIDATES = [
    "deepseek-ai/deepseek-v3.2-maas", "deepseek-ai/deepseek-v3.1-maas",
    "deepseek-ai/deepseek-r1-0528-maas", "deepseek-ai/deepseek-ocr-maas",
    "qwen/qwen3-next-80b-a3b-instruct-maas", "qwen/qwen3-next-80b-a3b-thinking-maas",
    "qwen/qwen3-coder-480b-a35b-instruct-maas", "qwen/qwen3-235b-a22b-instruct-2507-maas",
    "openai/gpt-oss-120b-maas", "openai/gpt-oss-20b-maas",
    "moonshotai/kimi-k2-thinking-maas",
    "minimax/minimax-m2-maas", "minimaxai/minimax-m2-maas",
    "zai-org/glm-4.7-maas", "zai-org/glm-5-maas", "zai-org/glm-5.2-maas",
    "meta/llama-4-maverick-17b-128e-instruct-maas", "meta/llama-4-scout-17b-16e-instruct-maas",
    "ai21/jamba-1.5-large", "ai21/jamba-1.5-mini",
    # xAI Grok — same OpenAI endpoint, xai/ publisher (NO -maas suffix). Empirically
    # grok-4.20-* and grok-4.1-fast-* serve on our SA; keep newer guesses for drift.
    "xai/grok-4.20-reasoning", "xai/grok-4.20-non-reasoning",
    "xai/grok-4.1-fast-reasoning", "xai/grok-4.1-fast-non-reasoning",
    "xai/grok-4.6", "xai/grok-4.7", "xai/grok-4.7-build-fast", "xai/grok-code-fast-1",
    # Muse Spark (Meta partner, reasoning/1M) — probe (404 until enabled on our projects).
    "meta/muse-spark-1-3",
]
REGIONS_OLD = ["global", "us-east5", "europe-west1", "asia-southeast1"]


def extract_creds_b64():
    """Pull the inline SA creds from the first vertex-sa entry of the config."""
    if not CFG.exists():
        return ""
    lines = CFG.read_text().splitlines()
    try:
        start = next(i for i, l in enumerate(lines) if "api-key: vertex-sa" in l)
        ci = next(i for i in range(start, len(lines)) if "credentials-b64:" in lines[i])
    except StopIteration:
        return ""
    buf = []
    for l in lines[ci + 1:]:
        s = l.strip()
        if l.startswith("    ") and s and ":" not in s and not s.startswith("- "):
            buf.append(s)
        else:
            break
    return "".join(buf)


def harvest_aistudio_gemini_ids():
    """Enumerate the AUTHORITATIVE Gemini catalog via generativelanguage
    models.list (AI-Studio surface, bypasses our SA's 403'd Vertex catalog API).

    This closes the ID-guess blind spot: the static UPCOMING_GEMINI list can only
    probe IDs we already know, so a Gemini model Google ships under a name we never
    guessed is invisible. models.list returns every current Gemini/Imagen/Veo/Lyria
    id; we probe those on Vertex too. Best-effort: returns [] if no AIza key found.
    """
    import re as _re
    import subprocess
    if not CFG.exists():
        return []
    # Config holds many AIza keys; some are stale/invalid. Try each distinct key
    # until one enumerates (first-match alone is unreliable).
    keys = list(dict.fromkeys(_re.findall(r"AIza[0-9A-Za-z_-]{30,}", CFG.read_text())))
    ids = []
    for key in keys:
        page = ""
        got = []
        for _ in range(10):
            url = ("https://generativelanguage.googleapis.com/v1beta/models"
                   f"?key={key}&pageSize=200" + (f"&pageToken={page}" if page else ""))
            try:
                r = subprocess.run(["curl", "-sS", "-m", "20", url], capture_output=True, text=True)
                j = json.loads(r.stdout)
            except Exception:
                break
            if "error" in j:
                break  # invalid/restricted key — try the next one
            got += [x["name"].split("/")[-1] for x in j.get("models", [])]
            page = j.get("nextPageToken", "")
            if not page:
                break
        if got:
            ids = got
            break
    # keep only families reachable via Vertex generateContent (chat/image);
    # drop embedding/aqa/tts/transcribe/live (separate surfaces handled elsewhere).
    fam = [i for i in set(ids)
           if any(i.startswith(p) for p in ("gemini", "gemma", "imagen", "learnlm"))
           and not any(x in i for x in ("-embedding", "aqa", "-tts", "transcribe", "-live", "native-audio"))]
    return sorted(fam)


def config_state():
    """Return (served_models:set, projects:set) parsed from the generated config."""
    served, projects = set(), set()
    if CFG.exists():
        txt = CFG.read_text()
        for m in re.findall(r"api-key: vertex-sa-[a-z0-9-]+?-((?:claude|gemini)[a-z0-9.-]+)", txt):
            served.add(m)
        projects |= set(re.findall(r"\bcla-[0-9a-zA-Z-]+", txt))
    # Always include the standard consumer pool so banned ones get reported too.
    projects |= {f"cla-{i:02d}" for i in range(1, 11)}
    return served, projects


def forward_window(served):
    """Generate plausible next Claude ids from the families already served."""
    fam = {}  # family -> list of (major, minor|None)
    for m in served:
        g = re.match(r"claude-([a-z]+)-(\d+)(?:-(\d+))?$", m)
        if not g:
            continue
        f, a, b = g.group(1), int(g.group(2)), (int(g.group(3)) if g.group(3) else None)
        fam.setdefault(f, []).append((a, b))
    out = set()
    for f, vers in fam.items():
        max_a = max(a for a, _ in vers)
        # next minors within the top major
        top_minors = [b for a, b in vers if a == max_a and b is not None]
        if top_minors:
            mb = max(top_minors)
            for d in (1, 2, 3):
                out.add(f"claude-{f}-{max_a}-{mb + d}")
        # next major, bare + a few minors
        out.add(f"claude-{f}-{max_a + 1}")
        for b in (1, 2, 5):
            out.add(f"claude-{f}-{max_a + 1}-{b}")
    return out


def probe_claude(tok, proj, model, region):
    host = "aiplatform.googleapis.com" if region == "global" else f"{region}-aiplatform.googleapis.com"
    url = f"https://{host}/v1/projects/{proj}/locations/{region}/publishers/anthropic/models/{model}:rawPredict"
    body = json.dumps({"anthropic_version": "vertex-2023-10-16", "max_tokens": 1,
                       "messages": [{"role": "user", "content": "hi"}]}).encode()
    return _post(url, tok, body)


def probe_gemini(tok, proj, model, region):
    host = "aiplatform.googleapis.com" if region == "global" else f"{region}-aiplatform.googleapis.com"
    url = f"https://{host}/v1/projects/{proj}/locations/{region}/publishers/google/models/{model}:generateContent"
    body = json.dumps({"contents": [{"role": "user", "parts": [{"text": "hi"}]}],
                       "generationConfig": {"maxOutputTokens": 1}}).encode()
    return _post(url, tok, body)


def probe_maas(tok, proj, model, region):
    host = "aiplatform.googleapis.com" if region == "global" else f"{region}-aiplatform.googleapis.com"
    url = f"https://{host}/v1/projects/{proj}/locations/{region}/endpoints/openapi/chat/completions"
    body = json.dumps({"model": model, "messages": [{"role": "user", "content": "hi"}],
                       "max_tokens": 1}).encode()
    return _post(url, tok, body)


def _post(url, tok, body):
    req = urllib.request.Request(url, data=body, headers={
        "Authorization": f"Bearer {tok}", "Content-Type": "application/json"})
    try:
        r = urllib.request.urlopen(req, timeout=12, context=ssl.create_default_context())
        return r.status
    except urllib.error.HTTPError as e:
        return e.code
    except Exception:
        return 0


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--gemini", action="store_true", help="also probe Gemini (google publisher) ids")
    ap.add_argument("--maas", action="store_true", help="also probe MaaS open/partner models (OpenAI endpoint)")
    ap.add_argument("--garden", action="store_true", help="probe the whole model garden (Claude + Gemini + MaaS)")
    ap.add_argument("--regions", action="store_true", help="probe old-model regions too, not just global")
    ap.add_argument("--json", action="store_true", help="emit machine-readable JSON summary")
    ap.add_argument("--creds-b64", default="", help="override SA creds (base64 JSON)")
    a = ap.parse_args()

    creds = a.creds_b64 or extract_creds_b64()
    if not creds:
        print("ERROR: no SA creds (config missing? pass --creds-b64)"); return 1
    try:
        sa = json.loads(base64.b64decode(creds))
    except Exception as e:
        print(f"ERROR: creds decode failed: {e}"); return 1
    try:
        from vertex_probe_common import mint_gcp_token
        tok = mint_gcp_token(creds, timeout=15)
    except Exception as e:
        print(f"ERROR: token mint failed: {e}"); return 1
    if not tok:
        print("ERROR: empty token"); return 1

    served, projects = config_state()
    projects = sorted(projects)
    print(f"SA: {sa.get('client_email')}  |  projects in scope: {len(projects)}")

    # 1) Classify projects: probe a known-good model on global across all projects.
    canary = next((m for m in sorted(served) if m.startswith("claude-opus")), "claude-haiku-4-5")
    healthy, banned, other_bad = [], [], []
    with ThreadPoolExecutor(max_workers=max(len(projects), 4)) as pool:
        st = dict(zip(projects, pool.map(lambda p: probe_claude(tok, p, canary, "global"), projects)))
    for p in projects:
        s = st[p]
        if s in (200, 429):
            healthy.append(p)
        elif s == 498:
            banned.append(p)
        else:
            other_bad.append((p, s))
    print(f"canary={canary}  healthy={healthy}  banned(498)={banned}" + (f"  other={other_bad}" if other_bad else ""))
    if not healthy:
        print("ERROR: no healthy project (all banned/failed) — channel is down");
        if a.json: print(json.dumps({"healthy": [], "banned": banned}))
        return 1

    # 2) Candidate ids.
    cand = set(served) | forward_window(served) | set(UPCOMING_CLAUDE)
    cand = sorted(c for c in cand if c.startswith("claude-"))
    regions = REGIONS_OLD if a.regions else ["global"]

    # 3) Probe candidates on healthy projects. A model's verdict = best status
    #    seen across (healthy project x region).
    tasks = [(c, p, r) for c in cand for p in healthy for r in regions]
    with ThreadPoolExecutor(max_workers=32) as pool:
        res = list(pool.map(lambda t: (t[0], probe_claude(tok, t[1], t[0], t[2])), tasks))
    best = {}
    RANK = {200: 5, 429: 4, 403: 3, 498: 2, 404: 1, 0: 0}
    for model, s in res:
        if RANK.get(s, 0) >= RANK.get(best.get(model, 0), 0):
            best[model] = s

    def bucket(models):  # sort helper
        return sorted(models)

    new_usable = bucket([m for m, s in best.items() if s in (200, 429) and m not in served])
    known_ok   = bucket([m for m, s in best.items() if s in (200, 429) and m in served])
    blocked    = bucket([m for m, s in best.items() if s == 403])          # data-sharing gate
    not_yet    = bucket([m for m, s in best.items() if s == 404])
    # 403 on a model we don't serve = newly-discoverable-if-unblocked (actionable)
    blocked_new = [m for m in blocked if m not in served]

    # ---- non-Claude garden surfaces (Gemini native + MaaS OpenAI-compat) ----
    def scan_surface(cands, probe_fn):
        tasks = [(c, p) for c in cands for p in healthy]
        with ThreadPoolExecutor(max_workers=32) as pool:
            r = list(pool.map(lambda t: (t[0], probe_fn(tok, t[1], t[0], "global")), tasks))
        b = {}
        for model, s in r:
            if RANK.get(s, 0) >= RANK.get(b.get(model, 0), 0):
                b[model] = s
        return b

    gem_usable = maas_usable = None
    if a.gemini or a.garden:
        harvested = harvest_aistudio_gemini_ids()  # authoritative live catalog (closes ID-guess blind spot)
        if harvested:
            print(f"[harvest] AI-Studio models.list → {len(harvested)} gemini-family ids (probing on Vertex)")
        gb = scan_surface(sorted(set(UPCOMING_GEMINI) | set(harvested)), probe_gemini)
        gem_usable = bucket([m for m, s in gb.items() if s in (200, 429)])
    if a.maas or a.garden:
        mb = scan_surface(sorted(set(MAAS_CANDIDATES)), probe_maas)
        maas_usable = bucket([m for m, s in mb.items() if s in (200, 429)])

    # ---- report ----
    print("\n===== VERTEX MODEL SCAN =====")
    print(f"served now ({len(known_ok)}): {known_ok}")
    print(f"NEW usable ({len(new_usable)}): {new_usable or '-'}")
    print(f"blocked-403 data-sharing ({len(blocked)}): {blocked or '-'}"
          + (f"   [NEW if unblocked: {blocked_new}]" if blocked_new else ""))
    print(f"not-on-vertex-404 ({len(not_yet)}): {not_yet or '-'}")
    if banned:
        print(f"BANNED projects (498): {banned}  <- lost quota; appeal/replace")
    if gem_usable is not None:
        print(f"GEMINI usable ({len(gem_usable)}): {gem_usable or '-'}   [not served via vertex today]")
    if maas_usable is not None:
        print(f"MaaS usable ({len(maas_usable)}): {maas_usable or '-'}   [not served via vertex today]")

    if a.json:
        print(json.dumps({
            "healthy": healthy, "banned": banned,
            "new_usable": new_usable, "known_ok": known_ok,
            "blocked_403": blocked, "blocked_new": blocked_new,
            "not_on_vertex_404": not_yet,
            "gemini_usable": gem_usable, "maas_usable": maas_usable,
        }, indent=2))

    # Exit 10 if there's something for the agent to act on: a new/unblocked Claude
    # model, or (when a garden surface was scanned) reachable Gemini/MaaS models we
    # don't yet serve via Vertex.
    if new_usable or blocked_new or gem_usable or maas_usable:
        return 10
    return 0


if __name__ == "__main__":
    sys.exit(main())
