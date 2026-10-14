#!/usr/bin/env python3
"""
gen-cpa-skills.py — the ONE-source → two-skins generator for the CPA skill family (M2-b).

Source of truth: skills-src/<domain>.md (a ```json spec block + human prose) + the machine-readable
frontier-aliases.json (default model per capability, refreshed every `cpa regen`). From these it emits, idempotently
(same inputs ⇒ byte-identical outputs):

  skills/cpa-<domain>/SKILL.md   — the swarm-seat skin (frontmatter + body + the source prose)
  snippets/cpa-<domain>.luau     — the Carly skin (a module returning a fn that invokes the cli/cpa-<domain> wrapper)
  cli/cpa-defaults.env           — bridges frontier-aliases → the hand-written CLI (CPA_DEFAULT_MODEL_<DOMAIN>=<id>)

The CLI wrappers (cli/cpa-<domain>) are HAND-WRITTEN (logic in code, tested) — this generator does NOT produce them; it
only resolves their default model into cpa-defaults.env so a `cpa regen` + re-gen refreshes every skin + the CLI default
from the single machine-readable source, with no drift.

Usage: python3 scripts/gen-cpa-skills.py
Env:   CPA_FRONTIER_ALIASES (default ~/Dev/CLIProxyAPIPlus/docs/frontier-aliases.json)
Deterministic: domains processed sorted; no wall-clock in output; the frontier "generated" date (from the source JSON)
is carried as provenance (deterministic per input).
"""
import json
import os
import re
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent  # the cpa-skills repo root
SRC_DIR = ROOT / "skills-src"
SKILLS_DIR = ROOT / "skills"
SNIPPETS_DIR = ROOT / "snippets"
CLI_DIR = ROOT / "cli"
DEFAULT_ALIASES = Path.home() / "Dev" / "CLIProxyAPIPlus" / "docs" / "frontier-aliases.json"

JSON_BLOCK = re.compile(r"```json\s*\n(.*?)\n```", re.DOTALL)


def fail(msg: str) -> "None":
    sys.stderr.write(f"gen-cpa-skills: {msg}\n")
    sys.exit(1)


def load_aliases() -> dict:
    p = Path(os.environ.get("CPA_FRONTIER_ALIASES", str(DEFAULT_ALIASES)))
    try:
        data = json.loads(p.read_text(encoding="utf-8"))
    except FileNotFoundError:
        fail(f"frontier-aliases.json not found: {p} (set CPA_FRONTIER_ALIASES)")
    except Exception as exc:
        fail(f"frontier-aliases.json unreadable/malformed: {p}: {exc}")
    if not isinstance(data, dict) or "media" not in data:
        fail(f"frontier-aliases.json missing 'media': {p}")
    return data


def parse_src(path: Path) -> "tuple[dict, str]":
    """Return (spec, prose). spec = the first ```json block; prose = everything after that block, stripped."""
    text = path.read_text(encoding="utf-8")
    m = JSON_BLOCK.search(text)
    if not m:
        fail(f"{path.name}: no ```json spec block")
    try:
        spec = json.loads(m.group(1))
    except Exception as exc:
        fail(f"{path.name}: spec JSON invalid: {exc}")
    for k in ("domain", "cli", "skill_name", "description", "endpoint", "method", "mode", "default_model_key", "params"):
        if k not in spec:
            fail(f"{path.name}: spec missing '{k}'")
    prose = text[m.end():].strip()
    return spec, prose


def resolve_model(aliases: dict, spec: dict) -> str:
    key = spec["default_model_key"]
    media = aliases.get("media", {})
    if key not in media or not isinstance(media[key], dict) or "id" not in media[key]:
        fail(f"{spec['domain']}: default_model_key '{key}' not in frontier-aliases media")
    return str(media[key]["id"])


# Param defaults that are RESOLVED from the single source (frontier-aliases) rather than the static spec: the flag name →
# the resolved-value key. The generator overrides the spec's static default with the resolved value so the rendered doc can
# never drift from what the CLI actually uses (CPA-IMAGE-DEFAULT-DOC fix).
RESOLVED_PARAM = {"--size": "size", "--voice": "voice"}


def md_params_table(params: list, resolved: dict) -> str:
    rows = ["| param | required | default | description |", "|---|---|---|---|"]
    for p in params:
        key = RESOLVED_PARAM.get(p["name"])
        default = resolved[key] if key and resolved.get(key) else p.get("default", "—")
        rows.append(f"| `{p['name']}` | {'yes' if p.get('required') else 'no'} | {default} | {p.get('desc', '')} |")
    return "\n".join(rows)


def gen_skill_md(spec: dict, prose: str, model: str, generated: str, resolved: dict) -> str:
    cli = spec["cli"]
    gotchas = "".join(f"- {g}\n" for g in spec.get("gotchas", []))
    size_line = f"- **Default size**: `{resolved['size']}` (resolved from frontier-aliases.json, generated {generated}; override with `--size`).\n" if resolved.get("size") else ""
    fm = (
        "---\n"
        f"name: {spec['skill_name']}\n"
        f"description: {spec['description']}\n"
        f"argument-hint: {spec.get('argument_hint', '')}\n"
        "allowed-tools: [Bash, Read]\n"
        "---\n"
    )
    body = (
        f"\n# {spec['skill_name']}\n\n"
        f"<!-- GENERATED from skills-src/{spec['domain']}.md by scripts/gen-cpa-skills.py — DO NOT EDIT HERE; edit the source. -->\n\n"
        f"Swarm-seat skin for CPA **{spec['domain']}**. It invokes the hand-written CLI wrapper, which calls CPA's "
        f"`{spec['method']} {{CPA}}{spec['endpoint']}` ({spec['mode']}).\n\n"
        f"- **Default model**: `{model}` (resolved from frontier-aliases.json, generated {generated}; override with `--model`).\n"
        f"{size_line}"
        f"- **Key**: the CLI reads `CPA_KEY_CLI` + `CPA_BASE_URL` from `~/.agenthop/cpa/keys.env` (0600). The key NEVER appears here.\n\n"
        f"## Invoke\n\n"
        f"```sh\n"
        f"cli/{cli} {spec.get('argument_hint', '')}\n"
        f"```\n\n"
        f"## Parameters\n\n{md_params_table(spec['params'], resolved)}\n\n"
        f"## Gotchas\n\n{gotchas}\n"
        f"{prose}\n"
    )
    return fm + body


