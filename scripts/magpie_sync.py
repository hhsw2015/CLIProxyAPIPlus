#!/usr/bin/env python3
"""magpie_sync — mirror CPA into magpie: ONE config, all chat models pickable, and a
transparent local/cloud backend switch for the models agents use.

Single source of truth = CPA's own config (gen_llm_config_v2.py). magpie's provider
`models.url` auto-mirrors CPA's /v1/models (id + context + reasoning levels) — nothing to
hand-maintain. This script:
  1. ensures providers cpa-local / cpa-cloud / bonsai-local (add-or-set) and keeps both CPA
     providers ON (the switch uses `group pick`, NOT provider off/on — off/on degrades groups
     and makes magpie revert managed agent configs);
  2. turns OFF commandcode-plan (auto-detected, NOT subscribed);
  3. EXPOSES the chat-usable CPA models on both (so ~1300 show as choices); media filtered by id;
  4. creates EXPLICIT routing groups  group/cpa-<slug> = [cpa-local/<model>, cpa-cloud/<model>]
     for every agent model (explicit groups persist across the switch; magpie's auto-<model>
     groups dissolve when a provider goes off);
  5. with --wire: points each agent at magpie by a MINIMAL, targeted edit of its OWN config
     (base_url -> 127.0.0.1:3425, model -> group/cpa-<slug>). We do NOT use `magpie <agent>
     <model>` — that makes magpie *manage* (and rewrite/revert) the config, and it mis-edits
     heavily-customized files (it set Codex's model but not its provider). Agents stay
     unmanaged by magpie; only their base_url+model change, everything else is preserved.

  python3 magpie_sync.py                 # mirror: providers + expose + groups (idempotent)
  python3 magpie_sync.py --dry-run
  python3 magpie_sync.py --wire codex    # also wire these agent(s); omit list to wire all wirers

Switch backend with scripts/magpie_mode.sh local|cloud (group pick, both providers on).
"""
import json, re, shutil, subprocess, sys, urllib.request
from pathlib import Path

M = "/Applications/magpie.app/Contents/MacOS/magpie"
TOKEN_CMD = str(Path("~/.commandcode/relay-token.sh").expanduser())
GW = "http://127.0.0.1:3425"
LOCAL = "http://127.0.0.1:8787"
CLOUD = "https://headroom.geeker.indevs.in"
BONSAI = "http://127.0.0.1:8091"
OFF_PROVIDERS = ("commandcode-plan",)

argv = sys.argv[1:]
DRY = "--dry-run" in argv
WIRE = "--wire" in argv
WIRE_ONLY = [a for a in (argv[argv.index("--wire") + 1:] if WIRE else []) if not a.startswith("-")]

# agent -> the CPA model it picks (served via group/cpa-<slug> so local/cloud is transparent).
# Only agents that CACHE config at startup belong here — magpie fixes their switch-breaks.
# Claude Code is deliberately EXCLUDED: it re-reads settings.json per request (so it never had
# the caching problem; the legacy _switch.sh base_url rewrite switches it instantly), AND users
# launch it with an explicit `--model` (e.g. claude-opus-4-8[1m]) that overrides settings.json,
# so a magpie group would be bypassed anyway. Keep Claude Code on headroom + _switch.sh.
AGENT_MODELS = {
    "codex": "gpt-6-astra",
    "opencode": "claude-opus-4-8",
    "pi": "claude-opus-4-8",
    "commandcode": "claude-opus-4-8",
}

# Claude Code's opus/sonnet/haiku/fable tier aliases (~/.claude/settings.json env
# ANTHROPIC_DEFAULT_*_MODEL) all default to claude-fable-5; give it a switchable
# [local,cloud] group too so those tiers follow magpie_mode like the agent models.
# Claude Code itself stays UNMANAGED by magpie (direct settings.json edit: base_url=:3425,
# token=magpie, aliases=group/claude-fable-5); we only keep its group alive here.
EXTRA_GROUP_MODELS = (
    "claude-fable-5",          # Claude Code tier-alias default; the WORKING fable. (claude-fable-5-1
                               # is advertised in /v1/models but CPA 400s "route not found /
                               # token_group=special" on both backends even with a CC UA -> not wired.)
    # Common flagships given a switchable [cpa-local, cpa-cloud] group so ANY agent (and Claude
    # Code via --model group/<id>) can pick them AND have them follow `cpa switch` local<->cloud.
    # (claude-opus-4-8 + gpt-6-astra already grouped via AGENT_MODELS.) Each is one overlap =>
    # one auto-group; ~16 total is far below the ~1096 that caused the CPU bomb. Verified present
    # as bare ids in the CPA catalog 2026-10-02. Add/remove freely — one line, re-run magpie_sync.
    "claude-opus-5-5", "claude-opus-4-6", "claude-sonnet-5",     # Claude
    "gpt-6-sol", "gpt-6.1-sol", "gpt-6-luna",                    # GPT
    "gemini-3.1-pro-preview",                                    # Gemini (gemini-3-pro-preview dropped: 400/timeout)
    "deepseek-v4-pro", "glm-5.3", "kimi-k3", "grok-4.7", "qwen3.8-max",  # open / other (grok-4.7=latest)
)

