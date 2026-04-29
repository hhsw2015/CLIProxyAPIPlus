#!/usr/bin/env python3
"""Generate CPA config.yaml v2 - Full Replacement for gpt-proxy.

Capabilities:
- Merges US + CN Nacos artifacts.
- Bedrock: Groups by (AK, Region), handles Mlxy/Xmind per-model ARN maps (~22 entries).
- Azure: Merges Skywork Azure (351) + Kuanbang (p8) + Response endpoints.
- Vertex: Claude (ADC/keys-other) + Gemini (AIza keys).
- TaijiAI: Claude (strips beta) + Gemini (openai-compat).
- Media: 7 direct providers (p7) + 21 gpt-proxy fallbacks (p1).
- Cookie Pool + Singularity: p2.
- ErrorListPass: 32 entries from Nacos.
- Priority-based output: p10 -> p1.

Usage:
  python3 scripts/gen_llm_config_v2.py --date 2026-04-18
"""

import json
import sys
import os
import secrets
import string
from collections import defaultdict
from pathlib import Path


def _gen_jwt_secret() -> str:
    """Generate a 48-char random JWT secret."""
    return secrets.token_urlsafe(36)


def _gen_db_password() -> str:
    """Generate a 24-char alphanumeric database password."""
    alphabet = string.ascii_letters + string.digits
    return ''.join(secrets.choice(alphabet) for _ in range(24))


# Cache: reuse secrets from existing config to avoid breaking DB/JWT on re-generation.
_cached_secrets = {}


def _load_existing_secrets(config_path: str):
    """Read JWT secret and DB password from an existing config file."""
    global _cached_secrets
    try:
        import yaml
        with open(config_path) as f:
            cfg = yaml.safe_load(f)
        if not cfg or 'commercial' not in cfg:
            return
        sub = cfg['commercial'].get('sub2api', {})
        db = sub.get('database', {})
        jwt_cfg = sub.get('jwt', {})
        if db.get('password'):
            _cached_secrets['db_password'] = db['password']
        if jwt_cfg.get('secret'):
            _cached_secrets['jwt_secret'] = jwt_cfg['secret']
    except Exception:
        pass


def _get_jwt_secret() -> str:
    """Get JWT secret: reuse existing or generate new."""
    return _cached_secrets.get('jwt_secret', _gen_jwt_secret())


def _get_db_password() -> str:
    """Get DB password: reuse existing or generate new."""
    return _cached_secrets.get('db_password', _gen_db_password())


# --- Configuration & Paths ---

CPA_ROOT = Path(__file__).parent.parent.absolute()
US_NACOS_BASE = Path('/Users/wowdd1/Dev/dvina-2api/artifacts/api-keys/us-nacos')
CN_NACOS_BASE = Path('/Users/wowdd1/Dev/dvina-2api/artifacts/api-keys/cn-nacos')
OUTPUT_DIR = CPA_ROOT / 'scripts/generated_v2'

# Priority-based Backup Duration (ms)
# p10 (Skywork Direct): None (default)
# p9 (OpenRouter/Taiji): 15000
# p8 (Kuanbang/Third-party): 18000
# p7 (Media Direct): 25000
# p6 (MaaS): 28000
# p5 (Legacy): 35000
# p3-p2 (Pools): 38000
# p1 (gpt-proxy): None
PRIORITY_BACKUP_DURATION = {
    10: None,
    9: 15000,
    8: 18000,
    7: 25000,
    6: 28000,
    5: 35000,
    3: 38000,
    2: 38000,
    1: None
}

# 32 Unique ErrorListPass from Nacos
ERROR_LIST_PASS = [
    'StatusCode: 429', 'StatusCode: 503', 'retry quota exceeded',
    'ModelStreamErrorException', 'failed to get rate limit token',
    'Error processing stream', 'Provider returned error',
    'X-Amzn-Bedrock-Context-Length', 'PERMISSION_DENIED',
    'UnrecognizedClientException', 'Insufficient credits.',
    'temporarily rate-limited upstream.', 'Internal Server Error',
    'Operation not allowed', 'Internal server error', 'overloaded_error',
    'Image does not match the provided media type image/jpeg',
    'InvalidSignatureException', '504 Gateway Time-out',
    '<title>403 Forbidden</title>', 'Resource exhausted. Please try again later',
    'connection reset by peer', 'Response Empty, [DONE]', 'Bad Request',
    'dial tcp ip:port: connect: connection refused', 'serivce_request_error',
    'invalid_request_error', 'RESOURCE_EXHAUSTED', 'Invalid JSON string',
    'exceeded maximum number of attempts', 'No auth credentials found',
    'Resource not found',  # Azure 404: single deployment missing, let scheduler try other keys
    '404 Not Found',
]

# Non-retryable substrings (fail fast)
NON_RETRYABLE = [
    "User not found", "Authentication failed", "Invalid API key", "Model not found"
]

# --- YAML Helpers ---

def yq(s):
    """Quote YAML string if needed."""
    if not isinstance(s, str): return str(s)
    if s == '': return '""'
    special = set('+/\\$#:{}[]&!|>%@`')
    if any(c in special for c in s) or s.startswith(('-', '*', '?', '"', "'")):
        escaped = s.replace('\\', '\\\\').replace('"', '\\"')
        return '"' + escaped + '"'
    return s

def indent(text, spaces=2):
    return '\n'.join(' ' * spaces + line for line in text.splitlines())

def extract_aws_region(arn):
    parts = arn.split(':')
    return parts[3] if len(parts) >= 4 else 'us-east-1'

# --- Data Loading ---

class NacosData:
    def __init__(self, date_str):
        self.us = self._load_dir(US_NACOS_BASE, date_str)
        self.cn = self._load_dir(CN_NACOS_BASE, date_str)
        self.all_keys = self._load_all_keys()

    def _load_dir(self, base, date_str):
        d = base / date_str
        data = {}
        files = ['keys-claude.json', 'keys-azure.json', 'keys-other.json', 'config_us_k8s.json', 'full-config.json', 'router.json']
        for f in files:
            p = d / f
            if p.exists():
                try:
                    with open(p) as f_in: data[f] = json.load(f_in)
                except: data[f] = {}
            else:
                data[f] = {}
        return data

    def _load_all_keys(self):
        p = Path('/Users/wowdd1/Dev/dvina-2api/artifacts/api-keys/ALL_API_KEYS.json')
        if p.exists():
            return json.load(open(p))
        return []

    def get(self, filename, key_path, default=None):
        """Deep get from merged US/CN. US takes priority."""
        for src in [self.us, self.cn]:
            if filename not in src: continue
            curr = src[filename]
            found = True
            for part in key_path.split('.'):
                if isinstance(curr, dict) and part in curr:
                    curr = curr[part]
                else:
                    found = False
                    break
            if found: return curr
        return default

# --- Router Model Extraction ---

def extract_models_by_router(nd, router_name):
    """Extract all model names routed to a specific router from router.json ModelRouter."""
    router_data = nd.get('router.json', 'ModelRouter', {})
    models = []
    for model, routes in router_data.items():
        if isinstance(routes, list):
            for r in routes:
                if isinstance(r, dict) and r.get('Router') == router_name:
                    models.append(model)
                    break
    return sorted(set(models))


def extract_claude_native_models(nd):
    """Extract models from ClaudeNativeRouter."""
    return sorted(nd.get('router.json', 'ClaudeNativeRouter', {}).keys())


def extract_response_models(nd):
    """Extract models from ResponseRouter."""
    return sorted(nd.get('router.json', 'ResponseRouter', {}).keys())


# --- Generators ---

