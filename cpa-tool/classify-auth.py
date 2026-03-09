#!/usr/bin/env python3
"""Scan Codex auth files, delete invalid tokens, and recycle recovered quota files."""

from __future__ import annotations

import argparse
import base64
from concurrent.futures import ThreadPoolExecutor, as_completed
import json
import os
import random
import re
import shutil
import sys
import threading
import time
from dataclasses import asdict, dataclass
from pathlib import Path
from typing import Any, Callable, Iterable
from urllib import error, parse, request

try:
    from curl_cffi import requests as curl_requests
except ImportError:  # pragma: no cover - optional dependency
    curl_requests = None

DEFAULT_CODEX_BASE_URL = "https://chatgpt.com/backend-api/codex"
DEFAULT_REFRESH_URL = "https://auth.openai.com/oauth/token"
DEFAULT_MODELS_URL = "https://api.openai.com/v1/models"
DEFAULT_AUTH_DIR = "~/.cli-proxy-api"
DEFAULT_CLIENT_ID = "app_EMoamEEZ73f0CkXaXp7hrann"
DEFAULT_VERSION = "0.98.0"
DEFAULT_USER_AGENT = "codex_cli_rs/0.98.0 (python-port)"
DEFAULT_WORKERS = min(32, max(4, (os.cpu_count() or 1) * 4))
DEFAULT_401_CONCURRENCY = 8
DEFAULT_RETRY_ATTEMPTS = 3
DEFAULT_RETRY_BACKOFF = 0.6
DEFAULT_EXCEEDED_DIR_NAME = "limit"
ANSI_RESET = "\033[0m"
ANSI_BOLD = "\033[1m"
ANSI_DIM = "\033[2m"
ANSI_RED = "\033[31m"
ANSI_GREEN = "\033[32m"
ANSI_YELLOW = "\033[33m"
ANSI_CYAN = "\033[36m"
ANSI_MAGENTA = "\033[35m"
DELETED_KEYWORDS = (
    "user_not_found",
    "account_deactivated",
    "account_deleted",
    "user_deactivated",
    "account not found",
    "deleted",
    "deactivated",
    "banned",
    "disabled",
    "suspended",
)
BROWSER_USER_AGENTS = (
    (
        "chrome",
        "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 "
        "(KHTML, like Gecko) Chrome/134.0.0.0 Safari/537.36",
    ),
    (
        "safari",
        "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 "
        "(KHTML, like Gecko) Version/17.6 Safari/605.1.15",
    ),
    (
        "edge",
        "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 "
        "(KHTML, like Gecko) Chrome/134.0.0.0 Safari/537.36 Edg/134.0.0.0",
    ),
)
BROWSER_IMPERSONATE = ("chrome", "safari", "edge")
_RECOVERY_SEMAPHORE = threading.BoundedSemaphore(DEFAULT_401_CONCURRENCY)


@dataclass
class CheckResult:
    file: str
    provider: str
    email: str
    account_id: str
    status_code: int | None
    unauthorized_401: bool
    no_limit_unlimited: bool
    quota_exceeded: bool
    quota_resets_at: int | None
    error: str
    response_preview: str
    delete_invalid: bool = False
    duplicate_deleted: bool = False


@dataclass
class FileOpError:
    file: str
    error: str


@dataclass
class AuthCandidate:
    path: Path
    payload: dict[str, Any] | None
    identity: str
    bucket: str
    freshness: tuple[int, int]
    scanable: bool
    parse_error: str = ""


@dataclass
class RefreshedTokenData:
    access_token: str
    refresh_token: str
    id_token: str = ""
    email: str = ""
    account_id: str = ""
    expired: str = ""


def _is_tty_stdout() -> bool:
    return hasattr(sys.stdout, "isatty") and sys.stdout.isatty()


def _supports_color(disabled: bool) -> bool:
    return (not disabled) and _is_tty_stdout() and ("NO_COLOR" not in os.environ)


def _paint(text: str, *codes: str, enabled: bool) -> str:
    if not enabled or not codes:
        return text
    return "".join(codes) + text + ANSI_RESET


def _truncate(text: str, limit: int) -> str:
    if limit <= 0 or len(text) <= limit:
        return text
    if limit <= 3:
        return "." * limit
    return text[: limit - 3] + "..."


class _ProgressDisplay:
    def __init__(self, enabled: bool) -> None:
        self.enabled = enabled
        self._last_len = 0
        self._finished = False

    def update(self, current: int, total: int, path: Path) -> None:
        if not self.enabled or total <= 0:
            return

        width = shutil.get_terminal_size(fallback=(100, 20)).columns
        bar_width = max(12, min(30, width - 52))
        percent = int((current * 100) / total)
        filled = int((current * bar_width) / total)
        bar = "#" * filled + "-" * (bar_width - filled)
        message = f"[{bar}] {current}/{total} {percent:>3}% {_truncate(path.name, 28)}"
        message = _truncate(message, max(10, width - 1))
        padding = " " * max(0, self._last_len - len(message))
        sys.stdout.write(f"\r{message}{padding}")
        sys.stdout.flush()
        self._last_len = len(message)

    def finish(self) -> None:
        if not self.enabled or self._finished:
            return
        self._finished = True
        sys.stdout.write("\n")
        sys.stdout.flush()


def _first_non_empty_str(values: Iterable[Any]) -> str:
    for value in values:
        if isinstance(value, str):
            stripped = value.strip()
            if stripped:
                return stripped
    return ""


def _dot_get(data: Any, dotted_key: str) -> Any:
    current = data
    for key in dotted_key.split("."):
        if not isinstance(current, dict):
            return None
        current = current.get(key)
    return current


def _pick(data: dict[str, Any], candidates: list[str]) -> str:
    values = [_dot_get(data, key) for key in candidates]
    return _first_non_empty_str(values)


def _contains_deleted_keyword(text: str) -> bool:
    lowered = (text or "").lower()
    return any(keyword in lowered for keyword in DELETED_KEYWORDS)


def _random_browser_user_agent() -> str:
    return random.choice(BROWSER_USER_AGENTS)[1]


def _extract_timestamp(path: Path) -> int:
    match = re.search(r"_(\d{10,})\.json$", path.name)
    if match:
        return int(match.group(1))
    return 0