MEDIA = (
    "video", "image", "-t2v", "-i2v", "-i2i", "t2i", "i2t", "tts", "-asr", "-stt", "audio",
    "voice", "speech", "-ocr", "rerank", "embed", "moderation", "upscal", "inpaint", "outpaint",
    "reference-to-", "text-to-video", "text-to-image", "image-to", "sfx", "music", "-sing",
    "vocal", "seedance", "seedream", "wan-", "kling", "hailuo", "veo", "sora", "recraft", "flux",
    "midjourney", "dall", "imagen", "cogvideo", "lipsync", "foley", "-edit", "photo", "portrait",
    "avatar", "motion", "whisper", "transcrib", "realtime", "-cv-", "sticker", "-draw", "paint",
    "render", "-3d", "depth", "-pose", "adapter", "removal", "workflow", "-frame", "pixelcut",
    "nano-banana", "csm-", "matting", "segment", "caption", "controlnet", "lora", "restore",
    "colorize", "relight", "tryon", "try-on", "background", "extract", "-vae", "omni",
)


def ischat(i): return not any(k in i.lower() for k in MEDIA)
# group id = the model id itself (no cpa- prefix) so agents pick e.g. group/claude-opus-4-8.
# magpie_mode.sh identifies switchable groups by their cpa-local/cpa-cloud MEMBERS, not the name.
def slug(model): return re.sub(r"[^a-z0-9]+", "-", model.lower()).strip("-")


def run(*args, timeout=180):
    if DRY:
        s = " ".join(a if len(a) < 44 else a[:41] + "..." for a in args[:5])
        print(f"DRY  magpie {s}" + (f"  (+{len(args)-5})" if len(args) > 5 else "")); return ""
    r = subprocess.run([M, *args], capture_output=True, text=True, timeout=timeout)
    return (r.stdout or "") + (r.stderr or "")


def token(): return subprocess.run([TOKEN_CMD], capture_output=True, text=True).stdout.strip()


def detect_mode():
    """Live backend from the current group pick ('local'/'cloud') so a re-sync preserves it.
    Defaults to 'cloud'. Reads magpie directly (not run(), which is DRY-gated/truncated)."""
    if DRY:
        return "cloud"
    try:
        r = subprocess.run([M, "groups"], capture_output=True, text=True, timeout=30)
        out = (r.stdout or "") + (r.stderr or "")
    except Exception:  # noqa: BLE001
        return "cloud"
    for line in out.splitlines():
        if "group/auto-" in line or "cpa-local/" not in line or "cpa-cloud/" not in line:
            continue
        if "● cpa-local" in line:
            return "local"
        if "● cpa-cloud" in line:
            return "cloud"
    return "cloud"


def fetch_ids(base):
    req = urllib.request.Request(base + "/v1/models",
                                 headers={"Authorization": "Bearer " + token(), "User-Agent": "curl/8"})
    return [m["id"] for m in json.load(urllib.request.urlopen(req, timeout=25)).get("data", []) if m.get("id")]


def ensure_provider(pid, name, base):
    kv = [f"url={base}/v1", f"anthropic={base}", f"responses={base}/v1",
          f"key={token()}", f"models.url={base}/v1/models"]
    if pid in run("providers"):
        run("provider", "set", pid, *kv); print(f"  provider {pid}: updated")
    else:
        run("provider", "add", name, *kv, f"id={pid}"); print(f"  provider {pid}: added")


def ensure_group(model):
    gid = slug(model)
    # Always `group add` — a name already in use REPLACES that group (idempotent per magpie).
    # (Don't use `gid in run("groups")`: a bare-model-id gid like "claude-opus-4-8" is a
    # substring of auto-claude-opus-4-8 / cpa-local/claude-opus-4-8, so that check false-matches
    # and we'd `set` a group that doesn't exist.)
    run("group", "add", f"CPA {model}", f"models=cpa-local/{model},cpa-cloud/{model}",
        "routing=order", f"id={gid}")
    return gid


