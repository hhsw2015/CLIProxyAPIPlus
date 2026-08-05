#!/usr/bin/env python3
"""Multi-provider credential probe with persistent state + blacklist.

Purpose: keep the CPA config in sync with reality, not just with what the
SKYROUTER upstream currently lists. Upstream retirement != AWS/Azure/etc
revocation — historical snapshots often contain AKs that still work.
Conversely, current snapshots sometimes contain entries the vendor has
already revoked.

Workflow:
  1. Walk every dated snapshot under artifacts/api-keys/us-nacos/ and
     collect a de-duplicated inventory of (provider, credential, endpoint,
     model) combos, keeping the newest secret per credential.
  2. Merge with the persistent probe_state.json:
       - blacklist entries (probe_state.status == "revoked" or dead_count
         >= 3) are skipped entirely — this is the speed win as history
         grows.
       - entries probed recently (< STALE_HOURS) are also skipped.
  3. Probe everything else in parallel per provider, obeying that
     provider's rate limits.
  4. Persist the new state so subsequent runs stay fast.

Output: probe_state.json holds the source of truth.  A separate command
      (--emit-additions) prints the combos that are alive today but
      missing from the current SKYROUTER snapshot — those are the ones
      gen_llm_config_v2.py should add back into the config.

Providers supported: bedrock, azure, anthropic, openai-compat, vertex.
Adding a new provider = one Probe subclass; the driver takes it from there.
"""
from __future__ import annotations

import argparse
import concurrent.futures as _cf
import datetime as _dt
import hashlib
import json
import os
import re
import sys
import time
from dataclasses import dataclass, field, asdict
from pathlib import Path
from typing import Any, Callable, Iterable

# --------------------------------------------------------------------------- config

ARTIFACTS_ROOT = Path("/Users/wowdd1/Dev/dvina-2api/artifacts/api-keys/us-nacos")
STATE_FILE = Path(__file__).with_name("probe_state.json")
WATERMARK_FILE = Path(__file__).with_name("probe_state.watermark")  # dates already fully processed
STALE_HOURS = 24 * 6                     # re-probe surviving entries every 6 days
BLACKLIST_STALE_DAYS = 30                # retry blacklisted entries once a month
DEFAULT_MAX_WORKERS = 64                 # global fan-out; per-provider capped below
DEFAULT_TIMEOUT = 8                      # per-request; 1-token payloads are tiny


# --------------------------------------------------------------------------- shared types

@dataclass
class Combo:
    """One credential+endpoint+model triple to probe."""
    provider: str                          # "bedrock" | "azure" | "anthropic" | "openai_compat" | ...
    key: str                               # unique combo id, used as state key
    payload: dict[str, Any]                # anything the probe needs: ak/sk/api_key/base_url/model/arn/region
    first_seen: str = ""                   # yyyy-mm-dd


@dataclass
class ProbeResult:
    status: str                            # "ok" | "revoked" | "forbidden" | "invalid_arg" | "throttle" | "network" | "unknown"
    http_status: int = 0
    detail: str = ""


@dataclass
class StateEntry:
    status: str = "unknown"
    http_status: int = 0
    detail: str = ""
    last_probe: str = ""                   # ISO
    last_ok: str = ""                      # ISO
    dead_count: int = 0
    first_seen: str = ""
    payload: dict[str, Any] = field(default_factory=dict)

    @property
    def blacklisted(self) -> bool:
        return self.dead_count >= 3 or self.status == "revoked"


# --------------------------------------------------------------------------- state persistence

def load_state() -> dict[str, StateEntry]:
    if not STATE_FILE.exists():
        return {}
    raw = json.loads(STATE_FILE.read_text())
    return {k: StateEntry(**v) for k, v in raw.items()}


def save_state(state: dict[str, StateEntry]) -> None:
    STATE_FILE.write_text(json.dumps({k: asdict(v) for k, v in state.items()}, indent=2, sort_keys=True))


def load_watermark() -> str:
    """Highest snapshot date already fully collected+probed. Empty on first run."""
    if not WATERMARK_FILE.exists():
        return ""
    return WATERMARK_FILE.read_text().strip()


def save_watermark(date_tag: str) -> None:
    WATERMARK_FILE.write_text(date_tag.strip())


def iso_now() -> str:
    return _dt.datetime.utcnow().replace(microsecond=0).isoformat() + "Z"


def parse_iso(s: str) -> _dt.datetime | None:
    if not s:
        return None
    try:
        return _dt.datetime.fromisoformat(s.rstrip("Z"))
    except Exception:
        return None


# --------------------------------------------------------------------------- Combo collection

def _classify_claude_conf_provider(section: str) -> str:
    # Legacy keys-claude.json sections map to providers by convention.
    if any(section.startswith(p) for p in ("Aws", "Xiamen", "Taixing", "Lewanyun", "Zhongxiang", "Jinwang")):
        return "bedrock"
    if section.startswith("Vertex"):
        return "vertex"
    return "anthropic"                     # everything else in keys-claude.json is an API key