def generate_bedrock(nd):
    """Priority 10. Groups Mlxy/Xmind/ClaudeAws by (AK, Region).
    Nova models (non-Anthropic) skipped - they use AWS Converse not /v1/messages.
    """
    groups = {} # (ak, region) -> {sk, models: {name: arn}}

    def _is_anthropic(mname):
        return 'claude' in mname.lower()

    # 1. MlxyConf
    mlxy = nd.get('keys-claude.json', 'MlxyConf', {})
    for mname, eps in mlxy.items():
        if not isinstance(eps, list) or not _is_anthropic(mname): continue
        for ep in eps:
            ak, sk, region, arn = ep.get('Ak'), ep.get('Sk'), ep.get('Region'), ep.get('ModelId')
            if not ak or not arn: continue
            key = (ak, region or extract_aws_region(arn))
            if key not in groups: groups[key] = {'sk': sk, 'models': {}}
            groups[key]['models'][mname] = arn

    # 2. XmindConf (skip nova-micro etc.)
    xmind = nd.get('keys-claude.json', 'XmindConf', {})
    for mname, eps in xmind.items():
        if not isinstance(eps, list) or not _is_anthropic(mname): continue
        for ep in eps:
            ak, sk, region, arn = ep.get('Ak'), ep.get('Sk'), ep.get('Region'), ep.get('ModelId')
            if not ak or not arn: continue
            key = (ak, region or extract_aws_region(arn))
            if key not in groups: groups[key] = {'sk': sk, 'models': {}}
            groups[key]['models'][mname] = arn

    # 3. AwsConf (new direct Bedrock AK, includes Opus 4.7)
    awsconf = nd.get('keys-claude.json', 'AwsConf', {})
    for mname, eps in awsconf.items():
        if not isinstance(eps, list) or not _is_anthropic(mname): continue
        for ep in eps:
            ak, sk, region, arn = ep.get('Ak'), ep.get('Sk'), ep.get('Region'), ep.get('ModelId')
            if not ak or not arn: continue
            key = (ak, region or extract_aws_region(arn))
            if key not in groups: groups[key] = {'sk': sk, 'models': {}}
            groups[key]['models'][mname] = arn

    # 4. ClaudeAws (config_us_k8s.json - legacy single AK/SK)
    caws = nd.get('config_us_k8s.json', 'ClaudeAws', {})
    if caws.get('Ak'):
        ak, sk = caws['Ak'], caws['Sk']
        # Regions: us-east-1, us-west-2
        for r_key in ['RegionUSEast1', 'RegionUSWest2']:
            region = caws.get(r_key)
            if region:
                key = (ak, region)
                if key not in groups: groups[key] = {'sk': sk, 'models': {}}
                # Use default model set for legacy ClaudeAws
                for m in ['claude-3-opus-20240229', 'claude-3-sonnet-20240229', 'claude-3-haiku-20240307']:
                    groups[key]['models'][m] = f"arn:aws:bedrock:{region}::foundation-model/{m}"

    entries = []
    for (ak, region), info in sorted(groups.items()):
        has_opus47 = any('opus-4-7' in m or 'opus-4.7' in m for m in info['models'])
        lines = [
            f"- aws-access-key-id: {ak}",
            f"  aws-secret-access-key: {yq(info['sk'])}",
            f"  aws-region: {region}",
            f"  priority: 10",
        ]
        if not has_opus47:
            lines.append(f"  excluded-models: [claude-opus-4-7, claude-opus-4.7]")
        # Dash/dot variants: register both names pointing to same ARN
        dot_dash_map = {
            'claude-opus-4-7': 'claude-opus-4.7',
            'claude-opus-4-6': 'claude-opus-4.6',
            'claude-opus-4-5': 'claude-opus-4.5',
            'claude-sonnet-4-6': 'claude-sonnet-4.6',
            'claude-sonnet-4-5': 'claude-sonnet-4.5',
            'claude-haiku-4-5': 'claude-haiku-4.5',
        }
        dot_dash_map.update({v: k for k, v in dot_dash_map.items()})
        lines.append(f"  models:")
        seen = set()
        for m, arn in sorted(info['models'].items()):
            lines.append(f"    - name: {m}")
            lines.append(f"      model-id: {yq(arn)}")
            seen.add(m)
            # Add dot/dash variant as separate entry with same ARN
            alt = dot_dash_map.get(m)
            if alt and alt not in seen and alt not in info['models']:
                lines.append(f"    - name: {alt}")
                lines.append(f"      model-id: {yq(arn)}")
                seen.add(alt)
        entries.append('\n'.join(lines))
    return entries

def generate_vertex_claude(nd):
    """Priority 10. keys-other.json GoogleConf.
    Returns (claude_entries, oa_entries):
      Claude models -> claude-api-key (Anthropic Messages protocol)
      Gemini models -> openai-compatibility (Vertex Gemini protocol)
    """
    conf = nd.get('keys-other.json', 'GoogleConf', {})
    claude_entries = []
    oa_entries = []
    if not conf: return claude_entries, oa_entries

    def _slug(m):
        return m.replace('.', '-').replace('/', '-')

    for mname, projects in conf.items():
        if not isinstance(projects, list): continue
        project_ids = [p['ProjectId'] for p in projects if 'ProjectId' in p]
        if not project_ids: continue

        lower = mname.lower()
        is_gemini = 'gemini' in lower

        pool_lines = f"  model-project-pool:\n    {mname}:\n"
        pool_lines += '\n'.join(f"      - {pid}" for pid in project_ids)

        if is_gemini:
            lines = [
                f"- name: vertex-gemini-{_slug(mname)}",
                f"  priority: 10",
                f"  auth-style: auto",
                f"  vertex-location: us-east5",
                pool_lines,
                f"  models:",
                f"    - name: {mname}",
            ]
            oa_entries.append('\n'.join(lines))
        else:
            lines = [
                f"- api-key: vertex-adc",
                f"  base-url: vertex://us-east5",
                f"  priority: 10",
                f"  vertex-location: us-east5",
                pool_lines,
                f"  models:",
                f"    - name: {mname}",
            ]
            claude_entries.append('\n'.join(lines))
    return claude_entries, oa_entries

def generate_azure(nd):
    """Priority 10/8. Merges US full-config.json + CN keys-azure.json.

    CPA scheduler does NOT route by endpoint-path. If a model name appears
    in BOTH AzureConf (chat/completions) and AzureResponseConf (responses),
    scheduler will pick Response entry for chat requests and fail.
    Fix: Response sections only emit models that are responses-only
    (o3-pro / *-codex / *-pro). Drop overlap with text sections.
    """
    sections = [
        ('AzureConf', 10, 'azure'),
        ('AzureResponseConf', 10, 'azure-resp'),
        ('KuanbangConf', 8, 'kb'),
        ('KuanbangResponseConf', 8, 'kb-resp'),
    ]
    # Collect text-side models first to avoid duplicates in Response sections
    text_models = set()
    for text_sect in ['AzureConf', 'KuanbangConf']:
        for data in [nd.us.get('full-config.json', {}).get(text_sect, {}),
                     nd.us.get('keys-azure.json', {}).get(text_sect, {}),
                     nd.cn.get('keys-azure.json', {}).get(text_sect, {})]:
            if isinstance(data, dict):
                text_models.update(data.keys())
    entries = []
    for sect, prio, prefix in sections:
        is_response = 'Response' in sect
        # Merge US (full-config.json + keys-azure.json) + CN (keys-azure.json)
        merged = defaultdict(list)  # model -> list of (source_tag, api, key)
        seen = set()  # dedup by (model, api, key)

        for src_tag, data in [
            ('us', nd.us.get('full-config.json', {}).get(sect, {})),
            ('us', nd.us.get('keys-azure.json', {}).get(sect, {})),
            ('cn', nd.cn.get('keys-azure.json', {}).get(sect, {})),
        ]:
            if not isinstance(data, dict): continue
            for mname, eps in data.items():
                if not isinstance(eps, list): continue
                # Response sections: skip models also on text side to avoid scheduler pick
                if is_response and mname in text_models: continue
                for ep in eps:
                    api, key = ep.get('Api'), ep.get('Key')
                    if not api or not key: continue
                    dedup_key = (mname, api, key)
                    if dedup_key in seen: continue
                    seen.add(dedup_key)
                    merged[mname].append((src_tag, api, key))

        # Emit per-(model, key) entries
        for mname, items in merged.items():
            safe_m = mname.replace('.', '').replace(' ', '')
            for i, (src_tag, api, key) in enumerate(items, 1):
                ename = f"{prefix}-{src_tag}-{safe_m}-{i}"
                # Split Azure URL: base-url = domain + /openai/deployments/<model>/
                # endpoint-path = /chat/completions?api-version=... (or /responses for Response)
                # CPA empty-string override doesn't work (line 329: override != "" check).
                # Must use non-empty override with full path + query.
                if '/chat/completions' in api:
                    split_idx = api.index('/chat/completions')
                    base_final = api[:split_idx]
                    endpoint_final = api[split_idx:]
                elif '/responses' in api:
                    split_idx = api.index('/responses')
                    base_final = api[:split_idx]
                    endpoint_final = api[split_idx:]
                else:
                    base_final = api
                    endpoint_final = ''
                lines = [
                    f"- name: {ename}",
                    f"  priority: {prio}",
                    f"  base-url: {base_final}",
                    f'  endpoint-path: {yq(endpoint_final)}',
                    f"  api-key-entries:",
                    f"    - api-key: {yq(key)}",
                    f"  models:",
                    f"    - name: {mname}",
                ]
                if 'Response' in sect:
                    lines.append(f"  auth-style: azure-api-key")
                    lines.append(f"  responses-format: true")
                else:
                    lines.append(f"  headers:")
                    lines.append(f"    api-key: {yq(key)}")
                entries.append('\n'.join(lines))

    return entries

