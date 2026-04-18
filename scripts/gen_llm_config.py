#!/usr/bin/env python3
"""Generate CPA config.yaml from exported Skywork API keys.

Reads:
  keys-claude.json       -> claude-api-key (Bedrock)
  keys-azure.json        -> openai-compatibility (Azure)
  ALL_API_KEYS.json      -> openai-compatibility (Groq, Deepseek, OpenRouter)
  config_us_k8s.json     -> gemini-api-key (62 Gemini API keys)

Outputs (to --output-dir, default: scripts/generated/):
  cpa-new-config.yaml   (port 8318: Bedrock + Gemini + Azure + Groq + Deepseek + OpenRouter + Cookie Pool)
  cpa-old-config.yaml   (port 8317: Cookie Pool only)

IMPORTANT: Uses string concatenation for YAML output. NEVER yaml.dump.
This prevents: field reordering, bcrypt hash corruption, format changes.

Usage:
  python3 scripts/gen_llm_config.py                  # auto-detect latest date
  python3 scripts/gen_llm_config.py --date 2026-04-10  # use specific date
  python3 scripts/gen_llm_config.py --keys-dir /path   # override keys directory
"""

import json
import sys
from collections import defaultdict
from pathlib import Path

# --- File Paths ---

NACOS_BASE_DIR = '/Users/wowdd1/Dev/dvina-2api/artifacts/api-keys/us-nacos'
DEFAULT_OUTPUT_DIR = '/Users/wowdd1/Dev/CLIProxyAPIPlus/scripts/generated'
ALL_API_KEYS_FILE = '/Users/wowdd1/Dev/dvina-2api/artifacts/api-keys/ALL_API_KEYS.json'
CONFIG_US_FILE = '/Users/wowdd1/Dev/dvina-2api/artifacts/gpt-proxy/binary/config_us_k8s.json'


def resolve_keys_dir(date_str=None):
    """Resolve the keys directory. If date not specified, use the latest available."""
    base = Path(NACOS_BASE_DIR)
    if date_str:
        d = base / date_str
        if not d.exists():
            print(f"ERROR: {d} not found", file=sys.stderr)
            sys.exit(1)
        return d
    # Auto-detect: pick the latest date directory (YYYY-MM-DD format)
    dirs = sorted([p for p in base.iterdir() if p.is_dir() and len(p.name) == 10], reverse=True)
    if not dirs:
        print(f"ERROR: no date directories found in {base}", file=sys.stderr)
        sys.exit(1)
    print(f"Auto-detected latest keys: {dirs[0].name}")
    return dirs[0]

# --- Constants ---

# Bedrock sections to process (MlxyConf expired, skip it)
BEDROCK_SECTIONS = ['XmindConf', 'nova-micro']

# Azure config sections
AZURE_SECTIONS = ['AzureConf', 'KuanbangConf', 'AzureResponseConf', 'KuanbangResponseConf']

# Cookie Pool models (14 total)
COOKIE_POOL_MODELS = [
    'claude-haiku-4.5', 'claude-opus-4.5', 'claude-opus-4.6',
    'claude-sonnet-4.5', 'claude-sonnet-4.6', 'gpt-5-pro',
    'gpt-5.1', 'gpt-5.2', 'gpt-5.2-pro', 'gpt-5.3-codex',
    'gpt-5.4', 'gpt-5.4-pro', 'grok-4', 'o3-pro',
]
COOKIE_POOL_BASE_URL = 'https://desktop-llm.skywork.ai/skycowork_llm/v1/proxy'

# Singularity (Gemini via Cookie Pool)
SINGULARITY_BASE_URL = 'https://desktop-llm.skywork.ai/skycowork_llm/v1/proxy/chat/completions'
SINGULARITY_MODELS = [
    'gemini-2.5-pro', 'gemini-3-flash-preview', 'gemini-3.1-pro-preview',
]

# Groq models
GROQ_MODELS = [
    'llama-3.3-70b-versatile', 'llama-3.1-8b-instant',
    'mixtral-8x7b-32768', 'gemma2-9b-it',
]

# Deepseek models
DEEPSEEK_MODELS = ['deepseek-chat', 'deepseek-reasoner']

# OpenRouter models (broad set)
OPENROUTER_MODELS = [
    'claude-sonnet-4.6', 'claude-opus-4.6', 'claude-4-sonnet',
    'claude-haiku-4.5', 'gemini-2.5-pro', 'gpt-oss-120b',
    'qwen3-coder-480b-a35b-instruct',
]

# TaijiAI (XP provider) - Claude via Anthropic Messages API, Gemini via OpenAI compat
TAIJIAI_CLAUDE_MODELS = [
    ('claude-opus-4-7', 'claude-opus-4.7'),
    ('claude-opus-4-6', 'claude-opus-4.6'),
    ('claude-sonnet-4-6', 'claude-sonnet-4.6'),
]
TAIJIAI_GEMINI_MODELS = [
    'gemini-2.5-pro', 'gemini-2.5-flash', 'gemini-2.5-flash-lite',
    'gemini-2.5-flash-image', 'gemini-3-pro-preview', 'gemini-3-flash-preview',
    'gemini-3.1-pro-preview',
]


