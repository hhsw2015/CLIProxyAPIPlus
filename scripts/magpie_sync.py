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
        return "local"
    for line in out.splitlines():
        if "group/auto-" in line or "cpa-local/" not in line or "cpa-cloud/" not in line:
            continue
        if "● cpa-local" in line:
            return "local"
        if "● cpa-cloud" in line:
            return "cloud"
    return "local"  # no groups / undetectable -> default to the reliable up backend (not cloud)


def fetch_ids(base):
    req = urllib.request.Request(base + "/v1/models",
                                 headers={"Authorization": "Bearer " + token(), "User-Agent": "curl/8"})
    return [m["id"] for m in json.load(urllib.request.urlopen(req, timeout=25)).get("data", []) if m.get("id")]


def current_exposed(pid):
    """magpie's currently-exposed BARE ids for a provider, from the gateway catalog. Lets us
    SKIP a `provider models` re-expose when nothing changed: that call makes magpie replace the
    whole set and re-index/re-validate every model (~95% CPU for several seconds even when the
    set is identical). Returns None on error -> caller re-exposes (safe default)."""
    try:
        pre = pid + "/"
        return {i[len(pre):] for i in fetch_ids(GW) if i.startswith(pre)}
    except Exception:
        return None


def reachable(base, timeout=6):
    """Fast up/down check before we make magpie probe a backend. A down cloud (headroom
    530 / connection hang) otherwise stalls `provider set ... models.url=<dead>` for up to
    the 180s magpie CLI timeout, hanging the whole sync. False on ANY error (530/timeout/refused)."""
    try:
        req = urllib.request.Request(base + "/v1/models",
                                     headers={"Authorization": "Bearer " + token(), "User-Agent": "curl/8"})
        urllib.request.urlopen(req, timeout=timeout).read(1)
        return True
    except Exception:
        return False


def ensure_provider(pid, name, base):
    kv = [f"url={base}/v1", f"anthropic={base}", f"responses={base}/v1",
          f"key={token()}", f"models.url={base}/v1/models"]
    if pid in run("providers"):
        run("provider", "set", pid, *kv); print(f"  provider {pid}: updated")
    else:
        run("provider", "add", name, *kv, f"id={pid}"); print(f"  provider {pid}: added")


# ---- per-agent MINIMAL wirers (edit the agent's own config; magpie never manages it) ----

def wire_codex(agent, model):
    """Codex TOML: write magpie's OWN structure (captured verbatim from magpie wiring a clean
    config) so magpie recognizes Codex like the JSON agents. magpie's auto-wire botches heavy
    configs (it only writes when `model` already differs), so we apply the structure ourselves,
    preserving everything else. Backs up first. Idempotent. Returns True on success.

    magpie's canonical Codex wiring = these top-level keys + a [model_providers.magpie] block:
        model              = "<bare id>"   (bare, e.g. gpt-6-astra — NOT group/<id>; no groups)
        model_provider     = "magpie"
        model_catalog_json = "~/.codex/magpie-models.json"
        openai_base_url    = "<GW>/backend-api/codex"
        [model_providers.magpie]  name/base_url(<GW>/v1)/wire_api=responses/bearer="magpie"
    """
    p = Path("~/.codex/config.toml").expanduser()
    if not p.exists():
        print("  codex: config.toml not found; skip"); return False
    s = p.read_text()
    if not DRY:
        shutil.copy2(p, p.with_suffix(".toml.magpie.bak"))
    cat = str(Path("~/.codex/magpie-models.json").expanduser())
    obu = f"{GW}/backend-api/codex"
    head = s.split("\n[", 1)[0]
    nh = head
    if re.search(r'^model\s*=\s*"', nh, re.M):
        nh = re.sub(r'^model\s*=\s*"[^"]*"', f'model = "{model}"', nh, count=1, flags=re.M)
    else:
        nh = f'model = "{model}"\n' + nh

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
    print(f"  codex -> {model} (bare id; magpie structure: model_provider=magpie + openai_base_url)")
    return True


def wire_managed(agent, model):  # model unused; signature kept uniform with wire_codex for WIRERS
    """JSON agents (OpenCode/Pi/Command Code): their magpie wiring — base_url -> :3425 with BARE
    model ids — is produced by gen_agent_configs.py, the single source. Delegate to it. The old
    `magpie <agent> group/<id>` path is gone: main() no longer creates groups (bare-id design +
    `group auto off`), so it pointed at a non-existent group AND let magpie *manage* (and revert)
    the config. Codex keeps its dedicated wirer (wire_codex). Returns True on success so --wire
    can propagate a generator write failure to its exit status."""
    gen = Path(__file__).resolve().parent / "gen_agent_configs.py"
    if not gen.exists():
        print(f"  {agent}: gen_agent_configs.py not found; skip"); return False
    if DRY:
        print(f"  DRY  python3 gen_agent_configs.py --only {agent}"); return True
    r = subprocess.run([sys.executable, str(gen), "--only", agent], capture_output=True, text=True, timeout=120)
    tail = ((r.stdout or "") + (r.stderr or "")).strip().splitlines()
    print(f"  {agent} -> magpie :3425 via gen_agent_configs.py (bare ids)"
          + (f"; {tail[-1]}" if tail else ""))
    if r.returncode != 0:
        print(f"    WARN gen_agent_configs.py {agent} failed (rc={r.returncode})")
        return False
    return True