def _file_freshness(path: Path) -> tuple[int, int]:
    try:
        stat = path.stat()
        mtime_ns = int(stat.st_mtime_ns)
    except OSError:
        mtime_ns = 0
    return (_extract_timestamp(path), mtime_ns)


def _utc_now_rfc3339() -> str:
    return time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())


def _future_rfc3339(expires_in: int | float | None) -> str:
    if not expires_in:
        return ""
    try:
        seconds = max(int(expires_in), 0)
    except (TypeError, ValueError):
        return ""
    return time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime(time.time() + seconds))


def _jwt_claims_no_verify(id_token: str) -> dict[str, Any]:
    if not id_token or id_token.count(".") < 2:
        return {}
    payload_b64 = id_token.split(".")[1]
    pad = "=" * ((4 - (len(payload_b64) % 4)) % 4)
    try:
        payload = base64.urlsafe_b64decode((payload_b64 + pad).encode("ascii"))
        parsed = json.loads(payload.decode("utf-8"))
        if isinstance(parsed, dict):
            return parsed
    except Exception:  # noqa: BLE001
        return {}
    return {}


def _looks_like_codex(path: Path, payload: dict[str, Any]) -> bool:
    provider = _pick(payload, ["type", "provider", "metadata.type"])
    if provider:
        return provider.lower() == "codex"

    name = path.name.lower()
    if name.startswith("codex-"):
        return True

    access_token = _pick(
        payload,
        [
            "access_token",
            "accessToken",
            "token.access_token",
            "token.accessToken",
            "metadata.access_token",
            "metadata.accessToken",
            "metadata.token.access_token",
            "metadata.token.accessToken",
            "attributes.api_key",
        ],
    )
    refresh_token = _pick(
        payload,
        [
            "refresh_token",
            "refreshToken",
            "token.refresh_token",
            "token.refreshToken",
            "metadata.refresh_token",
            "metadata.refreshToken",
            "metadata.token.refresh_token",
            "metadata.token.refreshToken",
        ],
    )
    account_id = _pick(
        payload,
        ["account_id", "accountId", "metadata.account_id", "metadata.accountId"],
    )

    return bool(access_token and (refresh_token or account_id))


def _extract_auth_fields(payload: dict[str, Any]) -> dict[str, str]:
    return {
        "provider": _pick(payload, ["type", "provider", "metadata.type"]) or "codex",
        "email": _pick(payload, ["email", "metadata.email", "attributes.email"]),
        "access_token": _pick(
            payload,
            [
                "access_token",
                "accessToken",
                "token.access_token",
                "token.accessToken",
                "metadata.access_token",
                "metadata.accessToken",
                "metadata.token.access_token",
                "metadata.token.accessToken",
                "attributes.api_key",
            ],
        ),
        "refresh_token": _pick(
            payload,
            [
                "refresh_token",
                "refreshToken",
                "token.refresh_token",
                "token.refreshToken",
                "metadata.refresh_token",
                "metadata.refreshToken",
                "metadata.token.refresh_token",
                "metadata.token.refreshToken",
            ],
        ),
        "account_id": _pick(
            payload,
            ["account_id", "accountId", "metadata.account_id", "metadata.accountId"],
        ),
        "base_url": _pick(
            payload,
            [
                "base_url",
                "baseUrl",
                "metadata.base_url",
                "metadata.baseUrl",
                "attributes.base_url",
                "attributes.baseUrl",
            ],
        ),
    }


def _candidate_identity(path: Path, payload: dict[str, Any] | None) -> str:
    if isinstance(payload, dict):
        email = _pick(payload, ["email", "metadata.email", "attributes.email"]).lower()
        if email:
            return f"email:{email}"
    return f"path:{path.resolve()}"


def _load_candidate(path: Path, bucket: str) -> AuthCandidate:
    try:
        payload = _load_json(path)
    except Exception as exc:  # noqa: BLE001
        return AuthCandidate(
            path=path,
            payload=None,
            identity=f"path:{path.resolve()}",
            bucket=bucket,
            freshness=_file_freshness(path),
            scanable=True,
            parse_error=f"parse error: {exc}",
        )

    return AuthCandidate(
        path=path,
        payload=payload,
        identity=_candidate_identity(path, payload),
        bucket=bucket,
        freshness=_file_freshness(path),
        scanable=_looks_like_codex(path, payload),
    )


def _choose_primary_candidate(items: list[AuthCandidate]) -> AuthCandidate:
    return max(
        items,
        key=lambda item: (
            1 if item.bucket == "active" else 0,
            item.freshness[0],
            item.freshness[1],
            item.path.name,
        ),
    )


def _build_scan_plan_with_duplicates(
    auth_dir: Path, limit_dir: Path
) -> tuple[list[Path], list[Path], list[Path]]:
    grouped: dict[str, list[AuthCandidate]] = {}

    for path in sorted(auth_dir.glob("*.json")):
        candidate = _load_candidate(path, "active")
        grouped.setdefault(candidate.identity, []).append(candidate)

    if limit_dir.exists() and limit_dir.is_dir():
        for path in sorted(limit_dir.glob("*.json")):
            candidate = _load_candidate(path, "limit")
            grouped.setdefault(candidate.identity, []).append(candidate)

    active_targets: list[Path] = []
    limit_targets: list[Path] = []
    duplicates: list[Path] = []

    for items in grouped.values():
        primary = _choose_primary_candidate(items)
        if primary.bucket == "active":
            active_targets.append(primary.path)
        else:
            limit_targets.append(primary.path)

        for item in items:
            if item.path == primary.path:
                continue
            duplicates.append(item.path)

    active_targets.sort()
    limit_targets.sort()
    duplicates.sort()
    return active_targets, limit_targets, duplicates


def _build_scan_plan(auth_dir: Path, limit_dir: Path) -> tuple[list[Path], list[Path]]:
    active_targets, limit_targets, _ = _build_scan_plan_with_duplicates(auth_dir, limit_dir)
    return active_targets, limit_targets


def _http_request(
    *,
    url: str,
    method: str,
    headers: dict[str, str],
    body: bytes | None,
    timeout: float,
) -> tuple[int, bytes]:
    if curl_requests is not None:
        response = curl_requests.request(
            method=method.upper(),
            url=url,
            data=body,
            headers=headers,
            timeout=timeout,
            impersonate=random.choice(BROWSER_IMPERSONATE),
        )
        return int(response.status_code), bytes(response.content or b"")

    req = request.Request(url=url, data=body, method=method.upper())
    for key, value in headers.items():
        req.add_header(key, value)

    try:
        with request.urlopen(req, timeout=timeout) as resp:
            return int(resp.status), resp.read()
    except error.HTTPError as exc:
        return int(exc.code), exc.read()


