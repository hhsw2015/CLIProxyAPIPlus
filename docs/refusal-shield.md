# Refusal Shield - Design & Implementation Document

## Overview

Refusal Shield is a CPA native feature that automatically detects model refusal responses in real-time streaming, rewrites the conversation history with a cooperative opener, and retries transparently through CPA's existing credential rotation loop. Users experience zero interruption — a refused request is silently retried with a modified context.

The feature integrates capabilities from two external projects:
- **refusal-relay** (Python): real-time proxy-level interception and retry
- **codex-session-patcher** (Python): smart refusal detection algorithms and AI-powered response rewriting

Both projects' core logic was ported to Go and deeply integrated with CPA's architecture rather than running as separate services.

## Architecture

```
Request Flow (refusal-shield enabled):

[Client Request]
       |
       v
[CPA ExecuteStream Loop]  ──────────────────────────────────────────┐
       |                                                             |
       v                                                             |
[Provider Executor] ─── stream chunks ───> [Bootstrap Buffer]        |
                                                |                    |
                                                v                    |
                                    [Refusal Shield Check]           |
                                         /     |     \               |
                                        /      |      \              |
                                 LevelNone  Uncertain  Confirmed     |
                                    |          |          |          |
                                    v          v          v          |
                              [Passthrough] [AI Verify?] [Rewrite]   |
                                           /       \        |        |
                                        NO refusal  YES     |        |
                                          |          |      |        |
                                          v          v      v        |
                                    [Passthrough]  [Rewrite Payload] |
                                                   [Discard Stream]  |
                                                        |            |
                                                        v            |
                                                [continue loop] ─────┘
                                                (retry with next credential)
```

### Design Principles

1. **Default-off**: The feature is entirely disabled when `refusal-shield.enabled` is `false` (the default). Zero code paths execute, zero performance impact.

2. **Non-invasive integration**: Only ~10 lines were added to `conductor.go`. All logic lives in separate files (`internal/refusal/` package and `sdk/cliproxy/auth/refusal_shield.go`).

3. **Fail-open**: If the detector, rewriter, AI verify, or AI rewrite call encounters any error, the system silently falls through to the next strategy or returns the original response. The feature never causes a request to fail.

4. **Lightweight by design**: The detector only inspects the first 256 bytes of each stream. For 99%+ of normal responses, this is a single string comparison taking microseconds. AI verify and AI rewrite are only triggered on edge cases.

## Module Layout

```
internal/refusal/
├── detector.go          # Precision detection engine (strong/weak + scoring)
├── detector_test.go     # 62 test cases
├── peeker.go            # Stream chunk text extraction utilities
├── peeker_test.go       # 7 test cases
├── rewriter.go          # Payload rewriting (static templates + history manipulation)
├── rewriter_test.go     # 6 test cases
├── ai_rewriter.go       # External OpenAI-compatible endpoint for AI rewrite
├── ai_rewriter_test.go  # 7 test cases (with mock HTTP server)
└── ai_verify.go         # AI-assisted binary refusal verification

sdk/cliproxy/auth/
└── refusal_shield.go    # Conductor integration layer (config + orchestration + AI verify/rewrite routing)

internal/config/
└── config.go            # RefusalShieldConfig struct + defaults (modified)

sdk/cliproxy/auth/
└── conductor.go         # ExecuteStream hook insertion point (modified, ~10 lines)
```

## Detection Engine

### Three-Level Analysis

The detector returns a `RefusalLevel` instead of a simple boolean:

| Level | Meaning | Action |
|-------|---------|--------|
| `LevelConfirmed` | Strong phrase match or weak score >= 2 | Immediately rewrite and retry |
| `LevelUncertain` | Weak score == 1 (borderline) | If `ai-verify` on: ask model. Otherwise: pass through |
| `LevelNone` | No signals found | Pass through (0 overhead) |

### Tier 1 — Strong Phrases (full-text scan)

