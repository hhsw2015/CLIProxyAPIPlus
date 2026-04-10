# Token Optimization Guide

## Overview

Six layers of token savings across the full request lifecycle. Combined savings ~93% per request on input, ~65% on output.

---

## Layer 1: RTK (Input Compression)

**What**: CLI proxy that compresses tool output before sending to Claude.

**Savings**: Input tokens -46%

**How**: Intercepts `git status`, `git diff` etc., strips redundant output. Claude sees compressed version, reasoning unaffected.

**Setup**: Installed as shell hook. `rtk gain` shows savings.

## Layer 2: CPA Region Affinity (Prompt Cache)

**What**: Keeps requests in the same AWS region to maximize Bedrock prompt cache hits.

**Savings**: Cached tokens at $1.875/1M (vs $15/1M) = -87.5% on cached portion

**How**:
- Bedrock prompt cache shared per-region across AWS accounts (verified)
- fill-first + sticky region + round-robin within region
- Typical cache hit rate: 99%+
- Auto-switch: if preferred region degrades, switch to healthiest

**Config**: Automatic with `routing.strategy: fill-first` + multiple Bedrock accounts.

**Docs**: `docs/region-affinity-scheduling.md`

## Layer 3: CLAUDE.md Rules (Output Reduction)

**What**: Behavioral rules that reduce verbose output.

**Savings**: Output -17%

**Current rules** (`~/.claude/CLAUDE.md`):
```
- Do not re-read files already read unless the file may have changed.
- No sycophantic openers or closing fluff.
- Execute tasks directly. No narration.
- No em dashes, smart quotes, or decorative Unicode.
- Never invent file paths, API endpoints, or function names.
```

**Sources**: `drona23/claude-token-efficient`, project rules in `~/.claude/rules/common/`

## Layer 4: Caveman Plugin (Output Compression)

**What**: Forces Claude to respond in compressed telegram-style language.

**Savings**: Output tokens -65% average (22%-87% range)

**How**: Drops articles, filler, pleasantries. Fragments and short synonyms. Code blocks unchanged. Thinking tokens unaffected.

**Setup**:
```bash
claude plugin marketplace add JuliusBrussee/caveman
claude plugin install caveman@caveman
```

**Modes**: lite / full (default) / ultra / wenyan-lite / wenyan-full / wenyan-ultra

**Activation**: Auto on new sessions (hooks). Manual: `/caveman`

## Layer 5: Graphify Knowledge Graph (Search Reduction)

**What**: Builds a knowledge graph from codebase, queries graph instead of reading raw files.

**Savings**: Up to 71.5x fewer tokens per code query

**How**:
- Tree-sitter AST parsing (local, free) + Claude semantic extraction (one-time cost)
- Graph stored in `graphify-out/graph.json`, persists across sessions
- PreToolUse hook intercepts Glob/Grep, checks graph first
- Incremental updates: only re-process changed files (SHA-256 cache)

**Setup**:
```bash
npx skills add safishamsi/graphify    # install skill
/graphify                              # build graph (one-time)
graphify claude install                # install hook
```

**Current graph**: 400 nodes, 521 edges, 66 communities, 10 god nodes

**Key commands**:
- `/graphify query "question"` -- BFS traversal
- `/graphify path "A" "B"` -- shortest path
- `/graphify explain "concept"` -- node explanation
- `/graphify --update` -- incremental rebuild

## Layer 6: Code Review Graph (Change Impact)

**What**: Blast radius analysis -- traces which files are affected by a code change.

**Savings**: 8.2x token reduction for code review context

**How**: Tree-sitter AST + SQLite graph, incremental updates, MCP server integration.

**Complementary with Graphify**: Graphify for architecture understanding, code-review-graph for change impact analysis.

---

## Combined Savings: Real-World Numbers

Based on production data (62K context, Opus 4.6):

### Per-Request Cost

| Scenario | Input | Output | Total |
|----------|-------|--------|-------|
| No optimization | $1.80 | $0.038 | $1.84 |
| All layers | $0.114 | $0.013 | $0.127 |
| **Savings** | **94%** | **65%** | **93%** |

### Request Lifecycle

```
[User types message]
    |
[RTK compresses CLI output]              <- Layer 1: -46% input
    |
[Graphify hook: check graph first]       <- Layer 5: skip unnecessary file reads
    |
[CPA routes to cached region]            <- Layer 2: 99% cache hit, 1/8 price
    |
[Claude thinks (extended thinking)]      <- NOT optimized (quality priority)
    |
[Claude generates response]
    |
[CLAUDE.md rules reduce verbosity]       <- Layer 3: -17% output
    |
[Caveman compresses text]                <- Layer 4: -65% output
    |
[User receives response]
```

### Remaining (Not Optimized by Design)

| Area | Why kept | Cost impact |
|------|----------|-------------|
| Thinking tokens | Quality priority, need strongest reasoning | ~$0.48/request at 32K budget |
| Model selection | Using Opus for all tasks | Sonnet would be 5x cheaper |
| MCP tools | Lazy-loaded, minimal overhead | Negligible |

---

## Cookie Pool Token Efficiency

Cookie pool design maximizes cache utilization:

- **Sticky cookie**: Same cookie reused for prompt cache locality
- **Fail-only switch**: Only change cookie on error, preserving cache
- **Internal retry**: Loops through pool without returning to conductor
- **MarkDead isolation**: Failed cookie excluded, others unaffected
- **Health check**: Zero-token validation on new cookie selection only

## Direct Account Token Efficiency

Region affinity scheduling maximizes Bedrock prompt cache:

- **Sticky region**: All requests stay in one region
- **Region-internal round-robin**: Multiple accounts share same cache
- **Auto-failover**: 3 consecutive failures -> 5min region blacklist -> switch
- **No guilt-by-association**: 401/403 only cooldowns the specific credential
- **Error classification**: 6 types with appropriate cooldown durations

---

## Pending / Future

### db8 Session Cache Fix
- Claude Code bug: session save filters `deferred_tools_delta`
- Causes cache miss on resume (26% vs 99% hit rate)
- Fix confirmed by Anthropic, waiting for release
- Cannot patch compiled binary (Mach-O)

### Token-Efficient Patterns
- Structured output (JSON/tables) cheaper than prose
- System prompt at beginning maximizes cache prefix
- Use `/compact` before context grows too large
- Use Agent subagents for heavy exploration (separate context)

---

## References

- RTK: `~/.claude/RTK.md`
- Region Affinity: `docs/region-affinity-scheduling.md`
- drona23 rules: `github.com/drona23/claude-token-efficient`
- Caveman: `github.com/JuliusBrussee/caveman`
- Graphify: `github.com/safishamsi/graphify`
- Code Review Graph: `github.com/tirth8205/code-review-graph`
- db8 analysis: `reddit.com/r/ClaudeAI/comments/1s8zxt4/`
