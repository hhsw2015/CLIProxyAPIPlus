#!/usr/bin/env python3
"""
check-size-doc.py — CPA-IMAGE-DEFAULT-DOC gate (M2-b r5): the generated skins must state the SAME default size the CLI
uses (cli/cpa-defaults.env), and that description must FOLLOW an alias change (a pure rerun must not leave it stale).
Asserts: (1) SKILL.md + snippet default-size == defaults.env CPA_DEFAULT_SIZE_IMAGE; (2) a private alias edit to 3072x3072
→ regenerate → all three (defaults.env, SKILL, snippet) show 3072 → regenerate from the real alias → back to the real value.
Run: python3 scripts/check-size-doc.py
"""
import json
import os
import pathlib
import subprocess
import sys
import tempfile

ROOT = pathlib.Path(__file__).resolve().parent.parent
GEN = ROOT / "scripts" / "gen-cpa-skills.py"
DEFAULTS = ROOT / "cli" / "cpa-defaults.env"
SKILL = ROOT / "skills" / "cpa-image" / "SKILL.md"
SNIPPET = ROOT / "snippets" / "cpa-image.luau"
ALIASES = pathlib.Path(os.environ.get("CPA_FRONTIER_ALIASES", pathlib.Path.home() / "Dev" / "CLIProxyAPIPlus" / "docs" / "frontier-aliases.json"))

fails = []


def gen(aliases_path=None):
    env = {**os.environ}
    if aliases_path:
        env["CPA_FRONTIER_ALIASES"] = str(aliases_path)
    subprocess.run([sys.executable, str(GEN)], env=env, check=True, capture_output=True)


def size_in_defaults():
    for line in DEFAULTS.read_text().splitlines():
        if line.startswith("CPA_DEFAULT_SIZE_IMAGE="):
            return line.split("=", 1)[1]
    return None


def want(name, cond):
    print(("ok  " if cond else "FAIL ") + name)
    if not cond:
        fails.append(name)


# 1) current consistency: defaults.env size appears in both skins' default-size description
gen()  # from the real alias
sz = size_in_defaults()
want("defaults.env has CPA_DEFAULT_SIZE_IMAGE", bool(sz))
want("SKILL.md Default-size line == defaults.env", sz is not None and f"`{sz}`" in SKILL.read_text())
want("SKILL.md --size param default == defaults.env", sz is not None and f"| {sz} |" in SKILL.read_text())
want("snippet Default-size comment == defaults.env", sz is not None and sz in SNIPPET.read_text())

# 2) follows an alias change (not just a static rerun)
try:
    data = json.loads(ALIASES.read_text())
    data["media"]["Image generation"]["size"] = "3072x3072"
    tf = tempfile.NamedTemporaryFile("w", suffix=".json", delete=False)
    json.dump(data, tf); tf.close()
    gen(tf.name)
    want("alias size→3072 → defaults.env follows", size_in_defaults() == "3072x3072")
    want("alias size→3072 → SKILL default follows", "`3072x3072`" in SKILL.read_text() and "| 3072x3072 |" in SKILL.read_text())
    want("alias size→3072 → snippet comment follows", "3072x3072" in SNIPPET.read_text())
finally:
    os.unlink(tf.name)
    gen()  # restore from the real alias
want("restore: defaults.env back to the real alias size", size_in_defaults() == sz)

print(f"\nsize-doc gate: {'ALL PASS' if not fails else 'FAIL ' + ', '.join(fails)}")
sys.exit(0 if not fails else 1)
