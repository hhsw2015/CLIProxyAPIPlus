"""Shared Vertex probe primitives.

Both `gen_llm_config_v2.py` and `vertex_watch.py` need to:
  1. Mint a GCP OAuth token from a base64 SA JSON blob.
  2. POST `messages` to `aiplatform.googleapis.com/.../publishers/anthropic/...`
     and read back (status_code, body).

Historically each script grew its own implementation. This module is the
single source of truth. Key design choices:

  * Uses `curl` via subprocess, not the `requests`/`urllib` Python stack.
    Reason: on macOS, Python's HTTPS to googleapis.com routinely takes 6-30s
    (SSL context init + certifi trust store quirks), while system `curl` on
    the same host consistently finishes in ~2s. curl gives deterministic
    latency for our fast-response watcher.
  * PyJWT signs the JWT bearer assertion locally (no network), then curl
    exchanges it at token_uri. Zero google-auth dependency.
  * Timeouts are per-call, all bounded. No implicit retries — callers decide.
"""

from __future__ import annotations

import base64
import json
import subprocess
import time
import urllib.parse as _urlparse
from typing import Any


def mint_gcp_token(creds_b64: str, timeout: int = 10) -> str:
    """Exchange a base64-encoded service-account JSON for an access_token.

    Signs a JWT bearer assertion locally with PyJWT (no network round-trip
    beyond token exchange) and swaps it via curl at the SA's token_uri.

    Args:
        creds_b64: base64-encoded service-account JSON string.
        timeout: seconds to allow for the curl token exchange.

    Raises:
        RuntimeError: on curl failure or malformed token response.
    """
    import jwt as _jwt

    sa = json.loads(base64.b64decode(creds_b64))
    now = int(time.time())
    assertion = _jwt.encode(
        {
            "iss": sa["client_email"],
            "scope": "https://www.googleapis.com/auth/cloud-platform",
            "aud": sa["token_uri"],
            "iat": now,
            "exp": now + 3600,
        },
        sa["private_key"],
        algorithm="RS256",
    )
    body = _urlparse.urlencode(
        {
            "grant_type": "urn:ietf:params:oauth:grant-type:jwt-bearer",
            "assertion": assertion,
        }
    )
    result = subprocess.run(
        [
            "curl",
            "-sS",
            "--max-time",
            str(timeout),
            "-X",
            "POST",
            "-H",
            "Content-Type: application/x-www-form-urlencoded",
            "--data",
            body,
            sa["token_uri"],
        ],
        capture_output=True,
        text=True,
        timeout=timeout + 2,
    )
    if result.returncode != 0:
        raise RuntimeError(f"curl token exchange failed: {result.stderr[:200]}")
    resp = json.loads(result.stdout)
    if "access_token" not in resp:
        raise RuntimeError(f"token response missing access_token: {resp}")
    return resp["access_token"]


def build_vertex_url(project: str, region: str, model: str, method: str = "rawPredict") -> str:
    """Build a Vertex Anthropic model URL.

    Global endpoint uses the region-less host; per-region endpoints use the
    `<region>-aiplatform.googleapis.com` host.
    """
    if region == "global":
        return (
            f"https://aiplatform.googleapis.com/v1/projects/{project}"
            f"/locations/global/publishers/anthropic/models/{model}:{method}"
        )
    return (
        f"https://{region}-aiplatform.googleapis.com/v1/projects/{project}"
        f"/locations/{region}/publishers/anthropic/models/{model}:{method}"
    )


def probe_vertex_model(
    project: str,
    region: str,
    model: str,
    token: str,
    timeout: int = 10,
    body: dict[str, Any] | None = None,
) -> tuple[int, str]:
    """POST a minimal `messages` request to Vertex; return (http_code, body).

    Args:
        project: GCP project id (e.g. "cla-06").
        region:  "global" or a Vertex region (e.g. "us-east5").
        model:   Anthropic model id (e.g. "claude-opus-4-7").
        token:   Access token from mint_gcp_token().
        timeout: seconds for the whole curl POST.
        body:    Optional request body override. Defaults to a 1-token probe.

    Returns:
        (http_code, response_body_text). http_code=0 signals curl-level failure.
    """
    url = build_vertex_url(project, region, model, method="rawPredict")
    payload = body or {
        "anthropic_version": "vertex-2023-10-16",
        "max_tokens": 1,
        "messages": [{"role": "user", "content": "hi"}],
    }
    result = subprocess.run(
        [
            "curl",
            "-sS",
            "--max-time",
            str(timeout),
            "-o",
            "/dev/stdout",
            "-w",
            "\n__HTTP__%{http_code}",
            "-X",
            "POST",
            "-H",
            f"Authorization: Bearer {token}",
            "-H",
            "Content-Type: application/json",
            "--data",
            json.dumps(payload),
            url,
        ],
        capture_output=True,
        text=True,
        timeout=timeout + 2,
    )
    if result.returncode != 0:
        return 0, f"curl error: {result.stderr[:200]}"
    out = result.stdout
    marker = "\n__HTTP__"
    idx = out.rfind(marker)
    if idx == -1:
        return 0, f"missing http_code marker: {out[:200]}"
    return int(out[idx + len(marker) :].strip()), out[:idx]


def classify_probe_result(http_code: int, body: str) -> str:
    """Categorise a probe response into an action-worthy status.

    Returns one of:
        "live"    — 2xx, model is callable
        "quota"   — 429, model exists but rate-limited (still selectable later)
        "banned"  — 498 or "access has been disabled" text
        "missing" — 404, SKU not activated on this (project, model)
        "error"   — anything else (5xx, curl failure, unexpected 4xx)
    """
    body_lc = body.lower() if body else ""
    if http_code == 498 or "access has been disabled" in body_lc:
        return "banned"
    if 200 <= http_code < 300:
        return "live"
    if http_code == 429:
        return "quota"
    if http_code == 404:
        return "missing"
    return "error"