# --- YAML Helpers ---

def yq(s):
    """Quote a YAML string value if it contains special characters."""
    if not isinstance(s, str):
        return str(s)
    if s == '':
        return '""'
    special = set('+/\\$#:{}[]&!|>%@`')
    if any(c in special for c in s) or s.startswith(('-', '*', '?', '"', "'")):
        escaped = s.replace('\\', '\\\\').replace('"', '\\"')
        return f'"{escaped}"'
    return s


def extract_arn_region(arn):
    """Extract region from ARN: arn:aws:bedrock:REGION:ACCOUNT:..."""
    parts = arn.split(':')
    return parts[3] if len(parts) >= 4 else ''


# --- Bedrock ---

def generate_bedrock_entries(claude_data):
    """keys-claude.json -> claude-api-key YAML.

    Group by (AK, region) and merge ALL models for that AK+region.
    This is critical because resolveConfigClaudeKey matches by AK only and
    returns the FIRST matching entry. If we split into many small entries
    (per index), only the first entry's models get registered for all auths
    with that AK, causing most models to be invisible to the scheduler.
    """
    groups = {}
    all_bedrock_models = set()

    for section in BEDROCK_SECTIONS:
        if section not in claude_data:
            continue
        section_data = claude_data[section]
        if not isinstance(section_data, dict):
            continue

        for model_name, endpoints in section_data.items():
            if model_name.endswith('_bak') or not isinstance(endpoints, list):
                continue
            for ep in endpoints:
                ak = ep.get('Ak', '').strip()
                sk = ep.get('Sk', '').strip()
                region = ep.get('Region', 'us-east-1').strip()
                arn = ep.get('ModelId', '').strip()
                if not ak or not arn:
                    continue
                all_bedrock_models.add(model_name)
                arn_region = extract_arn_region(arn)
                if arn_region and arn_region != region:
                    print(f"  WARN: ARN/region mismatch {model_name}: ep={region} ARN={arn_region}", file=sys.stderr)
                    region = arn_region
                key = (ak, region)
                if key not in groups:
                    groups[key] = {'sk': sk, 'region': region, 'models': {}}
                if model_name not in groups[key]['models']:
                    groups[key]['models'][model_name] = arn

    normalized_bedrock = {m.replace('.', '-') for m in all_bedrock_models} | all_bedrock_models
    taijiai_only = []
    for n, a in TAIJIAI_CLAUDE_MODELS:
        if n not in normalized_bedrock and a not in all_bedrock_models:
            taijiai_only.extend([n, a])

    lines = ['claude-api-key:']
    count = 0
    for (ak, region), info in sorted(groups.items()):
        if not info['models']:
            continue
        count += 1
        lines.append(f'  - aws-access-key-id: {ak}')
        lines.append(f'    aws-secret-access-key: {yq(info["sk"])}')
        lines.append(f'    aws-region: {info["region"]}')
        lines.append(f'    priority: 10')
        if taijiai_only:
            lines.append(f'    excluded-models:')
            for em in taijiai_only:
                lines.append(f'      - {em}')
        lines.append(f'    models:')
        for mname in sorted(info['models']):
            lines.append(f'      - name: {mname}')
            lines.append(f'        model-id: {yq(info["models"][mname])}')
    return '\n'.join(lines), count


# --- Gemini API Keys ---

# Gemini models: default registry models + additional image models not in registry
GEMINI_EXTRA_MODELS = [
    'gemini-2.5-flash-image',
    'gemini-2.5-flash-image-preview',
    'gemini-3.1-flash-preview',
    'gemini-3.1-pro-preview-customtools',
]


def generate_gemini_keys(config_us_data):
    """config_us_k8s.json -> gemini-api-key YAML.

    When GEMINI_EXTRA_MODELS is non-empty, adds explicit models list to each
    key entry so the extra models get registered. The default registry models
    are included automatically by CPA when no models field is present, but
    adding models overrides the default, so we must list them all.
    """
    keys = config_us_data.get('Google', {}).get('Keys', [])
    if not keys:
        return '', 0
    lines = ['gemini-api-key:']
    if GEMINI_EXTRA_MODELS:
        # Must include default registry models + extras since models field overrides default
        all_models = [
            'gemini-2.5-flash', 'gemini-2.5-flash-lite', 'gemini-2.5-pro',
            'gemini-3-flash-preview', 'gemini-3-pro-preview', 'gemini-3-pro-image-preview',
            'gemini-3.1-flash-lite-preview', 'gemini-3.1-flash-image-preview', 'gemini-3.1-pro-preview',
        ] + GEMINI_EXTRA_MODELS
        models_lines = '\n'.join(f'      - name: {m}' for m in all_models)
        for k in keys:
            lines.append(f'  - api-key: {k}')
            lines.append(f'    models:')
            lines.append(f'{models_lines}')
    else:
        for k in keys:
            lines.append(f'  - api-key: {k}')
    return '\n'.join(lines), len(keys)


