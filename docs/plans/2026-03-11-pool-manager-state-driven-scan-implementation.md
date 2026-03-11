# Pool Manager State-Driven Scan Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Preserve the current active/reserve pool behavior while reducing steady-state CPU and memory costs by replacing full cold-pool residency with state-driven loading and scanning.

**Architecture:** Keep `active` and `reserve` as the only high-frequency monitored pools. Replace the current "all root auths live in `poolCandidates`" model with a lightweight auth index plus bounded hot candidate cache. Only materialize cold auth objects when reserve falls below a ratio-based watermark or active quality degrades. Move `low_quota` and `limit` into very low-frequency maintenance loops.

**Tech Stack:** Go, fsnotify watcher, in-memory pool manager, YAML config, Go tests with `go test` and `-race`

---

### Task 1: Add Ratio-Based Pool Manager Controls

**Files:**
- Modify: `internal/config/config.go`
- Modify: `sdk/config/config.go`
- Modify: `sdk/cliproxy/service.go`
- Test: `sdk/cliproxy/service_pool_quality_test.go`

**Step 1: Write the failing test**

Add tests covering ratio-to-threshold conversion for:
- reserve refill low watermark
- reserve refill high watermark
- cold batch load size
- low-quota and limit recheck sample sizes

**Step 2: Run test to verify it fails**

Run: `go test ./sdk/cliproxy -run 'TestPoolManager.*Ratio|TestReserve.*Watermark' -count=1`
Expected: FAIL because helper functions and config fields do not exist yet.

**Step 3: Write minimal implementation**

- Add ratio-based config fields under `pool-manager`.
- Add helper methods on `Service` or pool helpers to compute integer thresholds from ratios with `ceil` and minimum guards.
- Keep existing absolute behavior as fallback defaults if the new ratios are unset.

**Step 4: Run test to verify it passes**

Run: `go test ./sdk/cliproxy -run 'TestPoolManager.*Ratio|TestReserve.*Watermark' -count=1`
Expected: PASS

**Step 5: Commit**

```bash
git add internal/config/config.go sdk/config/config.go sdk/cliproxy/service.go sdk/cliproxy/service_pool_quality_test.go
git commit -m "feat: add ratio-based pool scan controls"
```

### Task 2: Introduce Lightweight Auth Index

**Files:**
- Modify: `sdk/cliproxy/types.go`
- Modify: `sdk/cliproxy/watcher.go`
- Modify: `internal/watcher/file_snapshot.go`
- Modify: `sdk/cliproxy/service.go`
- Test: `sdk/cliproxy/service_startup_auth_bootstrap_test.go`

**Step 1: Write the failing test**

Add tests showing startup can build a cold-auth index from watcher snapshots without retaining full auth objects for the entire root auth set.

**Step 2: Run test to verify it fails**

Run: `go test ./sdk/cliproxy -run 'TestServiceRunBuildsColdAuthIndex|TestBootstrapPoolSnapshotDoesNotRetainAllRootAuthObjects' -count=1`
Expected: FAIL because there is no index abstraction yet.

**Step 3: Write minimal implementation**

- Add a lightweight index type, for example `poolAuthIndex`, storing:
  - auth ID
  - file path
  - source bucket (`root`, `limit`, `low_quota`)
  - cursor ordering metadata
- Extend watcher snapshots or file snapshot helpers so service startup can build the index from file paths without forcing long-lived `*Auth` residency.
- Keep `poolCandidates` only for hot objects currently used by active/reserve/near-term cold work.

**Step 4: Run test to verify it passes**

Run: `go test ./sdk/cliproxy -run 'TestServiceRunBuildsColdAuthIndex|TestBootstrapPoolSnapshotDoesNotRetainAllRootAuthObjects' -count=1`
Expected: PASS

**Step 5: Commit**

```bash
git add sdk/cliproxy/types.go sdk/cliproxy/watcher.go internal/watcher/file_snapshot.go sdk/cliproxy/service.go sdk/cliproxy/service_startup_auth_bootstrap_test.go
git commit -m "feat: add lightweight pool auth index"
```

### Task 3: Load Cold Candidates On Demand