def _probe_once(
    *,
    url: str,
    headers: dict[str, str],
    body: bytes,
    timeout: float,
    retry_attempts: int,
    retry_backoff: float,
) -> tuple[int | None, str, str]:
    try:
        status, resp_body = _http_request_with_retry(
            url=url,
            method="POST",
            headers=headers,
            body=body,
            timeout=timeout,
            retry_attempts=retry_attempts,
            retry_backoff=retry_backoff,
        )
        return status, resp_body.decode("utf-8", errors="replace"), ""
    except error.URLError as exc:
        return None, "", f"network error: {exc}"


def _http_request_with_retry(
    *,
    url: str,
    method: str,
    headers: dict[str, str],
    body: bytes | None,
    timeout: float,
    retry_attempts: int,
    retry_backoff: float,
) -> tuple[int, bytes]:
    last_exc: Exception | None = None
    for attempt in range(1, retry_attempts + 1):
        try:
            return _http_request(
                url=url,
                method=method,
                headers=headers,
                body=body,
                timeout=timeout,
            )
        except error.URLError as exc:
            last_exc = exc
            if attempt >= retry_attempts:
                break
            if retry_backoff > 0:
                time.sleep(retry_backoff * (2 ** (attempt - 1)))

    if last_exc is not None:
        raise last_exc
    raise RuntimeError("request failed without a captured exception")


_UNLIMITED_TEXT_MARKERS = (
    "unlimited",
    "no limit",
    "no-limit",
    "without limit",
    "limitless",
    "不限额",
    "无限额",
    "无限制",
)
_UNLIMITED_KEY_HINTS = ("unlimited", "no_limit", "nolimit", "limitless")
_LIMIT_LIKE_KEY_HINTS = ("quota", "limit", "cap")
_QUOTA_EXCEEDED_TEXT_MARKERS = (
    "usage_limit_reached",
    "usage limit has been reached",
    "quota exceeded",
    "limit exceeded",
    "超出配额",
    "额度已用完",
)


def _looks_unlimited_from_response(status_code: int | None, response_text: str) -> bool:
    if status_code is None or status_code < 200 or status_code >= 300:
        return False

    lowered = (response_text or "").lower()
    if any(marker in lowered for marker in _UNLIMITED_TEXT_MARKERS):
        return True

    try:
        parsed = json.loads(response_text)
    except json.JSONDecodeError:
        return False

    stack: list[Any] = [parsed]
    while stack:
        current = stack.pop()
        if isinstance(current, dict):
            for key, value in current.items():
                key_lc = str(key).lower()
                if any(hint in key_lc for hint in _UNLIMITED_KEY_HINTS):
                    if isinstance(value, bool) and value:
                        return True
                    if isinstance(value, str) and value.strip().lower() in {
                        "1",
                        "true",
                        "yes",
                        "unlimited",
                        "no_limit",
                        "nolimit",
                    }:
                        return True
                    if isinstance(value, (int, float)) and value == -1:
                        return True
                if any(hint in key_lc for hint in _LIMIT_LIKE_KEY_HINTS):
                    if value is None:
                        return True
                    if isinstance(value, (int, float)) and (value == -1 or value >= 9999):
                        return True
                    if isinstance(value, str) and value.strip().lower() in {
                        "none",
                        "null",
                        "unlimited",
                        "no limit",
                        "no-limit",
                        "无限",
                        "不限额",
                        "无限额",
                    }:
                        return True
                if isinstance(value, (dict, list)):
                    stack.append(value)
                elif isinstance(value, str):
                    text_value = value.lower()
                    if any(marker in text_value for marker in _UNLIMITED_TEXT_MARKERS):
                        return True
        elif isinstance(current, list):
            stack.extend(current)

    return False


def _detect_quota_exceeded(response_text: str) -> tuple[bool, int | None]:
    if not response_text:
        return False, None

    try:
        parsed = json.loads(response_text)
    except json.JSONDecodeError:
        parsed = None

    if isinstance(parsed, dict):
        err = parsed.get("error")
        if isinstance(err, dict) and err.get("type") == "usage_limit_reached":
            resets_at = err.get("resets_at")
            if isinstance(resets_at, (int, float)):
                return True, int(resets_at)
            return True, None

    lowered = response_text.lower()
    if any(marker in lowered for marker in _QUOTA_EXCEEDED_TEXT_MARKERS):
        return True, None

    return False, None


def _refresh_access_token(refresh_url: str, refresh_token: str, timeout: float) -> tuple[str, str]:
    body = parse.urlencode(
        {
            "client_id": DEFAULT_CLIENT_ID,
            "grant_type": "refresh_token",
            "refresh_token": refresh_token,
            "scope": "openid profile email",
        }
    ).encode("utf-8")

    status, resp_body = _http_request(
        url=refresh_url,
        method="POST",
        headers={
            "Content-Type": "application/x-www-form-urlencoded",
            "Accept": "application/json",
        },
        body=body,
        timeout=timeout,
    )

    if status != 200:
        msg = resp_body.decode("utf-8", errors="replace")[:300]
        raise RuntimeError(f"refresh failed with {status}: {msg}")

    parsed = json.loads(resp_body.decode("utf-8", errors="replace"))
    new_token = _first_non_empty_str([parsed.get("access_token")])
    new_refresh = _first_non_empty_str([parsed.get("refresh_token")])
    if not new_token:
        raise RuntimeError("refresh succeeded but access_token missing")
    return new_token, new_refresh