# --- Azure ---

def generate_azure_entries(azure_data):
    """keys-azure.json -> openai-compatibility entries."""
    entries = []
    name_counter = defaultdict(int)

    for conf_name in AZURE_SECTIONS:
        conf = azure_data.get(conf_name, {})
        if not isinstance(conf, dict):
            continue
        is_response = 'Response' in conf_name
        is_kuanbang = 'Kuanbang' in conf_name
        conf_prefix = 'kb' if is_kuanbang else 'azure'
        if is_response:
            conf_prefix += '-resp'

        for model_name in sorted(conf.keys()):
            if model_name.endswith('_bak') or not isinstance(conf[model_name], list):
                continue
            for i, ep in enumerate(conf[model_name]):
                api_url = ep.get('Api', '').strip()
                key = ep.get('Key', '').strip()
                if not api_url or not key:
                    continue
                safe_model = model_name.replace('.', '').replace(' ', '')
                name_counter[f'{conf_prefix}-{safe_model}'] += 1
                entry_name = f'{conf_prefix}-{safe_model}-{name_counter[f"{conf_prefix}-{safe_model}"]}'
                priority = 8 if (is_kuanbang or is_response or i > 0) else 10
                entries.append(
                    f'  - name: {entry_name}\n'
                    f'    priority: {priority}\n'
                    f'    base-url: {api_url}\n'
                    f'    api-key-entries:\n'
                    f'      - api-key: {yq(key)}\n'
                    f'    models:\n'
                    f'      - name: {model_name}\n'
                    f'    headers:\n'
                    f'      api-key: {yq(key)}'
                )
    return entries


# --- Groq ---

def generate_groq_entries(all_keys):
    """ALL_API_KEYS.json -> groq openai-compatibility entries."""
    groq_keys = [x for x in all_keys if x.get('provider') == 'groq' and x.get('status') == 'active']
    entries = []
    models_lines = '\n'.join(f'      - name: {m}' for m in GROQ_MODELS)
    for i, item in enumerate(groq_keys, 1):
        k = item['key']
        entries.append(
            f'  - name: groq-{i:02d}\n'
            f'    priority: 8\n'
            f'    base-url: https://api.groq.com/openai/v1\n'
            f'    api-key-entries:\n'
            f'      - api-key: {yq(k)}\n'
            f'    models:\n'
            f'{models_lines}'
        )
    return entries


# --- Deepseek ---

def generate_deepseek_entries(all_keys):
    """ALL_API_KEYS.json -> deepseek openai-compatibility entries."""
    ds_keys = [x for x in all_keys if x.get('provider') == 'deepseek' and x.get('status') == 'active']
    entries = []
    models_lines = '\n'.join(f'      - name: {m}' for m in DEEPSEEK_MODELS)
    for i, item in enumerate(ds_keys, 1):
        k = item['key']
        entries.append(
            f'  - name: deepseek-{i:02d}\n'
            f'    priority: 10\n'
            f'    base-url: https://api.deepseek.com/v1\n'
            f'    api-key-entries:\n'
            f'      - api-key: {yq(k)}\n'
            f'    models:\n'
            f'{models_lines}'
        )
    return entries


# --- OpenRouter ---

def generate_openrouter_entries(all_keys):
    """ALL_API_KEYS.json -> openrouter openai-compatibility entry (multi-key)."""
    or_keys = [x for x in all_keys if x.get('provider') == 'openrouter']
    if not or_keys:
        return []
    models_lines = '\n'.join(f'      - name: {m}' for m in OPENROUTER_MODELS)
    key_lines = '\n'.join(f'      - api-key: {yq(x["key"])}' for x in or_keys)
    entry = (
        f'  - name: openrouter\n'
        f'    priority: 6\n'
        f'    base-url: https://openrouter.ai/api/v1\n'
        f'    api-key-entries:\n'
        f'{key_lines}\n'
        f'    models:\n'
        f'{models_lines}'
    )
    return [entry]


# --- Other Direct Providers (from config_us_k8s.json) ---