def generate_taijiai(nd):
    """Priority 9. XP keys.
    Returns (claude_entries, oa_entries):
      Claude -> claude-api-key (Anthropic Messages protocol, strip-beta)
      Gemini -> openai-compatibility
    """
    xp = nd.get('config_us_k8s.json', 'XP', {})
    claude_entries = []
    oa_entries = []

    # 1. Claude (Anthropic API -> claude-api-key section)
    ckey = xp.get('ClaudeKey')
    if ckey:
        lines = [
            f"- api-key: {yq(ckey)}",
            f"  base-url: https://api.taijiaicloud.com",
            f"  priority: 1",
            f"  strip-anthropic-beta: true",
            f"  headers:",
            f'    anthropic-version: "2023-06-01"',
            f"  models:",
            f"    - name: claude-opus-4-7\n      alias: claude-opus-4.7",
            f"    - name: claude-opus-4.7",
            f"    - name: claude-opus-4-6\n      alias: claude-opus-4.6",
            f"    - name: claude-sonnet-4-6\n      alias: claude-sonnet-4.6",
            f"    - name: claude-3-5-sonnet-20240620",
        ]
        claude_entries.append('\n'.join(lines))

    # 2. Gemini (OpenAI Compat -> openai-compatibility section)
    gkey = xp.get('GeminiKey')
    if gkey:
        lines = [
            f"- name: taijiai-gemini",
            f"  priority: 1",
            f"  base-url: https://api.taijiaicloud.com/v1",
            f"  api-key-entries:",
            f"    - api-key: {yq(gkey)}",
            f"  models:",
            f"    - name: gemini-2.5-pro",
            f"    - name: gemini-2.5-flash",
        ]
        oa_entries.append('\n'.join(lines))
    return claude_entries, oa_entries

def generate_maas(nd):
    """Priority 8. MiniMax/VolEngine/Zhipu Anthropic-compatible endpoints.
    Returns claude-api-key entries (NOT openai-compatibility).

    These use Anthropic Messages protocol (/v1/messages + x-api-key auth).
    RE 2026-04-19 actual URLs:
      MiniMax:   https://api.minimax.io/anthropic/v1/messages
      VolEngine: https://ark.cn-beijing.volces.com/api/compatible/v1/messages
      Zhipu:     https://open.bigmodel.cn/api/anthropic/v1/messages
    """
    maas_models = [
        'claude-sonnet-4.6', 'claude-opus-4.6', 'claude-haiku-4.5',
        'claude-3-5-sonnet-20240620',
    ]
    mlines = '\n'.join(f"    - name: {m}" for m in maas_models)
    providers = [
        ('minimax-anthropic', 'https://api.minimax.io/anthropic', 'MiniMax.Key'),
        ('volengine-anthropic', 'https://ark.cn-beijing.volces.com/api/compatible', 'VolEngine.Key'),
        ('zhipu-anthropic', 'https://open.bigmodel.cn/api/anthropic', 'Zhipu.Key'),
    ]
    entries = []
    for name, base, key_path in providers:
        key = nd.get('config_us_k8s.json', key_path)
        if not key: continue
        entries.append(
            f"- api-key: {yq(key)}\n"
            f"  base-url: {base}\n"
            f"  priority: 8\n"
            f"  backup-duration-ms: 18000\n"
            f"  headers:\n"
            f'    anthropic-version: "2023-06-01"\n'
            f"  models:\n{mlines}"
        )
    return entries

def generate_pools():
    """Priority 2. Cookie Pool + Singularity (ultra + plus)."""
    claude_gpt_models = [
        'claude-opus-4.7', 'claude-opus-4.6', 'claude-opus-4.5',
        'claude-sonnet-4.6', 'claude-sonnet-4.5', 'claude-haiku-4.5',
        'gpt-5-pro', 'gpt-5.1', 'gpt-5.2', 'gpt-5.2-pro', 'gpt-5.3-codex',
        'gpt-5.4', 'gpt-5.4-pro', 'grok-4', 'o3-pro',
    ]
    singu_models = ['gemini-2.5-pro', 'gemini-3-flash-preview', 'gemini-3.1-pro-preview']
    cp_lines = '\n'.join(f"    - name: {m}" for m in claude_gpt_models)
    sg_lines = '\n'.join(f"    - name: {m}" for m in singu_models)
    entries = []
    for tier, fname in [('ultra', 'pool-ultra.json'), ('plus', 'pool-plus.json')]:
        entries.append(
            f"- name: cookie-pool-{tier}\n"
            f"  priority: 2\n"
            f"  base-url: https://desktop-llm.skywork.ai/skycowork_llm/v1/proxy\n"
            f"  cookie-pool-file: {fname}\n"
            f"  models:\n{cp_lines}"
        )
        entries.append(
            f"- name: singularity-{tier}\n"
            f"  priority: 2\n"
            f"  prefix: skyclaw\n"
            f"  base-url: https://desktop-llm.skywork.ai/skycowork_llm/v1/proxy/chat/completions\n"
            f"  cookie-pool-file: {fname}\n"
            f"  models:\n{sg_lines}"
        )
    return entries


def generate_gemini_keys(nd):
    """Priority 10. config_us_k8s.Google.Keys -> gemini-api-key.
    Model list dynamically extracted from router.json (Router=google, Gemini only)."""
    keys = nd.get('config_us_k8s.json', 'Google.Keys', []) or []
    if not keys: return []
    # Get all Gemini models routed through Google
    google_models = extract_models_by_router(nd, 'google')
    models = [m for m in google_models if m.startswith('gemini-')]
    if not models:
        models = ['gemini-2.5-pro', 'gemini-2.5-flash']
    mlines = '\n'.join(f"    - name: {m}" for m in models)
    out = []
    for k in keys:
        out.append(f"- api-key: {k}\n  priority: 10\n  models:\n{mlines}")
    return out


def generate_vertex_xmind(nd):
    """Priority 10. keys-other.XmindGoogleConf."""
    conf = nd.get('keys-other.json', 'XmindGoogleConf', {})
    if not conf: return []
    entries = []
    for mname, projects in conf.items():
        if not isinstance(projects, list): continue
        project_ids = [p['ProjectId'] for p in projects if 'ProjectId' in p]
        if not project_ids: continue
        pool_lines = f"  model-project-pool:\n    {mname}:\n"
        pool_lines += '\n'.join(f"      - {pid}" for pid in project_ids)
        entries.append(
            f"- name: vertex-xmind-{mname.replace('.','-').replace('/','-')}\n"
            f"  priority: 10\n"
            f"  auth-style: auto\n"
            f"  vertex-location: us-east5\n"
            f"{pool_lines}\n"
            f"  models:\n    - name: {mname}"
        )
    return entries


