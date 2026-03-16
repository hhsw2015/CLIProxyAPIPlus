# Skywork Smart Fallback — Requirements & Detailed Design

> This document captures the full requirements, design rationale, and implementation details
> for the Skywork smart fallback feature. It is intended to survive session interruptions
> and enable any future session to resume implementation without re-gathering context.

---

## 1. Problem Statement

### 1.1 Observed Failure Pattern

The proxy service running on VPS (`20.63.96.0`) routes requests through the Skywork
provider (`desktop-llm.skywork.ai`). Under heavy load — especially during agentic coding
sessions that read large numbers of files (high token count) — the following failures
occur intermittently:

- **All** Skywork auth accounts fail simultaneously (multiple account hashes affected)
- Failures last several minutes (observed: ~2-3 minutes), then spontaneously recover
- Error modes:
  - `"awaiting response headers"` — upstream stops responding entirely (timeout)
  - `500 internal_error: failed to read response ... while reading body` — stream breaks mid-response
  - `context deadline exceeded` — Go client timeout fires

### 1.2 Root Cause

All Skywork accounts share the same upstream domain (`desktop-llm.skywork.ai`).
Rotating accounts within the same provider does not help — the failure is at the
upstream infrastructure level, likely Skywork's internal third-party compatibility
layer (possibly Bedrock-backed) hitting context-length or rate limits.

### 1.3 Usage Scenarios

The user uses **multiple client tools** through this proxy, each requesting different models:

- **Claude CLI** — requests `claude-opus-4.6` for agentic coding
- **Codex CLI** — requests `gpt-5.4` for agentic coding (with `model_context_window = 1000000`
  config to enable 1M context at the client side; the model name stays `gpt-5.4`)

Both tools route through the same Skywork provider and are affected by the same
upstream instability. Either model can fail independently.

### 1.4 Impact

During the failure window, all requests to the affected model hang and eventually
time out. For agentic coding sessions, this means:

- Multi-minute interruptions to active coding work
- Lost context when sessions time out
- No automatic recovery path — user must manually retry or wait

### 1.5 Goal

Add a Skywork-only smart fallback feature that:
- Keeps the user's requested model as the default/first-try path
- Automatically retries with suitable fallback models when the primary fails
- Works bidirectionally: Claude models can fall back to GPT, and GPT models can fall back to Claude
- Minimizes disruption to the user's coding session

---

## 2. Requirements

### 2.1 Functional Requirements

| ID | Requirement | Priority |
|----|-------------|----------|
| FR-1 | Requested model is always attempted first | Must |
| FR-2 | Fallback triggers only on actual execution failure, never proactively | Must |
| FR-3 | Support bidirectional cross-family fallback (Claude ↔ GPT) | Must |
| FR-4 | Same-family fallback preferred for light requests | Must |
| FR-5 | Cross-family fallback acceptable for heavy requests or when same-family unavailable | Must |
| FR-6 | Fallback candidates filtered by provider's configured `models:` list | Must |
| FR-7 | Failed model lanes tracked individually for cooldown | Must |
| FR-8 | Feature gated by a single config switch | Must |
| FR-9 | Cross-family fallback must not send Claude-specific headers to GPT targets (and vice versa) | Must |
| FR-10 | Experience identical to no-fallback when all models are healthy | Must |

### 2.2 Non-Functional Requirements

| ID | Requirement |
|----|-------------|
| NFR-1 | Skywork-only in v1 — must not affect other providers |
| NFR-2 | No new execution framework — reuse existing executor/translator stack |
| NFR-3 | No new failure-state mechanism — reuse existing `ModelStates` / cooldown |
| NFR-4 | Default off (`skywork-smart-fallback: false`) |
| NFR-5 | Model capability table extensible via code (add entry for new models) |

### 2.3 Trigger Conditions

Fallback is triggered when the **conversation cannot continue** with the current model:

| Condition | Triggers Fallback? |
|-----------|--------------------|
| Awaiting response headers timeout | Yes |
| Stream interruption mid-response (`while reading body`) | Yes |
| Upstream 5xx error (`500`, `502`, `503`, `504`) | Yes |
| Rate limiting (`429`) / quota exhaustion | Yes |
| All auth lanes for the same model exhausted | Yes |
| Another model might be "stronger" | **No** |
| Another model might be "faster" | **No** |
| Request is heavy (alone, without failure) | **No** |