def generate_direct_provider_entries(config_us_data):
    """config_us_k8s.json -> siliconflow/nebius/deepinfra/dashscope/grok entries."""
    entries = []

    # SiliconFlow
    sk_key = config_us_data.get('Silicon', {}).get('Key', '')
    if sk_key:
        entries.append(
            f'  - name: siliconflow-direct\n'
            f'    priority: 8\n'
            f'    base-url: https://api.siliconflow.cn/v1\n'
            f'    api-key-entries:\n'
            f'      - api-key: {yq(sk_key)}\n'
            f'    models:\n'
            f'      - name: qwen3-coder'
        )

    # Nebius
    nb_key = config_us_data.get('Nebius', {}).get('Key', '')
    if nb_key:
        entries.append(
            f'  - name: nebius-direct\n'
            f'    priority: 8\n'
            f'    base-url: https://api.studio.nebius.ai/v1\n'
            f'    api-key-entries:\n'
            f'      - api-key: {yq(nb_key)}\n'
            f'    models:\n'
            f'      - name: deepseek-r1\n'
            f'      - name: qwen3-30b'
        )

    # DeepInfra
    di_key = config_us_data.get('DeepInfra', {}).get('Key', '')
    if di_key:
        entries.append(
            f'  - name: deepinfra-direct\n'
            f'    priority: 8\n'
            f'    base-url: https://api.deepinfra.com/v1/openai\n'
            f'    api-key-entries:\n'
            f'      - api-key: {yq(di_key)}\n'
            f'    models:\n'
            f'      - name: mistral-large'
        )

    # DashScope (Aliyun)
    ds_key = config_us_data.get('Aliyun', {}).get('Key', '')
    if ds_key:
        entries.append(
            f'  - name: dashscope-direct\n'
            f'    priority: 8\n'
            f'    base-url: https://dashscope.aliyuncs.com/compatible-mode/v1\n'
            f'    api-key-entries:\n'
            f'      - api-key: {yq(ds_key)}\n'
            f'    models:\n'
            f'      - name: qwen-max\n'
            f'      - name: qwen3-235b-a22b\n'
            f'      - name: qwen3-coder\n'
            f'      - name: qwen3-next-80b-a3b-instruct\n'
            f'      - name: kimi-k2.5'
        )

    # Grok-4 via Xmind Azure
    xmind = config_us_data.get('Xmind', {})
    grok4_url = xmind.get('Grok4', '')
    grok_key = xmind.get('Key', '')
    if grok4_url and grok_key:
        entries.append(
            f'  - name: azure-grok4\n'
            f'    priority: 10\n'
            f'    base-url: {grok4_url}\n'
            f'    api-key-entries:\n'
            f'      - api-key: {yq(grok_key)}\n'
            f'    models:\n'
            f'      - name: grok-4\n'
            f'    headers:\n'
            f'      api-key: {yq(grok_key)}'
        )

    return entries


# --- gpt-proxy Media Fallback ---

GPT_PROXY_APP_KEY = 'gpt-5739025d9e453d483a6595f95591'

def generate_gpt_proxy_fallback_entries():
    """gpt-proxy media fallback entries via Ghost Pod LLM API tunnel.

    Ghost Pod slot 0 -> VPS2:7000 (see skywork-ghost-pod.md)
    The LLM API (gpt_tunnel) proxies to gpt-proxy inside VPC.
    """
    entries = []
    k = GPT_PROXY_APP_KEY

    # Ghost Pod slot 0: VPS2:7000 -> gpt-proxy in VPC
    LLM_API = 'http://127.0.0.1:7000'

    media_routes = [
        ('skywork-veo', 5, f'{LLM_API}/gpt-proxy/google/veo', ['veo-3', 'veo-3.1', 'veo-3.1-fast']),
        ('skywork-imagen', 5, f'{LLM_API}/gpt-proxy/google/imagen', ['imagen-4']),
        ('skywork-sora', 5, f'{LLM_API}/gpt-proxy/azure/sora', ['sora-2', 'sora-2-pro']),
        ('skywork-azure-image', 5, f'{LLM_API}/gpt-proxy/azure/imagen', ['gpt-image-1']),
        ('skywork-tts', 5, f'{LLM_API}/gpt-proxy/azure/tts', ['tts-1', 'tts-1-hd']),
        ('skywork-kling', 5, f'{LLM_API}/gpt-proxy/klingai/text2video/submit', ['kling-v3.0', 'kling-v3-omni', 'kling-video-o1']),
        ('skywork-seedance', 5, f'{LLM_API}/gpt-proxy/volengine/video/submit', ['seedance-2.0', 'seedance-2.0-fast', 'seedance-1.5-pro']),
        ('skywork-fal', 5, f'{LLM_API}/gpt-proxy/fal/generate', ['fal-ai']),
        ('skywork-seedream', 5, f'{LLM_API}/gpt-proxy/volengine/image/generate', ['doubao-seedream-4.0', 'doubao-seedream-4.5', 'doubao-seedream-5.0']),
        ('skywork-gemini-image', 5, f'{LLM_API}/gpt-proxy/google/imagen', ['gemini-2.5-flash-image', 'gemini-3-pro-image-preview', 'gemini-3.1-flash-image-preview']),
        ('skywork-kling-image', 5, f'{LLM_API}/gpt-proxy/klingai/image/submit', ['kling-image']),
        ('skywork-vidu', 5, f'{LLM_API}/gpt-proxy/vidu/text2video/submit', ['vidu-q2', 'vidu-q2-pro', 'vidu-q2-turbo']),
        ('skywork-minimax', 5, f'{LLM_API}/gpt-proxy/minimax/video/submit', ['minimax-video']),
        ('skywork-suno', 5, f'{LLM_API}/gpt-proxy/suno/generate', ['suno-v4', 'mureka-7.5']),
        ('skywork-apicoco', 5, f'{LLM_API}/gpt-proxy/apicoco/video/submit', ['apicoco']),
        ('skywork-audioshake', 5, f'{LLM_API}/gpt-proxy/audioshake/separate', ['audioshake']),
        ('skywork-pixverse', 5, f'{LLM_API}/gpt-proxy/pixverse/video/submit', ['pixverse-v5.6']),
        ('skywork-wan', 5, f'{LLM_API}/gpt-proxy/volengine/wan/submit', ['wan-2.6']),
        ('skywork-skyreels', 5, f'{LLM_API}/gpt-proxy/skywork/video/submit', ['skyreels']),
        ('skywork-kolors', 5, f'{LLM_API}/gpt-proxy/fal/kolors', ['kolors-virtual-try-on-v1-5']),
        ('skywork-jimeng', 5, f'{LLM_API}/gpt-proxy/volengine/image/jimeng', ['jimeng_t2i_v40']),
    ]

    for name, priority, url, models in media_routes:
        models_lines = '\n'.join(f'      - name: {m}' for m in models)
        entries.append(
            f'  - name: {name}\n'
            f'    priority: {priority}\n'
            f'    base-url: {url}\n'
            f'    api-key-entries:\n'
            f'      - api-key: {k}\n'
            f'    models:\n'
            f'{models_lines}\n'
            f'    headers:\n'
            f'      app_key: {k}'
        )

    return entries