def generate_openrouter(nd):
    """Priority 9 (Nacos) + 9 (Public).
    V1 MATCHED: OpenRouter Claude nodes MUST be in claude-api-key section
    to maintain native Anthropic protocol without thinking translation side-effects.
    CPA automatically uses Bearer auth for non-Anthropic hosts in claude-api-key mode.
    """
    nacos_claude_entries = []
    openai_compat_entries = []

    nkeys = nd.get('keys-other.json', 'OpenRouter.KeyList', []) or []
    or_claude_models = [
        ('claude-opus-4-7', 'claude-opus-4.7'),
        ('anthropic/claude-opus-4.7', None),
        ('anthropic/claude-4.7-opus-20260416', None),
        ('anthropic/claude-opus-4.6', None),
        ('anthropic/claude-sonnet-4.6', None),
        ('anthropic/claude-haiku-4.5', None),
    ]
    # Dynamically extract all OpenRouter-routed models from router.json,
    # then filter out Claude/Gemini (handled by dedicated entries above)
    all_router_models = extract_models_by_router(nd, 'router')
    or_other_models = [
        m for m in all_router_models
        if not m.startswith('claude') and not m.startswith('gemini-')
    ]
    # Also add static OpenAI models not in router.json (direct OR access)
    or_static_extras = ['openai/gpt-5.4-pro', 'openai/gpt-5.4', 'openai/gpt-5-pro',
                        'openai/o3-pro', 'deepseek/deepseek-v3.2']
    for m in or_static_extras:
        if m not in or_other_models:
            or_other_models.append(m)

    if nkeys:
        # Claude models -> claude-api-key type (V1 mode: native Anthropic forwarding)
        ml_claude = []
        for name, alias in or_claude_models:
            if alias:
                ml_claude.append(f"    - name: {name}\n      alias: {alias}")
            else:
                ml_claude.append(f"    - name: {name}")
        ml_claude = '\n'.join(ml_claude)
        for k in nkeys:
            nacos_claude_entries.append(
                f"- api-key: {yq(k)}\n"
                f"  base-url: https://openrouter.ai/api\n"
                f"  priority: 9\n"
                f"  headers:\n"
                f"    anthropic-version: \"2023-06-01\"\n"
                f"    HTTP-Referer: \"https://skywork.ai\"\n"
                f"  models:\n{ml_claude}"
            )

        # Other models -> openai-compatibility
        ml_other = '\n'.join(f"    - name: {m}" for m in or_other_models)
        key_lines = '\n'.join(f"    - api-key: {yq(k)}" for k in nkeys)
        openai_compat_entries.append(
            f"- name: openrouter-nacos-other\n"
            f"  priority: 9\n"
            f"  base-url: https://openrouter.ai/api/v1\n"
            f"  backup-duration-ms: 15000\n"
            f"  headers:\n"
            f"    HTTP-Referer: \"https://skywork.ai\"\n"
            f"    X-Title: \"Skywork LLM\"\n"
            f"  api-key-entries:\n{key_lines}\n"
            f"  models:\n{ml_other}"
        )

    # Public (ALL_API_KEYS)
    pub = [x for x in nd.all_keys if x.get('provider') == 'openrouter' and x.get('status') == 'active']
    if pub:
        key_lines = '\n'.join(f"    - api-key: {yq(x['key'])}" for x in pub)
        pub_models = ['claude-sonnet-4.6', 'claude-opus-4.6', 'gemini-2.5-pro', 'qwen3-coder-480b-a35b-instruct']
        ml = '\n'.join(f"    - name: {m}" for m in pub_models)
        openai_compat_entries.append(
            f"- name: openrouter-public\n"
            f"  priority: 9\n"
            f"  base-url: https://openrouter.ai/api/v1\n"
            f"  backup-duration-ms: 15000\n"
            f"  api-key-entries:\n{key_lines}\n"
            f"  models:\n{ml}"
        )
    return nacos_claude_entries, openai_compat_entries


def generate_third_party_proxies(nd):
    """Priority 7. Polo/YesVG/YouBangGPT/Woyaochat (TaijiAI already p9).
    Returns (claude_entries, oa_entries):
      Polo/YesVG/YouBangGPT Claude -> claude-api-key (Anthropic Messages)
      Polo Responses / Woyaochat -> openai-compatibility
    """
    entries = []  # openai-compatibility
    cu = nd.us.get('config_us_k8s.json', {})

    # Polo (messages -> claude-api-key, responses -> openai-compat)
    polo = cu.get('Polo', {})
    polo_claude = []
    if polo.get('Key') and polo.get('Api'):
        base = polo['Api'].rsplit('/v1/messages', 1)[0]
        polo_claude.append(
            f"- api-key: {yq(polo['Key'])}\n"
            f"  base-url: {base}\n"
            f"  priority: 7\n"
            f"  models:\n"
            f"    - name: claude-opus-4-20250514\n      alias: claude-4-opus\n"
            f"    - name: claude-opus-4-6-thinking\n      alias: claude-opus-4.6\n"
            f"    - name: claude-sonnet-4-20250514\n      alias: claude-4-sonnet\n"
            f"    - name: claude-sonnet-4-5-20250929-thinking\n      alias: claude-sonnet-4.5\n"
            f"    - name: claude-3-7-sonnet-20250219\n      alias: claude37-sonnet\n"
            f"    - name: claude-3-5-sonnet-20241022\n      alias: claude35-sonnet-v2\n"
            f"    - name: claude-3-5-sonnet-20240620\n      alias: claude35-sonnet\n"
            f"    - name: claude-3-5-haiku-20241022\n      alias: claude35-haiku"
        )
    if polo.get('ResponseKey') and polo.get('ResponseApi'):
        base = polo['ResponseApi'].rsplit('/v1/responses', 1)[0]
        entries.append(
            f"- name: polo-responses\n"
            f"  priority: 7\n"
            f"  base-url: {base}\n"
            f"  endpoint-path: /v1/responses\n"
            f"  backup-duration-ms: 25000\n"
            f"  api-key-entries:\n    - api-key: {yq(polo['ResponseKey'])}\n"
            f"  models:\n"
            f"    - name: gpt-5.2-codex\n"
            f"    - name: gpt-5.3-codex\n"
            f"    - name: gpt-5.4"
        )

    # YesVG (Anthropic Messages -> claude-api-key)
    yesvg = cu.get('YesVG', {})
    if yesvg.get('Key') and yesvg.get('Api'):
        base = yesvg['Api'].rsplit('/v1/messages', 1)[0]
        polo_claude.append(
            f"- api-key: {yq(yesvg['Key'])}\n"
            f"  base-url: {base}\n"
            f"  priority: 7\n"
            f"  models:\n    - name: claude-sonnet-4.6"
        )

    # YouBangGPT (Anthropic Messages -> claude-api-key)
    ybg = cu.get('YouBangGPT', {})
    if ybg.get('Key') and ybg.get('ClaudeApi'):
        base = ybg['ClaudeApi'].rsplit('/v1/messages', 1)[0]
        polo_claude.append(
            f"- api-key: {yq(ybg['Key'])}\n"
            f"  base-url: {base}\n"
            f"  priority: 7\n"
            f"  models:\n    - name: claude-opus-4.6\n    - name: claude-sonnet-4.6"
        )

    # Woyaochat (OpenAI-compat: /v1/chat/completions + Bearer sk-*)
    woy = cu.get('Woyaochat', {})
    if woy.get('ClaudeKey') and woy.get('ClaudeApi'):
        api = woy['ClaudeApi']
        base = api.rsplit('/v1/chat/completions', 1)[0] if '/v1/chat/completions' in api else api
        entries.append(
            f"- name: woyaochat-claude\n"
            f"  priority: 7\n"
            f"  base-url: {base}\n"
            f"  endpoint-path: /v1/chat/completions\n"
            f"  auth-style: bearer\n"
            f"  backup-duration-ms: 25000\n"
            f"  api-key-entries:\n    - api-key: {yq(woy['ClaudeKey'])}\n"
            f"  models:\n    - name: claude-sonnet-4.6\n    - name: claude-opus-4.6"
        )
    return polo_claude, entries


def generate_maas_tier(nd):
    """Priority 6. Deepseek/Grok/DashScope/ApiCoco."""
    entries = []
    cu = nd.us.get('config_us_k8s.json', {})

    # Deepseek
    ds = cu.get('DeepseekGPT', {})
    ds_keys = ds.get('Keys', []) or ([ds.get('Key')] if ds.get('Key') else [])
    if ds_keys:
        key_lines = '\n'.join(f"    - api-key: {yq(k)}" for k in ds_keys if k)
        entries.append(
            f"- name: deepseek-direct\n"
            f"  priority: 6\n"
            f"  base-url: https://api.deepseek.com/v1\n"
            f"  backup-duration-ms: 28000\n"
            f"  api-key-entries:\n{key_lines}\n"
            f"  models:\n    - name: deepseek-chat\n    - name: deepseek-reasoner"
        )

    # Grok (Xmind Azure)
    xm = cu.get('Xmind', {})
    if xm.get('Grok4') and xm.get('GrokKey'):
        entries.append(
            f"- name: xmind-grok4\n"
            f"  priority: 6\n"
            f"  base-url: {xm['Grok4']}\n"
            f"  auth-style: azure-api-key\n"
            f"  backup-duration-ms: 28000\n"
            f"  api-key-entries:\n    - api-key: {yq(xm['GrokKey'])}\n"
            f"  headers:\n    api-key: {yq(xm['GrokKey'])}\n"
            f"  models:\n    - name: grok-4"
        )

    # Aliyun DashScope (models from router.json Router=aliyun)
    aly = cu.get('Aliyun', {})
    if aly.get('Key'):
        aliyun_models = extract_models_by_router(nd, 'aliyun')
        if not aliyun_models:
            aliyun_models = ['qwen-max', 'qwen3-coder', 'kimi-k2.5']
        ml = '\n'.join(f"    - name: {m}" for m in aliyun_models)
        entries.append(
            f"- name: dashscope-aliyun\n"
            f"  priority: 6\n"
            f"  base-url: https://dashscope.aliyuncs.com/compatible-mode/v1\n"
            f"  backup-duration-ms: 28000\n"
            f"  api-key-entries:\n    - api-key: {yq(aly['Key'])}\n"
            f"  models:\n{ml}"
        )

    # ApiCoco MaaS (ChatKey)
    ac = cu.get('ApiCoco', {})
    if ac.get('ChatKey') and ac.get('ChatCompletions'):
        entries.append(
            f"- name: apicoco-maas\n"
            f"  priority: 6\n"
            f"  base-url: {ac['ChatCompletions'].rsplit('/chat/completions',1)[0]}\n"
            f"  backup-duration-ms: 28000\n"
            f"  api-key-entries:\n    - api-key: {yq(ac['ChatKey'])}\n"
            f"  models:\n    - name: apicoco-chat"
        )

    # ApiCoco GLM (GLMKey -> Zhipu GLM models via ModelArts MaaS)
    if ac.get('GLMKey') and ac.get('ChatCompletions'):
        entries.append(
            f"- name: apicoco-glm\n"
            f"  priority: 6\n"
            f"  base-url: {ac['ChatCompletions'].rsplit('/chat/completions',1)[0]}\n"
            f"  backup-duration-ms: 28000\n"
            f"  api-key-entries:\n    - api-key: {yq(ac['GLMKey'])}\n"
            f"  models:\n    - name: glm-5"
        )
    return entries