def _try_refresh_token(
    refresh_token: str,
    refresh_url: str,
    timeout: float,
    retry_attempts: int,
    retry_backoff: float,
) -> tuple[str, RefreshedTokenData | None, str]:
    if not refresh_token:
        return "token_invalid", None, "refresh_token is empty"

    last_error = ""
    for attempt in range(1, retry_attempts + 1):
        try:
            with _RECOVERY_SEMAPHORE:
                body = parse.urlencode(
                    {
                        "grant_type": "refresh_token",
                        "client_id": DEFAULT_CLIENT_ID,
                        "refresh_token": refresh_token,
                        "scope": "openid profile email",
                    }
                ).encode("utf-8")
                status, resp_body = _http_request(
                    url=refresh_url,
                    method="POST",
                    headers={
                        "Content-Type": "application/x-www-form-urlencoded",
                        "Accept": "application/json",
                        "User-Agent": _random_browser_user_agent(),
                    },
                    body=body,
                    timeout=timeout,
                )

            text = resp_body.decode("utf-8", errors="replace")
            parsed: dict[str, Any] = {}
            try:
                parsed = json.loads(text) if text else {}
            except json.JSONDecodeError:
                parsed = {}

            if status == 200:
                access_token = _first_non_empty_str([parsed.get("access_token")])
                new_refresh = _first_non_empty_str([parsed.get("refresh_token"), refresh_token])
                if not access_token:
                    return "error", None, "refresh succeeded but access_token missing"
                id_token = _first_non_empty_str([parsed.get("id_token")])
                claims = _jwt_claims_no_verify(id_token)
                auth_claims = claims.get("https://api.openai.com/auth") if isinstance(claims, dict) else {}
                account_id = ""
                if isinstance(auth_claims, dict):
                    account_id = _first_non_empty_str([auth_claims.get("chatgpt_account_id")])
                email = _first_non_empty_str([claims.get("email")]) if isinstance(claims, dict) else ""
                refreshed = RefreshedTokenData(
                    access_token=access_token,
                    refresh_token=new_refresh,
                    id_token=id_token,
                    email=email,
                    account_id=account_id,
                    expired=_future_rfc3339(parsed.get("expires_in")),
                )
                return "alive", refreshed, ""

            error_code = str(parsed.get("error", ""))
            error_desc = str(parsed.get("error_description", ""))
            full_error = f"{error_code}: {error_desc} (HTTP {status})".strip()
            if _contains_deleted_keyword(text) or _contains_deleted_keyword(full_error):
                return "deleted", None, full_error or text[:200]
            if error_code == "invalid_grant":
                return "token_invalid", None, full_error or "invalid_grant"
            last_error = full_error or text[:200] or f"HTTP {status}"
        except Exception as exc:  # noqa: BLE001
            last_error = str(exc)

        if attempt < retry_attempts:
            time.sleep(retry_backoff * (2 ** (attempt - 1)))

    return "error", None, last_error or "refresh failed"


def _check_access_token(
    access_token: str,
    timeout: float,
    retry_attempts: int,
    retry_backoff: float,
) -> tuple[str, str]:
    if not access_token:
        return "expired", "access_token is empty"

    last_error = ""
    for attempt in range(1, retry_attempts + 1):
        try:
            with _RECOVERY_SEMAPHORE:
                status, resp_body = _http_request(
                    url=DEFAULT_MODELS_URL,
                    method="GET",
                    headers={
                        "Authorization": f"Bearer {access_token}",
                        "Accept": "application/json",
                        "User-Agent": _random_browser_user_agent(),
                    },
                    body=None,
                    timeout=timeout,
                )
            text = resp_body.decode("utf-8", errors="replace")

            if status == 200:
                return "alive", ""
            if status == 401:
                if _contains_deleted_keyword(text):
                    return "deleted", f"HTTP 401: {text[:200]}"
                return "expired", "HTTP 401: token expired or invalid"
            if status == 403:
                lowered = text.lower()
                if _contains_deleted_keyword(text):
                    return "deleted", f"HTTP 403: {text[:200]}"
                if "insufficient permissions" in lowered or "missing scopes" in lowered:
                    return "alive", ""
                if "country" in lowered or "unsupported" in lowered:
                    return "geo_blocked", "HTTP 403: geo blocked"
                return "expired", f"HTTP 403: {text[:200]}"

            last_error = f"HTTP {status}: {text[:200]}"
        except Exception as exc:  # noqa: BLE001
            last_error = str(exc)

        if attempt < retry_attempts:
            time.sleep(retry_backoff * (2 ** (attempt - 1)))

    return "error", last_error or "access token check failed"


def _build_probe_headers(access_token: str, account_id: str) -> dict[str, str]:
    headers = {
        "Authorization": f"Bearer {access_token}",
        "Content-Type": "application/json",
        "Accept": "application/json",
        "Version": DEFAULT_VERSION,
        "Openai-Beta": "responses=experimental",
        "User-Agent": DEFAULT_USER_AGENT,
        "Originator": "codex_cli_rs",
    }
    if account_id:
        headers["Chatgpt-Account-Id"] = account_id
    return headers


def _build_probe_body(model: str) -> bytes:
    payload = {
        "model": model,
        "stream": True,
        "store": False,
        "instructions": "",
        "input": [
            {
                "role": "user",
                "content": [{"type": "input_text", "text": "ping"}],
            }
        ],
    }
    return json.dumps(payload, ensure_ascii=False).encode("utf-8")


def _load_json(path: Path) -> dict[str, Any]:
    raw = path.read_text(encoding="utf-8-sig")
    obj = json.loads(raw)
    if not isinstance(obj, dict):
        raise ValueError("root JSON value is not an object")
    return obj


def _write_json_atomic(path: Path, payload: dict[str, Any]) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    temp_path = path.with_name(f".{path.name}.tmp-{os.getpid()}-{time.time_ns()}")
    try:
        temp_path.write_text(
            json.dumps(payload, ensure_ascii=False, separators=(",", ":")),
            encoding="utf-8",
        )
        os.replace(temp_path, path)
    finally:
        if temp_path.exists():
            temp_path.unlink()


def _write_refreshed_auth_file(
    path: Path,
    payload: dict[str, Any],
    refreshed: RefreshedTokenData,
    fields: dict[str, str],
) -> None:
    updated = dict(payload)
    updated["type"] = updated.get("type") or fields.get("provider") or "codex"
    updated["access_token"] = refreshed.access_token
    updated["refresh_token"] = refreshed.refresh_token or fields.get("refresh_token") or ""
    if refreshed.id_token:
        updated["id_token"] = refreshed.id_token
    if refreshed.account_id or fields.get("account_id"):
        updated["account_id"] = refreshed.account_id or fields.get("account_id") or ""
    if refreshed.email or fields.get("email"):
        updated["email"] = refreshed.email or fields.get("email") or ""
    if refreshed.expired:
        updated["expired"] = refreshed.expired
    updated["last_refresh"] = _utc_now_rfc3339()
    _write_json_atomic(path, updated)