# --- Direct Media Providers (from config_us_k8s.json) ---

def generate_media_direct_entries(config_us_data):
    """config_us_k8s.json -> direct media provider openai-compatibility entries.

    These provide direct API access to media providers, higher priority than
    gpt-proxy fallback (P7 vs P5). Only generated when keys are available.
    """
    entries = []

    # KlingAi (video + image generation)
    kling = config_us_data.get('KlingAi', {})
    kling_ak = kling.get('Ak', '')
    kling_sk = kling.get('Sk', '')
    if kling_ak and kling_sk:
        entries.append(
            f'  - name: kling-video-direct\n'
            f'    priority: 7\n'
            f'    base-url: https://api.klingai.com/v1/videos/text2video\n'
            f'    api-key-entries:\n'
            f'      - api-key: {yq(kling_ak)}\n'
            f'    models:\n'
            f'      - name: kling-v3.0\n'
            f'    headers:\n'
            f'      X-Kling-Ak: {yq(kling_ak)}\n'
            f'      X-Kling-Sk: {yq(kling_sk)}'
        )
        entries.append(
            f'  - name: kling-image-direct\n'
            f'    priority: 7\n'
            f'    base-url: https://api.klingai.com/v1/images/generations\n'
            f'    api-key-entries:\n'
            f'      - api-key: {yq(kling_ak)}\n'
            f'    models:\n'
            f'      - name: kling-image\n'
            f'    headers:\n'
            f'      X-Kling-Ak: {yq(kling_ak)}\n'
            f'      X-Kling-Sk: {yq(kling_sk)}'
        )

    # VolEngine Seedream (image generation)
    volengine = config_us_data.get('VolEngine', {})
    vol_key = volengine.get('Key', '')
    if vol_key:
        entries.append(
            f'  - name: seedream-direct\n'
            f'    priority: 7\n'
            f'    base-url: https://ark.cn-beijing.volces.com/api/v3/images/generations\n'
            f'    api-key-entries:\n'
            f'      - api-key: {yq(vol_key)}\n'
            f'    models:\n'
            f'      - name: doubao-seedream-4.0\n'
            f'      - name: doubao-seedream-4.5\n'
            f'      - name: doubao-seedream-5.0'
        )

    # Vidu (video generation)
    vidu = config_us_data.get('Vidu', {})
    vidu_token = vidu.get('Token', '')
    if vidu_token:
        entries.append(
            f'  - name: vidu-direct\n'
            f'    priority: 7\n'
            f'    base-url: https://api.vidu.com/ent/v2/text2video\n'
            f'    api-key-entries:\n'
            f'      - api-key: {yq(vidu_token)}\n'
            f'    models:\n'
            f'      - name: vidu-q2\n'
            f'      - name: vidu-q2-pro\n'
            f'      - name: vidu-q2-turbo'
        )

    # MiniMax (video generation)
    minimax = config_us_data.get('MiniMax', {})
    minimax_key = minimax.get('Key', '')
    if minimax_key:
        entries.append(
            f'  - name: minimax-direct\n'
            f'    priority: 7\n'
            f'    base-url: {minimax.get("VideoGeneration", "https://api.minimax.io/v1/video_generation")}\n'
            f'    api-key-entries:\n'
            f'      - api-key: {yq(minimax_key)}\n'
            f'    models:\n'
            f'      - name: minimax-video'
        )

    # Suno (music generation)
    suno_key = config_us_data.get('Suno', {}).get('Key', '')
    if suno_key:
        entries.append(
            f'  - name: suno-direct\n'
            f'    priority: 7\n'
            f'    base-url: https://apibox.erweima.ai/api/v1\n'
            f'    api-key-entries:\n'
            f'      - api-key: {yq(suno_key)}\n'
            f'    models:\n'
            f'      - name: suno-v4'
        )

    # Fal (image generation)
    fal_key = config_us_data.get('Fal', {}).get('Key', '')
    if fal_key:
        entries.append(
            f'  - name: fal-direct\n'
            f'    priority: 7\n'
            f'    base-url: https://queue.fal.run\n'
            f'    api-key-entries:\n'
            f'      - api-key: {yq(fal_key)}\n'
            f'    models:\n'
            f'      - name: fal-ai'
        )

    # ApiCoco (video generation)
    apicoco = config_us_data.get('ApiCoco', {})
    apicoco_key = apicoco.get('Key', '')
    if apicoco_key:
        entries.append(
            f'  - name: apicoco-direct\n'
            f'    priority: 7\n'
            f'    base-url: {apicoco.get("VideoGeneration", "https://apicoco.com/v1/video/generations")}\n'
            f'    api-key-entries:\n'
            f'      - api-key: {yq(apicoco_key)}\n'
            f'    models:\n'
            f'      - name: apicoco'
        )

    # AudioShake (audio separation)
    audioshake_key = config_us_data.get('AudioShake', {}).get('Key', '')
    if audioshake_key:
        entries.append(
            f'  - name: audioshake-direct\n'
            f'    priority: 7\n'
            f'    base-url: https://groovy.audioshake.ai/api/v1\n'
            f'    api-key-entries:\n'
            f'      - api-key: {yq(audioshake_key)}\n'
            f'    models:\n'
            f'      - name: audioshake'
        )

    return entries