# ---- per-agent MINIMAL wirers (edit the agent's own config; magpie never manages it) ----

def wire_codex(agent, gid):
    """Codex TOML: write magpie's OWN structure (captured verbatim from magpie wiring a clean
    config) so magpie recognizes Codex like the JSON agents. magpie's auto-wire botches heavy
    configs (it only writes when `model` already differs), so we apply the structure ourselves,
    preserving everything else. Backs up first. Idempotent.

    magpie's canonical Codex wiring = these top-level keys + a [model_providers.magpie] block:
        model              = "group/<gid>"
        model_provider     = "magpie"
        model_catalog_json = "~/.codex/magpie-models.json"
        openai_base_url    = "<GW>/backend-api/codex"
        [model_providers.magpie]  name/base_url(<GW>/v1)/wire_api=responses/bearer="magpie"
    """
    p = Path("~/.codex/config.toml").expanduser()
    if not p.exists():
        print("  codex: config.toml not found; skip"); return
    s = p.read_text()
    if not DRY:
        shutil.copy2(p, p.with_suffix(".toml.magpie.bak"))
    cat = str(Path("~/.codex/magpie-models.json").expanduser())
    obu = f"{GW}/backend-api/codex"
    head = s.split("\n[", 1)[0]
    nh = head
    if re.search(r'^model\s*=\s*"', nh, re.M):
        nh = re.sub(r'^model\s*=\s*"[^"]*"', f'model = "group/{gid}"', nh, count=1, flags=re.M)
    else:
        nh = f'model = "group/{gid}"\n' + nh

    def setkey(h, k, v):
        if re.search(rf'^{re.escape(k)}\s*=', h, re.M):
            return re.sub(rf'^{re.escape(k)}\s*=.*$', f'{k} = "{v}"', h, count=1, flags=re.M)
        return h.rstrip("\n") + f'\n{k} = "{v}"\n'

    for k, v in (("model_provider", "magpie"), ("model_catalog_json", cat), ("openai_base_url", obu)):
        nh = setkey(nh, k, v)
    s = s.replace(head, nh, 1)
    if "[model_providers.magpie]" not in s:
        s = s.rstrip("\n") + ('\n\n[model_providers.magpie]\nname = "magpie"\n'
                              f'base_url = "{GW}/backend-api/codex"\nwire_api = "responses"\n'
                              f'chatgpt_base_url = "{GW}"\n'
                              'experimental_bearer_token = "magpie"\n'
                              # Wire Codex as a ChatGPT-BACKEND provider (base_url=/backend-api/codex
                              # + chatgpt_base_url), like the codex-lb provider. Then ALL Codex calls
                              # go through magpie's codex backend on the RESPONSES wire — including the
                              # auto-compact, which arrives as a native compaction_trigger that magpie
                              # relays via responses (codexCompact). The /v1 provider path instead made
                              # Codex send the compact as /v1/chat/completions, which 400s on
                              # responses-only models like gpt-6-astra (magpie's compact relay lives
                              # only on /backend-api/codex, never on /chat/completions).
                              'requires_openai_auth = true\n')
    if not DRY:
        p.write_text(s)
    print(f"  codex -> group/{gid} (magpie structure: model_provider=magpie + openai_base_url)")


def wire_managed(agent, gid):
    """Simple JSON agents (OpenCode/Pi/Command Code/Claude): magpie wires these correctly, so
    let it manage them. SAFE with the group-pick switch (magpie only reverts a managed config
    when a group DEGRADES, which `provider off/on` causes but `magpie_mode.sh` group-pick never
    does). Backs up the config first."""
    paths = {
        "opencode": "~/.config/opencode/opencode.json", "pi": "~/.pi/agent/settings.json",
        "commandcode": "~/.commandcode/settings.json", "claude": "~/.claude/settings.json",
    }
    f = Path(paths.get(agent, "")).expanduser()
    if f.exists() and not DRY:
        shutil.copy2(f, f.with_suffix(f.suffix + ".magpie.bak"))
    run(agent, f"group/{gid}")
    print(f"  {agent} -> group/{gid} (magpie-managed JSON)")


WIRERS = {"codex": wire_codex, "opencode": wire_managed, "pi": wire_managed,
          "commandcode": wire_managed, "claude": wire_managed}


