"""Configuration and CLI options for quota fetching."""

from __future__ import annotations

import argparse
from dataclasses import dataclass
from pathlib import Path
from typing import Sequence

ANTIGRAVITY_QUOTA_URLS = [
    "https://daily-cloudcode-pa.googleapis.com/v1internal:fetchAvailableModels",
    "https://daily-cloudcode-pa.sandbox.googleapis.com/v1internal:fetchAvailableModels",
    "https://cloudcode-pa.googleapis.com/v1internal:fetchAvailableModels",
]

ANTIGRAVITY_REQUEST_HEADERS = {
    "Content-Type": "application/json",
    "User-Agent": "antigravity/1.11.5 windows/amd64",
}

GEMINI_CLI_QUOTA_URL = "https://cloudcode-pa.googleapis.com/v1internal:retrieveUserQuota"

GEMINI_CLI_REQUEST_HEADERS = {
    "Content-Type": "application/json",
}

CODEX_USAGE_URL = "https://chatgpt.com/backend-api/wham/usage"

CODEX_REQUEST_HEADERS = {
    "Content-Type": "application/json",
    "User-Agent": "codex_cli_rs/0.76.0 (Debian 13.0.0; x86_64) WindowsTerminal",
}

DEFAULT_ANTIGRAVITY_PROJECT_ID = "bamboo-precept-lgxtn"
SUPPORTED_PROVIDERS = {"codex", "antigravity", "gemini-cli"}

# Keep defaults anchored to this repo directory (fetchQuota/), not its parent.
PROJECT_ROOT = Path(__file__).resolve().parent
DEFAULT_AUTH_DIR = PROJECT_ROOT / "auth"
DEFAULT_DB_PATH = PROJECT_ROOT / "db" / "quota_results.db"


@dataclass(frozen=True)
class RuntimeOptions:
    auth_dir: Path
    db_path: Path
    recursive: bool = False
    timeout: float = 20.0


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        description="Read auth files, fetch quota, and store raw JSON results into SQLite."
    )
    parser.add_argument(
        "--auth-dir",
        default=str(DEFAULT_AUTH_DIR),
        help="Directory containing auth files (default: <project>/auth)",
    )
    parser.add_argument(
        "--db",
        default=str(DEFAULT_DB_PATH),
        help="SQLite database file path (default: <project>/db/quota_results.db)",
    )
    parser.add_argument("--recursive", action="store_true", help="Recursively scan sub-directories")
    parser.add_argument(
        "--timeout",
        type=float,
        default=20.0,
        help="HTTP timeout in seconds (default: 20)",
    )
    return parser


def parse_runtime_options(argv: Sequence[str] | None = None) -> RuntimeOptions:
    parser = build_parser()
    args = parser.parse_args(argv)

    auth_dir = Path(args.auth_dir).expanduser().resolve()
    db_path = Path(args.db).expanduser().resolve()
    db_path.parent.mkdir(parents=True, exist_ok=True)

    return RuntimeOptions(
        auth_dir=auth_dir,
        db_path=db_path,
        recursive=bool(args.recursive),
        timeout=float(args.timeout),
    )
