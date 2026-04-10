# Token Optimization Guide

## Overview

Four layers of token savings, each targeting different parts of the request lifecycle.

## Layer 1: RTK (Input Compression)

**What**: CLI proxy that compresses tool output before sending to Claude.

**Savings**: Input tokens -46%

**How it works**: Intercepts `git status`, `git diff` etc., strips redundant output (whitespace, headers, repeated context). Claude sees compressed version.

**Setup**: Already installed as shell hook. `rtk gain` shows savings.

## Layer 2: CPA Region Affinity (Prompt Cache)

**What**: Keeps requests in the same AWS region to maximize Bedrock prompt cache hits.

**Savings**: Cached tokens billed at $1.875/1M (vs $15/1M full price) = -87.5% on cached portion

**How it works**:
- Anthropic Bedrock prompt cache is shared per-region across AWS accounts (verified)
- CPA's fill-first strategy enhanced with sticky region + round-robin within region
- Same conversation prefix cached once, reused across multiple accounts
- Typical cache hit rate: 99%+ (only new delta charged at full price)

**Config**: Automatic when `routing.strategy: fill-first` and multiple Bedrock accounts configured.

**Docs**: `docs/region-affinity-scheduling.md`

## Layer 3: CLAUDE.md Rules (Output Reduction)

**What**: Behavioral rules that reduce verbose/wasteful output.

**Savings**: Output -17% to -63% depending on rules

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

**How it works**: Drops articles, filler words, pleasantries. Uses fragments and short synonyms. Code blocks unchanged.

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

## Combined Savings Estimate

For a typical CPA development session (long conversation, ~60K context):

| Layer | Target | Savings | Example |
|-------|--------|---------|---------|
| RTK | Input (CLI output) | -46% | 1.2M -> 659K tokens |
| Region Cache | Input (repeated context) | -87.5% on cached | 659K -> ~100 new + 559K cached at 1/8 price |
| CLAUDE.md | Output | -17% | Less verbose explanations |
| Caveman | Output | -65% | Telegram-style responses |

**Without any optimization**: ~$18/session (input) + ~$5 (output) = ~$23
**With all layers**: ~$2.50 (input) + ~$1.75 (output) = ~$4.25

**Total savings: ~82%**

## Additional Techniques (Not Yet Applied)

### db8 Session Fix
- Claude Code bug: session save filters out `deferred_tools_delta` attachments
- Causes prompt cache miss on session resume (26% hit rate instead of 99%)
- Fix confirmed by Anthropic engineer, expected in next Claude Code release
- Current workaround: avoid `/resume`, start fresh sessions
- Status: Waiting for official fix (cannot patch compiled binary)

### Token-Efficient Prompt Patterns
- Structured output (JSON/tables) cheaper than prose
- System prompt at beginning maximizes cache prefix length
- Avoid repeating context in follow-up messages
- Use `/compact` before context gets too large (reduces future input cost)

## References

- RTK: `~/.claude/RTK.md`
- Region Affinity: `docs/region-affinity-scheduling.md`
- drona23 rules: `github.com/drona23/claude-token-efficient`
- Caveman: `github.com/JuliusBrussee/caveman`
- db8 analysis: `reddit.com/r/ClaudeAI/comments/1s8zxt4/`