# Well-known provider sections in config_us_k8s.json and how to probe them.
# Each entry defines how to synthesize probe Combos from that section.
# fields:
#   base:       fixed base URL (or None if it lives in `base_field`)
#   base_field: name of the field on the section that holds the base URL
#   models:     list of test-model strings
#   key:        key_field  OR  a list of key_fields when the section has
#               several keys (e.g. Woyaochat has ClaudeKey/AzureKey/OpenaiKey)
#   provider:   optional forced provider ("anthropic" | "openai_compat")
_K8S_PROVIDERS: dict[str, dict[str, Any]] = {
    # provider_section: base_url, models[], key_field
    "Bingxing":    {"base": "https://llmapi.paratera.com/v1", "models": ["claude-opus-4-6", "claude-sonnet-4-5"], "key": "Key"},
    "Biaobei":     {"base": None, "models": ["claude-opus-4-6"], "key": "Key", "base_field": "ApiBase"},
    "Guoke":       {"base": "https://api.ddaibb2.com/v1", "models": ["claude-opus-4-6", "claude-sonnet-4-5"], "key": "Key"},
    "YouBangGPT":  {"base": "https://gptapi.youbangai.com/v1", "models": ["gpt-4o-mini"], "key": "Key"},
    "Silicon":     {"base": "https://api.siliconflow.cn/v1", "models": ["deepseek-ai/DeepSeek-V3", "Qwen/Qwen2.5-7B-Instruct"], "key": "Key"},
    "Kluster":     {"base": "https://api.kluster.ai/v1", "models": ["meta-llama/Meta-Llama-3.1-8B-Instruct-Turbo"], "key": "Key"},
    "Shubiaobiao": {"base": "https://api.shubiaobiao.cn/v1", "models": ["claude-sonnet-4-5"], "key": "Key"},
    "DeepInfra":   {"base": "https://api.deepinfra.com/v1/openai", "models": ["meta-llama/Meta-Llama-3.1-8B-Instruct"], "key": "Key"},
    "Nebius":      {"base": "https://api.studio.nebius.ai/v1", "models": ["meta-llama/Meta-Llama-3.1-8B-Instruct"], "key": "Key"},
    "Aliyun":      {"base": "https://dashscope.aliyuncs.com/compatible-mode/v1", "models": ["qwen-turbo"], "key": "Key"},
    "AliyunRDS":   {"base": "https://dashscope.aliyuncs.com/compatible-mode/v1", "models": ["qwen-turbo"], "key": "Key"},
    "Cloudsway":   {"base": "https://api.cloudsway.net/v1", "models": ["gpt-4o-mini"], "key": "Key"},
    "Sophnet":     {"base": "https://www.sophnet.com/api/open-apis/v1", "models": ["deepseek-v3"], "key": "Key"},
    "MiniMax":     {"base": "https://api.minimax.chat/v1", "models": ["MiniMax-M1"], "key": "Key"},
    "Taijia":      {"base": "https://api.taijiaicloud.com", "models": ["claude-opus-4-6"], "key": "Key"},
    "JDCloud":     {"base": "https://gpt.jdcloud.com/v1", "models": ["gpt-4o-mini"], "key": "Key"},
    "ApiCoco":     {"base": "https://api.apicoco.io/v1", "models": ["claude-opus-4-6"], "key": "Key"},
    "Polo":        {"base": "https://api.poloai.top/v1", "models": ["gpt-4o-mini"], "key": "Key"},
    "YesVG":       {"base": "https://api.yes.vg/v1", "models": ["gpt-4o-mini"], "key": "Key"},
    "Gradient":    {"base": "https://api.gradient.ai/v1", "models": ["llama3-8b"], "key": "Key"},
    "Cerebras":    {"base": "https://api.cerebras.ai/v1", "models": ["llama3.1-8b"], "key": "Key"},
    "Xmind":       {"base": None, "models": ["gpt-4o-mini"], "key": "Key", "base_field": "BaseUrl"},
}

# Multi-key sections: one section carries multiple independent credentials
# routed to different upstreams. Each entry maps key-field name -> probe spec.
_K8S_MULTIKEY_PROVIDERS: dict[str, dict[str, dict[str, Any]]] = {
    "Woyaochat": {
        # These are all sk-xxx keys pointing at Woyaochat's own gateway.
        "ClaudeKey":  {"base": "https://api.woyaochat.com/v1", "models": ["claude-opus-4-5"],  "provider": "anthropic"},
        "AzureKey":   {"base": "https://api.woyaochat.com/v1", "models": ["gpt-4o-mini"],     "provider": "openai_compat"},
        "GeminiKey":  {"base": "https://api.woyaochat.com/v1", "models": ["gemini-2.5-flash"], "provider": "openai_compat"},
        "OpenaiKey":  {"base": "https://api.woyaochat.com/v1", "models": ["gpt-4o-mini"],     "provider": "openai_compat"},
    },
    "XP": {
        "ClaudeKey":     {"base": "https://api.taijiaicloud.com",  "models": ["claude-opus-4-6"],   "provider": "anthropic"},
        "ClaudeKey_OLD": {"base": "https://api.taijiaicloud.com",  "models": ["claude-opus-4-6"],   "provider": "anthropic"},
        "GeminiKey":     {"base": "https://api.taijiaicloud.com/v1","models": ["gemini-2.5-flash"], "provider": "openai_compat"},
    },
}


def _combos_from_multikey_section(section: str, node: dict[str, Any], date_tag: str) -> list[Combo]:
    """Emit combos for sections that carry multiple independent keys."""
    spec_map = _K8S_MULTIKEY_PROVIDERS.get(section) or {}
    out: list[Combo] = []
    for key_field, spec in spec_map.items():
        api_key = (node.get(key_field) or "").strip()
        if not api_key:
            continue
        base = (spec.get("base") or "").rstrip("/")
        prov = spec.get("provider") or "openai_compat"
        for model in spec.get("models") or []:
            out.append(Combo(
                provider=prov,
                key=f"{prov}:{api_key}:{base}:{model}",
                payload={"api_key": api_key, "base_url": base, "model": model,
                         "section": section, "key_field": key_field},
                first_seen=date_tag,
            ))
    return out