### 2.4 Priority Principles

In order of priority:
1. **Model fidelity** — stay on the user's requested model as long as it works
2. **Cache continuity** — same user should stay on the same model to benefit from upstream caching
3. **Availability via degradation** — switch models only as a last resort to avoid session interruption

---

## 3. Design

### 3.1 Key Clarification: Model Names

There are no separate `xxx-1m` model names. The 1M context window is controlled by
**client-side configuration** (e.g., Codex CLI `model_context_window = 1000000`) and
**request-level parameters** (e.g., Anthropic beta header `context-1m-2025-08-07`),
not by distinct model names.

The actual Skywork model names (as configured in `config.yaml`) are:
- `claude-opus-4.6`
- `claude-sonnet-4.6`
- `claude-opus-4.5`
- `gpt-5.4`
- `gpt-5.3-codex`
- `gpt-5.2`

### 3.2 Architecture Overview

```
                         +---------------------+
                         |   Handler layer      |
                         |   (unchanged)        |
                         +----------+-----------+
                                    |
                         +----------v-----------+
                         |  AuthManager.Execute* |
                         |  (unchanged entry)    |
                         +----------+-----------+
                                    |
                    +---------------v----------------+
                    |  prepareExecutionModels()       |
                    |  +---------------------------+  |
                    |  | Existing alias/pool logic |  |
                    |  +-----------+---------------+  |
                    |              |                   |
                    |  +-----------v---------------+  |
                    |  | NEW: Skywork fallback      |  |
                    |  | planner injection          |  |
                    |  | (only when config enabled  |  |
                    |  |  + Skywork provider)       |  |
                    |  +---------------------------+  |
                    +---------------+----------------+
                                    | returns []string (model candidates)
                    +---------------v----------------+
                    |  Existing model-pool iteration  |
                    |  executeMixedOnce /             |
                    |  executeStreamWithModelPool     |
                    |  (unchanged loop)               |
                    +---------------+----------------+
                                    |
                    +---------------v----------------+
                    |  OpenAICompatExecutor           |
                    |  Execute / ExecuteStream        |
                    |  +---------------------------+  |
                    |  | NEW: family-aware          |  |
                    |  | passthrough (skip Claude   |  |
                    |  | headers for GPT, etc.)     |  |
                    |  +---------------------------+  |
                    +---------------+----------------+
                                    |
                    +---------------v----------------+
                    |  MarkResult() -- track by       |
                    |  actual exec model, not route   |
                    |  (reuses existing ModelStates)  |
                    +--------------------------------+
```

### 3.3 Model Capability Table (built-in)

Based on **actual Skywork-configured models**, organized by tier:

```
Tier | Model              | Family  | CodingRank
-----|--------------------|---------|-----------
T1   | claude-opus-4.6    | claude  | 1
T1   | gpt-5.4            | gpt     | 2
T2   | claude-sonnet-4.6  | claude  | 3
T2   | gpt-5.3-codex      | gpt     | 4
T3   | claude-opus-4.5    | claude  | 5
T3   | claude-sonnet-4.5  | claude  | 6
T3   | gpt-5.2            | gpt     | 7
```

Fields:
- `Tier`: capability tier (T1 = strongest, T2 = mid, T3 = weakest)
- `Family`: model vendor family (`"claude"`, `"gpt"`)
- `CodingRank`: overall coding capability ranking (lower = better), used as fallback
  priority. The chain tries every model from rank 1 to 7 (skipping the requested
  model which is always first) until one succeeds or all are exhausted.

This table is extensible by adding entries. No external config needed.

### 3.4 Request Classifier (lightweight)

Estimates whether a request is "heavy" (large context) or "light":

| Signal | Weight |
|--------|--------|
| Payload byte size > threshold (e.g., 100KB) | Heavy indicator |
| Many messages in conversation history | Heavy indicator |
| Large tool results in payload | Heavy indicator |
| File content attachments | Heavy indicator |

