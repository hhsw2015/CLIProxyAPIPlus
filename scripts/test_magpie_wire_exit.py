#!/usr/bin/env python3
"""Regression for MAG-WIRE-RESULT: `magpie_sync.py --wire` must propagate a generator
write failure to a non-zero exit status (and not report a failed item as success).

Self-contained (no magpie binary, no network, no live-config writes): the generator and
magpie_mode subprocesses are stubbed, so nothing is actually generated or switched.
Run: python3 scripts/test_magpie_wire_exit.py
"""
import sys
import types
from pathlib import Path

HERE = Path(__file__).resolve().parent
sys.path.insert(0, str(HERE))
import magpie_sync as m  # noqa: E402


def _drive(gen_rc):
    # Force a --wire run for a single managed agent, with every external side-effect stubbed.
    m.DRY = False
    m.WIRE = True
    m.WIRE_ONLY = ["pi"]
    m.run = lambda *a, **k: ""            # magpie CLI helper -> no-op
    m.detect_mode = lambda: "local"
    m.reachable = lambda *a, **k: False   # both backends "down" -> skip the expose loop
    m.token = lambda: "x"

    def fake_run(cmd, *a, **k):
        if any("gen_agent_configs.py" in str(c) for c in cmd):   # the generator call
            return types.SimpleNamespace(returncode=gen_rc, stdout="", stderr="boom" if gen_rc else "")
        return types.SimpleNamespace(returncode=0, stdout="", stderr="")  # magpie_mode.sh re-assert
    m.subprocess = types.SimpleNamespace(run=fake_run)
    return m.main()


def main():
    rc_fail = _drive(gen_rc=1)
    assert rc_fail == 1, f"injected generator failure -> main() returned {rc_fail}, expected 1"
    print(f"  injected gen failure  -> main()={rc_fail} (non-zero) OK")

    rc_ok = _drive(gen_rc=0)
    assert rc_ok == 0, f"generator success -> main() returned {rc_ok}, expected 0"
    print(f"  generator success     -> main()={rc_ok} OK")

    print("PASS: --wire propagates generator failure to exit status")


if __name__ == "__main__":
    main()