**Files:**
- Modify: `sdk/cliproxy/service.go`
- Modify: `sdk/cliproxy/watcher.go`
- Test: `sdk/cliproxy/service_pool_quality_test.go`
- Test: `sdk/cliproxy/service_startup_auth_bootstrap_test.go`

**Step 1: Write the failing test**

Add tests verifying:
- when reserve is healthy, cold candidates are not materialized
- when reserve falls below the low watermark, the next cold batch is loaded from the index
- startup chooses a randomized cold cursor offset instead of always starting from the first file

**Step 2: Run test to verify it fails**

Run: `go test ./sdk/cliproxy -run 'TestFillWarmReserveLoadsColdBatchOnDemand|TestColdScanSkipsWhenReserveHealthy|TestColdCursorStartsFromRandomOffset' -count=1`
Expected: FAIL because cold loading still depends on full `poolCandidates`.

**Step 3: Write minimal implementation**

- Replace direct cold iteration over the full candidate map with:
  - index cursor
  - bounded batch selection
  - lazy auth load by path
- Materialize only the selected batch into hot candidate memory.
- Remove hot candidate entries after failed/low-value outcomes unless they are promoted into active or reserve.

**Step 4: Run test to verify it passes**

Run: `go test ./sdk/cliproxy -run 'TestFillWarmReserveLoadsColdBatchOnDemand|TestColdScanSkipsWhenReserveHealthy|TestColdCursorStartsFromRandomOffset' -count=1`
Expected: PASS

**Step 5: Commit**

```bash
git add sdk/cliproxy/service.go sdk/cliproxy/watcher.go sdk/cliproxy/service_pool_quality_test.go sdk/cliproxy/service_startup_auth_bootstrap_test.go
git commit -m "feat: load cold pool candidates on demand"
```

### Task 4: Downgrade Low-Quota and Limit Maintenance

**Files:**
- Modify: `sdk/cliproxy/service.go`
- Modify: `sdk/config/config.go`
- Test: `sdk/cliproxy/service_pool_quality_test.go`

**Step 1: Write the failing test**

Add tests verifying:
- low-quota rechecks do not participate in the main refill loop
- limit rechecks are low-frequency and bounded by ratio
- active and reserve maintenance continue unchanged when low-quota and limit pools are large

**Step 2: Run test to verify it fails**

Run: `go test ./sdk/cliproxy -run 'TestLowQuotaPoolUsesLowFrequencyMaintenance|TestLimitPoolUsesLowFrequencyMaintenance' -count=1`
Expected: FAIL because low-quota and limit scanning policies are not separated enough yet.

**Step 3: Write minimal implementation**

- Add dedicated low-frequency scheduling or sampling logic for `low_quota` and `limit`.
- Ensure neither bucket can drive high-frequency cold scanning unless active/reserve health explicitly demands it.

**Step 4: Run test to verify it passes**

Run: `go test ./sdk/cliproxy -run 'TestLowQuotaPoolUsesLowFrequencyMaintenance|TestLimitPoolUsesLowFrequencyMaintenance' -count=1`
Expected: PASS

**Step 5: Commit**

```bash
git add sdk/cliproxy/service.go sdk/config/config.go sdk/cliproxy/service_pool_quality_test.go
git commit -m "feat: reduce low quota and limit scan pressure"
```

### Task 5: Add End-to-End Regression Coverage for Pool Stability

**Files:**
- Modify: `sdk/cliproxy/service_pool_candidates_test.go`
- Create: `sdk/cliproxy/service_pool_state_driven_scan_test.go`

**Step 1: Write the failing test**

Add regression coverage for:
- bounded hot candidate residency under large cold index
- no concurrent map issues under mixed active/reserve/cold operations
- reserve refill behavior remains intact under reduced scan pressure

**Step 2: Run test to verify it fails**

Run: `go test -race ./sdk/cliproxy -run 'TestServicePool.*StateDriven|TestServicePoolCandidatesConcurrentAccess' -count=1`
Expected: FAIL until the new state-driven path is fully wired.

**Step 3: Write minimal implementation**

- Adjust hot candidate lifecycle cleanup.
- Add any missing synchronization or cache eviction paths required by the new index model.

**Step 4: Run test to verify it passes**

Run: `go test -race ./sdk/cliproxy -run 'TestServicePool.*StateDriven|TestServicePoolCandidatesConcurrentAccess' -count=1`
Expected: PASS

