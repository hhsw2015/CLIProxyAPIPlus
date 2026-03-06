#!/usr/bin/env python3
"""CLI entrypoint for quota fetching."""

from __future__ import annotations

from typing import Sequence

try:
    from .config import parse_runtime_options
    from .quota_service import run
except ImportError:  # pragma: no cover
    from config import parse_runtime_options
    from quota_service import run


def main(argv: Sequence[str] | None = None) -> int:
    try:
        options = parse_runtime_options(argv)
        return run(options)
    except Exception as exc:
        print(f"ERROR: {exc}")
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
