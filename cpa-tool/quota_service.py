"""Main quota fetching workflow and persistence."""

from __future__ import annotations

import base64
import datetime as dt
import json
import os
import re
import shlex
import sqlite3
import concurrent.futures
from collections import deque
from pathlib import Path
from typing import Any, Iterable
from urllib import error, request

try:
    from .config import (
        ANTIGRAVITY_QUOTA_URLS,
        ANTIGRAVITY_REQUEST_HEADERS,
        CODEX_REQUEST_HEADERS,
        CODEX_USAGE_URL,
        DEFAULT_ANTIGRAVITY_PROJECT_ID,
        GEMINI_CLI_QUOTA_URL,
        GEMINI_CLI_REQUEST_HEADERS,
        SUPPORTED_PROVIDERS,
        RuntimeOptions,
    )
except ImportError:  # pragma: no cover
    from config import (
        ANTIGRAVITY_QUOTA_URLS,
        ANTIGRAVITY_REQUEST_HEADERS,
        CODEX_REQUEST_HEADERS,
        CODEX_USAGE_URL,
        DEFAULT_ANTIGRAVITY_PROJECT_ID,
        GEMINI_CLI_QUOTA_URL,
        GEMINI_CLI_REQUEST_HEADERS,
        SUPPORTED_PROVIDERS,
        RuntimeOptions,
    )

def utc_now_iso() -> str:
    return dt.datetime.now(dt.timezone.utc).replace(microsecond=0).isoformat().replace("+00:00", "Z")


def normalize_string(value: Any) -> str | None:
    if isinstance(value, str):
        trimmed = value.strip()
        return trimmed or None
    if isinstance(value, (int, float)) and not isinstance(value, bool):
        return str(value)
    return None


def normalize_bool(value: Any) -> bool:
    if isinstance(value, bool):
        return value
    if isinstance(value, (int, float)):
        return value != 0
    if isinstance(value, str):
        return value.strip().lower() in {"1", "true", "yes", "on"}
    return False


def normalize_provider(value: Any) -> str | None:
    raw = normalize_string(value)
    if not raw:
        return None
    lowered = raw.lower().replace("_", "-")
    if lowered in {"geminicli", "gemini-cli"}:
        return "gemini-cli"
    if lowered in SUPPORTED_PROVIDERS:
        return lowered
    return None


def clean_bearer_token(value: str | None) -> str | None:
    if not value:
        return None
    token = value.strip()
    if token.lower().startswith("bearer "):
        token = token[7:].strip()
    return token or None


def safe_json_loads(text: str) -> Any:
    trimmed = text.strip()
    if not trimmed:
        return None
    try:
        return json.loads(trimmed)
    except json.JSONDecodeError:
        return None


def read_json_file(path: Path) -> dict[str, Any]:
    raw_text: str
    try:
        raw_text = path.read_text(encoding="utf-8")
    except UnicodeDecodeError:
        raw_text = path.read_text(encoding="utf-8-sig")

    parsed = safe_json_loads(raw_text)
    if not isinstance(parsed, dict):
        raise ValueError("auth file is not a JSON object")
    return parsed


def iter_auth_files(auth_dir: Path, recursive: bool) -> Iterable[Path]:
    if recursive:
        candidates = auth_dir.rglob("*")
    else:
        candidates = auth_dir.iterdir()

    files: list[Path] = []
    for item in candidates:
        if item.is_file():
            files.append(item)
    return sorted(files)


def find_first_string_by_keys(data: Any, keys: Iterable[str], max_depth: int = 5) -> str | None:
    key_set = {k.lower() for k in keys}
    queue: deque[tuple[Any, int]] = deque([(data, 0)])

    while queue:
        node, depth = queue.popleft()
        if depth > max_depth:
            continue

        if isinstance(node, dict):
            for key, value in node.items():
                if str(key).lower() in key_set:
                    match = normalize_string(value)
                    if match:
                        return match
            for value in node.values():
                if isinstance(value, (dict, list)):
                    queue.append((value, depth + 1))
        elif isinstance(node, list):
            for value in node:
                if isinstance(value, (dict, list)):
                    queue.append((value, depth + 1))

    return None


