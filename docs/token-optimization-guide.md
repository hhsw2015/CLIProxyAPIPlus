# Token Optimization Guide

## Overview

Four active layers of token savings, targeting different parts of the request lifecycle. Combined savings ~93% per request.

## Layer 1: RTK (Input Compression)

**What**: CLI proxy that compresses tool output before sending to Claude.

**Savings**: Input tokens -46%

**How it works**: Intercepts `git status`, `git diff` etc., strips redundant output (whitespace, headers, repeated context). Claude sees compressed version, reasoning unaffected.

**Setup**: Installed as shell hook. `rtk gain` shows savings analytics.

## Layer 2: CPA Region Affinity (Prompt Cache)

**What**: Keeps requests in the same AWS region to maximize Bedrock prompt cache hits.

**Savings**: Cached tokens at $1.875/1M (vs $15/1M full price) = -87.5% on cached portion

**How it works**:
- Anthropic Bedrock prompt cache is shared per-region across AWS accounts (verified with production data)
- CPA's fill-first strategy enhanced with sticky region + round-robin within region
- Same conversation prefix cached once, reused across multiple accounts in same region
- Typical cache hit rate: 99%+ (only new message delta charged at full price)
- Region auto-switch: if preferred region degrades, switch to healthiest region

**Config**: Automatic when `routing.strategy: fill-first` and multiple Bedrock accounts configured.

**Docs**: `docs/region-affinity-scheduling.md`

## Layer 3: CLAUDE.md Rules (Output Reduction)

**What**: Behavioral rules that reduce verbose/wasteful output.

**Savings**: Output -17%

### Current rules (`~/.claude/CLAUDE.md`):
```
- Do not re-read files already read unless the file may have changed.
- No sycophantic openers or closing fluff.
- Execute tasks directly. No narration.
- No em dashes, smart quotes, or decorative Unicode.
- Never invent file paths, API endpoints, or function names.
```

### Sources:
- `drona23/claude-token-efficient` - 8 behavioral rules, ~80 tokens overhead
- Project-level rules in `~/.claude/rules/common/` - coding style, testing, workflow

### Compression option:
- `caveman:compress` can compress CLAUDE.md files by ~45%
- Current files already concise, compression ROI low
- Only worth compressing files with 200+ words of natural language

## Layer 4: Caveman Plugin (Output Compression)

**What**: Forces Claude to respond in compressed telegram-style language.

**Savings**: Output tokens -65% average (22%-87% range)

**How it works**: Drops articles, filler words, pleasantries. Uses fragments and short synonyms. Code blocks unchanged. Thinking/reasoning tokens unaffected.

**Setup**:
```bash
claude plugin marketplace add JuliusBrussee/caveman
claude plugin install caveman@caveman
```

**Modes**:
| Mode | Style | Compression |
|------|-------|-------------|
| lite | Professional, no filler | Low |
| full (default) | Telegram-style fragments | Medium |
| ultra | Abbreviations, arrows | High |
| wenyan-lite | Semi-classical Chinese | Medium |
| wenyan-full | Full classical Chinese | High |
| wenyan-ultra | Extreme classical | Maximum |

**Activation**: Auto-activates on new sessions (via hooks). Manual: `/caveman`

**Deactivation**: "normal mode" or "stop caveman"

## Combined Savings: Real-World Numbers

Based on actual production data (62K context, Opus 4.6):

### Per-Request Cost

| Scenario | Input Cost | Output Cost | Total |
|----------|-----------|-------------|-------|
| No optimization | $1.80 | $0.038 | **$1.84** |
| All 4 layers | $0.114 | $0.013 | **$0.127** |
| **Savings** | **94%** | **65%** | **93%** |

### 100-Turn Session

| Scenario | Cost |
|----------|------|
| No optimization | ~$184 |
| All 4 layers | ~$12.7 |

### Where Each Layer Contributes

```
Request lifecycle:

[User types message]
    |
[RTK compresses CLI output]           ← Layer 1: -46% input base size
    |
[CPA routes to cached region]         ← Layer 2: 99% cache hit, 1/8 price
    |
[Claude thinks (extended thinking)]   ← NOT optimized (quality priority)
    |
[Claude generates response]
    |
[CLAUDE.md rules reduce verbosity]    ← Layer 3: -17% output
    |
[Caveman compresses text]             ← Layer 4: -65% output
    |
[User receives response]
```

## Remaining Optimization Opportunities

### Already Near-Optimal
| Area | Status | Why |
|------|--------|-----|
| Input (history) | Optimized | RTK + Cache = 94% savings |
| Output (text) | Optimized | Caveman + Rules = 65% savings |
| Thinking | Keep as-is | Quality priority, need strongest reasoning |
| MCP tools | Fine | Lazy-loaded, minimal overhead |

### Possible Further Savings
| Area | Method | Trade-off |
|------|--------|-----------|
| Model selection | Sonnet for simple tasks (5x cheaper) | Slightly lower quality |
| Tool call count | Fewer Read/Grep calls per task | Depends on usage patterns |
| /compact timing | Compact earlier to keep context small | Lose some conversation history |

### Model Selection Guide (If Cost-Sensitive)
| Task Type | Recommended Model | Cost vs Opus |
|-----------|------------------|-------------|
| Complex architecture, debugging | Opus 4.6 | 1x (baseline) |
| Routine coding, file edits | Sonnet 4.6 | 1/5x |
| Simple queries, git ops | Haiku 4.5 | 1/19x |

## Pending Fixes (Not Yet Available)

### db8 Session Cache Fix
- Claude Code bug: session save filters out `deferred_tools_delta` attachments
- Causes prompt cache miss on session resume (26% hit rate instead of 99%)
- Fix confirmed by Anthropic engineer Boris, expected in next Claude Code release
- Cannot patch: Claude Code v2.1.97 is compiled Mach-O binary
- Workaround: avoid `/resume` on long sessions, start fresh

### Token-Efficient Patterns (Best Practices)
- Structured output (JSON/tables) cheaper than prose
- System prompt at beginning maximizes cache prefix length
- Avoid repeating context in follow-up messages
- Use `/compact` before context gets too large
- Use Agent subagents for heavy exploration (separate context window)

## References

- RTK: `~/.claude/RTK.md`
- Region Affinity: `docs/region-affinity-scheduling.md`
- drona23 rules: `github.com/drona23/claude-token-efficient`
- Caveman: `github.com/JuliusBrussee/caveman`
- db8 analysis: `reddit.com/r/ClaudeAI/comments/1s8zxt4/`
- Token savings script: `scripts/fix_session_tool_pairing.py`
