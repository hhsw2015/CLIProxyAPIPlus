# Skywork Smart Fallback Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Add a Skywork-only smart fallback feature that keeps the requested model as the default path, but automatically retries with more suitable fallback models when the requested Skywork model times out, returns transient upstream failures, or becomes unavailable.

**Architecture:** Keep the existing request flow intact: handlers still call `AuthManager.Execute*`, the auth manager still selects auths, and `OpenAICompatExecutor` still performs translation and upstream execution. The new logic only injects a Skywork-specific fallback model chain into the existing per-auth model pool path, plus a light request-load classifier and a built-in model capability catalog.

**Tech Stack:** Go, existing `sdk/cliproxy/auth` auth manager, existing `internal/runtime/executor/openai_compat_executor.go`, existing translator stack.

---

### Task 1: Capture the design in code-facing types

**Files:**
- Create: `sdk/cliproxy/auth/skywork_fallback.go`
- Test: `sdk/cliproxy/auth/skywork_fallback_test.go`

**Step 1: Write the failing tests**

Add focused tests for:
- Skywork smart fallback disabled returns only the requested model.
- Skywork smart fallback enabled keeps the requested model first.
- A heavy Claude 1M request falls back to `gpt-5.4-1m` before narrower-context Claude fallbacks.
- A light Claude 1M request falls back to same-family narrower Claude before cross-family GPT.
- Candidates not configured in the provider’s `models:` list are filtered out.

**Step 2: Run the tests to verify they fail**

Run:

```bash
go test ./sdk/cliproxy/auth -run 'TestSkyworkSmartFallback'
```

Expected: FAIL because the planner/types do not exist yet.

**Step 3: Add minimal implementation**

Create a new file containing:
- A built-in Skywork model capability table with fields like `name`, `family`, `contextWindowTokens`, `codingRank`, `fallbackRank`.
- A light request classifier that estimates request heaviness from `opts.OriginalRequest` / `req.Payload` size and structure.
- A planner that returns an ordered candidate list beginning with the requested model and followed by fallback models based on:
  - same requested family first when the request is light
  - same context class first when the request is heavy
  - only configured provider models are eligible

**Step 4: Run the tests to verify they pass**

Run:

```bash
go test ./sdk/cliproxy/auth -run 'TestSkyworkSmartFallback'
```

Expected: PASS

---

### Task 2: Add a simple config switch

**Files:**
- Modify: `internal/config/config.go`
- Test: `sdk/cliproxy/auth/skywork_fallback_test.go`

**Step 1: Write the failing test**

Add a test that builds a config with `skywork-smart-fallback: true` semantics and verifies the planner is only active when enabled.

**Step 2: Run the test to verify it fails**

Run:

```bash
go test ./sdk/cliproxy/auth -run 'TestSkyworkSmartFallback_RequiresConfigFlag'
```

Expected: FAIL because config has no such field.

**Step 3: Write minimal implementation**

Add a top-level config field:

```go
SkyworkSmartFallback bool `yaml:"skywork-smart-fallback" json:"skywork-smart-fallback"`
```

Do not add any extra knobs. Keep default `false`.

**Step 4: Run the test to verify it passes**

Run:

```bash
go test ./sdk/cliproxy/auth -run 'TestSkyworkSmartFallback_RequiresConfigFlag'
```

Expected: PASS

---

### Task 3: Reuse the existing model-pool hook

**Files:**
- Modify: `sdk/cliproxy/auth/conductor.go`
- Test: `sdk/cliproxy/auth/openai_compat_pool_test.go`

**Step 1: Write the failing tests**

Add tests that verify:
- `prepareExecutionModels()` returns only the requested model when fallback is disabled.
- `prepareExecutionModels()` returns a fallback chain for Skywork when enabled.
- The requested model remains first.
- A candidate with active `ModelState.NextRetryAfter` is skipped from the returned chain.

**Step 2: Run the tests to verify they fail**

Run:

```bash
go test ./sdk/cliproxy/auth -run 'TestManagerPrepareExecutionModels_Skywork'
```

Expected: FAIL because `prepareExecutionModels()` does not accept request context/options and cannot build fallback chains.

**Step 3: Write minimal implementation**

Refactor:
- Change `prepareExecutionModels(auth, routeModel)` to accept the request/options context it needs.
- Keep the existing alias-pool behavior as-is for non-Skywork providers.
- For Skywork + config enabled, call the new planner and return its candidate chain.
- Filter candidates using existing `auth.ModelStates` cooldown state so broken fallback lanes are temporarily skipped.