def decode_base64url(segment: str) -> str | None:
    padded = segment + "=" * ((4 - len(segment) % 4) % 4)
    try:
        decoded = base64.urlsafe_b64decode(padded.encode("utf-8"))
    except Exception:
        return None
    try:
        return decoded.decode("utf-8")
    except UnicodeDecodeError:
        return None


def parse_id_token_payload(value: Any) -> dict[str, Any] | None:
    if isinstance(value, dict):
        return value

    text = normalize_string(value)
    if not text:
        return None

    parsed = safe_json_loads(text)
    if isinstance(parsed, dict):
        return parsed

    parts = text.split(".")
    if len(parts) < 2:
        return None

    decoded_payload = decode_base64url(parts[1])
    if not decoded_payload:
        return None

    parsed_jwt = safe_json_loads(decoded_payload)
    if isinstance(parsed_jwt, dict):
        return parsed_jwt
    return None


def resolve_access_token(payload: dict[str, Any]) -> str | None:
    token = find_first_string_by_keys(
        payload,
        keys=(
            "access_token",
            "accessToken",
            "token",
            "auth_token",
            "authToken",
            "authorization",
            "bearer_token",
            "bearerToken",
            "session_token",
            "sessionToken",
        ),
    )
    return clean_bearer_token(token)


def resolve_codex_account_id(payload: dict[str, Any]) -> str | None:
    metadata = payload.get("metadata") if isinstance(payload.get("metadata"), dict) else {}
    attributes = payload.get("attributes") if isinstance(payload.get("attributes"), dict) else {}

    # Prefer explicit account-id fields commonly present in local codex auth files.
    for candidate in (
        payload.get("chatgpt_account_id"),
        payload.get("chatgptAccountId"),
        payload.get("account_id"),
        payload.get("accountId"),
        metadata.get("chatgpt_account_id"),
        metadata.get("chatgptAccountId"),
        metadata.get("account_id"),
        metadata.get("accountId"),
        attributes.get("chatgpt_account_id"),
        attributes.get("chatgptAccountId"),
        attributes.get("account_id"),
        attributes.get("accountId"),
    ):
        account_id = normalize_string(candidate)
        if account_id:
            return account_id

    id_token_candidates = [
        payload.get("id_token"),
        payload.get("idToken"),
        payload.get("access_token"),
        payload.get("accessToken"),
        metadata.get("id_token"),
        metadata.get("idToken"),
        metadata.get("access_token"),
        metadata.get("accessToken"),
        attributes.get("id_token"),
        attributes.get("idToken"),
        attributes.get("access_token"),
        attributes.get("accessToken"),
    ]

    for candidate in id_token_candidates:
        parsed = parse_id_token_payload(candidate)
        if not parsed:
            continue
        auth_claims = parsed.get("https://api.openai.com/auth")
        if isinstance(auth_claims, dict):
            account_id = normalize_string(
                auth_claims.get("chatgpt_account_id")
                or auth_claims.get("chatgptAccountId")
                or auth_claims.get("account_id")
                or auth_claims.get("accountId")
            )
            if account_id:
                return account_id
        account_id = normalize_string(
            parsed.get("chatgpt_account_id")
            or parsed.get("chatgptAccountId")
            or parsed.get("account_id")
            or parsed.get("accountId")
        )
        if account_id:
            return account_id

    return find_first_string_by_keys(
        payload,
        keys=("chatgpt_account_id", "chatgptAccountId", "account_id", "accountId"),
        max_depth=5,
    )


def resolve_antigravity_project_id(payload: dict[str, Any]) -> str:
    direct = normalize_string(payload.get("project_id") or payload.get("projectId"))
    if direct:
        return direct

    installed = payload.get("installed") if isinstance(payload.get("installed"), dict) else {}
    installed_project = normalize_string(installed.get("project_id") or installed.get("projectId"))
    if installed_project:
        return installed_project

    web = payload.get("web") if isinstance(payload.get("web"), dict) else {}
    web_project = normalize_string(web.get("project_id") or web.get("projectId"))
    if web_project:
        return web_project

    return DEFAULT_ANTIGRAVITY_PROJECT_ID


def extract_project_id_from_account(value: Any) -> str | None:
    account = normalize_string(value)
    if not account:
        return None
    matches = list(re.finditer(r"\(([^()]+)\)", account))
    if not matches:
        return None
    return normalize_string(matches[-1].group(1))