**No tokenization needed** — rough heuristic based on byte size and structure.
Classification affects **fallback ordering only**, never triggers fallback by itself.

### 3.5 Fallback Ordering Rules

**Core principle**: the requested model is always tried first. If it fails, walk
down the full fallback chain until a model succeeds. If all models fail, the request
fails as usual. The goal is to **keep the client's conversation alive**.

Given requested model `M` and request weight `W`:

**Light request** (same-family first, then cross-family):
```
1. M (requested model -- always first)
2. Same-family models, ordered by CodingRank
3. Cross-family models, ordered by CodingRank
4. If ALL fail -> return error to client
```

Rationale: light requests can be handled by weaker same-family models, so prefer
staying in the same family (less style drift) before crossing over.

**Heavy request** (cross-family OK immediately when same-tier exists):
```
1. M (requested model -- always first)
2. Same-tier cross-family model (if exists) -- a heavy request that crashed
   a T1 Claude may also crash a weaker same-family model, so try the T1 GPT
   (or vice versa) before going to T2
3. Remaining models ordered by CodingRank
4. If ALL fail -> return error to client
```

Rationale: if a T1 model can't handle a heavy request, a T2 same-family model is
even less likely to handle it. Better to try the other T1 model first.

#### Concrete Examples

**Claude Opus 4.6 fails (light request)**:
```
claude-opus-4.6 -> claude-sonnet-4.6 -> claude-opus-4.5 -> claude-sonnet-4.5
                -> gpt-5.4 -> gpt-5.3-codex -> gpt-5.2
(same-family first, then cross-family, all the way down)
```

**Claude Opus 4.6 fails (heavy request)**:
```
claude-opus-4.6 -> gpt-5.4 -> claude-sonnet-4.6 -> gpt-5.3-codex
                -> claude-opus-4.5 -> claude-sonnet-4.5 -> gpt-5.2
(same-tier cross-family first, then remaining by rank, all the way down)
```

**GPT-5.4 fails (light request)**:
```
gpt-5.4 -> gpt-5.3-codex -> gpt-5.2
        -> claude-opus-4.6 -> claude-sonnet-4.6 -> claude-opus-4.5 -> claude-sonnet-4.5
(same-family first, then cross-family, all the way down)
```

**GPT-5.4 fails (heavy request)**:
```
gpt-5.4 -> claude-opus-4.6 -> gpt-5.3-codex -> claude-sonnet-4.6
        -> gpt-5.2 -> claude-opus-4.5 -> claude-sonnet-4.5
(same-tier cross-family first, then remaining by rank, all the way down)
```

### 3.6 Candidate Filtering

Only models that appear in the provider's configured `models:` list are eligible.
If a provider config only has `claude-opus-4.6` and `claude-sonnet-4.6`, then
`gpt-5.4` will never appear in the fallback chain even if it exists in the
capability table.

### 3.7 Cooldown & Recovery

Reuses existing `ModelStates` mechanism in `conductor.go`:

- When a fallback candidate fails, `MarkResult()` records the failure against
  the **actual attempted model name** (not the user's route model)
- `isAuthBlockedForModel()` checks per-model cooldown before including a
  candidate in the fallback chain
- When cooldown expires, the model naturally becomes eligible again
- The highest-priority model (user's requested model) is always tried first
  once its cooldown clears

### 3.8 Cross-Family Execution

When the fallback target crosses model families:

**Falling back from Claude to GPT** — skip Claude-specific behaviors:
- Anthropic beta headers
- Anthropic passthrough body fields (e.g., `speed`)
- Anthropic image content rewriting (`requiresAnthropicImageContent`)

**Falling back from GPT to Claude** — add Claude-specific behaviors:
- Anthropic passthrough headers/betas should be applied
- Image content rewriting for Skywork Claude targets

**Keep in all cases** (provider-level, not model-level):
- Singularity transport (X-Skywork-Cookies, X-Skywork-Billing-Source)
- Stream handling, timeout normalization

Detection: check if the target upstream model name starts with `"claude-"` to
decide whether to apply Claude-specific passthrough.

### 3.9 Configuration

Single top-level field in `config.yaml`:

```yaml
skywork-smart-fallback: true
```

- Type: `bool`
- Default: `false`
- No additional knobs (model priorities, thresholds, etc. are code-internal)

---

## 4. Integration Points with Existing Code

### 4.1 Files to Create

| File | Purpose |
|------|---------|
| `sdk/cliproxy/auth/skywork_fallback.go` | Model capability table, request classifier, fallback planner |
| `sdk/cliproxy/auth/skywork_fallback_test.go` | Unit tests for planner logic |

### 4.2 Files to Modify

| File | Change |
|------|--------|
| `internal/config/config.go` | Add `SkyworkSmartFallback bool` field |
| `sdk/cliproxy/auth/conductor.go` | Modify `prepareExecutionModels()` to call planner for Skywork; track `Result.Model` by actual exec model |
| `sdk/cliproxy/auth/openai_compat_pool_test.go` | Add tests for fallback chain in model pool |
| `internal/runtime/executor/openai_compat_executor.go` | Conditional Claude-specific passthrough based on target model family |

### 4.3 Existing Code Hooks

| Hook | How It's Used |
|------|---------------|
| `prepareExecutionModels()` | Already returns `[]string` for model pool. Fallback planner appends candidates after existing alias/pool resolution. |
| `executeMixedOnce()` / `executeStreamWithModelPool()` | Already iterates model candidates. No change to loop -- just receives more candidates from `prepareExecutionModels()`. |
| `MarkResult()` + `ModelStates` | Already tracks per-model cooldowns with exponential backoff. Change: record against actual exec model, not route model. |
| `isSingularityAuth()` / `applySingularityHeaders()` | Still applied for all Skywork targets (both Claude and GPT). Transport is provider-level, not model-level. |
| `requiresAnthropicImageContent()` | Should only apply to Claude-family targets, not GPT-family. |
| `prepareOpenAICompatAnthropicPassthrough()` | Should only apply to Claude-family targets, not GPT-family. |
| `resolveCompatConfig()` | Used to detect Skywork provider for feature gating. |

### 4.4 Detection: Is This a Skywork Auth?

Existing detection methods (reuse, do not duplicate):
- `isSingularityAuth(auth)` -- checks provider/attributes for `"singularity"`
- `resolveCompatConfig(auth)` -- resolves to config entry with `Name == "skywork"`
- `auth.Provider == "skywork"` (case-insensitive)
- Base URL contains `"desktop-llm.skywork.ai"`

---

## 5. Worked Examples

### 5.1 Normal Operation (No Failures)

```
User requests: claude-opus-4.6 (via Claude CLI)
Config: skywork-smart-fallback: true
Provider models: [claude-opus-4.6, gpt-5.4, claude-sonnet-4.6, gpt-5.3-codex, ...]

1. prepareExecutionModels() builds full fallback chain
2. Try claude-opus-4.6 -> SUCCESS
3. Return response (identical to fallback-disabled behavior)
```

### 5.2 Claude Fails, Cascading Fallback (Heavy Request)

```
User requests: claude-opus-4.6 (large payload, classified as heavy)
Skywork upstream unstable for Claude

1. Chain: claude-opus-4.6 -> gpt-5.4 -> claude-sonnet-4.6 -> gpt-5.3-codex -> ...
2. Try claude-opus-4.6 -> TIMEOUT (504)
3. MarkResult(model="claude-opus-4.6", success=false) -> 1 min cooldown
4. Try gpt-5.4 -> SUCCESS (Claude-specific passthrough skipped for GPT target)
5. Return response from gpt-5.4
```

### 5.3 GPT Fails, Cascading All The Way Down (Light Request)

```
User requests: gpt-5.4 (via Codex CLI, small payload)
Skywork upstream unstable for all GPT models

1. Chain: gpt-5.4 -> gpt-5.3-codex -> gpt-5.2 -> claude-opus-4.6 -> claude-sonnet-4.6 -> ...
2. Try gpt-5.4 -> 500 error -> cooldown
3. Try gpt-5.3-codex -> 500 error -> cooldown
4. Try gpt-5.2 -> 500 error -> cooldown
5. Try claude-opus-4.6 -> SUCCESS (Claude passthrough applied)
6. Return response from claude-opus-4.6
   (conversation continues, not interrupted)
```

### 5.4 Everything Fails

```
User requests: claude-opus-4.6 (entire Skywork domain down)

1. Chain: claude-opus-4.6 -> gpt-5.4 -> claude-sonnet-4.6 -> ... -> gpt-5.2
2. Try each model in chain -> ALL FAIL
3. Return error to client (same as current behavior, no worse)
```

### 5.5 Cooldown Recovery

```
Request 1: claude-opus-4.6 -> TIMEOUT -> fallback to gpt-5.4 -> SUCCESS
  (claude-opus-4.6 enters 1 min cooldown)

Request 2 (within cooldown): chain starts from gpt-5.4 (cooled-down model skipped)
  -> gpt-5.4 -> SUCCESS

Request 3 (after cooldown expires): chain starts from claude-opus-4.6 again
  -> claude-opus-4.6 -> SUCCESS (recovered, back to primary)
```

### 5.6 Feature Disabled

```
Config: skywork-smart-fallback: false (default)

prepareExecutionModels() -> existing behavior unchanged
Only the requested model (or existing alias pool) is returned
No fallback chain injected
```

---

## 6. Out of Scope (v1)

- Fallback for non-Skywork providers
- User-configurable fallback chains or model priorities
- Proactive rerouting based on predicted load/latency
- Response-level quality comparison between models
- Persistent failure history across proxy restarts
- Notification to client about which model actually served the request
  (may be added later if needed)

---

## 7. Implementation Plan Reference

See `docs/plans/2026-03-16-skywork-smart-fallback-plan.md` for the task-by-task
TDD implementation plan (6 tasks). Note: the plan also needs updating to remove
references to non-existent `xxx-1m` model names.

---

## 8. Open Questions (Resolved)

| Question | Resolution |
|----------|------------|
| Should fallback stay within same model family? | No -- bidirectional cross-family (Claude <-> GPT) is acceptable to avoid minutes-long timeouts |
| How many config knobs? | Just one: `skywork-smart-fallback: true/false` |
| Are there separate 1M model names? | **No** -- 1M context is controlled by client config (`model_context_window`) and request parameters (Anthropic beta headers), not by distinct model names. Skywork models are `claude-opus-4.6`, `gpt-5.4`, etc. |
| How to detect Skywork? | Reuse existing `isSingularityAuth()` / `resolveCompatConfig()` |
| New failure tracking mechanism? | No -- reuse existing `ModelStates` / cooldown in conductor |
| Separate executor for fallback? | No -- reuse `OpenAICompatExecutor` with family-aware passthrough |
| GPT model name? | `gpt-5.4` (not `gpt-4.5`, not `gpt-5.4-1m`) |
| Does GPT-5.4 work on Skywork? | **Known issue**: Skywork's proxy of GPT-5.4 has a tool-calling format bug. When GPT-5.4 attempts parallel tool calls, Skywork's proxy concatenates multiple JSON objects into a single `arguments` field (e.g., `{"cmd":"a"}{"cmd":"b"}{"cmd":"c"}`) instead of properly formatting them. This causes clients to reject the malformed JSON, and the model enters an unrecoverable loop regenerating the same broken format. The same `gpt-5.4` model via other providers (e.g., zenscaleai) does NOT have this issue — confirmed in the same Codex session where it worked fine for 1780+ lines before switching to Skywork. This is a **Skywork proxy layer bug**, not a GPT-5.4 model bug. Impact on smart fallback: falling back to GPT-5.4 on Skywork may hit this bug when the client uses tool calling (agentic coding). Non-tool-calling requests (plain chat) should be unaffected. |

---

## 9. Server Reference

- VPS: `ssh azureuser@20.63.96.0 -i ~/Downloads/pikapk3216_vps_key.pem`
- Proxy runs in tmux session `cli-proxy-api-plus`
- Logs: `~/CLIProxyAPIPlus/logs/`
- Config: `~/CLIProxyAPIPlus/config.yaml`
- Skywork accounts: skywork, skywork2, skywork3, skyclaw2 (all share `desktop-llm.skywork.ai`)
- Singularity accounts: singularity, singularity1, singularity2 (Gemini models, same domain)