def generate_kimi(nd):
    """Priority 6. Kimi (Moonshot) - dual endpoint: OpenAI chat + Anthropic messages."""
    entries = []
    kimi = nd.us.get('config_us_k8s.json', {}).get('Kimi', {})
    keys = kimi.get('Keys', [])
    if not keys:
        return entries

    api_chat = kimi.get('Api', '')
    api_messages = kimi.get('ApiMessages', '')

    for i, key in enumerate(keys, 1):
        if not key:
            continue
        # OpenAI-compatible chat endpoint
        if api_chat:
            base = api_chat.rsplit('/chat/completions', 1)[0] if '/chat/completions' in api_chat else api_chat
            kimi_models = extract_models_by_router(nd, 'kimi')
            if not kimi_models:
                kimi_models = ['kimi-k2', 'kimi-k2.5', 'kimi-k2.6']
            ml = '\n'.join(f"    - name: {m}" for m in kimi_models)
            entries.append(
                f"- name: kimi-moonshot-{i}\n"
                f"  priority: 6\n"
                f"  base-url: {base}\n"
                f"  backup-duration-ms: 28000\n"
                f"  api-key-entries:\n    - api-key: {yq(key)}\n"
                f"  models:\n{ml}"
            )
    return entries


def generate_legacy(nd):
    """Priority 5. Silicon/DeepInfra/Sophnet/Cloudsway."""
    entries = []
    cu = nd.us.get('config_us_k8s.json', {})

    for name, conf_key, url_default, models in [
        ('silicon-direct', 'Silicon', 'https://api.siliconflow.cn/v1', ['qwen3-coder']),
        ('deepinfra-direct', 'DeepInfra', 'https://api.deepinfra.com/v1/openai', ['mistral-large']),
        ('sophnet-direct', 'Sophnet', 'https://www.sophnet.com/api/v1/chat/completions', ['sophnet-chat']),
    ]:
        conf = cu.get(conf_key, {})
        key = conf.get('Key')
        if not key: continue
        url = conf.get('Api', url_default)
        if url.endswith('/chat/completions'):
            url = url.rsplit('/chat/completions', 1)[0]
        ml = '\n'.join(f"    - name: {m}" for m in models)
        entries.append(
            f"- name: {name}\n"
            f"  priority: 5\n"
            f"  base-url: {url}\n"
            f"  backup-duration-ms: 35000\n"
            f"  api-key-entries:\n    - api-key: {yq(key)}\n"
            f"  models:\n{ml}"
        )

    # Cloudsway (per-model URLs: each model has a unique /v1/ai/{slug}/chat/completions)
    cs = cu.get('Cloudsway', {})
    cs_key = cs.get('Key')
    if cs_key:
        cs_routes = [
            ('O1Api', 'o1'),
            ('Gemini20Api', 'gemini-2.0-flash'),
            ('O3MiniApi', 'o3-mini'),
            ('Claude37SonnetApi', 'claude-3-7-sonnet-20250219'),
        ]
        for api_field, model in cs_routes:
            url = cs.get(api_field, '')
            if not url: continue
            base = url.rsplit('/chat/completions', 1)[0] if '/chat/completions' in url else url
            entries.append(
                f"- name: cloudsway-{model.replace('.', '')}\n"
                f"  priority: 5\n"
                f"  base-url: {base}\n"
                f"  backup-duration-ms: 35000\n"
                f"  api-key-entries:\n    - api-key: {yq(cs_key)}\n"
                f"  models:\n    - name: {model}"
            )
    return entries


def generate_shubiaobiao(nd):
    """Priority 3. Shubiaobiao (chat + responses).
    Models from router.json Router=shubiaobiao + hardcoded chat models."""
    entries = []
    sb = nd.us.get('config_us_k8s.json', {}).get('Shubiaobiao', {})
    if sb.get('Key') and sb.get('Api'):
        # Dynamic models from router + fixed chat models
        sb_models = extract_models_by_router(nd, 'shubiaobiao')
        chat_models = list(set(['gpt-4o', 'claude-sonnet-4.6'] + sb_models))
        ml = '\n'.join(f"    - name: {m}" for m in sorted(chat_models))
        entries.append(
            f"- name: shubiaobiao-chat\n"
            f"  priority: 3\n"
            f"  base-url: {sb['Api'].rsplit('/chat/completions',1)[0]}\n"
            f"  backup-duration-ms: 38000\n"
            f"  api-key-entries:\n    - api-key: {yq(sb['Key'])}\n"
            f"  models:\n{ml}"
        )
    if sb.get('Key') and sb.get('ApiResponse'):
        entries.append(
            f"- name: shubiaobiao-responses\n"
            f"  priority: 3\n"
            f"  base-url: {sb['ApiResponse'].rsplit('/responses',1)[0]}\n"
            f"  responses-format: true\n"
            f"  backup-duration-ms: 38000\n"
            f"  api-key-entries:\n    - api-key: {yq(sb['Key'])}\n"
            f"  models:\n    - name: gpt-5-pro"
        )
    return entries


def generate_media_direct(nd):
    """Priority 7. Direct media providers from config_us_k8s.json.
    CPA's task_handler.go has adaptors for these platforms, and media_proxy.go
    handles embedding/tts/whisper/image. Config entries tell CPA which upstream
    keys + URLs to use. RE confirmed AudioShake is client-direct, skip.
    """
    entries = []
    cu = nd.us.get('config_us_k8s.json', {})

    # KlingAi (video + image)
    kling = cu.get('KlingAi', {})
    if kling.get('Ak') and kling.get('Sk'):
        ak, sk = kling['Ak'], kling['Sk']
        entries.append(
            f"- name: kling-video-direct\n"
            f"  priority: 7\n"
            f"  base-url: https://api.klingai.com/v1/videos/text2video\n"
            f"  api-key-entries:\n    - api-key: {yq(ak)}\n"
            f"  models:\n    - name: kling-v3.0\n"
            f"  headers:\n    X-Kling-Ak: {yq(ak)}\n    X-Kling-Sk: {yq(sk)}"
        )
        entries.append(
            f"- name: kling-image-direct\n"
            f"  priority: 7\n"
            f"  base-url: https://api.klingai.com/v1/images/generations\n"
            f"  api-key-entries:\n    - api-key: {yq(ak)}\n"
            f"  models:\n    - name: kling-image\n"
            f"  headers:\n    X-Kling-Ak: {yq(ak)}\n    X-Kling-Sk: {yq(sk)}"
        )

    # VolEngine Seedream (image)
    vol = cu.get('VolEngine', {})
    if vol.get('Key'):
        entries.append(
            f"- name: seedream-direct\n"
            f"  priority: 7\n"
            f"  base-url: https://ark.cn-beijing.volces.com/api/v3/images/generations\n"
            f"  api-key-entries:\n    - api-key: {yq(vol['Key'])}\n"
            f"  models:\n    - name: doubao-seedream-4.0\n    - name: doubao-seedream-4.5\n    - name: doubao-seedream-5.0"
        )

    # Vidu (video)
    vidu = cu.get('Vidu', {})
    if vidu.get('Token'):
        entries.append(
            f"- name: vidu-direct\n"
            f"  priority: 7\n"
            f"  base-url: https://api.vidu.com/ent/v2/text2video\n"
            f"  api-key-entries:\n    - api-key: {yq(vidu['Token'])}\n"
            f"  models:\n    - name: vidu-q2\n    - name: vidu-q2-pro\n    - name: vidu-q2-turbo"
        )

    # MiniMax (video)
    mm = cu.get('MiniMax', {})
    if mm.get('Key'):
        url = mm.get('VideoGeneration', 'https://api.minimax.io/v1/video_generation')
        entries.append(
            f"- name: minimax-video-direct\n"
            f"  priority: 7\n"
            f"  base-url: {url}\n"
            f"  api-key-entries:\n    - api-key: {yq(mm['Key'])}\n"
            f"  models:\n    - name: minimax-video"
        )

    # Suno (music)
    suno = cu.get('Suno', {})
    if suno.get('Key'):
        entries.append(
            f"- name: suno-direct\n"
            f"  priority: 7\n"
            f"  base-url: https://apibox.erweima.ai/api/v1\n"
            f"  api-key-entries:\n    - api-key: {yq(suno['Key'])}\n"
            f"  models:\n    - name: suno-v4"
        )

    # Fal (image)
    fal = cu.get('Fal', {})
    if fal.get('Key'):
        entries.append(
            f"- name: fal-direct\n"
            f"  priority: 7\n"
            f"  base-url: https://queue.fal.run\n"
            f"  api-key-entries:\n    - api-key: {yq(fal['Key'])}\n"
            f"  models:\n    - name: fal-ai"
        )

    # ApiCoco (video)
    ac = cu.get('ApiCoco', {})
    if ac.get('Key'):
        url = ac.get('VideoGeneration', 'https://apicoco.com/v1/video/generations')
        entries.append(
            f"- name: apicoco-direct\n"
            f"  priority: 7\n"
            f"  base-url: {url}\n"
            f"  api-key-entries:\n    - api-key: {yq(ac['Key'])}\n"
            f"  models:\n    - name: apicoco"
        )

    return entries


