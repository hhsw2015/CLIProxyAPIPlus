# Region Affinity Scheduling

## Overview

CPA supports region-aware scheduling for AWS Bedrock accounts. When multiple Bedrock accounts are configured across different AWS regions, the scheduler automatically groups accounts by region and applies intelligent routing to maximize prompt cache utilization and improve fault tolerance.

## Background: Bedrock Prompt Cache Sharing

Anthropic's prompt cache on AWS Bedrock is **shared within the same region across different AWS accounts**. This means:

- Account A and Account B in `us-east-1` share the same prompt cache
- Account A in `us-east-1` and Account A in `us-east-2` have **separate** caches
- Cache TTL is approximately 5 minutes of inactivity

This was verified with production data showing ~350k cached tokens consistently shared between two different AWS access keys in the same region.

## How It Works

### Affinity Groups

Each auth entry has an `AffinityGroup()` that determines its cache-sharing group:

| Provider | AffinityGroup | Example |
|----------|---------------|---------|
| Bedrock (has `aws_region`) | `provider:region` | `claude:us-east-2` |
| All other providers | `auth.ID` | Original behavior, no grouping |

### Scheduling Strategy

The feature enhances the existing `fill-first` strategy:

1. **Region Selection**: On the first request for a model, the scheduler picks the region with the **most healthy accounts** supporting that model
2. **Region Sticky**: Subsequent requests stay in the chosen region
3. **Intra-Region Round-Robin**: Within the region, requests are distributed across accounts via round-robin (not stuck on one account)
4. **Automatic Failover**: If the preferred region loses capacity (fewer healthy accounts than another region), the scheduler switches to the better region
5. **Graceful Degradation**: If no region has 2+ accounts, the feature is disabled and standard `fill-first` applies

### Region Switch Logic

```
if preferred_region.healthy_count >= best_region.healthy_count AND preferred_region.healthy_count >= 2:
    → stay in preferred region (sticky)
else:
    → switch to region with most healthy accounts
```

### Examples

**Normal operation** (4 regions x 2 accounts each):
```
Request 1 → us-east-2/AccountA (region selected: most accounts)
Request 2 → us-east-2/AccountB (round-robin within region)
Request 3 → us-east-2/AccountA (round-robin continues)
Request 4 → us-east-2/AccountB ...
```

**One account fails** in preferred region:
```
us-east-2: AccountA (healthy), AccountB (cooldown) → 1 healthy
us-east-1: AccountA (healthy), AccountB (healthy)   → 2 healthy

→ Switch to us-east-1 (more capacity)
```

**Account recovers** (cooldown expires):
```
us-east-1: 2 healthy (current)
us-east-2: 2 healthy (recovered)

→ Stay in us-east-1 (equal capacity, no unnecessary switch)
```

## Configuration

No additional configuration is required. The feature activates automatically when:

1. `routing.strategy` is `fill-first` (the default)
2. Multiple Bedrock accounts are configured with `aws-region`
3. At least 2 accounts in the same region support the requested model

Example `config.yaml`:

```yaml
routing:
  strategy: fill-first

claude-api-key:
  - aws-access-key-id: AKIA4V5SSLGX...
    aws-secret-access-key: ...
    aws-region: us-east-1
    priority: 10
    models:
      - name: claude-opus-4.6
        model-id: arn:aws:bedrock:us-east-1:...:application-inference-profile/...

  - aws-access-key-id: AKIA5Y5R2LJ3...
    aws-secret-access-key: ...
    aws-region: us-east-1
    priority: 10
    models:
      - name: claude-opus-4.6
        model-id: arn:aws:bedrock:us-east-1:...:application-inference-profile/...

  # Same accounts in another region (separate cache)
  - aws-access-key-id: AKIA4V5SSLGX...
    aws-secret-access-key: ...
    aws-region: us-east-2
    priority: 10
    models:
      - name: claude-opus-4.6
        model-id: arn:aws:bedrock:us-east-2:...:application-inference-profile/...
```

## Benefits

| Aspect | Without Region Affinity | With Region Affinity |
|--------|------------------------|---------------------|
| Cache utilization | 4 regions each maintain separate cache | 1 region, all accounts share cache |
| Input tokens | Full prompt sent to cold regions | Only delta (1-10 tokens) from cache |
| Account failover | May jump to cold region, cache lost | Stays in region, cache preserved |
| Region selection | Random/round-robin across all | Prefers region with most healthy accounts |
| Cost | 4x cache maintenance overhead | 1x cache maintenance |

## Debugging

Enable `debug: true` in config to see scheduling decisions:

```
[debug] [conductor.go] Use API key AKIA...77FD [AKIA4V5SSLGXJITU77FD/us-east-2] for model claude-opus-4.6
```

The `[AK/region]` label shows which account and region was selected.

## Implementation Details

Key source files:

- `sdk/cliproxy/auth/types.go` — `Auth.AffinityGroup()` method
- `sdk/cliproxy/auth/scheduler.go` — `bestAffinityGroup()`, `pickReadyAtPriorityLocked()`, `affinityPreference` map