def _combos_from_k8s_config(path: Path, date_tag: str) -> list[Combo]:
    """Extract Anthropic/OpenAI-compat API-key combos from config_us_k8s.json."""
    try:
        data = json.loads(path.read_text())
    except Exception:
        return []
    out: list[Combo] = []
    for section, spec in _K8S_PROVIDERS.items():
        node = data.get(section) or {}
        if not isinstance(node, dict):
            continue
        api_key = (node.get(spec["key"]) or "").strip()
        if not api_key:
            continue
        base = spec.get("base")
        if base is None:
            bf = spec.get("base_field")
            if bf:
                base = (node.get(bf) or "").strip()
        base = (base or "").rstrip("/")
        if not base:
            continue
        for model in spec.get("models") or []:
            # Anthropic-style APIs get an anthropic Combo; everything else is
            # OpenAI-compatible chat completions. Heuristic: base URL contains
            # "anthropic" or is Taijia/Guoke/Biaobei/Bingxing/Shubiaobiao.
            if section in ("Taijia", "Guoke", "Biaobei", "Bingxing", "Shubiaobiao", "ApiCoco"):
                out.append(Combo(
                    provider="anthropic",
                    key=f"anthropic:{api_key}:{base}:{model}",
                    payload={"api_key": api_key, "base_url": base, "model": model, "section": section},
                    first_seen=date_tag,
                ))
            else:
                out.append(Combo(
                    provider="openai_compat",
                    key=f"openai_compat:{api_key}:{base}:{model}",
                    payload={"api_key": api_key, "base_url": base, "model": model, "section": section},
                    first_seen=date_tag,
                ))
    # Also walk multi-key sections
    for mk_section in _K8S_MULTIKEY_PROVIDERS:
        node = data.get(mk_section) or {}
        if isinstance(node, dict) and node:
            out.extend(_combos_from_multikey_section(mk_section, node, date_tag))
    # Vertex service accounts: config_us_k8s.json / other sources sometimes
    # embed a full service_account JSON under GcpConf / VertexConf / similar.
    for vertex_section in ("GcpConf", "VertexConf", "Vertex", "GcpVertex"):
        node = data.get(vertex_section) or {}
        if not isinstance(node, dict):
            continue
        # Look for a JSON credential blob (b64 or inline). Common field names:
        for f in ("credentials_b64", "vertex_credentials_b64", "ServiceAccount", "credentials"):
            blob = node.get(f)
            if not blob:
                continue
            project = (node.get("project_id") or node.get("Project") or "").strip()
            key_id = f"vertex_sa:{vertex_section}:{_sa_fingerprint(blob)}"
            out.append(Combo(
                provider="vertex",
                key=key_id,
                payload={"sa_blob": blob, "project": project, "section": vertex_section},
                first_seen=date_tag,
            ))
            break
    return out


def collect_historical_combos(since: str = "") -> list[Combo]:
    """Walk every dated snapshot and yield all Combo entries.

    Deduplication key: provider + credential-id + endpoint + model.
    We keep the NEWEST secret (some keys were rotated in place).

    If `since` is set, snapshots on or before that date are skipped for
    ONBOARDING new combos — combos they contain that don't appear later
    are already reflected in probe_state.json. The most recent snapshot
    is ALWAYS included (never watermark-skipped) so the caller can force
    re-probing of anything the upstream still lists today.
    """
    all_dates = sorted(d.name for d in ARTIFACTS_ROOT.iterdir() if d.is_dir())
    newest_date = all_dates[-1] if all_dates else ""

    latest: dict[str, Combo] = {}
    for date_dir in sorted(ARTIFACTS_ROOT.iterdir()):
        if not date_dir.is_dir():
            continue
        date_tag = date_dir.name
        # Always include the newest snapshot (that's what "still-in-latest"
        # detection depends on). Only skip strictly-older snapshots when a
        # watermark has been set.
        if since and date_tag <= since and date_tag != newest_date:
            continue
        # Legacy sections (older shape)
        for fname, kind_hint in [("keys-claude.json", "claude"),
                                 ("keys-azure.json",  "azure"),
                                 ("keys-other.json",  "other")]:
            fpath = date_dir / fname
            if not fpath.exists():
                continue
            try:
                data = json.loads(fpath.read_text())
            except Exception:
                continue
            for section, entries in (data or {}).items():
                if not isinstance(entries, dict):
                    continue
                for model, arr in entries.items():
                    if not isinstance(arr, list):
                        continue
                    for e in arr:
                        if not isinstance(e, dict):
                            continue
                        combo = _combo_from_legacy(kind_hint, section, model, e, date_tag)
                        if combo is None:
                            continue
                        prev = latest.get(combo.key)
                        if prev is None or prev.first_seen <= combo.first_seen:
                            latest[combo.key] = combo
        # config_us_k8s.json (Docker-image embedded API keys per provider)
        for k8s_name in ("config_us_k8s.json", "config_bj_k8s.json"):
            k8s_path = date_dir / k8s_name
            if not k8s_path.exists():
                continue
            for combo in _combos_from_k8s_config(k8s_path, date_tag):
                prev = latest.get(combo.key)
                if prev is None or prev.first_seen <= combo.first_seen:
                    latest[combo.key] = combo
        # SKYROUTER new-shape channel files
        for fname in ("SKYROUTER_channels-claude.json",
                      "SKYROUTER_channels-azure.json",
                      "SKYROUTER_channels-other.json"):
            fpath = date_dir / fname
            if not fpath.exists():
                continue
            try:
                data = json.loads(fpath.read_text())
            except Exception:
                continue
            for channel in (data.get("channels") or []):
                for combo in _combos_from_skyrouter_channel(channel, date_tag):
                    prev = latest.get(combo.key)
                    if prev is None or prev.first_seen <= combo.first_seen:
                        latest[combo.key] = combo
    return list(latest.values())


