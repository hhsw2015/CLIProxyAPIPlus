#!/usr/bin/env python3
"""Regression for gen_agent_configs._reconcile_pi_selection.

Contract:
  - corrupt settings.json  -> ERROR + non-zero exit (NOT silently skipped)   [MG-CO-PI-SETTINGS-UNREADABLE]
  - missing settings.json  -> skip (nothing to reconcile)
  - dangling selection     -> repoint to a provider serving the same model id
  - already-valid selection-> unchanged (positive control)
  - unservable model id    -> ERROR + non-zero exit

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
    print(f"  corrupt settings   -> exit {code} (non-zero) OK")

    code, _ = _reconcile(None)
    assert code is None, f"missing settings: expected skip, got exit {code!r}"
    print("  missing settings   -> skipped OK")

    code, after = _reconcile(json.dumps({"defaultModel": "cpa-local/claude-opus-4-8", "defaultProvider": "magpie"}))
    assert code is None and after["defaultModel"] == "cpa/claude-opus-4-8", f"repoint failed: {code!r} {after!r}"
    assert after["defaultProvider"] in PROVS, f"fabricated provider: {after['defaultProvider']!r}"
    print(f"  dangling (prov/id) -> repointed to {after['defaultModel']} prov={after['defaultProvider']} OK")

    # Second face: a BARE defaultModel (no slash) with a stale provider must NOT fabricate a
    # provider from the bare model string; it resolves to the provider that serves the id.
    code, after = _reconcile(json.dumps({"defaultModel": "gpt-6-astra", "defaultProvider": "magpie"}))
    assert code is None, f"bare model: unexpected exit {code!r}"
    assert after["defaultModel"] == "cpa-codex/gpt-6-astra", f"bare model not normalized: {after!r}"
    assert after["defaultProvider"] == "cpa-codex" and after["defaultProvider"] in PROVS, \
        f"bare model fabricated provider: {after['defaultProvider']!r}"
    print(f"  bare model + stale -> repointed to {after['defaultModel']} prov={after['defaultProvider']} OK")

    code, after = _reconcile(json.dumps({"defaultModel": "cpa/claude-opus-4-8", "defaultProvider": "cpa"}))
    assert code is None and after["defaultModel"] == "cpa/claude-opus-4-8", f"valid changed: {code!r} {after!r}"
    print("  already valid      -> unchanged OK")

    code, _ = _reconcile(json.dumps({"defaultModel": "cpa/nonexistent-xyz", "defaultProvider": "cpa"}))
    assert code not in (None, 0), f"unservable id: expected non-zero exit, got {code!r}"
    print(f"  unservable id      -> exit {code} (non-zero) OK")

    print("PASS: corrupt=error, missing=skip, dangling=repoint, valid=noop, unservable=error")


if __name__ == "__main__":
    main()
