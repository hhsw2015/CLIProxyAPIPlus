#!/usr/bin/env python3
"""Generate Command Code, Pi, and OpenCode configs from one canonical source.

Single source of truth: relay-models.json (this dir). Edit that (or re-run the
upstream probe to refresh it), then run this script to sync all three agents.

  python3 gen_agent_configs.py            # write all three
  python3 gen_agent_configs.py --dry-run  # print what would change, write nothing

Wire routing (a model is served by exactly one of these on the relay):
  anthropic  -> Anthropic Messages  /v1/messages    (Claude family)
  responses  -> OpenAI Responses    /v1/responses    (GPT-5.x / GPT-6 / codex)
  chat       -> OpenAI ChatCompletions /v1/chat/completions (everything else,
               including Claude-via-chat and Gemini, which the relay also serves)

Per-agent quirks the generator handles:
  - Command Code: one "cpa" provider, per-model `api`+`baseURL` override for the
    anthropic/responses wires, `reasoningEfforts` array, apiKey via `!command`.
  - Pi: three providers split by wire (Pi is provider-centric), literal apiKey,
    `reasoning:true` flag; local bonsai provider preserved.
  - OpenCode: AI-SDK based. Claude+Gemini+open all go through one
    `@ai-sdk/openai-compatible` provider (chat works for them); GPT responses
    models need `@ai-sdk/openai` (Responses API) as a second provider. Non-provider
    keys in an existing opencode.json (permission/plugin/etc.) are preserved.
"""
import json
import subprocess
import sys
from pathlib import Path

HERE = Path(__file__).resolve().parent
CANON = json.loads((HERE / "relay-models.json").read_text())
ENDPOINT = CANON["endpoint"]                       # relay-models.json curated source, NOT an agent endpoint
BASE_V1 = f"{ENDPOINT}/v1"
# Agents reach CPA through the fixed LOCAL magpie gateway (:3425), which flips local/cloud
# behind itself (magpie_mode.sh / cpa switch) so cached-config agents never break on a switch.
# The JSON clients (and omp) point here, with BARE model ids — magpie route-any + one-sided
# expose make a bare id follow the switch. ENDPOINT above stays the curated model/spec source.
MAGPIE = "http://127.0.0.1:3425"
MAGPIE_V1 = f"{MAGPIE}/v1"
MODELS = CANON["models"]
LOCAL = CANON.get("local", {})
TOKEN_CMD = str(Path(CANON["token_source"]).expanduser())
DRY = "--dry-run" in sys.argv

CC_PATH = Path.home() / ".commandcode/providers.json"
PI_PATH = Path.home() / ".pi/agent/models.json"
OC_PATH = Path.home() / ".config/opencode/opencode.json"
OMP_PATH = Path.home() / ".omp/agent/models.yml"


def token() -> str:
    return subprocess.run([TOKEN_CMD], capture_output=True, text=True).stdout.strip()


def write(path: Path, obj) -> None:
    text = json.dumps(obj, indent=2, ensure_ascii=False) + "\n"
    if DRY:
        print(f"--- would write {path} ({len(obj.get('providers', obj.get('provider', {})))} providers) ---")
        return
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(text)
    print(f"wrote {path}")


def gen_command_code():
    models = {}
    for m in MODELS:
        entry = {"contextWindow": m["context"]}
        if m["wire"] == "anthropic":
            entry = {"api": "anthropic-messages", "baseURL": MAGPIE, **entry}
        elif m["wire"] == "responses":
            entry = {"api": "openai-responses", **entry}
        if m["efforts"]:
            entry["reasoningEfforts"] = m["efforts"]
        models[m["id"]] = entry
    cfg = {
        "providers": {
            "cpa": {"baseURL": MAGPIE_V1, "apiKey": f"!{TOKEN_CMD}", "models": models},
            "bonsai-local": {
                "baseURL": "http://127.0.0.1:8091/v1", "apiKey": False,
                "models": {"bonsai-2-27b": {"contextWindow": 65536, "maxOutput": 8192}},
            },
        }
    }
    write(CC_PATH, cfg)