def _combos_from_skyrouter_channel(channel: dict[str, Any], date_tag: str) -> list[Combo]:
    """Convert a SKYROUTER channel object to Combos.

    SKYROUTER schema (uniform across all channel types):
      channel = {name, type, config: {nodes: [ ... ]}}
      Each node exposes credentials + a models map. Credential fields vary
      by channel type but the outer structure is stable, so one iteration
      handles bedrock / azure / anthropic_proxy / vertex / everything.
    """
    ctype = (channel.get("type") or "").strip().lower()
    if not ctype:
        return []
    out: list[Combo] = []
    nodes = (channel.get("config") or {}).get("nodes") or []
    if isinstance(nodes, dict):
        nodes = [nodes]
    for node in nodes:
        if not isinstance(node, dict):
            continue
        models = node.get("models") or {}
        if not isinstance(models, dict):
            continue
        for model_name, minfo in models.items():
            model_name = str(model_name).strip()
            if not model_name:
                continue
            if ctype == "anthropic_bedrock":
                aws = node.get("aws") or {}
                ak = (aws.get("access_key_id") or "").strip()
                sk = (aws.get("secret_access_key") or "").strip()
                region = (node.get("region") or "").strip()
                arn = ""
                if isinstance(minfo, dict):
                    arn = (minfo.get("arn") or "").strip()
                if not (ak.startswith("AKIA") and sk and region and arn):
                    continue
                out.append(Combo(
                    provider="bedrock",
                    key=f"bedrock:{ak}:{region}:{arn}",
                    payload={"ak": ak, "sk": sk, "region": region, "arn": arn, "model": model_name},
                    first_seen=date_tag,
                ))
                continue
            if ctype == "openai_azure":
                api_key = (node.get("api_key") or "").strip()
                base = (node.get("base_url") or "").strip()
                if not (api_key and base and isinstance(minfo, dict)):
                    continue
                deployment = (minfo.get("deployment") or model_name).strip()
                api_version = (minfo.get("api_version") or "2024-08-01-preview").strip()
                if _is_non_chat_media_model(model_name, deployment):
                    continue
                out.append(Combo(
                    provider="azure",
                    key=f"azure:{api_key}:{base}:{deployment}",
                    payload={"api_key": api_key, "endpoint": base, "deployment": deployment,
                             "api_version": api_version, "model": model_name},
                    first_seen=date_tag,
                ))
                continue
            if ctype in ("anthropic_proxy", "anthropic_direct"):
                api_key = (node.get("api_key") or "").strip()
                base = (node.get("base_url") or "https://api.anthropic.com").strip()
                if not api_key:
                    continue
                out.append(Combo(
                    provider="anthropic",
                    key=f"anthropic:{api_key}:{base}:{model_name}",
                    payload={"api_key": api_key, "base_url": base, "model": model_name},
                    first_seen=date_tag,
                ))
                continue
            if ctype in ("anthropic_vertex", "google_vertex"):
                # Vertex channels carry the SA JSON blob at channel.config
                # level, not per-node, so we only need to emit ONE probe per
                # channel (the token exchange is credential-scoped, not
                # model-scoped). We still tag the model on the combo so
                # emit-additions can rebuild the (auth, model) matrix.
                blob = (channel.get("config") or {}).get("vertex_credentials_b64") \
                    or (channel.get("config") or {}).get("credentials_b64") \
                    or (channel.get("config") or {}).get("service_account_json")
                if not blob:
                    continue
                project = (node.get("project") or "").strip()
                key_id = f"vertex_sa:{channel.get('name','')}:{project}:{_sa_fingerprint(blob)}"
                out.append(Combo(
                    provider="vertex",
                    key=key_id,
                    payload={"sa_blob": blob, "project": project, "channel": channel.get("name",""),
                             "model": model_name, "channel_type": ctype},
                    first_seen=date_tag,
                ))
                continue
            # Everything else — deepseek, zhipu, volcengine, minimax, aliyun,
            # openrouter, fal, google_direct — is a plain OpenAI-compatible
            # bearer-token endpoint. Base URL per-channel is fixed by CPA's
            # generator; here we only need the api_key + model for probing.
            api_key = (node.get("api_key") or "").strip()
            if not api_key:
                continue
            base = _skyrouter_default_base_url(ctype, node)
            if not base:
                continue
            out.append(Combo(
                provider="openai_compat",
                key=f"openai_compat:{api_key}:{base}:{model_name}",
                payload={"api_key": api_key, "base_url": base, "model": model_name, "channel_type": ctype},
                first_seen=date_tag,
            ))
    return out


# Well-known base URLs for OpenAI-compatible SKYROUTER channel types. If a
# node explicitly overrides base_url we use that; otherwise we fall back to
# the vendor's public endpoint so the probe can actually reach the service.
_SKYROUTER_DEFAULT_BASES = {
    "deepseek":       "https://api.deepseek.com",
    "zhipu":          "https://open.bigmodel.cn/api/paas/v4",
    "volcengine":     "https://ark.cn-beijing.volces.com/api/v3",
    "minimax":        "https://api.minimax.chat/v1",
    "aliyun":         "https://dashscope.aliyuncs.com/compatible-mode/v1",
    "openrouter":     "https://openrouter.ai/api/v1",
    "fal":            "https://fal.run",
    "google_direct":  "https://generativelanguage.googleapis.com/v1beta/openai",
}


_MEDIA_MODEL_NEEDLES = (
    "image", "sora", "tts", "whisper", "audio", "veo", "kling",
    "imagen", "dall-e", "gpt-image", "seedream", "seedance",
    "wan", "kolors", "jimeng", "pixverse", "vidu", "minimax-video",
    "suno", "apicoco", "skyreels", "gemini-image",
)


def _sa_fingerprint(blob) -> str:
    """Deterministic short fingerprint for a service-account blob.

    Python's built-in hash() is salted per-process (PYTHONHASHSEED), so it
    produces a different value each run — that would orphan vertex probe
    state on every invocation. SHA256 is stable across runs and platforms.
    """
    if isinstance(blob, bytes):
        data = blob
    else:
        data = str(blob).encode("utf-8", errors="replace")
    return hashlib.sha256(data).hexdigest()[:16]


def _is_non_chat_media_model(*names: str) -> bool:
    """Return True if the model looks like an image/video/tts model — those
    can't be probed with a chat/completions body, so probing them produces
    false-negative "invalid_arg" verdicts and would let the gen-config filter
    delete perfectly usable media keys."""
    for n in names:
        low = (n or "").lower()
        if any(needle in low for needle in _MEDIA_MODEL_NEEDLES):
            return True
    return False