def resolve_gemini_cli_project_id(payload: dict[str, Any]) -> str | None:
    metadata = payload.get("metadata") if isinstance(payload.get("metadata"), dict) else {}
    attributes = payload.get("attributes") if isinstance(payload.get("attributes"), dict) else {}

    for candidate in (
        payload.get("account"),
        metadata.get("account"),
        attributes.get("account"),
    ):
        project_id = extract_project_id_from_account(candidate)
        if project_id:
            return project_id

    return find_first_string_by_keys(
        payload,
        keys=("project_id", "projectId", "gcp_project_id", "gcpProjectId"),
        max_depth=3,
    )


def detect_provider(file_name: str, payload: dict[str, Any]) -> str:
    for key in ("provider", "type", "auth_provider", "authProvider"):
        provider = normalize_provider(payload.get(key))
        if provider:
            return provider

    lowered_name = file_name.lower().replace("_", "-")
    if "gemini-cli" in lowered_name:
        return "gemini-cli"
    if "antigravity" in lowered_name:
        return "antigravity"
    if "codex" in lowered_name:
        return "codex"

    if resolve_codex_account_id(payload):
        return "codex"
    if resolve_gemini_cli_project_id(payload):
        return "gemini-cli"
    if normalize_string(payload.get("project_id") or payload.get("projectId")):
        return "antigravity"

    return "unknown"


def get_api_error_message(status_code: int, body: Any, body_text: str) -> str:
    message = ""
    if isinstance(body, dict):
        raw_error = body.get("error")
        if isinstance(raw_error, dict):
            message = normalize_string(raw_error.get("message")) or ""
        else:
            message = normalize_string(raw_error) or ""
        if not message:
            message = normalize_string(body.get("message")) or ""
    elif isinstance(body, str):
        message = body.strip()

    if not message:
        message = body_text.strip()

    if status_code and message:
        return f"{status_code} {message}".strip()
    if status_code:
        return f"HTTP {status_code}"
    return message or "Request failed"


def http_request(
    method: str,
    url: str,
    headers: dict[str, str],
    data: str | None,
    timeout: float,
) -> dict[str, Any]:
    payload = data.encode("utf-8") if data is not None else None
    req = request.Request(url=url, method=method, headers=headers, data=payload)

    try:
        with request.urlopen(req, timeout=timeout) as response:
            body_bytes = response.read()
            body_text = body_bytes.decode("utf-8", errors="replace")
            parsed = safe_json_loads(body_text)
            return {
                "status_code": int(response.status),
                "headers": dict(response.headers.items()),
                "body_text": body_text,
                "body": parsed,
            }
    except error.HTTPError as exc:
        body_bytes = exc.read()
        body_text = body_bytes.decode("utf-8", errors="replace")
        parsed = safe_json_loads(body_text)
        return {
            "status_code": int(exc.code),
            "headers": dict(exc.headers.items()) if exc.headers else {},
            "body_text": body_text,
            "body": parsed,
        }


def fetch_codex_quota(access_token: str, account_id: str, timeout: float) -> dict[str, Any]:
    headers = {
        **CODEX_REQUEST_HEADERS,
        "Authorization": f"Bearer {access_token}",
        "Chatgpt-Account-Id": account_id,
    }
    response = http_request("GET", CODEX_USAGE_URL, headers=headers, data=None, timeout=timeout)

    status_code = response["status_code"]
    if status_code < 200 or status_code >= 300:
        raise RuntimeError(get_api_error_message(status_code, response["body"], response["body_text"]))
    return {
        "url": CODEX_USAGE_URL,
        "status_code": status_code,
        "response": response["body"] if response["body"] is not None else response["body_text"],
    }


def fetch_antigravity_quota(access_token: str, project_id: str, timeout: float) -> dict[str, Any]:
    request_body = json.dumps({"project": project_id}, separators=(",", ":"))
    errors: list[dict[str, Any]] = []

    headers = {
        **ANTIGRAVITY_REQUEST_HEADERS,
        "Authorization": f"Bearer {access_token}",
    }

    for url in ANTIGRAVITY_QUOTA_URLS:
        response = http_request("POST", url, headers=headers, data=request_body, timeout=timeout)
        status_code = response["status_code"]
        if 200 <= status_code < 300:
            return {
                "url": url,
                "status_code": status_code,
                "response": response["body"] if response["body"] is not None else response["body_text"],
            }

        errors.append(
            {
                "url": url,
                "status_code": status_code,
                "error": get_api_error_message(status_code, response["body"], response["body_text"]),
            }
        )

    if errors:
        raise RuntimeError(errors[-1]["error"])
    raise RuntimeError("no antigravity endpoint response")


