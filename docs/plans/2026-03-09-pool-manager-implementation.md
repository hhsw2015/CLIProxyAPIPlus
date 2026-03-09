# Pool Manager Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Add a pool manager for codex credentials that maintains a fixed-size healthy `active` pool, keeps the current routing behavior (`fill-first` / `round-robin`) intact, and minimizes request latency by keeping unhealthy auths out of runtime routing.

**Architecture:** Introduce a `PoolManager` runtime component that sits in front of the existing selector path. It owns only pool membership (`active` / `reserve` / `limit` in memory), while the existing auth lifecycle remains the source of truth for file deletion, movement to `limit`, token refresh, and final auth disposition. The service will publish only `active` auths to the existing `coreManager`, so the current selector logic remains unchanged and operates on a bounded, healthy buffer.

**Tech Stack:** Go, existing `sdk/cliproxy` service/auth pipeline, watcher `AuthUpdate` queue, file-backed auth storage, existing codex refresh and archive handling, Go unit tests.

## Current Status

截至当前版本，后端主体已经实现，当前代码状态可以概括为：

- `active / reserve / low_quota / limit` 四类运行时池状态已落地
- `Codex` 低成本 probe、`401 -> refresh -> verify` 已落地
- `active` 摘除后的补号已改为异步 worker
- 无效文件删除 / 限额归档已改为异步文件处理
- `in-flight` 保护已接入真实请求选中路径
- `low_quota` 软隔离池已对 `Codex` 生效
- `/v0/management/usage` 已返回 `pool` 指标
- 当前仓库中的 TUI 不展示 `pool`

当前剩余工作主要是策略调优和文档收口，而不是主功能补全。

---

## Scope Notes

- First version supports only `codex`.
- Do not change the on-disk root auth directory layout.
- Keep `auth-dir/limit` as the physical location for quota-exhausted auths.
- Keep existing `fill-first` and `round-robin` logic intact.
- Pool state is in memory only for v1.
- `low_quota` is a runtime-only soft isolation state.
- `low_quota` is not background-probed in v1.

## Success Metrics

Implementation is not complete when the code compiles. It must improve observable runtime behavior.

Primary acceptance metrics:

1. Request success rate

- Use the existing request statistics already exposed by the system.
- Compare:
  - `total_requests`
  - `success_count`
  - `failure_count`
  - derived success rate

2. Service health monitoring

- Existing health/status views should show fewer red states after pool mode is enabled.
- PoolManager must reduce bad-auth selection, not just reshuffle internal state.

3. Latency improvement

- Fewer retries caused by unhealthy auths
- Better request stability under `fill-first`

The implementation should make it easy to answer:

- did success rate improve?
- did the pool reduce bad-auth retries?
- did active pool churn correlate with healthier request outcomes?

## Phase Breakdown

- Phase 1: config + event model + scaffolding
- Phase 2: startup `active` pool bootstrap
- Phase 3: runtime publish/unpublish of `active` auths
- Phase 4: health disposition integration (`request` and `pool_probe`)
- Phase 5: active / reserve / limit probe loops
- Phase 6: logs, observability, docs, and regression coverage
- Phase 7: runtime hardening (`in-flight`, async rebalance, low-quota isolation)

## Task 1: Add Pool Manager Config Types

**Files:**
- Modify: `internal/config/config.go`
- Modify: `config.example.yaml`
- Test: `internal/config/config_test.go` or the closest config parsing test file

**Step 1: Write the failing config parsing test**

Add a test that loads config containing:

```yaml
pool-manager:
  size: 100
  provider: codex
  active-idle-scan-interval-seconds: 1800
  reserve-scan-interval-seconds: 300
  limit-scan-interval-seconds: 21600
  reserve-sample-size: 20
```

Assert parsed values are present and defaults are applied when optional fields are omitted.

**Step 2: Run the config test to verify it fails**

Run:

```bash
go test ./internal/config -run PoolManager -count=1
```

Expected: FAIL due to missing config fields/types.

**Step 3: Add config structs**

Implement:

- `PoolManagerConfig` in `internal/config/config.go`
- `Config.PoolManager PoolManagerConfig`

Add fields:

- `Size int`
- `Provider string`
- `ActiveIdleScanIntervalSeconds int`
- `ReserveScanIntervalSeconds int`
- `LimitScanIntervalSeconds int`
- `ReserveSampleSize int`

Add a small normalizer/sanitizer:

- default provider to `codex`
- clamp negative values to zero
- treat `size <= 0` as disabled