**Step 4: Run the tests to verify they pass**

Run:

```bash
go test ./sdk/cliproxy/auth -run 'TestManagerPrepareExecutionModels_Skywork|TestManagerExecute_OpenAICompatAliasPool'
```

Expected: PASS

---

### Task 4: Track failures against the actual attempted model lane

**Files:**
- Modify: `sdk/cliproxy/auth/conductor.go`
- Test: `sdk/cliproxy/auth/openai_compat_pool_test.go`

**Step 1: Write the failing test**

Add a test where the first fallback candidate fails with a transient upstream error and verify:
- The next request skips that candidate during cooldown.
- The requested route model still remains logically associated with the same conversation, but the failed lane is tracked by the actual execution model name.

**Step 2: Run the test to verify it fails**

Run:

```bash
go test ./sdk/cliproxy/auth -run 'TestManagerExecute_SkyworkFallbackSkipsCooledDownExecutionModel'
```

Expected: FAIL because current result tracking records the route model instead of the actual attempted model lane.

**Step 3: Write minimal implementation**

In `executeMixedOnce`, `executeCountMixedOnce`, and `executeStreamWithModelPool`:
- keep `routeModel` for user-facing request identity
- but record `Result.Model` using the actual `execReq.Model` being attempted

This reuses the existing `ModelStates` / cooldown system instead of inventing a second failure-state mechanism.

**Step 4: Run the test to verify it passes**

Run:

```bash
go test ./sdk/cliproxy/auth -run 'TestManagerExecute_SkyworkFallbackSkipsCooledDownExecutionModel'
```

Expected: PASS

---

### Task 5: Keep cross-family fallback inside the existing OpenAI-compatible executor

**Files:**
- Modify: `internal/runtime/executor/openai_compat_executor.go`
- Test: `internal/runtime/executor/openai_compat_executor_timeout_test.go`
- Test: `internal/runtime/executor/openai_compat_executor_passthrough_test.go`

**Step 1: Write the failing test**

Add a focused test that verifies when the fallback target is GPT-family:
- Claude-specific passthrough headers/betas are not forced onto the upstream request
- generic OpenAI-compatible translation still works

**Step 2: Run the test to verify it fails**

Run:

```bash
go test ./internal/runtime/executor -run 'TestOpenAICompatExecutor.*SkyworkFallback'
```

Expected: FAIL because Anthropic passthrough behavior is currently unconditional.

**Step 3: Write minimal implementation**

Reuse the existing executor/translator path. Only adjust the parts that are truly family-specific:
- detect whether the target upstream model is Claude-family or GPT-family
- keep Anthropic passthrough only for Claude-family targets
- continue to use the existing translator, request logging, timeout normalization, and stream handling

Do not create a separate fallback executor.

**Step 4: Run the tests to verify they pass**

Run:

```bash
go test ./internal/runtime/executor -run 'TestOpenAICompatExecutor'
```

Expected: PASS

---

### Task 6: End-to-end verification

**Files:**
- Verify only; no new files required unless implementation needs docs update

**Step 1: Run focused auth-manager tests**

```bash
go test ./sdk/cliproxy/auth -run 'TestManagerPrepareExecutionModels_Skywork|TestManagerExecute_SkyworkFallback|TestManagerExecute_OpenAICompatAliasPool'
```

Expected: PASS

**Step 2: Run focused executor tests**

```bash
go test ./internal/runtime/executor -run 'TestOpenAICompatExecutor'
```

Expected: PASS

**Step 3: Run a broader regression slice**

```bash
go test ./sdk/cliproxy/auth ./internal/runtime/executor
```

Expected: PASS

**Step 4: Smoke-check the live binary if needed**

Build and deploy only after tests pass, then confirm a minimal `POST /v1/messages?beta=true` still succeeds on the VPS.

---

### Design Constraints

- Smart fallback is **Skywork-only** in v1.
- Config is a **single top-level switch** only.
- Requested model is always attempted first.
- Fallback happens only after actual execution failure; there is no proactive reroute on healthy requests.
- Existing handlers, auth manager, translators, and executor flow must be reused.
- No second request execution framework.
- Cross-family fallback should happen only inside the existing OpenAI-compatible execution path.
- Lane health reuse should come from existing `ModelStates` / cooldown logic, not a new global failure database.