def fetch_gemini_cli_quota(access_token: str, project_id: str, timeout: float) -> dict[str, Any]:
    headers = {
        **GEMINI_CLI_REQUEST_HEADERS,
        "Authorization": f"Bearer {access_token}",
    }
    request_body = json.dumps({"project": project_id}, separators=(",", ":"))
    response = http_request("POST", GEMINI_CLI_QUOTA_URL, headers=headers, data=request_body, timeout=timeout)

    status_code = response["status_code"]
    if status_code < 200 or status_code >= 300:
        raise RuntimeError(get_api_error_message(status_code, response["body"], response["body_text"]))
    return {
        "url": GEMINI_CLI_QUOTA_URL,
        "status_code": status_code,
        "response": response["body"] if response["body"] is not None else response["body_text"],
    }


def should_skip(payload: dict[str, Any], provider: str) -> str | None:
    if normalize_bool(payload.get("disabled")):
        return "credential is disabled"

    runtime_only = payload.get("runtime_only")
    if runtime_only is None:
        runtime_only = payload.get("runtimeOnly")

    if provider == "gemini-cli" and normalize_bool(runtime_only):
        return "runtime-only gemini-cli credential is skipped"

    return None


def process_auth_file(path: Path, timeout: float) -> dict[str, Any]:
    fetched_at = utc_now_iso()
    base: dict[str, Any] = {
        "file_name": path.name,
        "file_path": str(path),
        "fetched_at": fetched_at,
    }

    try:
        payload = read_json_file(path)
    except Exception as exc:
        return {
            **base,
            "provider": "unknown",
            "success": False,
            "error": f"invalid auth file: {exc}",
        }

    provider = detect_provider(path.name, payload)
    result: dict[str, Any] = {
        **base,
        "provider": provider,
    }

    if provider not in SUPPORTED_PROVIDERS:
        return {
            **result,
            "success": False,
            "error": "unsupported provider",
        }

    skip_reason = should_skip(payload, provider)
    if skip_reason:
        return {
            **result,
            "success": False,
            "skipped": True,
            "error": skip_reason,
        }

    access_token = resolve_access_token(payload)
    if not access_token:
        return {
            **result,
            "success": False,
            "error": "missing access token",
        }

    try:
        if provider == "codex":
            account_id = resolve_codex_account_id(payload)
            if not account_id:
                return {
                    **result,
                    "success": False,
                    "error": "missing ChatGPT account id",
                }

            quota = fetch_codex_quota(access_token, account_id, timeout)
            return {
                **result,
                "success": True,
                "account_id": account_id,
                "quota": quota,
            }

        if provider == "antigravity":
            project_id = resolve_antigravity_project_id(payload)
            quota = fetch_antigravity_quota(access_token, project_id, timeout)
            return {
                **result,
                "success": True,
                "project_id": project_id,
                "quota": quota,
            }

        if provider == "gemini-cli":
            project_id = resolve_gemini_cli_project_id(payload)
            if not project_id:
                return {
                    **result,
                    "success": False,
                    "error": "missing gemini-cli project id",
                }

            quota = fetch_gemini_cli_quota(access_token, project_id, timeout)
            return {
                **result,
                "success": True,
                "project_id": project_id,
                "quota": quota,
            }
    except Exception as exc:
        return {
            **result,
            "success": False,
            "error": str(exc),
        }

    return {
        **result,
        "success": False,
        "error": "unsupported provider",
    }


def extract_quota_status_code(result: dict[str, Any]) -> int | None:
    quota = result.get("quota")
    if not isinstance(quota, dict):
        return None

    status_code = quota.get("status_code")
    if isinstance(status_code, bool):
        return None
    if isinstance(status_code, int):
        return status_code
    if isinstance(status_code, str):
        trimmed = status_code.strip()
        if trimmed and re.fullmatch(r"-?\d+", trimmed):
            return int(trimmed)
    return None


def _is_int(value: Any) -> bool:
    return isinstance(value, int) and not isinstance(value, bool)