**Step 4: Document the config**

Update `config.example.yaml` with commented examples and short behavior notes.

**Step 5: Re-run the config test**

Run:

```bash
go test ./internal/config -run PoolManager -count=1
```

Expected: PASS

**Step 6: Commit**

```bash
git add internal/config/config.go config.example.yaml
git commit -m "feat(config): add pool manager settings"
```

## Task 2: Define Pool Manager Runtime Types and Interfaces

**Files:**
- Create: `sdk/cliproxy/pool_manager.go`
- Create: `sdk/cliproxy/pool_manager_types.go`
- Test: `sdk/cliproxy/pool_manager_test.go`

**Step 1: Write the failing tests for basic state transitions**

Add tests for:

- initialize empty pool manager
- add active member
- remove active member
- underfilled pool detection
- reserve candidate ordering

**Step 2: Run tests to verify failure**

Run:

```bash
go test ./sdk/cliproxy -run PoolManager -count=1
```

Expected: FAIL because the new files/types do not exist.

**Step 3: Add core types**

Implement:

- `PoolState` enum: `active`, `reserve`, `limit`
- `PoolMember`
- `AuthDisposition`
- `PoolManager`

`PoolMember` should include:

- `AuthID`
- `Provider`
- `PoolState`
- `LastSelectedAt`
- `LastSuccessAt`
- `LastProbeAt`
- `NextProbeAt`
- `ConsecutiveFailures`
- `LastProbeReason`

`AuthDisposition` should include:

- `AuthID`
- `Provider`
- `Model`
- `Healthy`
- `PoolEligible`
- `Deleted`
- `MovedToLimit`
- `Refreshed`
- `QuotaExceeded`
- `NextRetryAfter`
- `NextRecoverAt`
- `Source`

**Step 4: Add thread-safe pool manager scaffolding**

Implement only:

- constructor
- getters for active size and active IDs
- methods for add/remove/replace membership

No probe logic yet.

**Step 5: Re-run tests**

Run:

```bash
go test ./sdk/cliproxy -run PoolManager -count=1
```

Expected: PASS for the initial state tests.

**Step 6: Commit**

```bash
git add sdk/cliproxy/pool_manager.go sdk/cliproxy/pool_manager_types.go sdk/cliproxy/pool_manager_test.go
git commit -m "feat(cliproxy): add pool manager core types"
```

## Task 3: Add Startup Candidate Discovery Without Full Runtime Load

**Files:**
- Modify: `internal/watcher/synthesizer/file.go`
- Modify: `internal/watcher/dispatcher.go`
- Modify: `sdk/cliproxy/types.go`
- Modify: `sdk/cliproxy/watcher.go`
- Test: `internal/watcher/watcher_test.go`
- Test: `sdk/cliproxy/service_startup_auth_bootstrap_test.go`

**Step 1: Write the failing tests**

Add tests proving:

- startup can obtain a snapshot of auth candidates without publishing them all to runtime
- `limit/` entries are excluded from startup active bootstrap candidates
- snapshot preserves metadata needed for pool selection

**Step 2: Run the tests**

Run:

```bash
go test ./internal/watcher ./sdk/cliproxy -run 'Pool|Startup' -count=1
```

Expected: FAIL because pool-aware bootstrap does not exist.

**Step 3: Add explicit candidate snapshot helpers**

Do not change file layout. Add helper methods that return:

- root auth candidates
- limit auth candidates

These should be lightweight snapshots suitable for `PoolManager` startup, not full runtime publication.

**Step 4: Keep existing behavior untouched when pool mode is disabled**

Guard the new path behind `cfg.PoolManager.Size > 0`.

**Step 5: Re-run tests**

Run:

```bash
go test ./internal/watcher ./sdk/cliproxy -run 'Pool|Startup' -count=1
```

Expected: PASS

**Step 6: Commit**

```bash
git add internal/watcher/synthesizer/file.go internal/watcher/dispatcher.go sdk/cliproxy/types.go sdk/cliproxy/watcher.go
git commit -m "feat(pool): add startup candidate discovery"
```

## Task 4: Bootstrap `active` Pool on Service Start

**Files:**
- Modify: `sdk/cliproxy/service.go`
- Modify: `sdk/cliproxy/builder.go`
- Test: `sdk/cliproxy/service_startup_auth_bootstrap_test.go`
- Test: `sdk/cliproxy/pool_manager_test.go`

**Step 1: Write the failing startup test**

Add a test for:

- pool size = 2
- four codex auth snapshots available
- startup chooses and publishes exactly two active auths

Assert:

- only active auths are published to `coreManager`
- model registry reflects only active auths

**Step 2: Run the test**

Run:

```bash
go test ./sdk/cliproxy -run 'Startup.*Pool|Pool.*Startup' -count=1
```

Expected: FAIL because startup still publishes all auths.

**Step 3: Add service startup bootstrap path**

Modify `Service.Run(...)` so that when pool mode is enabled:

- construct `PoolManager`
- discover startup candidates
- fill `active` until `size` is met or candidates exhausted
- publish only `active`

Do not replace existing startup path for disabled mode.

**Step 4: Re-run tests**

Run:

```bash
go test ./sdk/cliproxy -run 'Startup.*Pool|Pool.*Startup' -count=1
```

Expected: PASS

**Step 5: Commit**

```bash
git add sdk/cliproxy/service.go sdk/cliproxy/builder.go sdk/cliproxy/service_startup_auth_bootstrap_test.go
git commit -m "feat(pool): bootstrap active auth pool at startup"
```

## Task 5: Publish `active` Set Changes Through Existing `AuthUpdate`

**Files:**
- Modify: `sdk/cliproxy/service.go`
- Modify: `internal/watcher/watcher.go`
- Modify: `internal/watcher/dispatcher.go`
- Test: `internal/watcher/watcher_test.go`
- Test: `sdk/cliproxy/pool_manager_test.go`

**Step 1: Write the failing test**

Add a test proving:

- when an auth leaves `active`, a delete-like runtime update is emitted
- when an auth enters `active`, an add/modify-like runtime update is emitted

**Step 2: Run the test**

Run:

```bash
go test ./sdk/cliproxy ./internal/watcher -run 'Pool.*Publish|AuthUpdate.*Pool' -count=1
```

Expected: FAIL because `PoolManager` is not connected to `AuthUpdate`.

**Step 3: Add active set diff publication**

Implement:

- compare previous and current active IDs
- emit runtime updates for add/modify/delete
- route them into the existing `AuthUpdate` queue path

Keep these semantics:

- `active` add -> runtime add/modify
- `active` removal -> runtime delete

**Step 4: Re-run tests**

Run:

```bash
go test ./sdk/cliproxy ./internal/watcher -run 'Pool.*Publish|AuthUpdate.*Pool' -count=1
```

Expected: PASS

**Step 5: Commit**

```bash
git add sdk/cliproxy/service.go internal/watcher/watcher.go internal/watcher/dispatcher.go
git commit -m "feat(pool): publish active set changes via auth updates"
```

## Task 6: Add Disposition Event From Existing Auth Result Handling

**Files:**
- Modify: `sdk/cliproxy/auth/conductor.go`
- Modify: `sdk/cliproxy/auth/types.go`
- Test: `sdk/cliproxy/auth/conductor_archive_test.go`
- Test: `sdk/cliproxy/auth/conductor_overrides_test.go`
- Create or modify: `sdk/cliproxy/auth/conductor_pool_hook_test.go`

**Step 1: Write the failing test**

Add tests covering:

- 429 result emits disposition with `MovedToLimit=true`
- deleted auth emits disposition with `Deleted=true`
- refreshed auth emits disposition with `Refreshed=true`
- unknown 401 emits disposition with no destructive action

**Step 2: Run the test**

Run:

```bash
go test ./sdk/cliproxy/auth -run Disposition -count=1
```

Expected: FAIL because `AuthDisposition` hook path does not exist.

**Step 3: Extend the auth hook interface**

Add a new callback, for example:

```go
OnAuthDisposition(ctx context.Context, disposition AuthDisposition)
```

Emit it only after the existing result handling path has already completed the final auth action.

**Step 4: Re-run tests**

Run:

```bash
go test ./sdk/cliproxy/auth -run Disposition -count=1
```

Expected: PASS

**Step 5: Commit**

```bash
git add sdk/cliproxy/auth/conductor.go sdk/cliproxy/auth/types.go sdk/cliproxy/auth/conductor_pool_hook_test.go
git commit -m "feat(auth): emit final auth disposition events"
```

## Task 7: Connect Disposition Events Back Into Pool Membership

**Files:**
- Modify: `sdk/cliproxy/pool_manager.go`
- Modify: `sdk/cliproxy/service.go`
- Test: `sdk/cliproxy/pool_manager_test.go`
- Test: `sdk/cliproxy/service_test.go` or closest service-level pool test

**Step 1: Write the failing test**