def _scan_single_file(path: Path, args: argparse.Namespace) -> list[CheckResult]:
    try:
        payload = _load_json(path)
    except Exception as exc:  # noqa: BLE001
        return [
            CheckResult(
                file=str(path),
                provider="unknown",
                email="",
                account_id="",
                status_code=None,
                unauthorized_401=False,
                no_limit_unlimited=False,
                quota_exceeded=False,
                quota_resets_at=None,
                error=f"parse error: {exc}",
                response_preview="",
            )
        ]

    if not _looks_like_codex(path, payload):
        return []

    fields = _extract_auth_fields(payload)
    access_token = fields["access_token"]
    refresh_token = fields["refresh_token"]
    prefetched_refresh: RefreshedTokenData | None = None
    prefetched_refresh_error = ""

    if args.refresh_before_check and refresh_token:
        refresh_status, refreshed_data, refresh_error = _try_refresh_token(
            refresh_token=refresh_token,
            refresh_url=args.refresh_url,
            timeout=args.timeout,
            retry_attempts=args.retry_attempts,
            retry_backoff=args.retry_backoff,
        )
        prefetched_refresh_error = refresh_error
        if refresh_status == "alive" and refreshed_data:
            access_token = refreshed_data.access_token
            prefetched_refresh = refreshed_data
        elif refresh_status == "deleted":
            return [
                CheckResult(
                    file=str(path),
                    provider=fields["provider"],
                    email=fields["email"],
                    account_id=fields["account_id"],
                    status_code=401,
                    unauthorized_401=True,
                    no_limit_unlimited=False,
                    quota_exceeded=False,
                    quota_resets_at=None,
                    error=refresh_error or "refresh reports deleted",
                    response_preview="",
                    delete_invalid=True,
                )
            ]

    if not access_token:
        return [
            CheckResult(
                file=str(path),
                provider=fields["provider"],
                email=fields["email"],
                account_id=fields["account_id"],
                status_code=None,
                unauthorized_401=False,
                no_limit_unlimited=False,
                quota_exceeded=False,
                quota_resets_at=None,
                error="missing access token",
                response_preview="",
            )
        ]

    base_url = fields["base_url"] or args.base_url
    probe_url = base_url.rstrip("/") + "/" + args.quota_path.lstrip("/")
    headers = _build_probe_headers(access_token, fields["account_id"])
    headers["User-Agent"] = _random_browser_user_agent()
    body = _build_probe_body(args.model)
    status, response_text, request_error = _probe_once(
        url=probe_url,
        headers=headers,
        body=body,
        timeout=args.timeout,
        retry_attempts=args.retry_attempts,
        retry_backoff=args.retry_backoff,
    )
    preview = response_text[:300]

    if status == 401:
        refreshed_data = None
        refresh_status = ""
        refresh_error = ""
        if refresh_token:
            refresh_status, refreshed_data, refresh_error = _try_refresh_token(
                refresh_token=refresh_token,
                refresh_url=args.refresh_url,
                timeout=args.timeout,
                retry_attempts=args.retry_attempts,
                retry_backoff=args.retry_backoff,
            )
            if refresh_status == "alive" and refreshed_data:
                verify_status, verify_error = _check_access_token(
                    refreshed_data.access_token,
                    timeout=args.timeout,
                    retry_attempts=args.retry_attempts,
                    retry_backoff=args.retry_backoff,
                )
                if verify_status in {"alive", "geo_blocked"}:
                    _write_refreshed_auth_file(path, payload, refreshed_data, fields)
                    retry_headers = _build_probe_headers(refreshed_data.access_token, fields["account_id"])
                    retry_headers["User-Agent"] = _random_browser_user_agent()
                    retry_status, retry_text, retry_error = _probe_once(
                        url=probe_url,
                        headers=retry_headers,
                        body=body,
                        timeout=args.timeout,
                        retry_attempts=args.retry_attempts,
                        retry_backoff=args.retry_backoff,
                    )
                    if retry_status is not None and retry_status != 401:
                        quota_exceeded, resets_at = _detect_quota_exceeded(retry_text)
                        return [
                            CheckResult(
                                file=str(path),
                                provider=fields["provider"],
                                email=fields["email"],
                                account_id=fields["account_id"],
                                status_code=retry_status,
                                unauthorized_401=False,
                                no_limit_unlimited=_looks_unlimited_from_response(retry_status, retry_text),
                                quota_exceeded=quota_exceeded,
                                quota_resets_at=resets_at,
                                error=retry_error,
                                response_preview=retry_text[:300],
                                delete_invalid=False,
                            )
                        ]
                elif verify_status == "deleted":
                    return [
                        CheckResult(
                            file=str(path),
                            provider=fields["provider"],
                            email=fields["email"],
                            account_id=fields["account_id"],
                            status_code=401,
                            unauthorized_401=True,
                            no_limit_unlimited=False,
                            quota_exceeded=False,
                            quota_resets_at=None,
                            error=verify_error or refresh_error or "confirmed deleted after refresh",
                            response_preview=preview,
                            delete_invalid=True,
                        )
                    ]
            elif refresh_status == "deleted":
                return [
                    CheckResult(
                        file=str(path),
                        provider=fields["provider"],
                        email=fields["email"],
                        account_id=fields["account_id"],
                        status_code=401,
                        unauthorized_401=True,
                        no_limit_unlimited=False,
                        quota_exceeded=False,
                        quota_resets_at=None,
                        error=refresh_error or "refresh reports deleted",
                        response_preview=preview,
                        delete_invalid=True,
                    )
                ]

        verify_status, verify_error = _check_access_token(
            access_token,
            timeout=args.timeout,
            retry_attempts=args.retry_attempts,
            retry_backoff=args.retry_backoff,
        )
        if verify_status == "alive":
            return [
                CheckResult(
                    file=str(path),
                    provider=fields["provider"],
                    email=fields["email"],
                    account_id=fields["account_id"],
                    status_code=200,
                    unauthorized_401=False,
                    no_limit_unlimited=False,
                    quota_exceeded=False,
                    quota_resets_at=None,
                    error="access_token still valid after refresh failure",
                    response_preview=preview,
                    delete_invalid=False,
                )
            ]
        if verify_status == "deleted":
            return [
                CheckResult(
                    file=str(path),
                    provider=fields["provider"],
                    email=fields["email"],
                    account_id=fields["account_id"],
                    status_code=401,
                    unauthorized_401=True,
                    no_limit_unlimited=False,
                    quota_exceeded=False,
                    quota_resets_at=None,
                    error=verify_error or "confirmed deleted",
                    response_preview=preview,
                    delete_invalid=True,
                )
            ]
        return [
            CheckResult(
                file=str(path),
                provider=fields["provider"],
                email=fields["email"],
                account_id=fields["account_id"],
                status_code=401,
                unauthorized_401=True,
                no_limit_unlimited=False,
                quota_exceeded=False,
                quota_resets_at=None,
                error=refresh_error or verify_error or "401 requires review",
                response_preview=preview,
                delete_invalid=False,
            )
        ]

    if status is None:
        return [
            CheckResult(
                file=str(path),
                provider=fields["provider"],
                email=fields["email"],
                account_id=fields["account_id"],
                status_code=None,
                unauthorized_401=False,
                no_limit_unlimited=False,
                quota_exceeded=False,
                quota_resets_at=None,
                error=request_error,
                response_preview="",
            )
        ]

    quota_exceeded, resets_at = _detect_quota_exceeded(response_text)
    if prefetched_refresh is not None and status is not None and status != 401:
        _write_refreshed_auth_file(path, payload, prefetched_refresh, fields)
    return [
        CheckResult(
            file=str(path),
            provider=fields["provider"],
            email=fields["email"],
            account_id=fields["account_id"],
            status_code=status,
            unauthorized_401=(status == 401),
            no_limit_unlimited=_looks_unlimited_from_response(status, response_text),
            quota_exceeded=quota_exceeded,
            quota_resets_at=resets_at,
            error=request_error,
            response_preview=preview,
        )
    ]