def main():
    print(f"magpie_sync{' (dry-run)' if DRY else ''}")
    # Capture the live backend mode BEFORE ensure_group replaces the groups (a `group add` on an
    # existing name resets routing to `order` and drops the manual pick). Re-asserted at the end
    # via magpie_mode so `cpa agent-sync` never silently changes which backend is active.
    mode = detect_mode()
    # Auto-group creation OFF (magpie >=0.1.607 `group auto off`): magpie no longer invents an
    # auto-<model> group per model that 2+ providers serve. That was the CPU bomb at the root
    # (~1096 auto-groups recomputed at 60fps = ~900% CPU); with it off, overlapping catalogs on
    # both providers are safe. Explicit groups are unaffected. Idempotent. (Pre-0.1.607 had no such
    # flag, hence the one-sided-expose workaround in magpie_mode — now belt-and-suspenders.)
    run("group", "auto", "off")
    print("  group auto: OFF (no implicit auto-<model> groups)")
    for pid in OFF_PROVIDERS:
        if pid in run("providers"):
            run("provider", "off", pid); print(f"  provider {pid}: OFF (not subscribed)")
    if "bonsai-local" in run("providers"):
        run("provider", "set", "bonsai-local", f"url={BONSAI}/v1", "key=bonsai", "models=bonsai-2-27b")
    else:
        run("provider", "add", "Bonsai Local", f"url={BONSAI}/v1", "key=bonsai", "models=bonsai-2-27b", "id=bonsai-local")
    print("  provider bonsai-local: ok")

    # Ensure both CPA providers exist + are ON, then expose each backend's FULL chat catalog on
    # BOTH (in the loop below). With `group auto off` set above, overlapping catalogs no longer
    # spawn auto-<model> groups, so full-on-both is CPU-safe (was ~900% CPU / ~1100 auto-groups)
    # AND the picker lists every model in local AND cloud. Explicit groups supply the switchable
    # members; routing is route-any regardless of expose.
    anchors = list(dict.fromkeys(list(AGENT_MODELS.values()) + list(EXTRA_GROUP_MODELS)))
    for pid, name, base in (("cpa-cloud", "CPA Cloud", CLOUD), ("cpa-local", "CPA Local", LOCAL)):
        ensure_provider(pid, name, base)
        run("provider", "on", pid)  # both ON: switch uses group pick, never off/on
        # Expose each backend's FULL chat catalog on BOTH providers so the picker lists every
        # model in local AND cloud. Safe now that `group auto off` (set above) stops overlapping
        # catalogs from spawning auto-<model> groups (the old ~900% CPU bomb). Fall back to the
        # anchors if a backend is unreachable so the explicit group members stay ready.
        full = [i for i in fetch_ids(base) if ischat(i)] or anchors
        run("provider", "models", pid, *full)
        print(f"  provider {pid}: ensured + ON + {len(full)} models exposed (full catalog)")

    # NO groups: all agents use BARE model ids now. magpie_mode.sh makes a bare id follow the
    # switch by exposing the catalog ONE-SIDED (active full / standby a single throwaway), so each
    # bare id has exactly one exposer = the active backend. (Groups broke Codex: `group/<id>` isn't
    # recognized by Codex -> it compacted/called via chat, which 400s on responses-only gpt-6-astra.)
    print("no groups created (agents use bare ids; one-sided expose makes them follow the switch)")

    if WIRE:
        targets = [a for a in (WIRE_ONLY or AGENT_MODELS) if a in AGENT_MODELS]
        print(f"wiring agents (minimal edit, unmanaged by magpie): {', '.join(targets) or '(none)'}")
        for agent in targets:
            gid = slug(AGENT_MODELS[agent])
            if agent in WIRERS:
                WIRERS[agent](agent, gid)
            else:
                print(f"  {agent}: no automated wirer yet — point its base_url at {GW} + model=group/{gid} manually")
    else:
        print("(mirror + groups done; run with --wire [agent...] to point agents at magpie)")

    # Re-assert the backend mode: ensure_group above reset each group to routing=order and dropped
    # its manual pick, so re-point them to the live mode. magpie_mode now just group-picks (both
    # providers already carry the full catalog from the loop above), so this step is fast.
    if not DRY:
        mm = Path(__file__).resolve().parent / "magpie_mode.sh"
        if mm.exists():
            print(f"re-asserting backend mode: {mode}")
            subprocess.run(["bash", str(mm), mode], timeout=180)
        else:
            print(f"  magpie_mode.sh not found; run it manually to set mode={mode} (picker + pick)")


if __name__ == "__main__":
    main()