Add tests proving:

- active auth marked deleted is removed from active
- active auth moved to limit is removed from active
- underfilled active pool triggers replacement
- refreshed auth remains eligible

**Step 2: Run the test**

Run:

```bash
go test ./sdk/cliproxy -run 'Pool.*Disposition|Disposition.*Pool' -count=1
```

Expected: FAIL because disposition events do not alter pool membership yet.

**Step 3: Implement disposition handling**

In `PoolManager`, consume disposition events and update:

- active membership
- reserve membership
- underfill tracking

If active falls below `size`, trigger promotion attempt.

**Step 4: Re-run tests**

Run:

```bash
go test ./sdk/cliproxy -run 'Pool.*Disposition|Disposition.*Pool' -count=1
```

Expected: PASS

**Step 5: Commit**

```bash
git add sdk/cliproxy/pool_manager.go sdk/cliproxy/service.go
git commit -m "feat(pool): update pool membership from auth disposition"
```

## Task 8: Add Startup and Runtime Promotion Checks

**Files:**
- Modify: `sdk/cliproxy/pool_manager.go`
- Modify: `sdk/cliproxy/service.go`
- Test: `sdk/cliproxy/pool_manager_test.go`

**Step 1: Write the failing test**

Add tests proving:

- reserve auth is not promoted unless it passes a health check
- startup active fill stops when enough healthy auths are found
- reserve auths are not eagerly checked at startup once active is full

**Step 2: Run the test**

Run:

```bash
go test ./sdk/cliproxy -run 'Pool.*Promote|Promote.*Pool' -count=1
```

Expected: FAIL because promotion path is incomplete.

**Step 3: Implement promotion health gating**

Rules:

- reserve auths are cheap-discovered first
- they are health-checked only when needed for promotion
- once healthy, they can be added to active

**Step 4: Re-run tests**

Run:

```bash
go test ./sdk/cliproxy -run 'Pool.*Promote|Promote.*Pool' -count=1
```

Expected: PASS

**Step 5: Commit**

```bash
git add sdk/cliproxy/pool_manager.go sdk/cliproxy/service.go
git commit -m "feat(pool): gate active promotion on health checks"
```

## Task 9: Add Background Active Probe Loop

**Files:**
- Modify: `sdk/cliproxy/pool_manager.go`
- Modify: `sdk/cliproxy/service.go`
- Create or modify: `sdk/cliproxy/pool_manager_probe_test.go`

**Step 1: Write the failing test**

Add tests proving:

- active probe loop targets active auths only
- recently successful auths are skipped
- idle active auths become probe candidates

**Step 2: Run the test**

Run:

```bash
go test ./sdk/cliproxy -run 'Active.*Probe|Probe.*Active' -count=1
```

Expected: FAIL because no active probe loop exists.

**Step 3: Implement active probe scheduler**

The loop should:

- run at a modest interval
- prioritize active auths with stale health state
- avoid probing auths that just succeeded in real traffic

For v1, a simple interval + stale check is enough.

**Step 4: Re-run tests**

Run:

```bash
go test ./sdk/cliproxy -run 'Active.*Probe|Probe.*Active' -count=1
```

Expected: PASS

**Step 5: Commit**

```bash
git add sdk/cliproxy/pool_manager.go sdk/cliproxy/service.go sdk/cliproxy/pool_manager_probe_test.go
git commit -m "feat(pool): add active health probe loop"
```

## Task 10: Add Reserve Probe Loop

**Files:**
- Modify: `sdk/cliproxy/pool_manager.go`
- Test: `sdk/cliproxy/pool_manager_probe_test.go`

**Step 1: Write the failing test**

Add tests proving:

- reserve sampling is randomized
- reserve probing uses `reserve-sample-size`
- reserve is not fully scanned on each loop

**Step 2: Run the test**

Run:

```bash
go test ./sdk/cliproxy -run 'Reserve.*Probe|Probe.*Reserve' -count=1
```

Expected: FAIL because reserve probe loop does not exist.

**Step 3: Implement reserve random sampling**

Rules:

- no full reserve sweep
- use configured sample size
- keep concurrency low

**Step 4: Re-run tests**

Run:

```bash
go test ./sdk/cliproxy -run 'Reserve.*Probe|Probe.*Reserve' -count=1
```

Expected: PASS

**Step 5: Commit**

```bash
git add sdk/cliproxy/pool_manager.go sdk/cliproxy/pool_manager_probe_test.go
git commit -m "feat(pool): add reserve sampling probes"
```