def is_codex_result_structure_normal(result: dict[str, Any]) -> bool:
    quota = result.get("quota")
    if not isinstance(quota, dict):
        return False

    if not isinstance(quota.get("url"), str):
        return False
    if not _is_int(quota.get("status_code")):
        return False

    response = quota.get("response")
    if not isinstance(response, dict):
        return False

    if not isinstance(result.get("file_name"), str):
        return False
    if not isinstance(result.get("file_path"), str):
        return False
    if not isinstance(result.get("fetched_at"), str):
        return False
    if not isinstance(result.get("provider"), str):
        return False
    if not isinstance(result.get("success"), bool):
        return False
    if not isinstance(result.get("account_id"), str):
        return False

    if not isinstance(response.get("user_id"), str):
        return False
    if not isinstance(response.get("account_id"), str):
        return False
    if not isinstance(response.get("email"), str):
        return False
    if not isinstance(response.get("plan_type"), str):
        return False

    rate_limit = response.get("rate_limit")
    if not isinstance(rate_limit, dict):
        return False
    if not isinstance(rate_limit.get("allowed"), bool):
        return False
    if not isinstance(rate_limit.get("limit_reached"), bool):
        return False

    primary_window = rate_limit.get("primary_window")
    secondary_window = rate_limit.get("secondary_window")
    if not isinstance(primary_window, dict):
        return False
    if not isinstance(secondary_window, dict):
        return False

    for key in ("used_percent", "limit_window_seconds", "reset_after_seconds", "reset_at"):
        if not _is_int(primary_window.get(key)):
            return False
        if not _is_int(secondary_window.get(key)):
            return False

    code_review_rate_limit = response.get("code_review_rate_limit")
    if not isinstance(code_review_rate_limit, dict):
        return False
    if not isinstance(code_review_rate_limit.get("allowed"), bool):
        return False
    if not isinstance(code_review_rate_limit.get("limit_reached"), bool):
        return False
    code_review_primary_window = code_review_rate_limit.get("primary_window")
    if not isinstance(code_review_primary_window, dict):
        return False
    if code_review_rate_limit.get("secondary_window") is not None:
        return False
    for key in ("used_percent", "limit_window_seconds", "reset_after_seconds", "reset_at"):
        if not _is_int(code_review_primary_window.get(key)):
            return False

    credits = response.get("credits")
    if not isinstance(credits, dict):
        return False
    if not isinstance(credits.get("has_credits"), bool):
        return False
    if not isinstance(credits.get("unlimited"), bool):
        return False

    if response.get("promo") is not None:
        return False

    return True


def compute_is_normal(result: dict[str, Any]) -> int:
    status_code = extract_quota_status_code(result)
    if status_code != 200:
        return 2

    provider = normalize_provider(result.get("provider")) or "unknown"
    if provider == "codex" and not is_codex_result_structure_normal(result):
        return 1

    return 0


def ensure_database(conn: sqlite3.Connection) -> None:
    conn.execute(
        """
        CREATE TABLE IF NOT EXISTS quota_results (
            id INTEGER PRIMARY KEY AUTOINCREMENT,
            file_name TEXT NOT NULL,
            file_path TEXT NOT NULL,
            provider TEXT NOT NULL,
            fetched_at TEXT NOT NULL,
            success INTEGER NOT NULL,
            result_json TEXT NOT NULL,
            is_normal INTEGER NOT NULL DEFAULT 0
        )
        """
    )

    conn.execute(
        "DELETE FROM quota_results WHERE id NOT IN (SELECT MAX(id) FROM quota_results GROUP BY file_name)"
    )
    conn.execute("CREATE INDEX IF NOT EXISTS idx_quota_results_provider ON quota_results (provider)")
    conn.execute(
        "CREATE UNIQUE INDEX IF NOT EXISTS idx_quota_results_file_name_unique ON quota_results (file_name)"
    )
    conn.commit()