# --- Cookie Pool ---

def generate_cookie_pool_entries():
    """Cookie Pool ultra (P9) + plus (P8)."""
    models_lines = '\n'.join(f'      - name: {m}' for m in COOKIE_POOL_MODELS)
    return [
        f'  - name: cookie-pool-ultra\n'
        f'    priority: 9\n'
        f'    base-url: {COOKIE_POOL_BASE_URL}\n'
        f'    cookie-pool-file: pool-ultra.json\n'
        f'    models:\n'
        f'{models_lines}',
        f'  - name: cookie-pool-plus\n'
        f'    priority: 8\n'
        f'    base-url: {COOKIE_POOL_BASE_URL}\n'
        f'    cookie-pool-file: pool-plus.json\n'
        f'    models:\n'
        f'{models_lines}',
    ]


# --- Singularity (Gemini Cookie Pool) ---

def generate_singularity_entries():
    """Gemini via Cookie Pool. Name must contain 'singularity', prefix: skyclaw."""
    models_lines = '\n'.join(f'      - name: {m}' for m in SINGULARITY_MODELS)
    return [
        f'  - name: singularity-ultra\n'
        f'    prefix: skyclaw\n'
        f'    priority: 9\n'
        f'    base-url: {SINGULARITY_BASE_URL}\n'
        f'    cookie-pool-file: pool-ultra.json\n'
        f'    models:\n'
        f'{models_lines}',
        f'  - name: singularity-plus\n'
        f'    prefix: skyclaw\n'
        f'    priority: 8\n'
        f'    base-url: {SINGULARITY_BASE_URL}\n'
        f'    cookie-pool-file: pool-plus.json\n'
        f'    models:\n'
        f'{models_lines}',
    ]


# --- TaijiAI (XP provider) ---

def generate_taijiai_entries(config_us_data):
    """config_us_k8s.json XP section -> TaijiAI claude-api-key + openai-compatibility entries.

    Claude models use claude-api-key type with base-url + anthropic-version header.
    Gemini models use openai-compatibility type.
    """
    xp = config_us_data.get('XP', {})
    claude_key = xp.get('ClaudeKey', '')
    gemini_key = xp.get('GeminiKey', '')
    bedrock_yaml = ''
    oa_entries = []

    if claude_key:
        ml_lines = []
        for name, alias in TAIJIAI_CLAUDE_MODELS:
            ml_lines.append(f'      - name: {name}\n        alias: {alias}')
            ml_lines.append(f'      - name: {name}')
        ml = '\n'.join(ml_lines)
        bedrock_yaml = (
            f'  - api-key: {claude_key}\n'
            f'    base-url: https://api.taijiaicloud.com\n'
            f'    priority: 9\n'
            f'    headers:\n'
            f'      anthropic-version: "2023-06-01"\n'
            f'    models:\n{ml}'
        )

    if gemini_key:
        ml = '\n'.join(f'      - name: {m}' for m in TAIJIAI_GEMINI_MODELS)
        oa_entries.append(
            f'  - name: taijiai-gemini\n'
            f'    priority: 9\n'
            f'    base-url: https://api.taijiaicloud.com/v1\n'
            f'    api-key-entries:\n'
            f'      - api-key: {gemini_key}\n'
            f'    models:\n'
            f'{ml}'
        )

    n = (1 if claude_key else 0) + (1 if gemini_key else 0)
    return bedrock_yaml, oa_entries, n