def _skyrouter_default_base_url(ctype: str, node: dict[str, Any]) -> str:
    override = (node.get("base_url") or "").strip()
    if override:
        return override
    return _SKYROUTER_DEFAULT_BASES.get(ctype, "")


def _combo_from_legacy(kind: str, section: str, model: str, e: dict[str, Any], date_tag: str) -> Combo | None:
    if kind == "claude":
        prov = _classify_claude_conf_provider(section)
        if prov == "bedrock":
            ak = (e.get("Ak") or "").strip()
            sk = (e.get("Sk") or "").strip()
            region = (e.get("Region") or "").strip()
            arn = (e.get("ModelId") or "").strip()
            if not (ak.startswith("AKIA") and sk and region and arn):
                return None
            # probe_bedrock() sends an Anthropic-shaped body; using it against
            # Nova / Titan / non-Anthropic ARNs returns "malformed request" and
            # misleads the classifier. Skip those. The credential itself will
            # still be validated via any other Claude combo that shares the AK.
            low = f"{arn} {model}".lower()
            if "anthropic" not in low and "claude" not in low:
                return None
            return Combo(
                provider="bedrock",
                key=f"bedrock:{ak}:{region}:{arn}",
                payload={"ak": ak, "sk": sk, "region": region, "arn": arn, "model": model},
                first_seen=date_tag,
            )
        if prov == "vertex":
            # Vertex probing needs GCP credentials JSON; historical snapshots
            # rarely include the JSON blob itself. Skip for now.
            return None
        # Anthropic API key
        api_key = (e.get("Key") or e.get("ApiKey") or e.get("APIKey") or "").strip()
        base = (e.get("BaseURL") or e.get("BaseUrl") or "").strip() or "https://api.anthropic.com"
        if not api_key or not api_key.startswith("sk-"):
            return None
        return Combo(
            provider="anthropic",
            key=f"anthropic:{api_key}:{base}:{model}",
            payload={"api_key": api_key, "base_url": base, "model": model},
            first_seen=date_tag,
        )
    if kind == "azure":
        api_key = (e.get("Key") or e.get("ApiKey") or "").strip()
        endpoint = (e.get("Endpoint") or e.get("Api") or "").strip()
        deployment = (e.get("Deployment") or e.get("DeploymentName") or model or "").strip()
        api_version = (e.get("ApiVersion") or "").strip() or "2024-08-01-preview"
        if not (api_key and endpoint):
            return None
        # Skip non-chat-completion model families. probe_azure() uses a
        # /chat/completions POST which returns 400 "missing prompt" for
        # image/video/tts models — that's a body-format mismatch, NOT proof
        # that the credential is unusable. If we let those false negatives
        # into probe_state.json, the gen-config filter would drop hundreds
        # of perfectly good image/tts keys. Media probing needs per-family
        # endpoints (images/generations, audio/speech, video/generations)
        # which we don't cover yet; better to leave these out entirely.
        if _is_non_chat_media_model(model, deployment):
            return None
        return Combo(
            provider="azure",
            key=f"azure:{api_key}:{endpoint}:{deployment}",
            payload={"api_key": api_key, "endpoint": endpoint, "deployment": deployment,
                     "api_version": api_version, "model": model},
            first_seen=date_tag,
        )
    if kind == "other":
        api_key = (e.get("Key") or e.get("ApiKey") or "").strip()
        base = (e.get("BaseURL") or e.get("BaseUrl") or "").strip()
        if not (api_key and base):
            return None
        return Combo(
            provider="openai_compat",
            key=f"openai_compat:{api_key}:{base}:{model}",
            payload={"api_key": api_key, "base_url": base, "model": model, "section": section},
            first_seen=date_tag,
        )
    return None


# --------------------------------------------------------------------------- probes

def _http_status(exc: Exception) -> int:
    # Try to pull an int status out of common shapes
    resp = getattr(exc, "response", None)
    if resp is not None:
        # botocore
        try:
            status = resp.get("ResponseMetadata", {}).get("HTTPStatusCode", 0)
            if status:
                return int(status)
        except Exception:
            pass
        # requests
        code = getattr(resp, "status_code", None)
        if code:
            return int(code)
    return 0


def _classify_http_error(status: int, body: str) -> str:
    low = body.lower()
    if status == 200:
        return "ok"
    if status in (401, 403):
        if "unrecognized" in low or "invalid_api_key" in low or "invalid api key" in low or "invalid key" in low:
            return "revoked"
        if "not found" in low or "no such" in low or "deploymentnotfound" in low:
            # e.g. Azure deployment removed — the credential itself is fine
            return "invalid_arg"
        return "forbidden"
    if status == 404:
        return "invalid_arg"
    if status in (400, 422):
        return "invalid_arg"
    if status == 429:
        return "throttle"
    if status >= 500:
        return "network"
    return "unknown"


def probe_bedrock(combo: Combo, timeout: int) -> ProbeResult:
    import boto3
    from botocore.exceptions import ClientError, EndpointConnectionError
    from botocore.config import Config

    p = combo.payload
    body = json.dumps({
        "anthropic_version": "bedrock-2023-05-31",
        "max_tokens": 1,
        "messages": [{"role": "user", "content": "hi"}],
    })
    try:
        client = boto3.client(
            "bedrock-runtime",
            aws_access_key_id=p["ak"], aws_secret_access_key=p["sk"],
            region_name=p["region"],
            config=Config(connect_timeout=timeout, read_timeout=timeout, retries={"max_attempts": 1}),
        )
        client.invoke_model(modelId=p["arn"], body=body, contentType="application/json")
        return ProbeResult(status="ok", http_status=200)
    except ClientError as exc:
        code = (exc.response.get("Error", {}) or {}).get("Code", "")
        msg = (exc.response.get("Error", {}) or {}).get("Message", "")
        http = _http_status(exc)
        if code == "UnrecognizedClientException":
            return ProbeResult(status="revoked", http_status=http or 403, detail=msg[:200])
        if code == "AccessDeniedException":
            if "explicit deny in a service control policy" in msg.lower():
                return ProbeResult(status="forbidden", http_status=http or 403, detail="SCP")
            return ProbeResult(status="forbidden", http_status=http or 403, detail=msg[:200])
        if code == "ResourceNotFoundException":
            return ProbeResult(status="invalid_arg", http_status=http or 404, detail=msg[:200])
        if code == "ValidationException":
            return ProbeResult(status="invalid_arg", http_status=http or 400, detail=msg[:200])
        if code == "ThrottlingException":
            return ProbeResult(status="throttle", http_status=http or 429, detail=msg[:200])
        return ProbeResult(status="unknown", http_status=http, detail=f"{code}:{msg[:120]}")
    except EndpointConnectionError as exc:
        return ProbeResult(status="network", detail=str(exc)[:200])
    except Exception as exc:
        return ProbeResult(status="unknown", detail=str(exc)[:200])