def _scan_dir_flat(
    dir_path: Path,
    args: argparse.Namespace,
    progress_callback: Callable[[int, int, Path], None] | None = None,
) -> list[CheckResult]:
    if not dir_path.exists() or not dir_path.is_dir():
        return []

    json_files = sorted(dir_path.glob("*.json"))
    total = len(json_files)
    if total == 0:
        return []

    workers = min(args.workers, total)
    indexed_results: list[tuple[int, list[CheckResult]]] = []
    if workers <= 1:
        for index, path in enumerate(json_files, start=1):
            if progress_callback is not None:
                progress_callback(index, total, path)
            file_results = _scan_single_file(path, args)
            if file_results:
                indexed_results.append((index, file_results))
    else:
        with ThreadPoolExecutor(max_workers=workers) as pool:
            future_map = {
                pool.submit(_scan_single_file, path, args): (index, path)
                for index, path in enumerate(json_files, start=1)
            }
            completed = 0
            for future in as_completed(future_map):
                completed += 1
                index, path = future_map[future]
                if progress_callback is not None:
                    progress_callback(completed, total, path)
                try:
                    file_results = future.result()
                except Exception as exc:  # noqa: BLE001
                    file_results = [
                        CheckResult(
                            file=str(path),
                            provider="unknown",
                            email="",
                            account_id="",
                            status_code=None,
                            unauthorized_401=False,
                            no_limit_unlimited=False,
                            quota_exceeded=False,
                            quota_resets_at=None,
                            error=f"internal error: {exc}",
                            response_preview="",
                        )
                    ]
                if file_results:
                    indexed_results.append((index, file_results))

    indexed_results.sort(key=lambda item: item[0])
    return [row for _, group in indexed_results for row in group]


def _scan_dir_flat_from_paths(
    paths: list[Path],
    args: argparse.Namespace,
    progress_callback: Callable[[int, int, Path], None] | None = None,
) -> list[CheckResult]:
    total = len(paths)
    if total == 0:
        return []

    workers = min(args.workers, total)
    indexed_results: list[tuple[int, list[CheckResult]]] = []
    if workers <= 1:
        for index, path in enumerate(paths, start=1):
            if progress_callback is not None:
                progress_callback(index, total, path)
            file_results = _scan_single_file(path, args)
            if file_results:
                indexed_results.append((index, file_results))
    else:
        with ThreadPoolExecutor(max_workers=workers) as pool:
            future_map = {
                pool.submit(_scan_single_file, path, args): (index, path)
                for index, path in enumerate(paths, start=1)
            }
            completed = 0
            for future in as_completed(future_map):
                completed += 1
                index, path = future_map[future]
                if progress_callback is not None:
                    progress_callback(completed, total, path)
                try:
                    file_results = future.result()
                except Exception as exc:  # noqa: BLE001
                    file_results = [
                        CheckResult(
                            file=str(path),
                            provider="unknown",
                            email="",
                            account_id="",
                            status_code=None,
                            unauthorized_401=False,
                            no_limit_unlimited=False,
                            quota_exceeded=False,
                            quota_resets_at=None,
                            error=f"internal error: {exc}",
                            response_preview="",
                        )
                    ]
                if file_results:
                    indexed_results.append((index, file_results))

    indexed_results.sort(key=lambda item: item[0])
    return [row for _, group in indexed_results for row in group]


def scan_auth_files(
    args: argparse.Namespace,
    progress_callback: Callable[[int, int, Path], None] | None = None,
) -> list[CheckResult]:
    auth_dir = Path(args.auth_dir).expanduser().resolve()
    if not auth_dir.exists() or not auth_dir.is_dir():
        raise FileNotFoundError(f"auth directory not found: {auth_dir}")
    return _scan_dir_flat(auth_dir, args, progress_callback=progress_callback)


def _status_label(item: CheckResult, use_color: bool) -> str:
    if item.unauthorized_401:
        return _paint("401", ANSI_BOLD, ANSI_RED, enabled=use_color)
    if item.quota_exceeded:
        return _paint("LIM", ANSI_BOLD, ANSI_MAGENTA, enabled=use_color)
    if item.status_code is None:
        return _paint("ERR", ANSI_BOLD, ANSI_YELLOW, enabled=use_color)
    code = str(item.status_code)
    if 200 <= item.status_code < 300:
        return _paint(code, ANSI_GREEN, enabled=use_color)
    if 400 <= item.status_code < 500:
        return _paint(code, ANSI_YELLOW, enabled=use_color)
    if item.status_code >= 500:
        return _paint(code, ANSI_RED, enabled=use_color)
    return code


