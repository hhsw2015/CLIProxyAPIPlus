#!/usr/bin/env python3
"""Regression for gen_agent_configs._reconcile_pi_selection.

Pi 0.85.1 resolves getModel(defaultProvider, defaultModel) with defaultModel as a BARE id under
defaultProvider (no 'provider/id' split), so the target form is {defaultModel:<bare id>,
defaultProvider:<provider serving it>}.

Contract:
  - corrupt settings.json       -> ERROR + non-zero exit (NOT silently skipped)
  - missing file / missing key  -> skip (nothing selected)
  - non-string defaultModel     -> ERROR + non-zero exit (present but malformed)
  - dangling selection (bare or legacy 'x/y') -> repoint to {bare id, serving provider}
  - already-valid bare selection-> unchanged (positive control)
  - unservable model id         -> ERROR + non-zero exit

Self-contained: points HOME at a temp dir, so it only touches that sandbox. No network.
Run: python3 scripts/test_pi_selection_reconcile.py
"""
import json
import os
import sys
import tempfile
from pathlib import Path

HERE = Path(__file__).resolve().parent
sys.path.insert(0, str(HERE))
import gen_agent_configs as g  # noqa: E402

PROVS = {"cpa": {"models": [{"id": "claude-opus-4-8"}]},
         "cpa-codex": {"models": [{"id": "gpt-6-astra"}]},
         "bonsai-local": {"models": [{"id": "bonsai-2-27b"}]}}


def _reconcile(settings_text):
    """Run _reconcile_pi_selection with a temp HOME; return (exit_code_or_None, settings_after)."""
    d = tempfile.mkdtemp(prefix="pi_sel_test_")
    old = os.environ.get("HOME")
    try:
        os.environ["HOME"] = d
        sp = Path(d) / ".pi/agent/settings.json"
        if settings_text is not None:
            sp.parent.mkdir(parents=True, exist_ok=True)
            sp.write_text(settings_text)
        g.DRY = False
        code = None
        try:
            g._reconcile_pi_selection(PROVS)
        except SystemExit as e:
            code = e.code if e.code is not None else 0
        after = None
        if sp.exists():
            try:
                after = json.loads(sp.read_text())
            except Exception:  # noqa: BLE001
                after = None
        return code, after
    finally:
        if old is not None:
            os.environ["HOME"] = old
        else:
            os.environ.pop("HOME", None)


def main():
    code, _ = _reconcile("{not valid json")
    assert code not in (None, 0), f"corrupt settings: expected non-zero exit, got {code!r}"
    print(f"  corrupt settings    -> exit {code} (non-zero) OK")

    code, _ = _reconcile(None)
    assert code is None, f"missing file: expected skip, got exit {code!r}"
    print("  missing file        -> skipped OK")

    code, _ = _reconcile(json.dumps({"defaultProvider": "cpa"}))                 # key absent
    assert code is None, f"missing defaultModel key: expected skip, got exit {code!r}"
    print("  missing key         -> skipped OK")

    code, _ = _reconcile(json.dumps({"defaultModel": {"id": "x"}, "defaultProvider": "cpa"}))  # non-string
    assert code not in (None, 0), f"non-string defaultModel: expected non-zero exit, got {code!r}"
    print(f"  non-string model    -> exit {code} (non-zero) OK")

    # Pi's resolvable form is BARE defaultModel + a serving defaultProvider (NOT 'provider/id').
    code, after = _reconcile(json.dumps({"defaultModel": "cpa-local/claude-opus-4-8", "defaultProvider": "magpie"}))
    assert code is None and after["defaultModel"] == "claude-opus-4-8" and after["defaultProvider"] == "cpa", \
        f"legacy x/y repoint failed: {code!r} {after!r}"
    print(f"  legacy x/y + stale  -> defaultModel={after['defaultModel']} defaultProvider={after['defaultProvider']} OK")

    code, after = _reconcile(json.dumps({"defaultModel": "gpt-6-astra", "defaultProvider": "magpie"}))
    assert code is None and after["defaultModel"] == "gpt-6-astra" and after["defaultProvider"] == "cpa-codex", \
        f"bare id + stale repoint failed: {code!r} {after!r}"
    print(f"  bare id + stale     -> defaultModel={after['defaultModel']} defaultProvider={after['defaultProvider']} OK")

    code, after = _reconcile(json.dumps({"defaultModel": "claude-opus-4-8", "defaultProvider": "cpa"}))
    assert code is None and after["defaultModel"] == "claude-opus-4-8" and after["defaultProvider"] == "cpa", \
        f"valid bare selection changed: {code!r} {after!r}"
    print("  already valid (bare)-> unchanged OK")

    code, _ = _reconcile(json.dumps({"defaultModel": "nonexistent-xyz", "defaultProvider": "cpa"}))
    assert code not in (None, 0), f"unservable id: expected non-zero exit, got {code!r}"
    print(f"  unservable id       -> exit {code} (non-zero) OK")

    print("PASS: corrupt/non-string=error, missing(file|key)=skip, dangling(bare|x/y)=repoint to bare+provider, valid=noop, unservable=error")


if __name__ == "__main__":
    main()