def generate_gpt_proxy_media(nd):
    """Priority 1. gpt-proxy media fallback (21 routes via LLM_API tunnel)."""
    gkey = 'gpt-5739025d9e453d483a6595f95591'
    llm_api = 'http://127.0.0.1:7000'
    routes = [
        ('skywork-veo', f'{llm_api}/gpt-proxy/google/veo', ['veo-3', 'veo-3.1']),
        ('skywork-imagen', f'{llm_api}/gpt-proxy/google/imagen', ['imagen-4']),
        ('skywork-sora', f'{llm_api}/gpt-proxy/azure/sora', ['sora-2', 'sora-2-pro']),
        ('skywork-azure-image', f'{llm_api}/gpt-proxy/azure/imagen', ['gpt-image-1']),
        ('skywork-tts', f'{llm_api}/gpt-proxy/azure/tts', ['tts-1', 'tts-1-hd']),
        ('skywork-kling', f'{llm_api}/gpt-proxy/klingai/text2video/submit', ['kling-v3.0', 'kling-v3-omni']),
        ('skywork-seedance', f'{llm_api}/gpt-proxy/volengine/video/submit', ['seedance-2.0', 'seedance-1.5-pro']),
        ('skywork-fal', f'{llm_api}/gpt-proxy/fal/generate', ['fal-ai']),
        ('skywork-seedream', f'{llm_api}/gpt-proxy/volengine/image/generate', ['doubao-seedream-4.0', 'doubao-seedream-5.0']),
        ('skywork-gemini-image', f'{llm_api}/gpt-proxy/google/imagen', ['gemini-2.5-flash-image', 'gemini-3.1-flash-image-preview']),
        ('skywork-vidu', f'{llm_api}/gpt-proxy/vidu/text2video/submit', ['vidu-q2', 'vidu-q2-pro']),
        ('skywork-minimax', f'{llm_api}/gpt-proxy/minimax/video/submit', ['minimax-video']),
        ('skywork-suno', f'{llm_api}/gpt-proxy/suno/generate', ['suno-v4', 'mureka-7.5']),
        ('skywork-apicoco', f'{llm_api}/gpt-proxy/apicoco/video/submit', ['apicoco']),
        ('skywork-audioshake', f'{llm_api}/gpt-proxy/audioshake/separate', ['audioshake']),
        ('skywork-pixverse', f'{llm_api}/gpt-proxy/pixverse/video/submit', ['pixverse-v5.6']),
        ('skywork-wan', f'{llm_api}/gpt-proxy/volengine/wan/submit', ['wan-2.6']),
        ('skywork-skyreels', f'{llm_api}/gpt-proxy/skywork/video/submit', ['skyreels']),
        ('skywork-kolors', f'{llm_api}/gpt-proxy/fal/kolors', ['kolors-virtual-try-on-v1-5']),
        ('skywork-jimeng', f'{llm_api}/gpt-proxy/volengine/image/jimeng', ['jimeng_t2i_v40']),
        ('skywork-kling-image', f'{llm_api}/gpt-proxy/klingai/image/submit', ['kling-image']),
    ]
    entries = []
    for name, url, models in routes:
        ml = '\n'.join(f"    - name: {m}" for m in models)
        entries.append(
            f"- name: {name}\n"
            f"  priority: 1\n"
            f"  base-url: {url}\n"
            f"  api-key-entries:\n    - api-key: {gkey}\n"
            f"  headers:\n    app_key: {gkey}\n"
            f"  models:\n{ml}"
        )
    return entries

# --- Main Logic ---

