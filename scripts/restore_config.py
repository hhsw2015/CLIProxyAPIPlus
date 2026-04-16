import json
import yaml

def build_full_config():
    # 1. Base config from user snippet
    config = {
        'host': '',
        'port': 8318,
        'proxy-url': 'socks5://127.0.0.1:1080', # Enabled for VPS2 usage
        'skywork-smart-fallback': True,
        'skywork-throttle-delay-seconds': 0,
        'tls': {'enable': False},
        'remote-management': {
            'allow-remote': True,
            'secret-key': '$2a$10$ASf7hOgqBIpBnZwQIxCjEuEppGbCNNofzpk.PmkC3TQVP3TDyW/Pm',
            'disable-control-panel': False,
            'panel-github-repository': 'https://github.com/hhsw2015/Cli-Proxy-API-Management-Center'
        },
        'auth-dir': '/home/azureuser/.cli-proxy-api',
        'archive-failed-auth': True,
        'api-keys': ['sk-1Fna1Bm7umJdI5ADt'],
        'debug': False,
        'pprof': {'enable': False, 'addr': '127.0.0.1:8316'},
        'commercial-mode': False,
        'incognito-browser': True,
        'logging-to-file': False,
        'error-logs-max-files': 10,
        'usage-statistics-enabled': True,
        'force-model-prefix': False,
        'passthrough-headers': False,
        'quota-exceeded': {
            'switch-project': True,
            'switch-preview-model': True
        },
        'routing': {'strategy': 'fill-first'},
        'pool-manager': {
            'size': 0,
            'active-idle-scan-interval-seconds': 1800,
            'reserve-scan-interval-seconds': 300,
            'limit-scan-interval-seconds': 21600,
            'reserve-sample-size': 20,
            'reserve-refill-low-ratio': 0.25,
            'reserve-refill-high-ratio': 0.5,
            'cold-batch-load-ratio': 0.05,
            'low-quota-threshold-percent': 20,
            'provider': 'codex',
            'active-quota-refresh-interval-seconds': 60,
            'active-quota-refresh-sample-size': 10,
            'background-probe-budget-window-seconds': 10,
            'background-probe-budget-max': 2
        },
        'ws-auth': False,
        'streaming': {'bootstrap-retries': 5},
        'request-log': True
    }

    # Load artifacts
    claude_data = json.load(open('/Users/wowdd1/Dev/dvina-2api/artifacts/api-keys/us-nacos/2026-04-09/keys-claude.json'))
    azure_data = json.load(open('/Users/wowdd1/Dev/dvina-2api/artifacts/api-keys/us-nacos/2026-04-09/keys-azure.json'))
    other_data = json.load(open('/Users/wowdd1/Dev/dvina-2api/artifacts/api-keys/us-nacos/2026-04-09/keys-other.json'))

    # 2. Bedrock Claude
    bedrock_entries = []
    for section in ['XmindConf', 'nova-micro']:
        if section not in claude_data: continue
        groups = {}
        for m_name, eps in claude_data[section].items():
            if m_name.endswith('_bak'): continue
            for ep in eps:
                ak, idx = ep.get('Ak'), ep.get('index', 0)
                if not ak: continue
                key = (ak, idx)
                if key not in groups:
                    groups[key] = {'sk': ep['Sk'], 'region': ep['Region'], 'models': []}
                # Rule: de-duplicate model name per (AK, index)
                if not any(m['name'] == m_name for m in groups[key]['models']):
                    groups[key]['models'].append({'name': m_name, 'model-id': ep['ModelId']})

        for (ak, idx), info in sorted(groups.items()):
            bedrock_entries.append({
                'aws-access-key-id': ak,
                'aws-secret-access-key': info['sk'],
                'aws-region': info['region'],
                'priority': 10,
                'models': sorted(info['models'], key=lambda x: x['name'])
            })
    config['claude-api-key'] = bedrock_entries

    # 3. Azure & Other (openai-compatibility)
    oa_compat = []

    # Process AzureConf
    for m_name, eps in azure_data.get('AzureConf', {}).items():
        for i, ep in enumerate(eps):
            oa_compat.append({
                'name': f'azure-{m_name}-{i}',
                'priority': 10 if i == 0 else 8,
                'base-url': ep['Api'],
                'api-key-entries': [{'api-key': ep['Key']}],
                'models': [{'name': m_name}],
                'headers': {'api-key': ep['Key']}
            })

    # Process KuanbangConf
    for m_name, eps in azure_data.get('KuanbangConf', {}).items():
        for i, ep in enumerate(eps):
            oa_compat.append({
                'name': f'kb-{m_name}-{i}',
                'priority': 8,
                'base-url': ep['Api'],
                'api-key-entries': [{'api-key': ep['Key']}],
                'models': [{'name': m_name}],
                'headers': {'api-key': ep['Key']}
            })

    # Process Groq (if available in artifacts or static)
    # Based on guide, Groq keys are from ALL_API_KEYS.json, I'll skip for now or add placeholder

    config['openai-compatibility'] = oa_compat

    # 4. Cookie Pool (the plugin section)
    config['cookiepool'] = {
        'pools': [
            {'name': 'ultra', 'file': './pool-ultra.json', 'priority': 10},
            {'name': 'plus', 'file': './pool-plus.json', 'priority': 8}
        ]
    }

    # Write to file
    with open('config_restored.yaml', 'w') as f:
        yaml.dump(config, f, sort_keys=False, allow_unicode=True)
    print("Reconstructed config saved to config_restored.yaml")

build_full_config()
