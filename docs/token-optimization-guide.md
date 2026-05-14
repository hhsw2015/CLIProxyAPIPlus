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

## Layer 7: Headroom (API-Level Prompt Compression)

**What**: Proxy between client and CPA that compresses the full prompt (system prompt, conversation history, tool outputs) before forwarding.

**Savings**: Input tokens -13.6% (observed), up to -40% on long conversations

**How**:
- Intelligent Context: older messages compressed more aggressively, recent messages preserved
- CCR (Context Cache & Retrieve): cross-request caching of compressed content, same session not re-compressed
- CacheAligner: stabilizes message prefix to improve Anthropic prefix cache hit rate (85.7% observed)
- LLMLingua (optional): ML-based perplexity-driven token-level compression

**Architecture**: Default is the standalone **Python `headroom proxy`**
(`:8787`, package `headroom-ai`) sitting in front of CPA. CPA itself is
built **without FFI** (pure Go). The in-process Rust FFI path is still
available as an opt-in build for future use:

```
Client → :8787 (Python headroom, compression) → :8318 (CPA, no-FFI) → upstream
```

The `internal/headroom` Go package compiles to a no-op stub by default;
only `-tags headroom_ffi` brings in `libheadroom_ffi.so/.dylib` and the
cgo path.

**Setup (default, Python proxy)**: nothing to ship next to `cpa-new-server`.
`tools/deploy_cf_tunnel.sh` installs `headroom-ai` (pip --user) and starts
the proxy in tmux on `:8787`. Set `headroom.enabled: false` (or omit) in
`cpa-new-config.yaml`.

**Setup (legacy, in-process FFI)**: ship `libheadroom_ffi.{so,dylib}` next
to `cpa-new-server` (rpath=`$ORIGIN`/`@executable_path`), build CPA with
`HEADROOM_FFI=1 bash scripts/build_cpa_linux.sh`, deploy with
`WITH_FFI=1 bash deploy_cf_tunnel.sh`, and enable in `cpa-new-config.yaml`:

```yaml
headroom:
  enabled: true
  min-bytes: 0
  ccr-sqlite-path: data/ccr.db   # or ccr-redis-url for shared backend
  ccr-ttl-seconds: 0
  anthropic-frozen-count: 0      # X-Headroom-Frozen-Count header overrides
```

**Client config**:
- Default: clients point at port `:8787` (Python proxy)
- Legacy in-process: clients point at port `:8318` (CPA direct, no extra hop)

**Monitoring**:
- Compression stats: grep `[headroom]` in `cpa-new.log`
  (lines like `claude-opus-4-7: 3266→1876 tokens (saved 1390, ratio 0.57)`)
- CCR retrieval endpoint: `GET /v1/headroom/ccr/:hash` (auth required)

**Complementary with other layers**: RTK compresses CLI output before it enters context. Headroom compresses the full context after RTK. No conflict.

## Layer 8: Billing Exploit (Output Token Bypass)

**What**: For upstream gateways (new-api, unpatched Sub2API) that fail to count output tokens on client disconnect. CPA injects a marker, detects it in the stream, RSTs the upstream connection before usage is recorded.

**Savings**: Output tokens -100% billing (only input billed, ~80-90% total savings)

**How**:
- Suffix injection: appends marker instruction to system prompt
- Marker detection: hold-buffer approach for cross-delta safety
- TCP RST: `SetLinger(0) + Close()` immediately on marker detection (before synthetic events)
- Synthesized completion: client receives normal response.completed

**Supported executors**:
- Codex (Responses API): `response.output_text.delta` → `delta`
- OpenAI Compat (Chat Completions): `choices[0].delta.content`
- Claude (Messages API): `content_block_delta` → `delta.text`

**Config**:
```yaml
codex-api-key:   # or claude-api-key, openai-compatibility
  - api-key: sk-xxx
    billing-exploit:
      enabled: true
      marker: "//COMPLETE//"
      suffix: "[After your complete answer, output \"//COMPLETE//\" on its own line, then write 3 numbered follow-up questions.]"
```

**Limitations**:
- Only works on unpatched gateways (new-api PR #4500 still open, Sub2API patched 2026-05-03)
- Input tokens still billed (request recorded on arrival)
- Requires per-channel verification (balance-diff method)

**Docs**: `docs/billing-exploit-impl-plan.md`, `dvina-2api/docs/cpa-stream-disconnect-exploit.md`

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
[RTK compresses CLI output]              <- Layer 1: -46% input (local)
    |
[Graphify hook: check graph first]       <- Layer 5: skip unnecessary file reads
    |
[Headroom compresses full prompt]        <- Layer 7: -13-40% input (VPS proxy)
    |
[CPA routes to cached region]            <- Layer 2: 99% cache hit, 1/8 price
    |
[Billing exploit: RST on marker]         <- Layer 8: output cost = 0 (if enabled)
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