def save_result(conn: sqlite3.Connection, result: dict[str, Any]) -> int:
    is_normal = compute_is_normal(result)

    conn.execute(
        """
        INSERT INTO quota_results (
            file_name, file_path, provider, fetched_at, success, result_json, is_normal
        ) VALUES (?, ?, ?, ?, ?, ?, ?)
        ON CONFLICT(file_name) DO UPDATE SET
            file_path = excluded.file_path,
            provider = excluded.provider,
            fetched_at = excluded.fetched_at,
            success = excluded.success,
            result_json = excluded.result_json,
            is_normal = excluded.is_normal
        """,
        (
            str(result.get("file_name", "")),
            str(result.get("file_path", "")),
            str(result.get("provider", "unknown")),
            str(result.get("fetched_at", utc_now_iso())),
            1 if bool(result.get("success")) else 0,
            json.dumps(result, ensure_ascii=False, separators=(",", ":")),
            is_normal,
        ),
    )

    return is_normal


def write_delete_script(script_path: Path, auth_dir: Path, is_normal: int, file_names: Iterable[str]) -> None:
    unique_names = sorted({normalized for name in file_names if (normalized := normalize_string(name))})

    lines = [
        "#!/usr/bin/env bash",
        "set -euo pipefail",
        "",
        f"# Generated by fetchQuota at {utc_now_iso()}",
        "# Files are removed from the current auth directory by file name only.",
        f"cd -- {shlex.quote(str(auth_dir))}",
    ]

    if unique_names:
        for name in unique_names:
            lines.append(f"sudo rm -f -- {shlex.quote(name)}")
    else:
        lines.append(f"echo 'No files with is_normal = {is_normal}.'")

    script_path.write_text("\n".join(lines) + "\n", encoding="utf-8")
    script_path.chmod(0o755)


def generate_delete_scripts(auth_dir: Path, abnormal_by_status: dict[int, list[str]]) -> list[Path]:
    project_root = Path(__file__).resolve().parent
    script_dir = project_root / "delete" / auth_dir.name
    script_dir.mkdir(parents=True, exist_ok=True)

    generated_paths: list[Path] = []
    for status in (1, 2):
        script_path = script_dir / f"delete_is_normal_{status}.sh"
        write_delete_script(
            script_path=script_path,
            auth_dir=auth_dir,
            is_normal=status,
            file_names=abnormal_by_status.get(status, []),
        )
        generated_paths.append(script_path)

    return generated_paths


def run(options: RuntimeOptions) -> int:
    if not options.auth_dir.exists() or not options.auth_dir.is_dir():
        raise ValueError(f"auth directory does not exist or is not a directory: {options.auth_dir}")

    files = list(iter_auth_files(options.auth_dir, recursive=options.recursive))
    if not files:
        print(f"No files found under: {options.auth_dir}")
        generated_paths = generate_delete_scripts(options.auth_dir, abnormal_by_status={1: [], 2: []})
        for script_path in generated_paths:
            print(f"generated delete script: {script_path}")
        return 0

    conn = sqlite3.connect(options.db_path)
    try:
        ensure_database(conn)

        success_count = 0
        abnormal_by_status: dict[int, list[str]] = {1: [], 2: []}
        max_workers = min(32, (os.cpu_count() or 1) + 4)
        with concurrent.futures.ThreadPoolExecutor(max_workers=max_workers) as executor:
            future_map = {
                executor.submit(process_auth_file, file_path, options.timeout): file_path
                for file_path in files
            }

            for future in concurrent.futures.as_completed(future_map):
                file_path = future_map[future]
                try:
                    result = future.result()
                except Exception as exc:
                    result = {
                        "file_name": file_path.name,
                        "file_path": str(file_path),
                        "fetched_at": utc_now_iso(),
                        "provider": "unknown",
                        "success": False,
                        "error": str(exc),
                    }

                provider = str(result.get("provider", "unknown"))
                is_normal = save_result(conn, result=result)
                if is_normal in abnormal_by_status:
                    file_name = normalize_string(result.get("file_name"))
                    if file_name:
                        abnormal_by_status[is_normal].append(file_name)

                if result.get("success"):
                    success_count += 1

                status = "OK" if result.get("success") else "FAIL"
                print(f"[{status}] {file_path.name} ({provider})")

        conn.commit()

        print("-")
        print(f"database: {options.db_path}")
        print(f"processed files: {len(files)}")
        print(f"successful fetches: {success_count}")
        print(f"failed/skipped fetches: {len(files) - success_count}")

        generated_paths = generate_delete_scripts(options.auth_dir, abnormal_by_status=abnormal_by_status)
        for script_path in generated_paths:
            print(f"generated delete script: {script_path}")
    finally:
        conn.close()

    return 0