def probe_azure(combo: Combo, timeout: int) -> ProbeResult:
    """Azure endpoint may arrive as either the root ("https://x.openai.azure.com")
    or a fully-baked URL ("https://x/openai/deployments/gpt-4.1-mini/chat/completions?api-version=...").
    We detect which and only append what's missing so we don't double-suffix.
    """
    import requests
    p = combo.payload
    endpoint = p["endpoint"].rstrip("/")
    deployment = p["deployment"]
    api_version = p.get("api_version") or "2024-08-01-preview"
    if "/chat/completions" in endpoint or "/openai/deployments/" in endpoint:
        # Endpoint already includes deployment + path; use verbatim, just
        # ensure api-version is present.
        url = endpoint
        if "api-version=" not in url:
            sep = "&" if "?" in url else "?"
            url = f"{url}{sep}api-version={api_version}"
    else:
        url = f"{endpoint}/openai/deployments/{deployment}/chat/completions?api-version={api_version}"
    headers = {"api-key": p["api_key"], "Content-Type": "application/json"}
    body = {"messages": [{"role": "user", "content": "hi"}], "max_tokens": 1}
    try:
        r = requests.post(url, headers=headers, json=body, timeout=timeout)
        cls = _classify_http_error(r.status_code, r.text)
        return ProbeResult(status=cls, http_status=r.status_code, detail=r.text[:200])
    except requests.RequestException as exc:
        return ProbeResult(status="network", detail=str(exc)[:200])


def probe_anthropic(combo: Combo, timeout: int) -> ProbeResult:
    import requests
    p = combo.payload
    url = _join_openai_url(p["base_url"], "/v1/messages")
    headers = {
        "x-api-key": p["api_key"], "anthropic-version": "2023-06-01",
        "Content-Type": "application/json",
    }
    body = {
        "model": p["model"], "max_tokens": 1,
        "messages": [{"role": "user", "content": "hi"}],
    }
    try:
        r = requests.post(url, headers=headers, json=body, timeout=timeout)
        cls = _classify_http_error(r.status_code, r.text)
        return ProbeResult(status=cls, http_status=r.status_code, detail=r.text[:200])
    except requests.RequestException as exc:
        return ProbeResult(status="network", detail=str(exc)[:200])


def _join_openai_url(base: str, path: str) -> str:
    """Append path to base without producing /v1/v1/ or /v4/v1/ style duplicates.

    - base already ends in /v1 or /vN (any digit tail): append path minus leading /vN
    - base ends in /chat/completions: return as-is (path baked into base)
    - otherwise: append /v1 + path
    """
    base = base.rstrip("/")
    if "/chat/completions" in base:
        return base
    # If the path starts with /vN and base already ends with /vN (matching or not),
    # strip the prefix from path so we don't stack version segments.
    m = re.search(r"/v\d[a-z0-9]*$", base)
    if m and path.startswith("/v"):
        # strip leading /vN/ segment from path
        path = re.sub(r"^/v\d[a-z0-9]*", "", path)
        if not path.startswith("/"):
            path = "/" + path
        return base + path
    # No version tail on base: add default /v1
    if not path.startswith("/"):
        path = "/" + path
    return base + path


def probe_openai_compat(combo: Combo, timeout: int) -> ProbeResult:
    import requests
    p = combo.payload
    url = _join_openai_url(p["base_url"], "/v1/chat/completions")
    headers = {"Authorization": f"Bearer {p['api_key']}", "Content-Type": "application/json"}
    body = {"model": p["model"], "messages": [{"role": "user", "content": "hi"}], "max_tokens": 1}
    try:
        r = requests.post(url, headers=headers, json=body, timeout=timeout)
        cls = _classify_http_error(r.status_code, r.text)
        return ProbeResult(status=cls, http_status=r.status_code, detail=r.text[:200])
    except requests.RequestException as exc:
        return ProbeResult(status="network", detail=str(exc)[:200])


def probe_vertex(combo: Combo, timeout: int) -> ProbeResult:
    """Exchange the service-account JSON for a short-lived OAuth token.

    We stop at the token exchange — the credential is valid if IAM issues a
    token. Calling predict/generate would consume quota unnecessarily. If
    the exchange itself fails with 401/403 the service account is revoked
    or its key rotated."""
    p = combo.payload
    blob = p.get("sa_blob") or ""
    if not blob:
        return ProbeResult(status="invalid_arg", detail="no sa_blob")
    try:
        import base64
        text = blob
        if isinstance(blob, str) and not blob.strip().startswith("{"):
            # Assume base64
            try:
                text = base64.b64decode(blob).decode("utf-8", errors="replace")
            except Exception as exc:
                return ProbeResult(status="invalid_arg", detail=f"base64: {exc}")
        info = json.loads(text)
    except Exception as exc:
        return ProbeResult(status="invalid_arg", detail=f"parse sa: {exc}")
    try:
        from google.oauth2 import service_account
        from google.auth.transport.requests import Request as GRequest
    except ImportError:
        # google-auth not installed; treat as unable-to-probe rather than dead.
        return ProbeResult(status="unknown", detail="google-auth missing")
    try:
        creds = service_account.Credentials.from_service_account_info(
            info, scopes=["https://www.googleapis.com/auth/cloud-platform"])
        creds.refresh(GRequest())
        if creds.token:
            return ProbeResult(status="ok", http_status=200)
        return ProbeResult(status="revoked", http_status=401, detail="no token")
    except Exception as exc:
        msg = str(exc)[:200]
        low = msg.lower()
        if "invalid_grant" in low or "invalid grant" in low or "invalid_client" in low:
            return ProbeResult(status="revoked", http_status=401, detail=msg)
        if "unauthorized" in low or "permission_denied" in low or "permission denied" in low:
            return ProbeResult(status="forbidden", http_status=403, detail=msg)
        return ProbeResult(status="network", detail=msg)