WIRERS = {"codex": wire_codex, "opencode": wire_managed, "pi": wire_managed,
          "commandcode": wire_managed, "claude": wire_managed}


def main():
    print(f"magpie_sync{' (dry-run)' if DRY else ''}")
    # Capture the live backend mode now so a re-sync preserves it (re-asserted at the end via
    # magpie_mode so `cpa agent-sync` never silently changes which backend is active).
    mode = detect_mode()
    # Never re-assert 'cloud' when cloud is unreachable: magpie_mode.sh cloud runs
    # `provider on cpa-cloud`, and magpie then SPINS (~155% CPU) trying to activate/probe the
    # dead backend. Fall back to local (the up backend). (2026-10-15 CPU incident)
    if mode == "cloud" and not reachable(CLOUD):
        print("  mode was 'cloud' but cloud UNREACHABLE -> forcing 'local' (avoids magpie spinning on dead cloud)")
        mode = "local"
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
        if not reachable(base):
            # A down backend (cloud headroom 530 / unreachable) stalls the magpie
            # `provider set ... models.url=<dead>` probe for up to the 180s CLI timeout,
            # hanging the whole sync. Skip it: leave its provider as-is (not re-probed);
            # re-run when it recovers. Local mode is unaffected (cpa-local still syncs).
            print(f"  provider {pid}: UNREACHABLE ({base}) — skipped, not re-probed (re-run when it recovers)")
            continue
        ensure_provider(pid, name, base)
        run("provider", "on", pid)  # both ON: switch uses group pick, never off/on
        # Expose each backend's FULL chat catalog on BOTH providers so the picker lists every
        # model in local AND cloud. Safe now that `group auto off` (set above) stops overlapping
        # catalogs from spawning auto-<model> groups (the old ~900% CPU bomb). Fall back to the
        # anchors if a backend is unreachable so the explicit group members stay ready.
        # Drop mirage/ -prefixed ids: the mirage P1 free pool advertises duplicate
        # `mirage/claude-*` chat ids (forced-mirage variants of real models). Exposing
        # them clutters the magpie picker and makes `--model .../claude-fable-5-1` fuzzy-
        # match the mirage variant (recurring modelPicker-mirage reinjection). The clean
        # ids stay exposed; mirage still serves as CPA's internal P1 fallback via fill-
        # first — it just isn't a pickable magpie id. (2026-10-15)
        full = [i for i in fetch_ids(base) if ischat(i) and not i.startswith("mirage/")] or anchors
        # Idempotence guard: a `provider models` call recomputes magpie's whole model index
        # (~95% CPU for seconds) EVEN when the set is unchanged. Most re-syncs don't change the
        # catalog, so skip the re-expose when the exposed set already matches.
        if current_exposed(pid) == set(full):
            print(f"  provider {pid}: {len(full)} models already exposed — skip re-expose (no CPU recompute)")
        else:
            run("provider", "models", pid, *full)
            print(f"  provider {pid}: ensured + ON + {len(full)} models exposed (mirage/ filtered)")

    # NO groups: all agents use BARE model ids now. magpie_mode.sh makes a bare id follow the
    # switch by exposing the catalog ONE-SIDED (active full / standby a single throwaway), so each
    # bare id has exactly one exposer = the active backend. (Groups broke Codex: `group/<id>` isn't
    # recognized by Codex -> it compacted/called via chat, which 400s on responses-only gpt-6-astra.)
    print("no groups created (agents use bare ids; one-sided expose makes them follow the switch)")

    wire_failed = []
    if WIRE:
        targets = [a for a in (WIRE_ONLY or AGENT_MODELS) if a in AGENT_MODELS]
        print(f"wiring agents (minimal edit, unmanaged by magpie): {', '.join(targets) or '(none)'}")
        for agent in targets:
            model = AGENT_MODELS[agent]
            if agent in WIRERS:
                if not WIRERS[agent](agent, model):
                    wire_failed.append(agent)
            else:
                print(f"  {agent}: no automated wirer yet — point its base_url at {GW} + model={model} manually")
                wire_failed.append(agent)
    else:
        print("(mirror + groups done; run with --wire [agent...] to point agents at magpie)")

    # Re-assert the backend mode via magpie_mode (both providers already carry the full catalog
    # from the loop above, so this just re-points / group-picks the live mode; fast).
    if not DRY:
        mm = Path(__file__).resolve().parent / "magpie_mode.sh"
        if mm.exists():
            print(f"re-asserting backend mode: {mode}")
            subprocess.run(["bash", str(mm), mode], timeout=180)
        else:
            print(f"  magpie_mode.sh not found; run it manually to set mode={mode} (picker + pick)")

    if wire_failed:
        print(f"wire FAILED for: {', '.join(wire_failed)} (configs for these were NOT updated)")
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main() or 0)