def main():
    import argparse
    parser = argparse.ArgumentParser()
    parser.add_argument('--date', default='2026-04-18')
    args = parser.parse_args()

    nd = NacosData(args.date)
    OUTPUT_DIR.mkdir(parents=True, exist_ok=True)

    sections = {
        'claude-api-key': [],
        'openai-compatibility': [],
        'gemini-api-key': [],
        'vertex-api-key': []
    }

    # model_registry: model_name -> [channel, channel, ...]
    model_registry = defaultdict(list)

    def register(channel, entries_list):
        """Extract model names from YAML entry text (only under 'models:' block)."""
        import re
        for entry in entries_list:
            in_models = False
            for line in entry.split('\n'):
                s = line.strip()
                if s == 'models:':
                    in_models = True
                elif in_models and s.startswith('- name:'):
                    model_registry[s.split('- name:',1)[1].strip()].append(channel)
                elif in_models and not s.startswith('-') and not s.startswith('alias:') and not s.startswith('model-id:') and s:
                    in_models = False

    # p10 Bedrock
    bedrock_entries = generate_bedrock(nd)
    sections['claude-api-key'].extend(bedrock_entries)
    for entry in bedrock_entries:
        import re
        ak = re.search(r'aws-access-key-id: (\S+)', entry)
        region = re.search(r'aws-region: (\S+)', entry)
        ch = f"Bedrock({ak.group(1)[:8]}/{region.group(1)})" if ak and region else "Bedrock"
        for m in re.findall(r'- name: (.+)', entry):
            model_registry[m.strip()].append(ch)
    # p10 Vertex (Claude -> claude-api-key, Gemini -> openai-compat)
    vc_claude, vc_oa = generate_vertex_claude(nd)
    sections['claude-api-key'].extend(vc_claude)
    register('VertexClaude', vc_claude)
    sections['openai-compatibility'].extend(vc_oa)
    register('VertexGemini', vc_oa)
    # p10 Vertex Xmind (XmindGoogleConf)
    vx = generate_vertex_xmind(nd)
    sections['openai-compatibility'].extend(vx)
    register('VertexXmind', vx)
    # p10 Gemini API keys
    gk = generate_gemini_keys(nd)
    sections['gemini-api-key'].extend(gk)
    register('GeminiKey', gk)
    # p10/p8 Azure
    az = generate_azure(nd)
    sections['openai-compatibility'].extend(az)
    register('Azure', az)
    # p9 OpenRouter (Nacos + Public)
    or_claude, or_oa = generate_openrouter(nd)
    sections['claude-api-key'].extend(or_claude)
    register('OpenRouter', or_claude)
    sections['openai-compatibility'].extend(or_oa)
    register('OpenRouter', or_oa)
    # p9 TaijiAI (Claude -> claude-api-key, Gemini -> openai-compat)
    tj_claude, tj_oa = generate_taijiai(nd)
    sections['claude-api-key'].extend(tj_claude)
    register('TaijiAI', tj_claude)
    sections['openai-compatibility'].extend(tj_oa)
    register('TaijiAI', tj_oa)
    # p8 MaaS Anthropic (MiniMax/VolEngine/Zhipu -> claude-api-key)
    maas = generate_maas(nd)
    sections['claude-api-key'].extend(maas)
    register('MaaS', maas)
    # p7 Third-party proxies (Polo/YesVG/YouBangGPT -> claude-api-key, Responses/Woyaochat -> openai-compat)
    tp_claude, tp_oa = generate_third_party_proxies(nd)
    sections['claude-api-key'].extend(tp_claude)
    register('Polo/YesVG/YouBang', tp_claude)
    sections['openai-compatibility'].extend(tp_oa)
    register('Polo/Woyaochat', tp_oa)
    # p7 Media Direct
    md = generate_media_direct(nd)
    sections['openai-compatibility'].extend(md)
    register('MediaDirect', md)
    # p6 MaaS tier (Deepseek/Grok/DashScope/ApiCoco)
    mt = generate_maas_tier(nd)
    sections['openai-compatibility'].extend(mt)
    register('DashScope/Deepseek', mt)
    # p6 Kimi (Moonshot)
    km = generate_kimi(nd)
    sections['openai-compatibility'].extend(km)
    register('Kimi', km)
    # p5 Legacy (Silicon/DeepInfra/Sophnet/Cloudsway)
    lg = generate_legacy(nd)
    sections['openai-compatibility'].extend(lg)
    register('Legacy', lg)
    # p3 Shubiaobiao
    sb = generate_shubiaobiao(nd)
    sections['openai-compatibility'].extend(sb)
    register('Shubiaobiao', sb)
    # p2 Pools (Cookie + Singularity, ultra + plus)
    pl = generate_pools()
    sections['openai-compatibility'].extend(pl)
    register('CookiePool', pl)
    # p1 gpt-proxy media fallback (21 routes)
    gpm = generate_gpt_proxy_media(nd)
    sections['openai-compatibility'].extend(gpm)
    register('gpt-proxy', gpm)

    # Reuse secrets from existing config if present
    _load_existing_secrets(str(OUTPUT_DIR / 'cpa-new-config.yaml'))

    # Global Config (aligned with v1 base_config + routing)
    conf_lines = [
        'host: ""',
        'port: 8318',
        'proxy-url: socks5://127.0.0.1:1080',
        'skywork-smart-fallback: true',
        'skywork-throttle-delay-seconds: 0',
        'tls:',
        '  enable: false',
        'remote-management:',
        '  allow-remote: true',
        '  secret-key: "$2a$10$ASf7hOgqBIpBnZwQIxCjEuEppGbCNNofzpk.PmkC3TQVP3TDyW/Pm"',
        '  disable-control-panel: false',
        '  panel-github-repository: https://github.com/hhsw2015/Cli-Proxy-API-Management-Center',
        'auth-dir: /home/azureuser/.cli-proxy-api',
        'archive-failed-auth: true',
        'api-keys:',
        '  - sk-1Fna1Bm7umJdI5ADt',  # v1 legacy + v2 production (business key)
        'debug: false',
        'pprof:',
        '  enable: false',
        '  addr: 127.0.0.1:8316',
        'commercial-mode: false',
        'incognito-browser: true',
        'logging-to-file: false',
        'error-logs-max-files: 10',
        'usage-statistics-enabled: true',
        'force-model-prefix: false',
        'passthrough-headers: false',
        'request-retry: 2',
        'max-retry-credentials: 0',
        'max-retry-interval: 60',
        'quota-exceeded:',
        '  switch-project: true',
        '  switch-preview-model: true',
        'routing:',
        '  strategy: round-robin',
        '  latency-aware: true',
        '  session-affinity: true',
        f'  error-pass-list: {json.dumps(ERROR_LIST_PASS)}',
        f'  non-retryable-substrings: {json.dumps(NON_RETRYABLE)}',
        'pool-manager:',
        '  size: 0',
        '  active-idle-scan-interval-seconds: 1800',
        '  reserve-scan-interval-seconds: 300',
        '  limit-scan-interval-seconds: 21600',
        '  provider: codex',
        'ws-auth: false',
        'streaming:',
        '  keepalive-seconds: 15',
        '  bootstrap-retries: 5',
        'request-log: true',
        'refusal-shield:',
        '  enabled: false',
        '',
        '# ====== Commercial Layer (Sub2API) ======',
        '# Requires: go build -tags commercial,embed + PostgreSQL + Redis',
        f'commercial:',
        f'  enabled: true',
        f'  sub2api:',
        f'    database:',
        f'      host: localhost',
        f'      port: 5432',
        f'      user: sub2api',
        f'      password: "{_get_db_password()}"',
        f'      dbname: sub2api',
        f'      sslmode: disable',
        f'    redis:',
        f'      addr: localhost:6379',
        f'    jwt:',
        f'      secret: "{_get_jwt_secret()}"',
        f'    server:',
        f'      mode: release',
        f'    log:',
        f'      level: info',
        f'      format: json',
    ]

    # Extract priority from entry text for sort/stats
    def get_prio(entry):
        try:
            return int(entry.split('priority: ')[1].split('\n')[0].strip())
        except Exception:
            return 0

    # Stats by priority
    prio_counts = defaultdict(int)
    for e in sections['openai-compatibility']:
        prio_counts[get_prio(e)] += 1
    for e in sections['claude-api-key']:
        prio_counts[10] += 1
    for e in sections['gemini-api-key']:
        prio_counts[10] += 1

    # Sort openai-compat by priority desc
    sections['openai-compatibility'].sort(key=get_prio, reverse=True)

    output_path = OUTPUT_DIR / 'cpa-new-config.yaml'
    with open(output_path, 'w') as f:
        f.write('\n'.join(conf_lines) + '\n\n')
        if sections['claude-api-key']:
            f.write('# ====== claude-api-key (Bedrock p10) ======\n')
            f.write('claude-api-key:\n')
            f.write(indent('\n'.join(sections['claude-api-key'])) + '\n\n')
        if sections['gemini-api-key']:
            f.write('# ====== gemini-api-key (p10) ======\n')
            f.write('gemini-api-key:\n')
            f.write(indent('\n'.join(sections['gemini-api-key'])) + '\n\n')
        if sections['openai-compatibility']:
            f.write('# ====== openai-compatibility (sorted by priority desc) ======\n')
            f.write('openai-compatibility:\n')
            f.write(indent('\n'.join(sections['openai-compatibility'])) + '\n\n')

    # Generate rollback script
    rollback_path = OUTPUT_DIR / 'rollback.sh'
    with open(rollback_path, 'w') as f:
        f.write('#!/bin/bash\n')
        f.write('# Rollback cpa-new-config.yaml to prior version.\n')
        f.write('# Manual trigger only.\n')
        f.write('set -e\n')
        f.write('VPS=azureuser@4.151.241.30\n')
        f.write('SSH_KEY=~/Downloads/pikapk3219_vps_key.pem\n')
        f.write('echo "Rolling back to cpa-new-config.yaml.bak..."\n')
        f.write('ssh -i $SSH_KEY $VPS "cd ~/CLIProxyAPIPlus-new && cp cpa-new-config.yaml.bak cpa-new-config.yaml && tmux send-keys -t cpa-new C-c && sleep 2 && tmux send-keys -t cpa-new \\"./cli-proxy-api-plus -config cpa-new-config.yaml\\" Enter"\n')
        f.write('echo "Rollback complete. Tail logs with: ssh -i $SSH_KEY $VPS \'tmux capture-pane -t cpa-new -p\'"\n')
    os.chmod(rollback_path, 0o755)

    # Summary
    total = sum(prio_counts.values())
    print(f"\n✅ Generated: {output_path}")
    print(f"✅ Rollback: {rollback_path}")
    print(f"\nEntry distribution by priority:")
    for p in sorted(prio_counts.keys(), reverse=True):
        print(f"  p{p}: {prio_counts[p]} entries")
    print(f"\nTotal: {total} entries")
    print(f"  claude-api-key: {len(sections['claude-api-key'])}")
    print(f"  gemini-api-key: {len(sections['gemini-api-key'])}")
    print(f"  openai-compatibility: {len(sections['openai-compatibility'])}")

    # Model statistics -- per-model entry count by channel
    print_model_stats(model_registry, nd, sections)