PROBES: dict[str, Callable[[Combo, int], ProbeResult]] = {
    "bedrock": probe_bedrock,
    "azure": probe_azure,
    "anthropic": probe_anthropic,
    "openai_compat": probe_openai_compat,
    "vertex": probe_vertex,
}


# --------------------------------------------------------------------------- driver

def should_probe(entry: StateEntry, now: _dt.datetime, still_in_latest: bool = False) -> bool:
    # Rule: whenever the upstream still lists a combo in the latest snapshot,
    # re-probe it. Upstream keeping it around implies they believe it should
    # work; our stale verdict may no longer reflect reality.
    if still_in_latest:
        return True
    if entry.blacklisted:
        # Retry once a month to catch resurrections (rare but possible).
        last = parse_iso(entry.last_probe)
        return not last or (now - last).days >= BLACKLIST_STALE_DAYS
    # Indeterminate outcomes (network, unknown, throttle) are re-probed every
    # run — we haven't actually confirmed the credential yet. Only definite
    # verdicts (ok, revoked, forbidden, invalid_arg) get the 6-day cache.
    if entry.status not in ("ok", "revoked", "forbidden", "invalid_arg"):
        return True
    last_ok = parse_iso(entry.last_ok)
    last_probe = parse_iso(entry.last_probe)
    if last_ok and (now - last_ok).total_seconds() < STALE_HOURS * 3600:
        return False
    if last_probe and (now - last_probe).total_seconds() < STALE_HOURS * 3600:
        return False
    return True


def merge_result(entry: StateEntry, combo: Combo, result: ProbeResult) -> StateEntry:
    # Categorize the outcome:
    #   ok / revoked / forbidden  -> definite; overwrite status
    #   invalid_arg               -> credential is likely fine but (model, arn,
    #                                deployment) is wrong; do NOT overwrite a
    #                                previous ok/forbidden verdict
    #   network / throttle / unknown -> transient; leave prior status intact
    entry.http_status = result.http_status
    entry.detail = result.detail
    entry.last_probe = iso_now()
    if combo.first_seen and (not entry.first_seen or entry.first_seen > combo.first_seen):
        entry.first_seen = combo.first_seen
    entry.payload = combo.payload
    if result.status == "ok":
        entry.status = "ok"
        entry.last_ok = iso_now()
        entry.dead_count = 0
    elif result.status in ("revoked", "forbidden"):
        entry.status = result.status
        entry.dead_count += 1
    elif result.status == "invalid_arg":
        # Preserve prior verdict; only set to "invalid_arg" if we had nothing.
        if entry.status in ("", "unknown"):
            entry.status = "invalid_arg"
    else:
        # network / throttle / unknown — leave status alone; retry next run.
        if entry.status in ("", "unknown"):
            entry.status = result.status
    return entry


def run_probes(combos: Iterable[Combo], state: dict[str, StateEntry], workers: int, timeout: int,
               force: bool = False, only_providers: set[str] | None = None,
               sample_per_provider: int = 0) -> tuple[int, int, int]:
    now = _dt.datetime.utcnow()
    # Build the set of credentials already known to be revoked, from any
    # other combo in state. If a credential is dead we don't even need to
    # probe combos we've never seen before that share it — the credential
    # can't come back to life without a new secret being issued (in which
    # case the combo key changes, since the api_key/AK is part of the key).
    def _cred_of(payload: dict[str, Any]) -> str:
        return payload.get("ak") or payload.get("api_key") or ""

    revoked_creds: set[str] = set()
    for e in state.values():
        if e.status == "revoked" and e.payload:
            c = _cred_of(e.payload)
            if c:
                revoked_creds.add(c)

    # Set of combo keys that appear in the newest snapshot. Anything listed
    # by the upstream today is treated as "still-in-latest" and re-probed
    # unconditionally — our stale verdict from days ago may be wrong.
    newest_date = max((c.first_seen for c in combos if c.first_seen), default="")
    latest_keys: set[str] = set()
    if newest_date:
        latest_keys = {c.key for c in combos if c.first_seen == newest_date}

    to_run: list[Combo] = []
    reused = 0
    skipped_bl = 0
    skipped_dead_cred = 0
    for combo in combos:
        if only_providers and combo.provider not in only_providers:
            continue
        still_in_latest = combo.key in latest_keys
        if not force and not still_in_latest and _cred_of(combo.payload) in revoked_creds:
            skipped_dead_cred += 1
            continue
        entry = state.get(combo.key)
        if entry is None:
            # New combo — will be added to state ONLY if we actually probe it.
            to_run.append(combo)
            continue
        if force or should_probe(entry, now, still_in_latest=still_in_latest):
            to_run.append(combo)
        elif entry.blacklisted:
            skipped_bl += 1
        else:
            reused += 1

    by_provider: dict[str, list[Combo]] = {}
    for c in to_run:
        by_provider.setdefault(c.provider, []).append(c)

    if sample_per_provider > 0:
        for prov in list(by_provider):
            by_provider[prov] = by_provider[prov][:sample_per_provider]

    total_probed_planned = sum(len(v) for v in by_provider.values())

    # Per-provider concurrency to respect rate limits
    for prov, group in by_provider.items():
        probe_fn = PROBES.get(prov)
        if probe_fn is None:
            continue
        prov_workers = min(workers, len(group)) or 1
        print(f"[{prov}] probing {len(group)} combos (workers={prov_workers})", file=sys.stderr)
        started = time.time()
        with _cf.ThreadPoolExecutor(max_workers=prov_workers) as ex:
            futs = {ex.submit(probe_fn, combo, timeout): combo for combo in group}
            for i, fut in enumerate(_cf.as_completed(futs), 1):
                combo = futs[fut]
                try:
                    result = fut.result()
                except Exception as exc:
                    result = ProbeResult(status="unknown", detail=str(exc)[:200])
                entry = state.get(combo.key) or StateEntry(first_seen=combo.first_seen)
                state[combo.key] = merge_result(entry, combo, result)
                if i % 25 == 0 or i == len(group):
                    print(f"  {prov} progress {i}/{len(group)} elapsed={time.time()-started:.1f}s", file=sys.stderr)
    return total_probed_planned, reused, skipped_bl + skipped_dead_cred