# --- Base Config ---

def base_config(port):
    return f'''host: ""
port: {port}
proxy-url: socks5://127.0.0.1:1080
skywork-smart-fallback: true
skywork-throttle-delay-seconds: 0
tls:
  enable: false
remote-management:
  allow-remote: true
  secret-key: "$2a$10$ASf7hOgqBIpBnZwQIxCjEuEppGbCNNofzpk.PmkC3TQVP3TDyW/Pm"
  disable-control-panel: false
  panel-github-repository: https://github.com/hhsw2015/Cli-Proxy-API-Management-Center
auth-dir: /home/azureuser/.cli-proxy-api
archive-failed-auth: true
api-keys:
  - sk-1Fna1Bm7umJdI5ADt
debug: false
pprof:
  enable: false
  addr: 127.0.0.1:8316
commercial-mode: false
incognito-browser: true
logging-to-file: false
error-logs-max-files: 10
usage-statistics-enabled: true
force-model-prefix: false
passthrough-headers: false
request-retry: 2
max-retry-credentials: 0
max-retry-interval: 60
quota-exceeded:
  switch-project: true
  switch-preview-model: true
routing:
  strategy: fill-first
  latency-aware: true
pool-manager:
  size: 0
  active-idle-scan-interval-seconds: 1800
  reserve-scan-interval-seconds: 300
  limit-scan-interval-seconds: 21600
  reserve-sample-size: 20
  reserve-refill-low-ratio: 0.25
  reserve-refill-high-ratio: 0.5
  cold-batch-load-ratio: 0.05
  low-quota-threshold-percent: 20
  provider: codex
  active-quota-refresh-interval-seconds: 60
  active-quota-refresh-sample-size: 10
  background-probe-budget-window-seconds: 10
  background-probe-budget-max: 2
ws-auth: false
streaming:
  keepalive-seconds: 15
  bootstrap-retries: 5
request-log: true
refusal-shield:
  enabled: false
  max-retries: 2
  peek-bytes: 256
  ai-rewrite: false'''


# --- Validation ---

def validate_bedrock(claude_data):
    mismatches = []
    for section in BEDROCK_SECTIONS:
        if section not in claude_data:
            continue
        for model_name, endpoints in claude_data[section].items():
            if model_name.endswith('_bak') or not isinstance(endpoints, list):
                continue
            for ep in endpoints:
                region = ep.get('Region', '')
                arn = ep.get('ModelId', '')
                arn_region = extract_arn_region(arn)
                if arn_region and region and arn_region != region:
                    mismatches.append(f"  {model_name}: ep={region} ARN={arn_region}")
    return mismatches


# --- Main ---