def print_model_stats(model_channels, nd, sections):
    """Print channel x model pivot tables grouped by family."""
    import re

    # Normalize channel names: Bedrock(AKIAV7PM/us-east-1) -> group by AK prefix
    def norm_channel(ch):
        if ch.startswith('Bedrock('):
            ak = ch.split('(')[1].split('/')[0]
            if 'AKIAV7PM' in ak: return 'Bedrock AwsConf'
            elif 'AKIAW4LK' in ak or 'AKIAWVV7' in ak: return 'Bedrock XmindConf'
            elif 'AKIARA4X' in ak: return 'Bedrock MlxyConf'
            elif 'AKIA4V5S' in ak or 'AKIA5Y5R' in ak: return 'Bedrock XmindConf'
            return 'Bedrock ' + ak[:8]
        return ch

    # Build: normalized_channel -> model -> count
    table = defaultdict(lambda: defaultdict(int))
    for model, channels in model_channels.items():
        for ch in channels:
            table[norm_channel(ch)][model] += 1

    # Merge aliases: combine counts for models that are the same
    alias_map = {
        'claude-opus-4.7': 'claude-opus-4-7',
        'anthropic/claude-opus-4.7': 'claude-opus-4-7',
        'anthropic/claude-4.7-opus-20260416': 'claude-opus-4-7',
        'anthropic/claude-opus-4.6': 'claude-opus-4.6',
        'anthropic/claude-sonnet-4.6': 'claude-sonnet-4.6',
        'anthropic/claude-haiku-4.5': 'claude-haiku-4.5',
    }
    merged = defaultdict(list)
    for model, channels in model_channels.items():
        canonical = alias_map.get(model, model)
        merged[canonical].extend(channels)
    model_channels = merged

    # Key models per family (premium/flagship only)
    families = [
        ('Claude Opus', [
            'claude-opus-4-7', 'claude-opus-4.6',
            'claude-opus-4.5', 'claude-opus-4.1',
        ]),
        ('Claude Sonnet/Haiku', [
            'claude-sonnet-4.6', 'claude-sonnet-4.5',
            'claude-haiku-4.5',
        ]),
        ('GPT', [
            'gpt-5', 'gpt-5-pro', 'gpt-5.4', 'gpt-5.4-pro',
            'gpt-5.3-codex', 'gpt-5.2', 'gpt-5.2-codex',
        ]),
        ('o-series', ['o3-pro', 'o3', 'o4-mini']),
        ('Gemini', [
            'gemini-2.5-pro', 'gemini-3-pro-preview', 'gemini-3.1-pro-preview',
            'gemini-3-flash-preview',
        ]),
        ('Other Premium', [
            'grok-4', 'deepseek-reasoner', 'kimi-k2',
            'qwen3-max', 'glm-5',
        ]),
        ('Image/Video', [
            'gpt-image-1', 'gpt-image-2', 'sora-2', 'kling-v3.0', 'vidu-q2',
        ]),
    ]

    # Channel display order + descriptions
    ch_order = [
        'Bedrock XmindConf', 'Bedrock AwsConf', 'Bedrock MlxyConf',
        'VertexClaude', 'VertexGemini', 'VertexXmind',
        'GeminiKey', 'Azure', 'OpenRouter', 'TaijiAI', 'MaaS',
        'CookiePool', 'Polo/YesVG/YouBang', 'Polo/Woyaochat',
        'MediaDirect', 'DashScope/Deepseek', 'Legacy', 'Shubiaobiao', 'gpt-proxy',
    ]
    # Build dynamic descriptions from raw channel data
    ch_desc = {}
    # Count AKs and regions per Bedrock group
    bedrock_groups = defaultdict(lambda: {'aks': set(), 'regions': set()})
    for model, channels in model_channels.items():
        for ch in channels:
            if ch.startswith('Bedrock('):
                inner = ch[len('Bedrock('):-1]  # "AKIAV7PM/us-east-1"
                ak, region = inner.split('/', 1) if '/' in inner else (inner, '')
                group = norm_channel(ch)
                bedrock_groups[group]['aks'].add(ak)
                if region:
                    bedrock_groups[group]['regions'].add(region)
    for group, info in bedrock_groups.items():
        ch_desc[group] = f"{len(info['aks'])} AK x {len(info['regions'])} regions"

    # Count keys for other channels
    key_counts = defaultdict(int)
    for section_entries in [sections.get('gemini-api-key', []), sections.get('openai-compatibility', [])]:
        for entry in section_entries:
            if 'api-key-entries:' in entry:
                key_counts['keys'] += entry.count('- api-key:')

    # Gemini key count
    gk_count = len(sections.get('gemini-api-key', []))
    if gk_count: ch_desc['GeminiKey'] = f'{gk_count} API keys'

    # OpenRouter key count from entries
    or_keys = nd.get('keys-other.json', 'OpenRouter.KeyList', []) or []
    if or_keys: ch_desc['OpenRouter'] = f'{len(or_keys)} Nacos keys'

    # Count Azure entries
    az_count = sum(1 for e in sections.get('openai-compatibility', [])
                   if 'azure-' in e[:30].lower() or e.strip().startswith('- name: kb-'))
    if az_count: ch_desc['Azure'] = f'{az_count} deployments'

    # Count CookiePool pools
    pool_count = sum(1 for e in sections.get('openai-compatibility', [])
                     if 'cookie' in e[:50].lower() or 'singularity' in e[:50].lower())
    if pool_count: ch_desc['CookiePool'] = f'{pool_count} pools (ultra+plus)'

    # Static descriptions for channels that don't need dynamic data
    ch_desc.setdefault('VertexClaude', 'Google Cloud ADC')
    ch_desc.setdefault('VertexGemini', 'Google Cloud ADC')
    ch_desc.setdefault('VertexXmind', 'Xmind GCP projects')
    ch_desc.setdefault('TaijiAI', 'XP proxy (strip-beta)')
    ch_desc.setdefault('MaaS', 'MiniMax/VolEngine/Zhipu')
    ch_desc.setdefault('Polo/YesVG/YouBang', 'Anthropic Messages')
    ch_desc.setdefault('Polo/Woyaochat', 'Responses + Chat')
    ch_desc.setdefault('MediaDirect', 'Direct API keys')
    ch_desc.setdefault('DashScope/Deepseek', 'Aliyun DashScope')
    ch_desc.setdefault('Legacy', 'Silicon/DeepInfra/Cloudsway')
    ch_desc.setdefault('Shubiaobiao', 'chat + responses')
    ch_desc.setdefault('gpt-proxy', 'P1 fallback')

    print(f"\n{'='*80}")
    print(f"Model x Channel Statistics")
    print(f"{'='*80}")

    for family_name, key_models in families:
        # Only show models that actually exist in the config
        models = [m for m in key_models if m in model_channels]
        if not models:
            continue

        # Find channels that have any of these models
        relevant_chs = []
        for ch in ch_order:
            if any(table[ch].get(m, 0) > 0 for m in models):
                relevant_chs.append(ch)
        # Add unlisted channels
        for ch in sorted(table.keys()):
            if ch not in relevant_chs and any(table[ch].get(m, 0) > 0 for m in models):
                relevant_chs.append(ch)
        if not relevant_chs:
            continue

        # Column widths
        ch_width = max(max(len(ch) for ch in relevant_chs), len('Channel'), 6)
        col_widths = [max(len(m), 3) + 2 for m in models]

        # Description column width
        desc_width = max(max((len(ch_desc.get(ch, '')) for ch in relevant_chs), default=0), 4)
        has_desc = any(ch_desc.get(ch) for ch in relevant_chs)

        # Box drawing helpers
        def hline(left, mid, right, fill='─'):
            parts = [left + fill * (ch_width + 2)]
            for w in col_widths:
                parts.append(mid + fill * (w + 2))
            if has_desc:
                parts.append(mid + fill * (desc_width + 2))
            return ''.join(parts) + right

        def row_str(first, cells, desc=''):
            parts = [f'│ {first:<{ch_width}} │']
            for i, cell in enumerate(cells):
                parts.append(f' {cell:>{col_widths[i]}} │')
            if has_desc:
                parts.append(f' {desc:<{desc_width}} │')
            return ''.join(parts)

        # Print table
        print(f"\n  ■ {family_name}")
        print(hline('┌', '┬', '┐'))
        header_desc = 'Note' if has_desc else ''
        print(row_str('Channel', models, header_desc))
        print(hline('├', '┼', '┤'))

        for ch in relevant_chs:
            cells = []
            for m in models:
                v = table[ch].get(m, 0)
                cells.append(str(v) if v > 0 else '-')
            print(row_str(ch, cells, ch_desc.get(ch, '')))

        print(hline('├', '┼', '┤', '═'))
        totals = []
        for m in models:
            totals.append(str(sum(table[ch].get(m, 0) for ch in relevant_chs)))
        print(row_str('TOTAL', totals, ''))
        print(hline('└', '┴', '┘'))

    unique_models = len(model_channels)
    total_entries = sum(len(v) for v in model_channels.values())
    print(f"\n{'='*80}")
    print(f"Total: {unique_models} unique models, {total_entries} model-entry mappings")
    print(f"{'='*80}")


if __name__ == '__main__':
    main()