# --------------------------------------------------------------------------- emit additions

def load_current_config_credentials() -> dict[str, set[str]]:
    """Return which credentials the latest generated config already uses."""
    cfg_path = Path(__file__).parent / "generated_v2" / "cpa-new-config.yaml"
    seen: dict[str, set[str]] = {"bedrock": set(), "azure": set(), "anthropic": set(), "openai_compat": set()}
    if not cfg_path.exists():
        return seen
    try:
        import yaml
        cfg = yaml.safe_load(cfg_path.read_text())
    except Exception:
        return seen
    for ck in (cfg.get("claude-api-key") or []):
        ak = (ck.get("aws-access-key-id") or "").strip()
        if ak:
            seen["bedrock"].add(ak)
        api = (ck.get("api-key") or ck.get("apikey") or "").strip()
        if api:
            seen["anthropic"].add(api)
    for cc in (cfg.get("openai-compatibility") or []):
        api = (cc.get("api-key") or cc.get("apikey") or "").strip()
        if api:
            seen["openai_compat"].add(api)
    for az in (cfg.get("azure-api-key") or []):
        api = (az.get("api-key") or "").strip()
        if api:
            seen["azure"].add(api)
    return seen


def emit_additions(state: dict[str, StateEntry]) -> None:
    """Print OK combos whose credential is NOT in the current generated config."""
    current = load_current_config_credentials()
    additions: list[tuple[str, StateEntry]] = []
    for key, entry in state.items():
        if entry.status != "ok":
            continue
        p = entry.payload or {}
        prov = key.split(":", 1)[0]
        cred = p.get("ak") or p.get("api_key") or ""
        if cred and cred in current.get(prov, set()):
            continue
        additions.append((key, entry))
    print(json.dumps({
        "generated_at": iso_now(),
        "additions": [
            {"key": k, "provider": k.split(":", 1)[0], "payload": e.payload,
             "first_seen": e.first_seen, "last_ok": e.last_ok}
            for k, e in additions
        ],
    }, indent=2, sort_keys=True))


# --------------------------------------------------------------------------- CLI

def main() -> int:
    global STATE_FILE  # allow --state to override the default before any read
    default_state = str(STATE_FILE)
    ap = argparse.ArgumentParser(description="Probe historical credentials across all providers")
    ap.add_argument("--workers", type=int, default=DEFAULT_MAX_WORKERS)
    ap.add_argument("--timeout", type=int, default=DEFAULT_TIMEOUT)
    ap.add_argument("--providers", default="", help="comma-separated subset, empty = all")
    ap.add_argument("--force", action="store_true", help="ignore staleness and re-probe everything not on blacklist")
    ap.add_argument("--force-all", action="store_true", help="also re-probe blacklisted entries")
    ap.add_argument("--rescan-all", action="store_true", help="ignore the date watermark and re-scan every snapshot")
    ap.add_argument("--emit-additions", action="store_true", help="after probing, print combos to add back into config")
    ap.add_argument("--dry-run", action="store_true", help="collect combos and print counts, no probing")
    ap.add_argument("--sample", type=int, default=0, help="if >0, only probe this many combos per provider (for validation)")
    ap.add_argument("--state", default=default_state)
    args = ap.parse_args()

    STATE_FILE = Path(args.state)

    only = {s.strip() for s in args.providers.split(",") if s.strip()} or None
    state = load_state()
    watermark = "" if args.rescan_all else load_watermark()
    if watermark:
        print(f"resuming after watermark date={watermark} (--rescan-all to override)", file=sys.stderr)
    combos = collect_historical_combos(since=watermark)
    print(f"collected {len(combos)} unique combos across snapshots", file=sys.stderr)
    if args.force_all:
        for entry in state.values():
            entry.dead_count = 0
            entry.status = "unknown"

    if args.dry_run:
        by_prov: dict[str, int] = {}
        for c in combos:
            by_prov[c.provider] = by_prov.get(c.provider, 0) + 1
        for p, n in sorted(by_prov.items()):
            print(f"  {p}: {n}", file=sys.stderr)
        return 0

    ran, reused, bl = run_probes(combos, state, args.workers, args.timeout, force=args.force, only_providers=only,
                                 sample_per_provider=args.sample)
    save_state(state)

    # Advance the watermark to the highest snapshot date we saw, so subsequent
    # runs can skip everything already covered. Only when we did NOT limit
    # providers / sample (which would leave gaps).
    if not only and args.sample == 0 and combos:
        newest = max((c.first_seen for c in combos if c.first_seen), default="")
        if newest and newest > load_watermark():
            save_watermark(newest)
            print(f"advanced watermark to {newest}", file=sys.stderr)

    print(f"done: probed={ran} reused={reused} skipped_bl_or_dead_cred={bl}", file=sys.stderr)

    if args.emit_additions:
        emit_additions(state)
    return 0


if __name__ == "__main__":
    sys.exit(main())