def main():
    output_dir = Path(DEFAULT_OUTPUT_DIR)
    date_str = None
    keys_dir_override = None

    args = sys.argv[1:]
    i = 0
    while i < len(args):
        if args[i] == '--date' and i + 1 < len(args):
            date_str = args[i + 1]; i += 2
        elif args[i] == '--keys-dir' and i + 1 < len(args):
            keys_dir_override = Path(args[i + 1]); i += 2
        elif args[i] == '--output-dir' and i + 1 < len(args):
            output_dir = Path(args[i + 1]); i += 2
        else:
            i += 1

    keys_dir = keys_dir_override if keys_dir_override else resolve_keys_dir(date_str)

    # Load data files
    claude_file = keys_dir / 'keys-claude.json'
    azure_file = keys_dir / 'keys-azure.json'
    all_keys_file = Path(ALL_API_KEYS_FILE)
    config_us_file = Path(CONFIG_US_FILE)

    if not azure_file.exists():
        cn_dir = Path(NACOS_BASE_DIR).parent / 'cn-nacos' / keys_dir.name
        azure_fallback = cn_dir / 'keys-azure.json'
        if azure_fallback.exists():
            azure_file = azure_fallback
            print(f"Azure keys fallback: {azure_fallback}")
    for f in [claude_file, azure_file]:
        if not f.exists():
            print(f"ERROR: {f} not found", file=sys.stderr); sys.exit(1)

    with open(claude_file) as f: claude_data = json.load(f)
    with open(azure_file) as f: azure_data = json.load(f)

    all_keys = []
    if all_keys_file.exists():
        with open(all_keys_file) as f: all_keys = json.load(f)
    else:
        print(f"WARN: {all_keys_file} not found, skipping Groq/Deepseek/OpenRouter", file=sys.stderr)

    config_us_data = {}
    # Try dated config first (e.g. us-nacos/2026-04-13/config_us_k8s.json), fallback to static path
    dated_config = keys_dir / 'config_us_k8s.json'
    if dated_config.exists():
        with open(dated_config) as f: config_us_data = json.load(f)
        print(f"Using dated config: {dated_config}")
    elif Path(CONFIG_US_FILE).exists():
        with open(CONFIG_US_FILE) as f: config_us_data = json.load(f)
    else:
        print(f"WARN: config_us_k8s.json not found, skipping Gemini API keys + media providers", file=sys.stderr)

    output_dir.mkdir(parents=True, exist_ok=True)

    # Validate
    mismatches = validate_bedrock(claude_data)
    if mismatches:
        print(f"WARN: {len(mismatches)} ARN/region mismatches (auto-corrected):", file=sys.stderr)
        for m in mismatches: print(m, file=sys.stderr)

    # Generate all sections
    bedrock_yaml, bedrock_count = generate_bedrock_entries(claude_data)
    gemini_yaml, gemini_count = generate_gemini_keys(config_us_data)
    taijiai_claude_yaml, taijiai_oa_entries, taijiai_count = generate_taijiai_entries(config_us_data)
    azure_entries = generate_azure_entries(azure_data)
    groq_entries = generate_groq_entries(all_keys)
    deepseek_entries = generate_deepseek_entries(all_keys)
    openrouter_entries = generate_openrouter_entries(all_keys)
    direct_entries = generate_direct_provider_entries(config_us_data)
    media_direct_entries = generate_media_direct_entries(config_us_data)
    media_entries = generate_gpt_proxy_fallback_entries()
    cookie_entries = generate_cookie_pool_entries()
    singularity_entries = generate_singularity_entries()

    # === cpa-new-config.yaml (port 8318: full) ===
    all_oa = (azure_entries + deepseek_entries + groq_entries + openrouter_entries
              + direct_entries + media_direct_entries + media_entries
              + taijiai_oa_entries + cookie_entries + singularity_entries)
    new_config = base_config(8318)
    new_config += '\n\n' + bedrock_yaml
    if taijiai_claude_yaml:
        new_config += '\n' + taijiai_claude_yaml
    if gemini_yaml:
        new_config += '\n\n' + gemini_yaml
    new_config += '\n\nopenai-compatibility:\n'
    new_config += '\n'.join(all_oa)
    new_config += '\n'

    new_path = output_dir / 'cpa-new-config.yaml'
    with open(new_path, 'w') as f: f.write(new_config)

    print(f"Generated: {new_path}")
    print(f"  Bedrock: {bedrock_count} entries (P10)")
    print(f"  TaijiAI: {taijiai_count} entries (P9)")
    print(f"  Gemini API keys: {gemini_count}")
    print(f"  Azure: {len(azure_entries)} entries (P10/P8)")
    print(f"  Deepseek: {len(deepseek_entries)} entries (P10)")
    print(f"  Groq: {len(groq_entries)} entries (P8)")
    print(f"  OpenRouter: {len(openrouter_entries)} entries (P6)")
    print(f"  Direct providers: {len(direct_entries)} entries (SiliconFlow/Nebius/DeepInfra/DashScope/Grok)")
    print(f"  Media direct: {len(media_direct_entries)} entries (P7, Kling/Vidu/MiniMax/Suno/Fal/ApiCoco/AudioShake)")
    print(f"  gpt-proxy media: {len(media_entries)} entries (P5 fallback)")
    print(f"  Cookie Pool: ultra (P9) + plus (P8), {len(COOKIE_POOL_MODELS)} models")
    print(f"  Singularity: ultra (P9) + plus (P8), {len(SINGULARITY_MODELS)} Gemini models")

    # === cpa-old-config.yaml (port 8317: Cookie Pool only) ===
    old_config = base_config(8317)
    old_config += '\n\nopenai-compatibility:\n'
    old_config += '\n'.join(cookie_entries + singularity_entries)
    old_config += '\n'

    old_path = output_dir / 'cpa-old-config.yaml'
    with open(old_path, 'w') as f: f.write(old_config)

    print(f"\nGenerated: {old_path}")
    print(f"  Cookie Pool + Singularity only")

    # Summary
    total_oa = len(all_oa)
    print(f"\nTotal: {bedrock_count} Bedrock + {gemini_count} Gemini + {total_oa} OpenAI-compat entries")
    print(f"\nDeploy:")
    print(f"  scp {new_path} azureuser@4.151.241.30:~/CLIProxyAPIPlus-new/cpa-new-config.yaml")
    print(f"  scp {old_path} azureuser@4.151.241.30:~/CLIProxyAPIPlus/config.yaml")


if __name__ == '__main__':
    main()