**Step 5: Commit**

```bash
git add sdk/cliproxy/service_pool_candidates_test.go sdk/cliproxy/service_pool_state_driven_scan_test.go sdk/cliproxy/service.go
git commit -m "test: cover state driven pool scanning"
```

### Task 6: Verify Memory and CPU Improvements

**Files:**
- Modify: `docs/plans/2026-03-11-pool-manager-state-driven-scan-implementation.md`

**Step 1: Capture before/after commands**

Run:

```bash
go test ./sdk/cliproxy -count=1
go test -race ./sdk/cliproxy -run 'TestServicePool.*|TestServicePoolCandidatesConcurrentAccess' -count=1
```

On VPS, capture:

```bash
ps -o pid,%cpu,%mem,rss,vsz,etime,stat -p <pid>
free -m
grep -E "pool-eval:|cold scan sampled" /tmp/cli-proxy-api-plus.startup.log | tail -n 40
```

**Step 2: Record expected outcomes**

- `active` remains at target size
- `reserve` refills to ratio-based high watermark when underfilled
- `cold` scans pause or drop to low duty cycle when reserve is healthy
- RSS and steady CPU both decrease materially from the current baseline

**Step 3: Commit final docs update**

```bash
git add docs/plans/2026-03-11-pool-manager-state-driven-scan-implementation.md
git commit -m "docs: record state driven pool scan verification"
```

---

## Implemented Status

The following parts of this plan have been implemented and shipped:

- Ratio-based pool controls were added in `internal/config/config.go` and wired into runtime sizing helpers in `sdk/cliproxy/pool_manager_tuning.go`.
- Reserve refill now uses ratio-based low/high watermarks in `sdk/cliproxy/service.go`.
- Active quota refresh can use ratio-derived sample sizes while preserving existing absolute sample-size behavior when ratios are unset.
- Pool startup now builds a lightweight cold candidate index and supports lazy auth materialization by path via `sdk/cliproxy/pool_candidate_index.go`.
- Indexed cold candidates that fall into `low_quota` or `limit` are evicted from the hot candidate map while remaining addressable through the index.
- Pool-mode runtime auth updates and file-backed watcher updates are now handled with separate semantics to avoid deleting pool state on runtime unpublish.
- Pool probes no longer archive or delete auth files.
- Generic `401 unauthorized` failures no longer immediately delete auth files; only explicit unrecoverable signals such as `refresh_token_reused` still trigger deletion.

## VPS Configuration Applied

The following production tuning was applied on the VPS:

```yaml
pool-manager:
  reserve-refill-low-ratio: 0.25
  reserve-refill-high-ratio: 0.50
  cold-batch-load-ratio: 0.05
```

This keeps `active=100`, allows `reserve` to refill up to `50`, and constrains cold-batch loading to a smaller fraction of pool size.

## Observed Results

Observed on the VPS after deployment:

- Service remained healthy with repeated `GET / -> 200`.
- `active_size=100` and `reserve_size=50` were reached and maintained.
- Steady-state RSS dropped from the original pre-change range of roughly `3.7-4.0 GB` to approximately `1.8-2.0 GB` after the full set of pool changes and ratio tuning.
- CPU improved from near full-core saturation in the earlier versions to materially lower levels after removing archive/delete churn from `pool_probe`.
- Log churn analysis showed the original hot path was not just cold scanning; a major share came from `pool_probe -> MarkResult -> archive/delete -> watcher REMOVE`.
- After disabling archive/delete on `pool_probe`, the remaining churn was dominated by `source=refresh`.
- After tightening invalid-auth deletion semantics so generic `401` is not treated as a dead auth, `source=refresh` deletion churn also dropped significantly.

## Remaining Work

The following parts of the original design are still incomplete or intentionally deferred:

- `low_quota` and `limit` maintenance are reduced indirectly through hot-map eviction, but there is not yet a dedicated separate low-frequency scheduler for these buckets.
- The full candidate universe is now indexed, but the watcher path still snapshots full auth objects at startup before the service distills them into lightweight refs.
- Background `refresh` still generates some auth-file churn and may merit its own strike-based or delayed-delete policy if further CPU reduction is needed.
- Longer-duration production observation under real traffic is still recommended before making larger architectural changes.
