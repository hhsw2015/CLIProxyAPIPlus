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
ENDPOINT = CANON["endpoint"]                       # https://headroom.geeker.indevs.in
BASE_V1 = f"{ENDPOINT}/v1"
MODELS = CANON["models"]
LOCAL = CANON.get("local", {})
TOKEN_CMD = str(Path(CANON["token_source"]).expanduser())
DRY = "--dry-run" in sys.argv

CC_PATH = Path.home() / ".commandcode/providers.json"
PI_PATH = Path.home() / ".pi/agent/models.json"
OC_PATH = Path.home() / ".config/opencode/opencode.json"


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
            entry = {"api": "anthropic-messages", "baseURL": ENDPOINT, **entry}
        elif m["wire"] == "responses":
            entry = {"api": "openai-responses", **entry}
        if m["efforts"]:
            entry["reasoningEfforts"] = m["efforts"]
        models[m["id"]] = entry
    cfg = {
        "providers": {
            "cpa": {"baseURL": BASE_V1, "apiKey": f"!{TOKEN_CMD}", "models": models},
            "bonsai-local": {
                "baseURL": "http://127.0.0.1:8091/v1", "apiKey": False,
                "models": {"bonsai-2-27b": {"contextWindow": 65536, "maxOutput": 8192}},
            },
        }
    }
    write(CC_PATH, cfg)


def gen_pi():
    tok = token()
    # Pi's anthropic-messages client sends `thinking.type.enabled`, which the relay's
    # Claude path rejects (it wants adaptive thinking). Claude works over plain chat
    # completions here (incl. reasoning_effort), so fold the anthropic wire into chat.
    # Only Command Code uses the native anthropic wire, where it is compatible.
    wire_to_prov = {
        "anthropic": ("cpa", BASE_V1, "openai-completions"),
        "responses": ("cpa-codex", BASE_V1, "openai-responses"),
        "chat": ("cpa", BASE_V1, "openai-completions"),
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
            "options": {"apiKey": tok, "baseURL": BASE_V1}, "models": chatish,
        },
        "cpa-codex": {
            "name": "CPA (relay, responses)", "npm": "@ai-sdk/openai",
            "options": {"apiKey": tok, "baseURL": BASE_V1}, "models": responses,
        },
        "bonsai-local": {
            "name": "Bonsai 2 27B (Local)", "npm": "@ai-sdk/openai-compatible",
            "options": {"apiKey": "bonsai", "baseURL": "http://127.0.0.1:8091/v1"},
            "models": {"bonsai-2-27b": {"name": "Bonsai 2 27B (Local)"}},
        },
    }
    write(OC_PATH, cfg)


if __name__ == "__main__":
    print(f"source: {HERE/'relay-models.json'}  ({len(MODELS)} relay models)")
    gen_command_code()
    gen_pi()
    gen_opencode()
    print("done" + (" (dry-run)" if DRY else ""))