## Task 11: Add Limit Probe Loop

**Files:**
- Modify: `sdk/cliproxy/pool_manager.go`
- Test: `sdk/cliproxy/pool_manager_probe_test.go`

**Step 1: Write the failing test**

Add tests proving:

- limit auths are not published to runtime routing
- limit auths are retried at the lowest frequency
- recovered limit auths return to reserve, not directly to active

**Step 2: Run the test**

Run:

```bash
go test ./sdk/cliproxy -run 'Limit.*Probe|Probe.*Limit' -count=1
```

Expected: FAIL because limit recovery loop does not exist.

**Step 3: Implement limit recovery**

Rules:

- honor `NextRecoverAt` when available
- otherwise use configured interval
- successful recovery returns auth to reserve

**Step 4: Re-run tests**

Run:

```bash
go test ./sdk/cliproxy -run 'Limit.*Probe|Probe.*Limit' -count=1
```

Expected: PASS

**Step 5: Commit**

```bash
git add sdk/cliproxy/pool_manager.go sdk/cliproxy/pool_manager_probe_test.go
git commit -m "feat(pool): add limit recovery probes"
```

## Task 12: Implement Low-Cost Codex Probe Path

**Files:**
- Create: `sdk/cliproxy/pool_probe_codex.go`
- Test: `sdk/cliproxy/pool_probe_codex_test.go`
- Reference: `temp/apitest/main.py`
- Reference: `temp/apitest/backend/clients.py`

**Step 1: Write the failing tests**

Cover:

- active probe uses `wham/usage`
- 429 maps to quota path
- 401 invokes refresh + verify path
- deleted auth emits dead disposition

**Step 2: Run the tests**

Run:

```bash
go test ./sdk/cliproxy -run 'Codex.*Probe|Probe.*Codex' -count=1
```

Expected: FAIL because no dedicated codex probe implementation exists.

**Step 3: Implement probe path**

Use low-cost request behavior similar to existing reference scripts:

- `GET https://chatgpt.com/backend-api/wham/usage`
- include `Chatgpt-Account-Id` when available
- keep probe traffic conservative

**Step 4: Re-run tests**

Run:

```bash
go test ./sdk/cliproxy -run 'Codex.*Probe|Probe.*Codex' -count=1
```

Expected: PASS

**Step 5: Commit**

```bash
git add sdk/cliproxy/pool_probe_codex.go sdk/cliproxy/pool_probe_codex_test.go
git commit -m "feat(pool): add low-cost codex probe path"
```

## Task 13: Add 401 Refresh + Verify Integration

**Files:**
- Modify: `sdk/cliproxy/pool_probe_codex.go`
- Reuse: existing codex refresh code in auth path
- Test: `sdk/cliproxy/pool_probe_codex_test.go`

**Step 1: Write the failing tests**

Cover:

- 401 + refresh success + verify success -> healthy
- 401 + refresh success + verify deleted -> deleted
- 401 + ambiguous verify -> unknown, not deleted

**Step 2: Run tests**

Run:

```bash
go test ./sdk/cliproxy -run '401|Refresh|Verify' -count=1
```

Expected: FAIL because the probe path does not fully align with codex auth semantics.

**Step 3: Implement the shared 401 decision path**

Do not duplicate delete/archive semantics in `PoolManager`.

Instead:

- probe path should produce a normalized result
- existing auth disposition path should remain the final owner of delete / limit / refresh persistence decisions

**Step 4: Re-run tests**

Run:

```bash
go test ./sdk/cliproxy -run '401|Refresh|Verify' -count=1
```

Expected: PASS

**Step 5: Commit**

```bash
git add sdk/cliproxy/pool_probe_codex.go sdk/cliproxy/pool_probe_codex_test.go
git commit -m "feat(pool): normalize codex 401 recovery path"
```

## Task 14: Add Structured Logs

**Files:**
- Modify: `sdk/cliproxy/pool_manager.go`
- Modify: `sdk/cliproxy/service.go`
- Modify: `sdk/cliproxy/auth/conductor.go`
- Test: `sdk/cliproxy/pool_manager_test.go` where feasible

**Step 1: Add the log prefix constants**

Add stable prefixes:

- `pool-manager:`
- `auth-disposition:`
- `pool-probe:`
- `pool-publish:`

**Step 2: Implement logging at key transitions**

Minimum required logs:

- pool enabled / disabled
- startup discovery counts
- startup active fill counts
- auth removed from active with reason
- auth promoted to active
- active set publish diff
- reserve sample result summary
- limit recovery summary
- final auth disposition summary

