#!/usr/bin/env bash
# cpa-common.sh — shared helpers for the cpa-<domain> CLI wrappers (M2-b). SOURCED by each cpa-<domain>, not executed.
# Loads CPA credentials from ~/.agenthop/cpa/keys.env (0600) and the generated per-domain defaults from cpa-defaults.env.
# The CPA key is NEVER printed (not even in --dry: the Authorization header is shown redacted).
set -euo pipefail

CPA_CLI_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CPA_KEYS_ENV="${CPA_KEYS_ENV:-$HOME/.agenthop/cpa/keys.env}"
CPA_DEFAULTS_ENV="${CPA_DEFAULTS_ENV:-$CPA_CLI_DIR/cpa-defaults.env}"

# Load keys + defaults (KEY=VALUE files; empty values allowed). The 0600 keys.env holds CPA_BASE_URL + CPA_KEY_CLI.
cpa_load() {
  if [ -f "$CPA_KEYS_ENV" ]; then set -a; . "$CPA_KEYS_ENV"; set +a; fi
  if [ -f "$CPA_DEFAULTS_ENV" ]; then set -a; . "$CPA_DEFAULTS_ENV"; set +a; fi
  : "${CPA_BASE_URL:=}"; : "${CPA_KEY_CLI:=}"
}

# True when a real call can be made (base URL + key both present).
cpa_have_creds() { [ -n "${CPA_BASE_URL:-}" ] && [ -n "${CPA_KEY_CLI:-}" ]; }

# Print the base URL with a trailing slash stripped (so "$base/v1/..." is clean).
cpa_base() { printf '%s' "${CPA_BASE_URL%/}"; }

cpa_die() { echo "cpa: $*" >&2; exit 2; }

# Emit a redacted request preview for --dry (NEVER the real key).
cpa_dry_header() {
  echo "[dry-run] $1 $(cpa_base)$2"
  echo "[dry-run] Authorization: Bearer ***redacted*** (CPA_KEY_CLI from $CPA_KEYS_ENV)"
}

# When a real call is requested but creds are missing, fail clearly (point the user at keys.env). Used by non-dry paths.
cpa_require_creds() {
  cpa_have_creds || cpa_die "no CPA creds — set CPA_BASE_URL + CPA_KEY_CLI in $CPA_KEYS_ENV (or pass --dry to preview the request)"
}

# Parse a JSON field from stdin via python3 (no jq dependency). $1 = a python expression over the parsed object `d`.
cpa_json() { python3 -c 'import sys,json
d=json.load(sys.stdin)
print(eval(sys.argv[1]))' "$1"; }

# Validate an option value by STATUS ONLY — returns 0 when present and not itself a flag, else 1 (CPA-ARGS fix). It does
# NOT echo the value: the caller assigns the RAW "$2" so the exact bytes are kept (a `$(opt_ok …)` capture would strip a
# trailing newline — e.g. `--out $'x.mp3\n'` would silently write to `x.mp3`). This also stops `--model --dry` from
# swallowing `--dry` as the model. Our option values never legitimately start with "-"; pass such a value after `--`.
opt_ok() { case "${1-}" in ""|-*) return 1;; *) return 0;; esac; }
