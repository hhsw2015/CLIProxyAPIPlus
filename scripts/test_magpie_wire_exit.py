#!/usr/bin/env python3
"""Regression for MAG-WIRE-RESULT (real-CLI exit) + MAG-WIRE-TEST-ISOLATION (token isolation).

A generator write failure during `magpie_sync.py --wire` must make the PROCESS exit non-zero.
This drives magpie_sync.py as a SUBPROCESS through its `__main__` (`sys.exit(main() or 0)`), so
reverting that line to a bare `main()` makes this test RED (exit would become 0).

Isolated: a temp dir holds a COPY of magpie_sync.py next to a stub gen_agent_configs.py
(chosen exit code), a stub magpie_mode.sh, and a no-op magpie binary via MAGPIE_BIN; the backend
URLs are pointed at an unreachable loopback port; and the relay-token command is overridden to a
STUB via MAGPIE_SYNC_TOKEN_CMD that LOGS each call. So no magpie mutation, no real credential use,
and no live-config write; the real ~/.commandcode/relay-token.sh is never invoked (asserted via
the stub's call log). It DOES make loopback connection attempts to 127.0.0.1:1 (reachable()
probes), which are immediately refused — no real gateway request / no network egress.

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
        tok_log = d / "token_calls.log"
        tok_stub = d / "token_stub.sh"
        tok_stub.write_text(f'#!/bin/sh\necho call >> "{tok_log}"\necho stub-token\n')
        tok_stub.chmod(0o755)
        env = {
            **os.environ,
            "MAGPIE_BIN": str(d / "magpie_noop"),          # no-op magpie -> no real mutations
            "MAGPIE_SYNC_LOCAL": "http://127.0.0.1:1",      # unreachable -> skip expose loop
            "MAGPIE_SYNC_CLOUD": "http://127.0.0.1:1",
            "MAGPIE_SYNC_GW": "http://127.0.0.1:1",
            "MAGPIE_SYNC_TOKEN_CMD": str(tok_stub),         # stub token -> real relay-token.sh unused
        }
        r = subprocess.run([sys.executable, str(d / "magpie_sync.py"), "--wire", "pi"],
                           env=env, capture_output=True, text=True, timeout=60)
        stub_calls = len(tok_log.read_text().splitlines()) if tok_log.exists() else 0
        return r.returncode, stub_calls
    finally:
        shutil.rmtree(d, ignore_errors=True)


def main():
    rc_fail, calls_fail = _run_cli(gen_exit=1)
    assert rc_fail != 0, f"injected generator failure: CLI exited {rc_fail}, expected non-zero"
    print(f"  gen fails -> CLI exit {rc_fail} (non-zero) OK")

    rc_ok, calls_ok = _run_cli(gen_exit=0)
    assert rc_ok == 0, f"generator success: CLI exited {rc_ok}, expected 0"
    print(f"  gen ok    -> CLI exit {rc_ok} OK")

    # Token isolation: every token lookup went through the stub, so the real relay-token.sh had
    # zero calls. If the TOKEN_CMD override regressed, the real script would be used and the stub
    # log would be empty -> this assertion fails.
    assert calls_fail > 0 and calls_ok > 0, (
        f"token not isolated: stub saw {calls_fail}/{calls_ok} calls "
        "(real relay-token.sh may have been invoked)")
    print(f"  token isolated -> stub handled all token calls ({calls_fail}+{calls_ok}); "
          "real relay-token.sh unused OK")

    print("PASS: --wire exit propagation + token isolation")


if __name__ == "__main__":
    main()