High-confidence indicators matched anywhere in the response. ~60 patterns covering English and Chinese:
- English: `"i cannot assist"`, `"against my safety policy"`, `"as an ai"`, `"my programming prevents"`
- Chinese: `"我无法协助"`, `"作为AI助手"`, `"违反了我的"`, `"无法满足这个请求"`, `"请理解，我"`, `"帮不了你"`

### Tier 2 — Grouped Weak Signal Scoring (windowed)

Lower-confidence signals scanned within the first 150 characters. Related signals are **grouped** so overlapping phrases (e.g. "抱歉" and "很抱歉") count as only 1 point:

| Group | Signals |
|-------|---------|
| Apology (EN) | sorry, apologize |
| Inability (EN) | i cannot, i can't, i'm unable |
| Apology (CN) | 抱歉, 很抱歉, 非常抱歉, 十分抱歉, 实在抱歉 |
| Excuse (CN) | 对不起, 不好意思 |
| Inability (CN) | 无法, 做不到, 没办法 |
| Prohibition (CN) | 禁止, 不允许 |
| Policy (CN) | 不符合, 违反 |

Score >= 2 (two different groups) = `LevelConfirmed`. Score == 1 = `LevelUncertain`.

### Anti-False-Positive Measures

1. **Thinking block stripping**: `<thinking>...</thinking>` blocks are removed before detection.
2. **Safe passthrough patterns**: Code blocks (` ``` `), `import`, `func`, `def`, `class`, `package`, `namespace` → skip detection entirely.
3. **Grouped deduplication**: "很抱歉没能第一次就发现" only scores 1 point (apology group), not 2, so it stays below the threshold.
4. **Short message special case**: Messages < 40 chars starting with a weak signal are treated as confirmed (e.g. "抱歉。").

### AI-Assisted Verification (ai-verify)

For `LevelUncertain` cases, an optional AI verification step can be enabled:
- Sends the text to a model with a binary classifier prompt: "Is this a refusal? YES or NO"
- `max_tokens=3`, `temperature=0` — extremely fast and cheap (~5 tokens)
- Only triggered for score == 1 cases (rare)
- Fail-open: any error → pass through
- Default: **off**

## Rewrite Strategy

### Three Paths (Fallback Chain)

```
ai-rewrite: true + endpoint set    → External OpenAI-compatible API
    ↓ (fails/times out)
ai-rewrite: true + no endpoint     → CPA's own provider pool
    ↓ (fails/no providers)
ai-rewrite: false (or all failed)  → Static templates (always succeeds)
```

### Payload Manipulation

1. **Replace last assistant message**: The refused response is replaced with the cooperative opener.
2. **Append continue message**: A user message like "Continue." or "继续" is appended.
3. **Strip thinking fields**: Any `"thinking"` or `"thinking_content"` fields are removed to save tokens.

Supports both `{"messages": [...]}` (OpenAI/Anthropic) and `{"input": [...]}` (Responses API) formats.

## Conductor Integration

The hook is placed in `executeStreamWithModelPool()` in `conductor.go`, immediately after `readStreamBootstrap()` succeeds but before the stream result is returned to the client.

```go
// conductor.go — the only modification to existing code
if shieldCfg := m.refusalShieldConfig(); shieldCfg != nil {
    if shieldErr := m.refusalShieldCheck(ctx, shieldCfg, buffered, req.Payload); shieldErr != nil {
        discardStreamChunks(remaining)
        if rse, ok := shieldErr.(*refusalShieldError); ok {
            req.Payload = rse.rewrittenPayload
        }
        lastErr = shieldErr
        continue  // triggers next iteration of the model pool loop
    }
}
```

When a refusal is detected:
1. The current stream is discarded.
2. `req.Payload` is replaced with the rewritten version.
3. The `continue` statement re-enters the existing model pool loop, which tries the next available model/credential.
4. CPA's existing Skywork fallback, cross-account rotation, and credential retry logic all work normally.

## Configuration Reference

```yaml
refusal-shield:
  # Master switch. Default: false.
  enabled: true

  # Max refusal-triggered retries per request. Default: 2. Max: 5.
  max-retries: 2

  # Bytes to buffer for detection. Default: 256. Range: 64-1024.
  peek-bytes: 256

  # --- AI Rewrite ---

  # Enable AI-powered rewriting. Default: false (uses static templates).
  ai-rewrite: true

  # External OpenAI-compatible endpoint for AI rewrite.
  # If empty, uses CPA's own provider pool.
  # ai-rewrite-endpoint: "https://api.openai.com/v1/chat/completions"
  # ai-rewrite-endpoint: "http://localhost:11434/v1/chat/completions"  # Ollama

  # Bearer token for external endpoint. Empty = no auth (Ollama, etc.).
  # ai-rewrite-key: "sk-..."

  # Model for AI rewrite and AI verify. Default: "gpt-4o-mini".
  ai-rewrite-model: "gpt-4o-mini"

  # Timeout for AI rewrite/verify calls in seconds. Default: 10.
  ai-rewrite-timeout-seconds: 10

  # --- AI Verify ---

  # Enable AI-assisted verification for borderline cases (score == 1).
  # Reuses the same endpoint/key/model as ai-rewrite.
  # Default: false (uncertain cases pass through without AI check).
  ai-verify: false

  # --- Custom Patterns ---

  # Add custom strong-match patterns (matched anywhere in text).
  # extra-strong-patterns: ["custom phrase"]

  # Add custom weak-match patterns (scored in first 150 chars).
  # extra-weak-patterns: ["custom weak signal"]
```

## Performance Impact

| Scenario | Overhead |
|----------|----------|
| Feature disabled (`enabled: false`) | Zero — no code executes |
| Normal response (no refusal, score=0) | ~1 microsecond string scan |
| Uncertain response (score=1, ai-verify off) | ~1 microsecond (pass through) |
| Uncertain response (score=1, ai-verify on) | ~0.5-2s (one API call, ~5 tokens) |
| Refusal detected, static rewrite | ~10ms (JSON manipulation) |
| Refusal detected, AI rewrite via CPA pool | ~1-5s (depends on model) |
| Refusal detected, AI rewrite via external endpoint | ~1-10s (depends on endpoint) |

## Testing

62 test cases across 5 test files, all passing:

- `detector_test.go`: Strong phrases (EN+CN), grouped weak scoring, false positive prevention (12 cases including Chinese mid-text sorry), thinking stripping, extra patterns
- `peeker_test.go`: Stream interception, passthrough, byte threshold, context cancel, upstream errors
- `rewriter_test.go`: Chat/Responses format, thinking stripping, field preservation
- `ai_rewriter_test.go`: Mock server success, no-auth, timeout, errors, message extraction
- `ai_verify.go`: Binary classifier with fail-open design

Run tests:
```bash
go test ./internal/refusal/... -v
```

## Future Enhancement Ideas

1. **Per-model configuration**: Allow enabling refusal-shield only for specific models.
2. **Metrics and logging**: Add counters for refusal detection rate, retry success rate, and rewrite latency.
3. **Adaptive detection**: Track which patterns trigger most often and auto-tune the peek-bytes window.
4. **Webhook notifications**: Notify external systems when refusals are detected.
5. **Blocklist/allowlist by auth ID**: Skip detection for certain trusted auth entries.

## Commit History

- `78aaa5c7` — feat: add refusal-shield for automatic model refusal detection and retry
- `95fffa5f` — docs: add refusal-shield design and implementation document
- `d5be4095` — fix: expand Chinese refusal detection patterns for comprehensive coverage
- `51c17714` — refactor: improve refusal detection with grouped scoring algorithm
- `0e24c5c8` — feat: add AI-assisted refusal verification for borderline cases