def _print_table(results: list[CheckResult], use_color: bool) -> None:
    if not results:
        print(_paint("No codex auth files found.", ANSI_YELLOW, enabled=use_color))
        return

    unauthorized = [r for r in results if r.unauthorized_401]
    quota_exceeded_list = [r for r in results if r.quota_exceeded and not r.unauthorized_401]
    unlimited = [r for r in results if r.no_limit_unlimited]
    ok_count = sum(1 for item in results if item.status_code is not None and 200 <= item.status_code < 300)
    failed_count = len(results) - ok_count

    print(_paint("Scan Summary", ANSI_BOLD, ANSI_CYAN, enabled=use_color))
    print(f"  checked codex files : {len(results)}")
    print(f"  unauthorized (401)  : {len(unauthorized)}")
    print(f"  quota-exceeded      : {len(quota_exceeded_list)}")
    print(f"  no-limit/unlimited  : {len(unlimited)}")
    print(f"  non-2xx or errors   : {failed_count}")
    print()

    if unauthorized:
        print(_paint("401 Files", ANSI_BOLD, ANSI_RED, enabled=use_color))
        for item in unauthorized:
            email = f" ({item.email})" if item.email else ""
            print(f"  [{_status_label(item, use_color)}] {item.file}{email}")
        print()

    if quota_exceeded_list:
        print(_paint("Quota-Exceeded Files", ANSI_BOLD, ANSI_MAGENTA, enabled=use_color))
        for item in quota_exceeded_list:
            email = f" ({item.email})" if item.email else ""
            suffix = ""
            if item.quota_resets_at is not None:
                import datetime as _dt

                resets_str = _dt.datetime.fromtimestamp(item.quota_resets_at, tz=_dt.timezone.utc).strftime("%Y-%m-%d %H:%M UTC")
                suffix = f" [resets {resets_str}]"
            print(f"  [{_status_label(item, use_color)}] {item.file}{email}{suffix}")
        print()

    others = [
        r
        for r in results
        if (not r.unauthorized_401)
        and (not r.quota_exceeded)
        and (not r.no_limit_unlimited)
    ]
    if unlimited:
        print(_paint("No-limit/Unlimited Files", ANSI_BOLD, ANSI_GREEN, enabled=use_color))
        for item in unlimited:
            email = f" ({item.email})" if item.email else ""
            print(f"  [{_status_label(item, use_color)}] {item.file}{email}")
        print()

    if others:
        print(_paint("Other Results", ANSI_BOLD, ANSI_CYAN, enabled=use_color))
        for item in others:
            reason = item.error or item.response_preview.replace("\n", " ")[:120]
            reason = reason.strip() or "-"
            print(f"  [{_status_label(item, use_color)}] {item.file} :: {_truncate(reason, 120)}")


def _move_file_safely(src: Path, dst_dir: Path) -> tuple[str | None, str | None]:
    try:
        dst_dir.mkdir(parents=True, exist_ok=True)
        dst = dst_dir / src.name
        counter = 1
        while dst.exists():
            dst = dst_dir / f"{src.stem}_{counter}{src.suffix}"
            counter += 1
        shutil.move(str(src), str(dst))
        return str(dst), None
    except Exception as exc:  # noqa: BLE001
        return None, str(exc)


def _delete_files(paths: list[str]) -> tuple[list[str], list[FileOpError]]:
    deleted: list[str] = []
    errors: list[FileOpError] = []
    seen: set[str] = set()
    for raw_path in paths:
        path = Path(raw_path)
        normalized = str(path.resolve())
        if normalized in seen:
            continue
        seen.add(normalized)
        try:
            path.unlink()
            deleted.append(str(path))
        except Exception as exc:  # noqa: BLE001
            errors.append(FileOpError(file=str(path), error=str(exc)))
    return deleted, errors


def _build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        description="Scan active Codex auth files, delete invalid tokens, quarantine quota-exhausted files, and restore recovered files from limit/."
    )
    parser.add_argument("--auth-dir", default=DEFAULT_AUTH_DIR, help=f"Auth directory (default: {DEFAULT_AUTH_DIR})")
    parser.add_argument("--base-url", default=DEFAULT_CODEX_BASE_URL, help=f"Codex base URL (default: {DEFAULT_CODEX_BASE_URL})")
    parser.add_argument("--quota-path", default="/responses", help="API path used for auth/quota probe (default: /responses)")
    parser.add_argument("--model", default="gpt-5", help="Model used in probe request body (default: gpt-5)")
    parser.add_argument("--timeout", type=float, default=20, help="HTTP timeout in seconds (default: 20)")
    parser.add_argument("--workers", type=int, default=DEFAULT_WORKERS, help=f"Concurrent workers (default: {DEFAULT_WORKERS})")
    parser.add_argument("--retry-attempts", type=int, default=DEFAULT_RETRY_ATTEMPTS, help=f"Retry attempts for network errors (default: {DEFAULT_RETRY_ATTEMPTS})")
    parser.add_argument("--retry-backoff", type=float, default=DEFAULT_RETRY_BACKOFF, help=f"Base seconds for exponential retry backoff (default: {DEFAULT_RETRY_BACKOFF})")
    parser.add_argument("--refresh-before-check", action="store_true", help="Refresh access token with refresh_token before probing.")
    parser.add_argument("--refresh-url", default=DEFAULT_REFRESH_URL, help=f"Token refresh endpoint (default: {DEFAULT_REFRESH_URL})")
    parser.add_argument("--output-json", action="store_true", help="Print full results as JSON.")
    parser.add_argument("--no-progress", action="store_true", help="Disable live scan progress.")
    parser.add_argument("--no-color", action="store_true", help="Disable ANSI color output.")
    parser.add_argument("--keep-401", action="store_true", help="Do not delete HTTP 401 auth files.")
    parser.add_argument("--exceeded-dir", default=None, help="Directory used to store quota-exceeded auth files (default: auth-dir/limit)")
    parser.add_argument("--no-quarantine", action="store_true", help="Disable quota quarantine and recovery scan.")
    return parser