**Step 3: Run focused tests**

Run:

```bash
go test ./sdk/cliproxy ./sdk/cliproxy/auth -run 'Pool|Disposition' -count=1
```

Expected: PASS

**Step 4: Commit**

```bash
git add sdk/cliproxy/pool_manager.go sdk/cliproxy/service.go sdk/cliproxy/auth/conductor.go
git commit -m "chore(pool): add structured pool logs"
```

## Task 15: Expose Pool Metrics Through Existing Monitoring

**Files:**
- Modify: `internal/api/handlers/management/usage.go`
- Modify: pool-related runtime files as needed
- Test: add focused tests near existing usage/management handlers

**Step 1: Write the failing test**

Add a test proving management usage output includes pool metrics when pool mode is enabled.

Suggested fields:

- `pool_active_size`
- `pool_reserve_count`
- `pool_limit_count`
- `pool_promotions`
- `pool_active_removals`
- `pool_refresh_recoveries`

**Step 2: Run the test**

Run:

```bash
go test ./internal/api/handlers/management -run Pool -count=1
```

Expected: FAIL because no pool metrics are exposed yet.

**Step 3: Implement the metric export**

Requirements:

- do not replace current usage statistics
- append pool metrics to existing usage/health responses
- keep behavior backward-compatible when pool mode is disabled
- current repository TUI does not need to render `pool`

**Step 4: Re-run the test**

Run:

```bash
go test ./internal/api/handlers/management -run Pool -count=1
```

Expected: PASS

**Step 5: Commit**

```bash
git add internal/api/handlers/management/usage.go
git commit -m "feat(pool): expose pool metrics in monitoring"
```

## Task 16: Final Regression Pass

**Files:**
- No code changes expected unless regressions appear

**Step 1: Run the targeted pool/auth tests**

Run:

```bash
go test ./sdk/cliproxy ./sdk/cliproxy/auth ./internal/watcher -count=1
```

Expected: PASS

**Step 2: Run config tests**

Run:

```bash
go test ./internal/config -count=1
```

Expected: PASS

**Step 3: Review design doc alignment**

Re-read:

- `docs/plans/2026-03-09-pool-manager-design.md`

Verify implementation still matches:

- no directory structure change
- selector preserved
- pool mode optional
- file operations owned by existing auth lifecycle

**Step 4: Commit any final fixes**

```bash
git add .
git commit -m "test: finalize pool manager rollout"
```

## Task 17: Add In-Flight Protection

**Status:** Implemented

Summary:

- selected auth callback is forwarded from request execution into `PoolManager`
- selected auth receives an `in-flight` lease
- active/reserve/limit probe scheduling skips in-flight auths

## Task 18: Decouple Rebalance And File Archive

**Status:** Implemented

Summary:

- `active` removal now publishes immediately
- replacement runs in async rebalance worker
- invalid delete / limit archive file operations run asynchronously after disposition emission

## Task 19: Add Low-Quota Isolation

**Status:** Implemented

Summary:

- `PoolStateLowQuota` added
- `pool-manager.low-quota-threshold-percent` added to config
- `Codex` probe parses weekly used/remaining quota and reset time
- startup fill, reserve probe, and active idle probe isolate low-quota auths
- `low_quota` is not background-probed

## Known Follow-ups

- if needed later, define a manual or explicit refresh path for `low_quota`
- if needed later, add provider-specific numeric quota support beyond `codex`

## Suggested Execution Order

Recommended batch order:

1. Tasks 1-4
2. Tasks 5-8
3. Tasks 9-13
4. Tasks 14-16

This keeps risk low and makes rollback simple.

## Rollback Boundaries

- After Task 4: startup-only pool bootstrap exists, but no background maintenance yet
- After Task 8: active pool and promotion path exist
- After Task 13: full health maintenance path exists
- After Task 16: rollout-ready

## Notes For Implementation

- Keep v1 codex-only.
- Do not generalize to all providers yet.
- Do not add durable state storage in v1.
- Reuse existing auth result handling semantics wherever possible.
- Prefer adding adapters around current behavior rather than replacing core request logic.

Plan complete and saved to `docs/plans/2026-03-09-pool-manager-implementation.md`. Two execution options:

1. Subagent-Driven (this session) - I dispatch fresh subagent per task, review between tasks, fast iteration

2. Parallel Session (separate) - Open new session with executing-plans, batch execution with checkpoints

Which approach?