def _reconcile_pi_selection(provs):
    """Pi keeps its default selection in a SEPARATE file (~/.pi/agent/settings.json). Pi 0.85.1
    resolves it as modelRuntime.getModel(defaultProvider, defaultModel), where defaultModel is a
    BARE model id looked up UNDER defaultProvider (model-resolver.js: m.provider == provider and
    m.id == defaultId) — it does NOT split a 'provider/id' prefix. So the only resolvable form is
    defaultModel=<bare id> + defaultProvider=<provider that serves it>. Regenerating models.json can
    drop the provider the selection named, leaving it unresolvable (Pi falls back to
    providerModels[0]). Reconcile to the correct form:
      - a MISSING defaultModel key is a legitimate 'nothing selected' -> skip;
      - a present non-string defaultModel is malformed -> ERROR + non-zero;
      - a bare id (or a legacy 'x/y', taking y as the id) is resolved to a provider in the rebuilt
        map that serves it (preferring the current defaultProvider if it does) and written as
        {defaultModel: <bare id>, defaultProvider: <provider>}; if none serves it, ERROR + non-zero.
    Verified with Pi's own semantics: defaultProvider in the map AND it serves the bare defaultModel."""
    sp = Path.home() / ".pi/agent/settings.json"
    if not sp.exists():
        return                                       # no selection to reconcile -> nothing to do
    try:
        s = json.loads(sp.read_text())
    except json.JSONDecodeError as e:
        # A corrupt settings.json is NOT skippable: we cannot verify the selection resolves after
        # wiring, so fail loudly instead of silently reporting success.
        print(f"  ERROR pi settings.json unreadable ({e}); cannot verify/repoint selection")
        if not DRY:
            sys.exit(1)
        return

    if "defaultModel" not in s:
        return                                        # legitimately absent -> nothing to reconcile
    dm = s["defaultModel"]
    if not isinstance(dm, str):
        # present but malformed (e.g. an object/null/number) -> cannot verify; fail loudly.
        print(f"  ERROR pi settings.json: defaultModel is not a string ({type(dm).__name__}: {dm!r})")
        if not DRY:
            sys.exit(1)
        return

    mid = dm.split("/", 1)[1] if "/" in dm else dm    # bare id, or y from a legacy 'x/y'
    dp = s.get("defaultProvider")

    def serves(p):
        return any(m.get("id") == mid for m in provs.get(p, {}).get("models", []))

    # Target form is a BARE defaultModel + a defaultProvider that serves it (Pi looks up
    # getModel(defaultProvider, defaultModel) without splitting). Keep the current provider if it
    # serves the id; else pick one in the rebuilt map that does. Never fabricate a provider.
    resolved = dp if (isinstance(dp, str) and dp in provs and serves(dp)) else next((p for p in provs if serves(p)), None)
    if resolved is None:
        print(f"  ERROR pi settings.json: no rebuilt provider serves model '{mid}' "
              f"(defaultModel '{dm}', defaultProvider '{dp}')")
        if not DRY:
            sys.exit(1)
        return

    if s.get("defaultModel") == mid and s.get("defaultProvider") == resolved:
        return                                        # positive control: already {bare id, serving provider}
    s["defaultModel"], s["defaultProvider"] = mid, resolved
    verb = "would repoint" if DRY else "repointed"
    if not DRY:
        sp.write_text(json.dumps(s, indent=2, ensure_ascii=False) + "\n")
    print(f"  pi settings.json: {verb} selection -> defaultModel={mid} defaultProvider={resolved}")


def gen_pi():
    tok = token()
    # Pi's anthropic-messages client sends `thinking.type.enabled`, which the relay's
    # Claude path rejects (it wants adaptive thinking). Claude works over plain chat
    # completions here (incl. reasoning_effort), so fold the anthropic wire into chat.
    # Only Command Code uses the native anthropic wire, where it is compatible.
    wire_to_prov = {
        "anthropic": ("cpa", MAGPIE_V1, "openai-completions"),
        "responses": ("cpa-codex", MAGPIE_V1, "openai-responses"),
        "chat": ("cpa", MAGPIE_V1, "openai-completions"),
    }
    provs = {}
    for m in MODELS:
        pname, base, api = wire_to_prov[m["wire"]]
        # The relay rejects Pi's "developer" role (sent for reasoning models on the
        # chat/responses wires); send it as a system message instead. Reasoning
        # effort itself is kept (the relay models support reasoning_effort).
        p = provs.setdefault(pname, {"baseUrl": base, "api": api, "apiKey": tok,
                                     "compat": {"supportsDeveloperRole": False}, "models": []})
        entry = {"id": m["id"], "contextWindow": m["context"]}
        if m["efforts"]:
            entry["reasoning"] = True
        p["models"].append(entry)
    provs["bonsai-local"] = {
        "baseUrl": "http://127.0.0.1:8091/v1", "api": "openai-completions", "apiKey": "bonsai",
        "compat": {"supportsDeveloperRole": False, "supportsReasoningEffort": False},
        "models": [{"id": "bonsai-2-27b", "name": "Bonsai 2 27B (Local)", "reasoning": False,
                    "input": ["text"], "contextWindow": 65536, "maxTokens": 8192,
                    "cost": {"input": 0, "output": 0, "cacheRead": 0, "cacheWrite": 0}}],
    }
    write(PI_PATH, {"providers": provs})
    _reconcile_pi_selection(provs)