def main() -> int:
    parser = _build_parser()
    args = parser.parse_args()
    if args.workers < 1:
        parser.error("--workers must be >= 1")
    if args.retry_attempts < 1:
        parser.error("--retry-attempts must be >= 1")
    if args.retry_backoff < 0:
        parser.error("--retry-backoff must be >= 0")

    use_color = _supports_color(args.no_color) and (not args.output_json)
    progress_enabled = _is_tty_stdout() and (not args.no_progress) and (not args.output_json)
    progress = _ProgressDisplay(progress_enabled)

    auth_dir = Path(args.auth_dir).expanduser().resolve()
    exceeded_dir = Path(args.exceeded_dir).expanduser().resolve() if args.exceeded_dir else auth_dir / DEFAULT_EXCEEDED_DIR_NAME

    active_targets, limit_targets, duplicate_targets = _build_scan_plan_with_duplicates(auth_dir, exceeded_dir)

    if progress_enabled:
        print(_paint("Scanning active auth JSON files...", ANSI_DIM, enabled=use_color))

    try:
        results = _scan_dir_flat_from_paths(active_targets, args, progress_callback=progress.update if progress_enabled else None)
    except Exception as exc:  # noqa: BLE001
        progress.finish()
        print(f"Error: {exc}", file=sys.stderr)
        return 2
    progress.finish()

    deleted_duplicates, duplicate_delete_errors = _delete_files([str(path) for path in duplicate_targets])

    moved_to_exceeded: list[str] = []
    move_to_exceeded_errors: list[FileOpError] = []
    if not args.no_quarantine:
        for item in results:
            if item.quota_exceeded:
                dst, err = _move_file_safely(Path(item.file), exceeded_dir)
                if err:
                    move_to_exceeded_errors.append(FileOpError(file=item.file, error=err))
                elif dst is not None:
                    moved_to_exceeded.append(dst)

    exceeded_results: list[CheckResult] = []
    moved_from_exceeded: list[str] = []
    move_from_exceeded_errors: list[FileOpError] = []
    if not args.no_quarantine and exceeded_dir.exists():
        if progress_enabled:
            print(_paint(f"Scanning limit dir: {exceeded_dir} ...", ANSI_DIM, enabled=use_color))
        exceeded_results = _scan_dir_flat_from_paths(limit_targets, args)
        for item in exceeded_results:
            recovered = (not item.quota_exceeded) and item.status_code is not None and 200 <= item.status_code < 300
            if recovered:
                dst, err = _move_file_safely(Path(item.file), auth_dir)
                if err:
                    move_from_exceeded_errors.append(FileOpError(file=item.file, error=err))
                elif dst is not None:
                    moved_from_exceeded.append(dst)

    unauthorized_files = [item.file for item in results + exceeded_results if item.delete_invalid]
    deleted_files: list[str] = []
    delete_errors: list[FileOpError] = []
    if unauthorized_files and not args.keep_401:
        deleted_files, delete_errors = _delete_files(unauthorized_files)

    if args.output_json:
        print(
            json.dumps(
                {
                    "results": [asdict(item) for item in results],
                    "limit_dir_results": [asdict(item) for item in exceeded_results],
                    "dedupe": {
                        "duplicate_candidates": [str(path) for path in duplicate_targets],
                        "deleted_duplicates": deleted_duplicates,
                        "errors": [asdict(item) for item in duplicate_delete_errors],
                    },
                    "quarantine": {
                        "enabled": not args.no_quarantine,
                        "limit_dir": str(exceeded_dir),
                        "moved_to_limit": moved_to_exceeded,
                        "move_to_limit_errors": [asdict(item) for item in move_to_exceeded_errors],
                        "moved_from_limit": moved_from_exceeded,
                        "move_from_limit_errors": [asdict(item) for item in move_from_exceeded_errors],
                    },
                    "deletion": {
                        "enabled": not args.keep_401,
                        "deleted_files": deleted_files,
                        "errors": [asdict(item) for item in delete_errors],
                    },
                },
                ensure_ascii=False,
                indent=2,
            )
        )
    else:
        _print_table(results, use_color=use_color)

        if duplicate_targets or duplicate_delete_errors:
            print()
            print(_paint("Duplicate Cleanup", ANSI_BOLD, ANSI_CYAN, enabled=use_color))
            print(f"  duplicate candidates : {len(duplicate_targets)}")
            print(f"  deleted duplicates   : {len(deleted_duplicates)}")
            for path in deleted_duplicates:
                print(f"  [{_paint('deleted', ANSI_GREEN, enabled=use_color)}] {path}")
            for item in duplicate_delete_errors:
                print(f"  [{_paint('delete-failed', ANSI_RED, enabled=use_color)}] {item.file} :: {item.error}")

        if exceeded_results:
            print(_paint(f"Limit Dir Scan ({exceeded_dir})", ANSI_BOLD, ANSI_MAGENTA, enabled=use_color))
            _print_table(exceeded_results, use_color=use_color)

        if moved_to_exceeded or move_to_exceeded_errors:
            print()
            print(_paint("Quarantine Moves (auth-dir -> limit)", ANSI_BOLD, ANSI_MAGENTA, enabled=use_color))
            for dst in moved_to_exceeded:
                print(f"  [{_paint('moved', ANSI_MAGENTA, enabled=use_color)}] -> {dst}")
            for item in move_to_exceeded_errors:
                print(f"  [{_paint('move-failed', ANSI_RED, enabled=use_color)}] {item.file} :: {item.error}")

        if moved_from_exceeded or move_from_exceeded_errors:
            print()
            print(_paint("Recovery Moves (limit -> auth-dir)", ANSI_BOLD, ANSI_GREEN, enabled=use_color))
            for dst in moved_from_exceeded:
                print(f"  [{_paint('recovered', ANSI_GREEN, enabled=use_color)}] -> {dst}")
            for item in move_from_exceeded_errors:
                print(f"  [{_paint('recover-failed', ANSI_RED, enabled=use_color)}] {item.file} :: {item.error}")

        if unauthorized_files:
            print()
            if args.keep_401:
                print(_paint("401 files detected; keeping them because --keep-401 is set.", ANSI_YELLOW, enabled=use_color))
            else:
                print(_paint(f"Deleted {len(deleted_files)}/{len(unauthorized_files)} invalid (401) auth files.", ANSI_BOLD, ANSI_GREEN, enabled=use_color))
                for path in deleted_files:
                    print(f"  [{_paint('deleted', ANSI_GREEN, enabled=use_color)}] {path}")
                for item in delete_errors:
                    print(f"  [{_paint('delete-failed', ANSI_RED, enabled=use_color)}] {item.file} :: {item.error}")

    has_401 = any(item.unauthorized_401 for item in results + exceeded_results)
    return 1 if has_401 else 0


if __name__ == "__main__":
    raise SystemExit(main())