def gen_luau(spec: dict, model: str, generated: str, resolved: dict) -> str:
    cli = spec["cli"]
    # the positional (required param with no leading --) and the --flag params
    positional = next((p["name"] for p in spec["params"] if not p["name"].startswith("--")), "arg")
    flags = [p["name"][2:] for p in spec["params"] if p["name"].startswith("--")]
    flag_lines = "".join(
        f'  if opts.{f} ~= nil then argv[#argv + 1] = "--{f}"; argv[#argv + 1] = tostring(opts.{f}) end\n'
        for f in flags
    )
    size_line = f"-- Default size: {resolved['size']} (resolved from frontier-aliases.json; pass opts.size to override).\n" if resolved.get("size") else ""
    return (
        f"-- cpa-{spec['domain']} — Carly snippet (GENERATED from skills-src/{spec['domain']}.md; DO NOT EDIT HERE).\n"
        f"-- Returns a function that runs the hand-written cli/{cli} wrapper via tern.process.run and awaits the result.\n"
        f"-- Default model: {model} (resolved from frontier-aliases.json, generated {generated}; pass opts.model to override).\n"
        f"{size_line}"
        f"-- CARLY SNIPPET CACHE: Carly caches snippet modules per VM — an edit here is picked up only on a VM RESET\n"
        f"-- (reload). After re-running the generator (e.g. a new strongest model), RESET the VM to refresh this snippet.\n"
        f"-- The CPA key never appears here; the CLI reads CPA_KEY_CLI from ~/.agenthop/cpa/keys.env.\n"
        f"return function({positional}, opts)\n"
        f"  opts = opts or {{}}\n"
        f'  local argv = {{ "{cli}", {positional} }}\n'
        f"{flag_lines}"
        f"  return tern.process.run(argv) -- Awaitable<ProcessResult>; Carly awaits it ({{status, stdout, stderr}})\n"
        f"end\n"
    )


def main() -> None:
    aliases = load_aliases()
    generated = str(aliases.get("generated", "unknown"))
    # Domain sources only: skip README.md and _*.md (reserved for docs that live alongside the sources).
    srcs = [p for p in sorted(SRC_DIR.glob("*.md")) if p.name.lower() != "readme.md" and not p.name.startswith("_")]
    if not srcs:
        fail(f"no source docs in {SRC_DIR}")
    defaults = {}  # domain -> model, for cpa-defaults.env
    voices = {}    # domain -> default voice (tts), optional
    sizes = {}     # domain -> default size (image), from frontier-aliases media[key].size when present
    for src in srcs:
        spec, prose = parse_src(src)
        domain = spec["domain"]
        model = resolve_model(aliases, spec)
        defaults[domain] = model
        if isinstance(spec.get("defaults"), dict) and "voice" in spec["defaults"]:
            voices[domain] = spec["defaults"]["voice"]
        media_entry = aliases.get("media", {}).get(spec["default_model_key"], {})
        if isinstance(media_entry, dict) and media_entry.get("size"):
            sizes[domain] = str(media_entry["size"])  # model-specific default size (e.g. doubao-seedream-5.0 ⇒ 2048x2048)
        # resolved defaults that drive BOTH skins' docs (so the rendered default can never drift from the CLI/defaults.env)
        resolved = {"size": sizes.get(domain), "voice": voices.get(domain)}
        # SKILL.md
        skill_dir = SKILLS_DIR / spec["cli"]
        skill_dir.mkdir(parents=True, exist_ok=True)
        (skill_dir / "SKILL.md").write_text(gen_skill_md(spec, prose, model, generated, resolved), encoding="utf-8")
        # luau snippet
        SNIPPETS_DIR.mkdir(parents=True, exist_ok=True)
        (SNIPPETS_DIR / f"{spec['cli']}.luau").write_text(gen_luau(spec, model, generated, resolved), encoding="utf-8")

    # cli/cpa-defaults.env — generated bridge from frontier-aliases to the hand-written CLIs (deterministic, sorted).
    CLI_DIR.mkdir(parents=True, exist_ok=True)
    lines = [
        "# GENERATED by scripts/gen-cpa-skills.py from frontier-aliases.json — DO NOT EDIT; re-run the generator.",
        f"# Resolved from frontier-aliases.json (generated {generated}). Sourced by cli/cpa-common.sh for per-domain defaults.",
    ]
    for domain in sorted(defaults):
        lines.append(f"CPA_DEFAULT_MODEL_{domain.upper()}={defaults[domain]}")
    for domain in sorted(sizes):
        lines.append(f"CPA_DEFAULT_SIZE_{domain.upper()}={sizes[domain]}")
    for domain in sorted(voices):
        lines.append(f"CPA_DEFAULT_VOICE_{domain.upper()}={voices[domain]}")
    (CLI_DIR / "cpa-defaults.env").write_text("\n".join(lines) + "\n", encoding="utf-8")

    print(f"gen-cpa-skills: {len(srcs)} domains → skills/ + snippets/ + cli/cpa-defaults.env (frontier generated {generated})")


if __name__ == "__main__":
    main()
