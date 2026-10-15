#!/usr/bin/env python3
"""Regression for MAG-WIRE-RESULT via the REAL CLI entry point.

A generator write failure during `magpie_sync.py --wire` must make the PROCESS exit non-zero.
This drives magpie_sync.py as a SUBPROCESS through its `__main__` (`sys.exit(main() or 0)`), so
reverting that line to a bare `main()` makes this test RED (exit would become 0).

Side-effect-free: runs a COPY of magpie_sync.py in a temp dir next to a stub gen_agent_configs.py
(exits with a chosen code), a stub magpie_mode.sh, and a no-op magpie binary via MAGPIE_BIN; the
backend URLs are pointed at an unreachable port so no magpie mutation / no network / no live-config
write happens.

Run: python3 scripts/test_magpie_wire_exit.py
"""
import os
import shutil
import subprocess
import sys
import tempfile
from pathlib import Path

SRC = Path(__file__).resolve().parent / "magpie_sync.py"


def _run_cli(gen_exit):
    d = Path(tempfile.mkdtemp(prefix="magpie_wire_test_"))
    try:
        shutil.copy2(SRC, d / "magpie_sync.py")
        # stub generator: ignores args, exits with the injected code
        (d / "gen_agent_configs.py").write_text(f"import sys\nsys.exit({gen_exit})\n")
        for name in ("magpie_mode.sh", "magpie_noop"):
            p = d / name
            p.write_text("#!/bin/sh\nexit 0\n")
            p.chmod(0o755)
        env = {
            **os.environ,
            "MAGPIE_BIN": str(d / "magpie_noop"),          # no-op magpie -> no real mutations
            "MAGPIE_SYNC_LOCAL": "http://127.0.0.1:1",      # unreachable -> skip expose loop
            "MAGPIE_SYNC_CLOUD": "http://127.0.0.1:1",
            "MAGPIE_SYNC_GW": "http://127.0.0.1:1",
        }
        r = subprocess.run([sys.executable, str(d / "magpie_sync.py"), "--wire", "pi"],
                           env=env, capture_output=True, text=True, timeout=60)
        return r.returncode
    finally:
        shutil.rmtree(d, ignore_errors=True)


def main():
    rc_fail = _run_cli(gen_exit=1)
    assert rc_fail != 0, f"injected generator failure: CLI exited {rc_fail}, expected non-zero"
    print(f"  gen fails -> CLI exit {rc_fail} (non-zero) OK")

    rc_ok = _run_cli(gen_exit=0)
    assert rc_ok == 0, f"generator success: CLI exited {rc_ok}, expected 0"
    print(f"  gen ok    -> CLI exit {rc_ok} OK")

    print("PASS: --wire propagates generator failure to the process exit status")


if __name__ == "__main__":
    main()