def gen_opencode():
    tok = token()
    # Claude + Gemini + open models run through openai-compatible (chat); GPT
    # responses models need the openai (Responses API) provider.
    chatish = {m["id"]: {"name": m["id"]} for m in MODELS if m["wire"] in ("chat", "anthropic")}
    responses = {m["id"]: {"name": m["id"]} for m in MODELS if m["wire"] == "responses"}
    # preserve existing non-provider keys if the file exists
    existing = {}
    if OC_PATH.exists():
        try:
            existing = json.loads(OC_PATH.read_text())
        except json.JSONDecodeError:
            existing = {}
    cfg = {k: v for k, v in existing.items() if k not in ("provider", "$schema", "model")}
    cfg["$schema"] = "https://opencode.ai/config.json"
    cfg["model"] = "cpa/claude-opus-4-8"
    cfg["provider"] = {
        "cpa": {
            "name": "CPA (relay, chat)", "npm": "@ai-sdk/openai-compatible",
            "options": {"apiKey": tok, "baseURL": MAGPIE_V1}, "models": chatish,
        },
        "cpa-codex": {
            "name": "CPA (relay, responses)", "npm": "@ai-sdk/openai",
            "options": {"apiKey": tok, "baseURL": MAGPIE_V1}, "models": responses,
        },
        "bonsai-local": {
            "name": "Bonsai 2 27B (Local)", "npm": "@ai-sdk/openai-compatible",
            "options": {"apiKey": "bonsai", "baseURL": "http://127.0.0.1:8091/v1"},
            "models": {"bonsai-2-27b": {"name": "Bonsai 2 27B (Local)"}},
        },
    }
    write(OC_PATH, cfg)


def gen_omp():
    # omp (omp.sh) reads ~/.omp/agent/models.yml — provider-centric camelCase YAML.
    # Split by wire like Pi: anthropic+chat -> one openai-completions provider (the relay
    # serves Claude over chat too), responses (GPT-5.x/6/codex) -> an openai-responses
    # provider. apiKey "!<cmd>" => omp runs the relay-token script and uses its stdout.
    # Curated bare ids from relay-models.json (clean — NOT a magpie /v1/models discovery,
    # which would surface backend-prefixed cpa-local/cpa-cloud noise). Model roles (which
    # model is Default/plan/smol) live in omp's config.yml via `/model`, not here.
    #
    # ENDPOINT = the LOCAL magpie gateway (:3425), NOT relay-models.json's cloud `endpoint`
    # (headroom.geeker). omp joins the other agents on magpie so it (a) works in local mode
    # (cloud headroom can be down), and (b) follows `cpa switch` local<->cloud transparently
    # (magpie flips the backend behind the fixed :3425; omp never needs a restart/re-edit).
    # magpie routes BARE ids (route-any + one-sided expose makes them follow the switch).
    MAGPIE_V1 = "http://127.0.0.1:3425/v1"
    comp = [m for m in MODELS if m["wire"] in ("chat", "anthropic")]
    resp = [m for m in MODELS if m["wire"] == "responses"]

    def block(name, api, models):
        out = [f"  {name}:", f"    baseUrl: {MAGPIE_V1}", f"    api: {api}",
               f'    apiKey: "!{TOKEN_CMD}"', "    models:"]
        for m in models:
            out.append(f"      - id: {m['id']}")
            out.append(f"        contextWindow: {m['context']}")
            if m["efforts"]:
                out.append("        reasoning: true")
                # Pin a default thinking effort for flagships that set defaultEffort
                # (opus-4.8+ -> xhigh). reasoning:true is the prerequisite; efforts is
                # the supported set (reconcile-managed), defaultLevel the chosen default.
                de = m.get("defaultEffort")
                if de and de in m["efforts"]:
                    out.append("        thinking:")
                    out.append("          mode: effort")
                    out.append(f"          efforts: [{', '.join(m['efforts'])}]")
                    out.append(f"          defaultLevel: {de}")
        return "\n".join(out)

    parts = [
        "# Generated by cpa agent-sync (gen_agent_configs.py) from relay-models.json — do not hand-edit.",
        "# omp (omp.sh) -> CPA relay (curated frontier). Wire-split providers; bare clean ids.",
        "providers:",
        block("cpa", "openai-completions", comp),
    ]
    if resp:
        parts.append(block("cpa-responses", "openai-responses", resp))
    parts.append("  bonsai-local:\n    baseUrl: http://127.0.0.1:8091/v1\n    api: openai-completions\n    auth: none\n"
                 "    models:\n      - id: bonsai-2-27b\n        contextWindow: 65536\n        maxTokens: 8192")
    text = "\n".join(parts) + "\n"
    if DRY:
        print(f"--- would write {OMP_PATH} (cpa:{len(comp)} + cpa-responses:{len(resp)} models) ---")
        return
    OMP_PATH.parent.mkdir(parents=True, exist_ok=True)
    OMP_PATH.write_text(text)
    print(f"wrote {OMP_PATH}")


GENERATORS = {"commandcode": gen_command_code, "cc": gen_command_code, "pi": gen_pi,
              "opencode": gen_opencode, "oc": gen_opencode, "omp": gen_omp}

if __name__ == "__main__":
    only = None
    if "--only" in sys.argv:
        i = sys.argv.index("--only")
        only = sys.argv[i + 1] if i + 1 < len(sys.argv) else None
    print(f"source: {HERE/'relay-models.json'}  ({len(MODELS)} relay models)"
          + (f"  [--only {only}]" if only else ""))
    if only:
        fn = GENERATORS.get(only)
        if not fn:
            print(f"unknown --only target: {only} (choices: {sorted(set(GENERATORS))})")
            sys.exit(1)
        fn()
    else:
        gen_command_code()
        gen_pi()
        gen_opencode()
        gen_omp()
    print("done" + (" (dry-run)" if DRY else ""))
