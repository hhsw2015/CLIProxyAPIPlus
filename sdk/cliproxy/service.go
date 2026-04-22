// Package cliproxy provides the core service implementation for the CLI Proxy API.
// It includes service lifecycle management, authentication handling, file watching,
// and integration with various AI service providers through a unified interface.
package cliproxy

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/api"
	kiroauth "github.com/router-for-me/CLIProxyAPI/v6/internal/auth/kiro"
	internalconfig "github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/runtime/executor"
	internalusage "github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/watcher"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/wsrelay"
	sdkaccess "github.com/router-for-me/CLIProxyAPI/v6/sdk/access"
	sdkAuth "github.com/router-for-me/CLIProxyAPI/v6/sdk/auth"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	coreusage "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/usage"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/config"
	log "github.com/sirupsen/logrus"
)

// Service wraps the proxy server lifecycle so external programs can embed the CLI proxy.
// It manages the complete lifecycle including authentication, file watching, HTTP server,
// and integration with various AI service providers.
type Service struct {
	// cfg holds the current application configuration.
	cfg *config.Config

	// cfgMu protects concurrent access to the configuration.
	cfgMu sync.RWMutex

	// configPath is the path to the configuration file.
	configPath string

	// tokenProvider handles loading token-based clients.
	tokenProvider TokenClientProvider

	// apiKeyProvider handles loading API key-based clients.
	apiKeyProvider APIKeyClientProvider

	// watcherFactory creates file watcher instances.
	watcherFactory WatcherFactory

	// hooks provides lifecycle callbacks.
	hooks Hooks

	// serverOptions contains additional server configuration options.
	serverOptions []api.ServerOption

	// server is the HTTP API server instance.
	server *api.Server

	// pprofServer manages the optional pprof HTTP debug server.
	pprofServer *pprofServer

	// serverErr channel for server startup/shutdown errors.
	serverErr chan error

	// watcher handles file system monitoring.
	watcher *WatcherWrapper

	// watcherCancel cancels the watcher context.
	watcherCancel context.CancelFunc

	// authUpdates channel for authentication updates.
	authUpdates chan watcher.AuthUpdate

	// authQueueStop cancels the auth update queue processing.
	authQueueStop context.CancelFunc

	// poolProbeStop cancels background pool probe loops.
	poolProbeStop context.CancelFunc

	// poolRebalanceStop cancels the async rebalance worker.
	poolRebalanceStop context.CancelFunc

	// authManager handles legacy authentication operations.
	authManager *sdkAuth.Manager

	// accessManager handles request authentication providers.
	accessManager *sdkaccess.Manager

	// usageStats allows tests and future integrations to provide a dedicated usage statistics source.
	usageStats *internalusage.RequestStatistics

	// coreManager handles core authentication and execution.
	coreManager *coreauth.Manager

	// shutdownOnce ensures shutdown is called only once.
	shutdownOnce sync.Once

	// wsGateway manages websocket Gemini providers.
	wsGateway *wsrelay.Manager

	// poolManager maintains the active/reserve/limit auth pool when enabled.
	poolManager *PoolManager

	// publishedActive tracks auth IDs currently exposed to runtime routing in pool mode.
	publishedActive map[string]time.Time

	// poolCandidates stores hot auth snapshots needed immediately by active/reserve workflows.
	poolCandidates map[string]*coreauth.Auth
	// poolCandidateIndex stores lightweight metadata for the full known candidate universe.
	poolCandidateIndex map[string]*poolCandidateRef

	// poolCandidateOrder stores the cold-candidate iteration order for background scanning.
	poolCandidateOrder []string

	// poolCandidateCursor tracks the next cold candidate scan position.
	poolCandidateCursor int

	poolCandidateMu sync.RWMutex

	// poolMetrics stores pool observability counters.
	poolMetrics *PoolMetrics

	poolRebalanceMu      sync.Mutex
	poolRebalanceQueue   chan struct{}
	poolEvalMu           sync.Mutex
	poolEvalLast         *poolEvalWindowSnapshot
	poolUnderfilledMu    sync.Mutex
	poolUnderfilledSince time.Time
	poolLowSuccessMu     sync.Mutex
	poolLowSuccessStreak int
	poolLowSuccessWarned bool
	probeBudgetMu        sync.Mutex
	probeBudgetStart     time.Time
	probeBudgetUsed      int
}

type poolEvalWindowSnapshot struct {
	At                 time.Time
	TotalRequests      int64
	SuccessCount       int64
	FailureCount       int64
	PromotionsTotal    int64
	ActiveRemovedTotal int64
	RefreshedTotal     int64
	MovedToLimitTotal  int64
	DeletedTotal       int64
	RestoredTotal      int64
}

const poolEvalLogInterval = 5 * time.Minute
const poolEvalLowSuccessThreshold = 80.0
const poolEvalLowSuccessConsecutiveWindows = 3
const poolSelectedAuthLeaseDuration = 30 * time.Second
const poolUnknownRemainingPercent = 101
const poolStateCold = "cold"
const poolStateDeleted = "deleted"

// RegisterUsagePlugin registers a usage plugin on the global usage manager.
// This allows external code to monitor API usage and token consumption.
//
// Parameters:
//   - plugin: The usage plugin to register
func (s *Service) RegisterUsagePlugin(plugin coreusage.Plugin) {
	coreusage.RegisterPlugin(plugin)
}

func (s *Service) consumeBackgroundProbeBudget(now time.Time) bool {
	if s == nil || s.cfg == nil {
		return true
	}
	windowSeconds := s.cfg.PoolManager.BackgroundProbeBudgetWindowSeconds
	maxBudget := s.cfg.PoolManager.BackgroundProbeBudgetMax
	if windowSeconds <= 0 || maxBudget <= 0 {
		return true
	}
	if now.IsZero() {
		now = time.Now()
	}
	window := time.Duration(windowSeconds) * time.Second

	s.probeBudgetMu.Lock()
	defer s.probeBudgetMu.Unlock()
	if s.probeBudgetStart.IsZero() || now.Sub(s.probeBudgetStart) >= window {
		s.probeBudgetStart = now
		s.probeBudgetUsed = 0
	}
	if s.probeBudgetUsed >= maxBudget {
		return false
	}
	s.probeBudgetUsed++
	return true
}

// GetWatcher returns the underlying WatcherWrapper instance.
// This allows external components (e.g., RefreshManager) to interact with the watcher.
// Returns nil if the service or watcher is not initialized.
func (s *Service) GetWatcher() *WatcherWrapper {
	if s == nil {
		return nil
	}
	return s.watcher
}

// newDefaultAuthManager creates a default authentication manager with all supported providers.
func newDefaultAuthManager() *sdkAuth.Manager {
	return sdkAuth.NewManager(
		sdkAuth.GetTokenStore(),
		sdkAuth.NewGeminiAuthenticator(),
		sdkAuth.NewCodexAuthenticator(),
		sdkAuth.NewClaudeAuthenticator(),
		sdkAuth.NewGitLabAuthenticator(),
	)
}

type serviceAuthHook struct {
	coreauth.NoopHook
	service *Service
}

func (h *serviceAuthHook) OnAuthDisposition(ctx context.Context, disposition AuthDisposition) {
	if h == nil || h.service == nil {
		return
	}
	h.service.handleAuthDisposition(ctx, disposition)
}

func (h *serviceAuthHook) OnResult(ctx context.Context, result coreauth.Result) {
	if h == nil || h.service == nil {
		return
	}
	h.service.handlePoolResult(ctx, result)
}

func (s *Service) ensureAuthUpdateQueue(ctx context.Context) {
	if s == nil {
		return
	}
	if s.authUpdates == nil {
		s.authUpdates = make(chan watcher.AuthUpdate, 256)
	}
	if s.authQueueStop != nil {
		return
	}
	queueCtx, cancel := context.WithCancel(ctx)
	s.authQueueStop = cancel
	go s.consumeAuthUpdates(queueCtx)
}

func (s *Service) consumeAuthUpdates(ctx context.Context) {
	ctx = coreauth.WithSkipPersist(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case update, ok := <-s.authUpdates:
			if !ok {
				return
			}
			s.handleAuthUpdate(ctx, update)
		labelDrain:
			for {
				select {
				case nextUpdate := <-s.authUpdates:
					s.handleAuthUpdate(ctx, nextUpdate)
				default:
					break labelDrain
				}
			}
		}
	}
}

func (s *Service) emitAuthUpdate(ctx context.Context, update watcher.AuthUpdate) {
	if s == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	update.Runtime = true
	if s.watcher != nil && s.watcher.DispatchRuntimeAuthUpdate(update) {
		return
	}
	if s.authUpdates != nil {
		select {
		case s.authUpdates <- update:
			return
		default:
			log.Debugf("auth update queue saturated, applying inline action=%v id=%s", update.Action, update.ID)
		}
	}
	if s.poolManager != nil {
		switch update.Action {
		case watcher.AuthUpdateActionAdd, watcher.AuthUpdateActionModify:
			if update.Auth != nil {
				s.applyCoreAuthAddOrUpdate(ctx, update.Auth)
			}
		case watcher.AuthUpdateActionDelete:
			id := update.ID
			if id == "" && update.Auth != nil {
				id = update.Auth.ID
			}
			if id != "" {
				s.applyCoreAuthRemoval(ctx, id)
			}
		}
		return
	}
	s.handleAuthUpdate(ctx, update)
}

func (s *Service) bootstrapAuthSnapshot(ctx context.Context, watcherWrapper *WatcherWrapper) {
	if s == nil || watcherWrapper == nil {
		return
	}
	if s.cfg != nil && s.cfg.PoolManager.Size > 0 {
		s.bootstrapNonPoolSnapshotAuths(ctx, watcherWrapper)
		s.bootstrapPoolSnapshot(ctx, watcherWrapper)
		return
	}
	auths := watcherWrapper.SnapshotAuths()
	if len(auths) == 0 {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ctx = coreauth.WithSkipPersist(ctx)
	for _, auth := range auths {
		if auth == nil || auth.ID == "" {
			continue
		}
		s.applyCoreAuthAddOrUpdate(ctx, auth)
	}
}

func (s *Service) bootstrapNonPoolSnapshotAuths(ctx context.Context, watcherWrapper *WatcherWrapper) {
	if s == nil || watcherWrapper == nil {
		return
	}
	auths := watcherWrapper.SnapshotAuths()
	if len(auths) == 0 {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ctx = coreauth.WithSkipPersist(ctx)
	for _, auth := range auths {
		if auth == nil || strings.TrimSpace(auth.ID) == "" {
			continue
		}
		if s.poolManager != nil && s.poolManager.HasTrackedState(auth.ID) {
			continue
		}
		if s.poolCandidateRef(auth.ID) != nil {
			continue
		}
		s.applyCoreAuthAddOrUpdate(ctx, auth)
	}
}

func (s *Service) seedLoadedCoreAuthModels() {
	if s == nil || s.coreManager == nil {
		return
	}
	if s.cfg != nil && s.cfg.PoolManager.Size > 0 {
		return
	}

	loaded := s.coreManager.List()
	if len(loaded) == 0 {
		return
	}
	for _, auth := range loaded {
		if auth == nil || strings.TrimSpace(auth.ID) == "" {
			continue
		}
		s.registerModelsForAuth(auth)
	}
	// s.coreManager.RebuildScheduler()
}

func (s *Service) bootstrapPoolSnapshot(ctx context.Context, watcherWrapper *WatcherWrapper) {
	if s == nil || watcherWrapper == nil || s.cfg == nil {
		return
	}
	pm := NewPoolManager(s.cfg.PoolManager)
	if !pm.Enabled() {
		return
	}
	s.poolManager = pm
	s.poolMetrics = NewPoolMetrics(s.cfg.PoolManager)
	log.Infof("pool-manager: enabled size=%d provider=%s", pm.TargetSize(), pm.Provider())

	rootAuths := watcherWrapper.SnapshotRootFileAuths()
	limitAuths := watcherWrapper.SnapshotLimitAuths()
	if len(rootAuths) == 0 && len(limitAuths) == 0 {
		return
	}
	log.Infof("pool-manager: startup discovered root=%d limit=%d", len(rootAuths), len(limitAuths))

	if ctx == nil {
		ctx = context.Background()
	}
	ctx = coreauth.WithSkipPersist(ctx)
	s.poolCandidates = make(map[string]*coreauth.Auth, pm.TargetSize()+pm.ReserveTargetSize())
	s.poolCandidateIndex = make(map[string]*poolCandidateRef, len(rootAuths)+len(limitAuths))
	for _, auth := range rootAuths {
		if auth == nil || strings.TrimSpace(auth.ID) == "" {
			continue
		}
		if poolCandidatePath(auth) == "" {
			s.storePoolCandidate(auth)
		} else {
			s.indexPoolCandidate(auth)
		}
	}
	for _, auth := range limitAuths {
		if auth == nil || strings.TrimSpace(auth.ID) == "" {
			continue
		}
		if poolCandidatePath(auth) == "" {
			s.storePoolCandidate(auth)
		} else {
			s.indexPoolCandidate(auth)
		}
	}
	rootAuths = sortAuthCandidatesByRemaining(rootAuths)
	s.resetPoolCandidateOrder(rootAuths)

	for _, auth := range rootAuths {
		if auth == nil || strings.TrimSpace(auth.ID) == "" {
			continue
		}
		if pm.Snapshot().ActiveCount >= pm.TargetSize() {
			break
		}
		beforeProbe := auth.Clone()
		probedAuth, result := poolProbeAuthFunc(ctx, s.cfg, auth.Clone())
		if probedAuth != nil {
			s.storePoolCandidate(probedAuth)
		}
		s.recordPoolProbe(PoolStateReserve, beforeProbe, probedAuth, result, "")
		if result.Success && probedAuth != nil {
			s.placeProbedAuth(ctx, probedAuth, true)
			continue
		}
		s.handlePoolProbeResult(auth.ID, auth.Provider, result)
	}
	for _, auth := range limitAuths {
		if auth == nil || strings.TrimSpace(auth.ID) == "" {
			continue
		}
		s.setPoolMemberState(PoolMember{AuthID: auth.ID, Provider: auth.Provider}, PoolStateLimit, "startup_limit")
	}
	s.syncPoolActiveToRuntime(ctx)
	candidateCount, coldCandidateCount := s.poolCandidateCounts()
	log.Infof(
		"pool-manager: startup active target=%d selected=%d reserve=%d low_quota=%d limit=%d candidate_size=%d cold_candidate_size=%d",
		pm.TargetSize(),
		pm.Snapshot().ActiveCount,
		pm.Snapshot().ReserveCount,
		pm.Snapshot().LowQuotaCount,
		pm.Snapshot().LimitCount,
		candidateCount,
		coldCandidateCount,
	)
	s.logPoolEvaluation()
}

func (s *Service) resetPoolCandidateOrder(auths []*coreauth.Auth) {
	if s == nil {
		return
	}
	order := make([]string, 0, len(auths))
	seen := make(map[string]struct{}, len(auths))
	for _, auth := range auths {
		if auth == nil {
			continue
		}
		id := strings.TrimSpace(auth.ID)
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		order = append(order, id)
	}
	s.poolCandidateMu.Lock()
	s.poolCandidateOrder = order
	s.poolCandidateCursor = 0
	s.poolCandidateMu.Unlock()
}

func sortAuthCandidatesByRemaining(auths []*coreauth.Auth) []*coreauth.Auth {
	if len(auths) <= 1 {
		return auths
	}
	sorted := append([]*coreauth.Auth(nil), auths...)
	sort.SliceStable(sorted, func(i, j int) bool {
		left := sorted[i]
		right := sorted[j]
		leftRemaining, leftKnown := authWeeklyRemainingPercent(left)
		rightRemaining, rightKnown := authWeeklyRemainingPercent(right)
		if leftKnown != rightKnown {
			return leftKnown
		}
		if leftKnown && rightKnown && leftRemaining != rightRemaining {
			return leftRemaining > rightRemaining
		}
		leftID := ""
		rightID := ""
		if left != nil {
			leftID = strings.TrimSpace(left.ID)
		}
		if right != nil {
			rightID = strings.TrimSpace(right.ID)
		}
		return leftID < rightID
	})
	return sorted
}

func (s *Service) poolCandidate(authID string) *coreauth.Auth {
	if s == nil {
		return nil
	}
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return nil
	}
	s.poolCandidateMu.RLock()
	auth := s.poolCandidates[authID]
	var ref *poolCandidateRef
	if auth == nil && s.poolCandidateIndex != nil {
		if candidateRef := s.poolCandidateIndex[authID]; candidateRef != nil {
			copyRef := *candidateRef
			ref = &copyRef
		}
	}
	s.poolCandidateMu.RUnlock()
	if auth == nil {
		return s.loadPoolCandidateByRef(authID, ref)
	}
	return auth.Clone()
}

func (s *Service) deletePoolCandidate(authID string) {
	if s == nil {
		return
	}
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return
	}
	s.poolCandidateMu.Lock()
	defer s.poolCandidateMu.Unlock()
	delete(s.poolCandidates, authID)
	delete(s.poolCandidateIndex, authID)
}

func (s *Service) storePoolCandidate(auth *coreauth.Auth) {
	if s == nil || auth == nil || strings.TrimSpace(auth.ID) == "" {
		return
	}
	s.poolCandidateMu.Lock()
	defer s.poolCandidateMu.Unlock()
	if s.poolCandidates == nil {
		s.poolCandidates = make(map[string]*coreauth.Auth)
	}
	if s.poolCandidateIndex == nil {
		s.poolCandidateIndex = make(map[string]*poolCandidateRef)
	}
	id := strings.TrimSpace(auth.ID)
	s.poolCandidates[id] = auth.Clone()
	if ref := newPoolCandidateRef(auth); ref != nil {
		s.poolCandidateIndex[id] = ref
	}
	for _, existing := range s.poolCandidateOrder {
		if existing == id {
			return
		}
	}
	s.poolCandidateOrder = append(s.poolCandidateOrder, id)
}

func (s *Service) resortPoolCandidateOrder() {
	if s == nil {
		return
	}
	s.poolCandidateMu.Lock()
	defer s.poolCandidateMu.Unlock()
	if len(s.poolCandidateOrder) <= 1 {
		return
	}
	sort.SliceStable(s.poolCandidateOrder, func(i, j int) bool {
		leftRef := s.poolCandidateIndex[s.poolCandidateOrder[i]]
		rightRef := s.poolCandidateIndex[s.poolCandidateOrder[j]]
		leftRemaining, leftKnown := -1, false
		rightRemaining, rightKnown := -1, false
		if leftRef != nil {
			leftRemaining, leftKnown = leftRef.RemainingPercent, leftRef.RemainingKnown
		}
		if rightRef != nil {
			rightRemaining, rightKnown = rightRef.RemainingPercent, rightRef.RemainingKnown
		}
		if !leftKnown {
			leftRemaining, leftKnown = authWeeklyRemainingPercent(s.poolCandidates[s.poolCandidateOrder[i]])
		}
		if !rightKnown {
			rightRemaining, rightKnown = authWeeklyRemainingPercent(s.poolCandidates[s.poolCandidateOrder[j]])
		}
		if leftKnown != rightKnown {
			return leftKnown
		}
		if leftKnown && rightKnown && leftRemaining != rightRemaining {
			return leftRemaining > rightRemaining
		}
		return s.poolCandidateOrder[i] < s.poolCandidateOrder[j]
	})
}

func (s *Service) poolCandidateCounts() (candidateCount, coldCandidateCount int) {
	if s == nil {
		return 0, 0
	}
	s.poolCandidateMu.RLock()
	defer s.poolCandidateMu.RUnlock()
	seen := make(map[string]struct{}, len(s.poolCandidateIndex)+len(s.poolCandidates))
	for id := range s.poolCandidateIndex {
		seen[id] = struct{}{}
	}
	for id := range s.poolCandidates {
		seen[id] = struct{}{}
	}
	candidateCount = len(seen)
	if candidateCount == 0 || s.poolManager == nil {
		return candidateCount, candidateCount
	}
	for id := range seen {
		if !s.poolManager.HasTrackedState(id) {
			coldCandidateCount++
		}
	}
	return candidateCount, coldCandidateCount
}

func (s *Service) poolMetricsSnapshotCurrent() PoolMetricsSnapshot {
	if s == nil || s.poolManager == nil {
		return PoolMetricsSnapshot{}
	}
	snapshot := s.poolMetrics.Snapshot(s.poolManager.Snapshot(), len(s.publishedActive))
	snapshot.CandidateCount, snapshot.ColdCandidateCount = s.poolCandidateCounts()
	return snapshot
}

func (s *Service) poolObservedState(authID string) string {
	if s == nil {
		return ""
	}
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return ""
	}
	if s.poolManager != nil && s.poolManager.HasTrackedState(authID) {
		if member, ok := s.poolManager.LastSeenMember(authID); ok {
			if state := strings.TrimSpace(string(member.PoolState)); state != "" {
				return state
			}
		}
	}
	s.poolCandidateMu.RLock()
	defer s.poolCandidateMu.RUnlock()
	if _, ok := s.poolCandidateIndex[authID]; ok {
		return poolStateCold
	}
	if _, ok := s.poolCandidates[authID]; ok {
		return poolStateCold
	}
	return ""
}

func (s *Service) logPoolTransition(authID, provider, fromState, toState, reason string, remainingPercent int) {
	if s == nil {
		return
	}
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return
	}
	fromState = strings.TrimSpace(fromState)
	toState = strings.TrimSpace(toState)
	if fromState == toState && fromState != "" {
		return
	}
	provider = strings.ToLower(strings.TrimSpace(provider))
	reason = strings.TrimSpace(reason)
	if remainingPercent >= 0 && remainingPercent <= 100 {
		log.Infof("pool-transition: auth=%s provider=%s from=%s to=%s reason=%s remaining_percent=%d", authID, provider, fromState, toState, reason, remainingPercent)
		return
	}
	log.Infof("pool-transition: auth=%s provider=%s from=%s to=%s reason=%s", authID, provider, fromState, toState, reason)
}

func (s *Service) setPoolMemberState(member PoolMember, state PoolState, reason string) {
	if s == nil || s.poolManager == nil || strings.TrimSpace(member.AuthID) == "" {
		return
	}
	fromState := s.poolObservedState(member.AuthID)
	switch state {
	case PoolStateLimit:
		s.poolManager.SetLimit(member)
	case PoolStateLowQuota:
		s.poolManager.SetLowQuota(member)
	case PoolStateReserve:
		s.poolManager.SetReserve(member)
	default:
		s.poolManager.SetActive(member)
	}
	s.logPoolTransition(member.AuthID, member.Provider, fromState, string(state), reason, member.RemainingPercent)
	if state == PoolStateLimit || state == PoolStateLowQuota {
		s.evictPoolCandidateIfIndexed(member.AuthID)
	}
}

func (s *Service) removePoolMember(authID, provider, reason string) {
	if s == nil || s.poolManager == nil {
		return
	}
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return
	}
	fromState := s.poolObservedState(authID)
	s.poolManager.Remove(authID)
	s.logPoolTransition(authID, provider, fromState, poolStateDeleted, reason, -1)
	s.evictPoolCandidateIfIndexed(authID)
}

func (s *Service) keepPoolCandidateHot(authID string) bool {
	if s == nil || s.poolManager == nil {
		return false
	}
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return false
	}
	member, ok := s.poolManager.LastSeenMember(authID)
	if !ok {
		return false
	}
	return member.PoolState == PoolStateActive || member.PoolState == PoolStateReserve
}

func (s *Service) trimActiveOverflow() int {
	if s == nil || s.poolManager == nil {
		return 0
	}
	snapshot := s.poolManager.Snapshot()
	overflow := snapshot.ActiveCount - snapshot.TargetSize
	if overflow <= 0 {
		return 0
	}

	now := time.Now()
	threshold := s.poolManager.LowQuotaThresholdPercent()
	type candidate struct {
		member     PoolMember
		comparable int
	}

	candidates := make([]candidate, 0, snapshot.ActiveCount)
	for _, authID := range snapshot.ActiveIDs {
		member, ok := s.poolManager.LastSeenMember(authID)
		if !ok {
			continue
		}
		if member.InFlightCount > 0 {
			continue
		}
		if !member.ProtectedUntil.IsZero() && member.ProtectedUntil.After(now) {
			continue
		}
		comparable := member.RemainingPercent
		if comparable < 0 || comparable > 100 {
			comparable = -1
		}
		candidates = append(candidates, candidate{member: member, comparable: comparable})
	}

	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].comparable == candidates[j].comparable {
			return candidates[i].member.AuthID < candidates[j].member.AuthID
		}
		return candidates[i].comparable < candidates[j].comparable
	})

	trimmed := 0
	for _, item := range candidates {
		if trimmed >= overflow {
			break
		}
		demoteState := PoolStateReserve
		if item.comparable >= 0 && item.comparable <= threshold {
			demoteState = PoolStateLowQuota
		}
		s.setPoolMemberState(item.member, demoteState, "trim_active_overflow")
		trimmed++
	}

	if trimmed > 0 {
		log.Infof("pool-manager: trimmed active overflow trimmed=%d target=%d active_after=%d", trimmed, snapshot.TargetSize, s.poolManager.Snapshot().ActiveCount)
	}
	return trimmed
}

func (s *Service) poolMemberForAuth(auth *coreauth.Auth, state PoolState) PoolMember {
	member := PoolMember{}
	if auth != nil {
		member.AuthID = strings.TrimSpace(auth.ID)
		member.Provider = strings.ToLower(strings.TrimSpace(auth.Provider))
		if remaining, ok := authWeeklyRemainingPercent(auth); ok {
			member.RemainingPercent = remaining
		} else {
			member.RemainingPercent = poolUnknownRemainingPercent
		}
	}
	member.PoolState = state
	if s == nil || s.poolManager == nil || member.AuthID == "" {
		return member
	}
	if existing, ok := s.poolManager.LastSeenMember(member.AuthID); ok {
		existing.Provider = member.Provider
		existing.PoolState = state
		existing.RemainingPercent = member.RemainingPercent
		return existing
	}
	return member
}

func (s *Service) poolMemberForFreshProbe(auth *coreauth.Auth, state PoolState, now time.Time) PoolMember {
	member := s.poolMemberForAuth(auth, state)
	if now.IsZero() {
		now = time.Now()
	}
	member.LastProbeAt = now
	member.LastSuccessAt = now
	member.LastProbeReason = ""
	var cfg *config.Config
	if s != nil {
		cfg = s.cfg
	}
	switch state {
	case PoolStateActive:
		if cfg != nil {
			if interval := time.Duration(cfg.PoolManager.ActiveIdleScanIntervalSeconds) * time.Second; interval > 0 {
				member.NextProbeAt = now.Add(interval)
			}
			if interval := time.Duration(cfg.PoolManager.ActiveQuotaRefreshIntervalSeconds) * time.Second; interval > 0 {
				member.LastQuotaProbeAt = now
				member.NextQuotaProbeAt = now.Add(interval)
				member.LastQuotaProbeReason = ""
			}
		}
	case PoolStateReserve:
		if cfg != nil {
			if interval := time.Duration(cfg.PoolManager.ReserveScanIntervalSeconds) * time.Second; interval > 0 {
				member.NextProbeAt = now.Add(interval)
			}
		}
	}
	return member
}

func (s *Service) placeProbedAuth(ctx context.Context, auth *coreauth.Auth, allowFallback bool) bool {
	if s == nil || s.poolManager == nil || auth == nil || strings.TrimSpace(auth.ID) == "" {
		return false
	}
	now := time.Now()
	member := s.poolMemberForFreshProbe(auth, PoolStateReserve, now)
	lowQuota := authIsLowQuota(auth, s.poolManager.LowQuotaThresholdPercent())
	snapshot := s.poolManager.Snapshot()

	if lowQuota {
		if allowFallback && snapshot.ActiveCount < snapshot.TargetSize {
			member = s.poolMemberForFreshProbe(auth, PoolStateActive, now)
			s.setPoolMemberState(member, PoolStateActive, "fallback_active")
			s.applyCoreAuthAddOrUpdate(ctx, auth)
			log.Infof("pool-manager: fallback active auth=%s remaining_percent=%d threshold=%d", auth.ID, mustWeeklyRemainingPercent(auth), s.poolManager.LowQuotaThresholdPercent())
			return true
		}
		member.PoolState = PoolStateLowQuota
		s.setPoolMemberState(member, PoolStateLowQuota, "low_quota")
		log.Infof("pool-manager: low quota auth=%s remaining_percent=%d threshold=%d", auth.ID, mustWeeklyRemainingPercent(auth), s.poolManager.LowQuotaThresholdPercent())
		return false
	}

	if snapshot.ActiveCount < snapshot.TargetSize {
		member = s.poolMemberForFreshProbe(auth, PoolStateActive, now)
		s.setPoolMemberState(member, PoolStateActive, "fill_active")
		s.applyCoreAuthAddOrUpdate(ctx, auth)
		return true
	}
	if snapshot.ReserveCount < s.poolManager.ReserveTargetSize() {
		s.setPoolMemberState(member, PoolStateReserve, "fill_reserve")
		return true
	}
	if s.replaceFallbackActive(ctx, auth) {
		return true
	}
	return false
}

func (s *Service) replaceFallbackActive(ctx context.Context, auth *coreauth.Auth) bool {
	if s == nil || s.poolManager == nil || auth == nil {
		return false
	}
	threshold := s.poolManager.LowQuotaThresholdPercent()
	fallbacks := s.poolManager.ActiveFallbackIDs(threshold)
	candidateMember := s.poolMemberForFreshProbe(auth, PoolStateActive, time.Now())
	candidateRemaining := candidateMember.RemainingPercent
	candidateKnown := candidateRemaining >= 0 && candidateRemaining <= 100

	if len(fallbacks) > 0 {
		worstID := fallbacks[0]
		worstMember, ok := s.poolManager.LastSeenMember(worstID)
		if !ok {
			return false
		}
		candidateComparable := candidateRemaining
		if !candidateKnown {
			candidateComparable = poolUnknownRemainingPercent
		}
		if worstMember.RemainingPercent >= 0 && candidateComparable <= worstMember.RemainingPercent {
			return false
		}
		s.setPoolMemberState(worstMember, PoolStateLowQuota, "replace_fallback_active")
		s.setPoolMemberState(candidateMember, PoolStateActive, "replace_fallback_active")
		s.applyCoreAuthAddOrUpdate(ctx, auth)
		s.syncPoolActiveToRuntime(ctx)
		log.Infof("pool-manager: replaced fallback active old=%s old_remaining=%d new=%s new_remaining=%d", worstID, worstMember.RemainingPercent, auth.ID, candidateComparable)
		return true
	}

	if !candidateKnown {
		return false
	}
	if candidateRemaining <= threshold {
		return false
	}

	snapshot := s.poolManager.Snapshot()
	if snapshot.ActiveCount < snapshot.TargetSize {
		return false
	}

	now := time.Now()
	worstID := ""
	var worstMember PoolMember
	worstRemaining := 101
	for _, id := range snapshot.ActiveIDs {
		member, ok := s.poolManager.LastSeenMember(id)
		if !ok {
			continue
		}
		if member.InFlightCount > 0 {
			continue
		}
		if !member.ProtectedUntil.IsZero() && member.ProtectedUntil.After(now) {
			continue
		}
		remaining := member.RemainingPercent
		if remaining < 0 || remaining > 100 {
			remaining = -1
		}
		if worstID == "" || remaining < worstRemaining || (remaining == worstRemaining && member.AuthID < worstID) {
			worstID = member.AuthID
			worstMember = member
			worstRemaining = remaining
		}
	}
	if worstID == "" {
		return false
	}
	if strings.TrimSpace(worstID) == strings.TrimSpace(auth.ID) {
		return false
	}
	if candidateRemaining <= worstRemaining {
		return false
	}

	demoteState := PoolStateReserve
	if worstRemaining >= 0 && worstRemaining <= threshold {
		demoteState = PoolStateLowQuota
	}

	s.setPoolMemberState(worstMember, demoteState, "replace_low_quality_active")
	s.setPoolMemberState(candidateMember, PoolStateActive, "replace_low_quality_active")
	s.applyCoreAuthAddOrUpdate(ctx, auth)
	s.syncPoolActiveToRuntime(ctx)
	log.Infof("pool-manager: replaced active old=%s old_remaining=%d new=%s new_remaining=%d", worstID, worstMember.RemainingPercent, auth.ID, candidateRemaining)
	return true
}

func (s *Service) replaceLowQualityActiveFromReserve(ctx context.Context, maxReplacements int) int {
	if s == nil || s.poolManager == nil || maxReplacements <= 0 {
		return 0
	}

	replaced := 0
	for replaced < maxReplacements {
		snapshot := s.poolManager.Snapshot()
		if snapshot.ActiveCount < snapshot.TargetSize || snapshot.ReserveCount == 0 {
			break
		}

		now := time.Now()
		bestReserveID := ""
		bestReserveRemaining := -1
		for _, authID := range snapshot.ReserveIDs {
			member, ok := s.poolManager.LastSeenMember(authID)
			if !ok {
				continue
			}
			if member.InFlightCount > 0 {
				continue
			}
			if !member.ProtectedUntil.IsZero() && member.ProtectedUntil.After(now) {
				continue
			}
			remaining := member.RemainingPercent
			if remaining < 0 || remaining > 100 {
				continue
			}
			if bestReserveID == "" || remaining > bestReserveRemaining || (remaining == bestReserveRemaining && member.AuthID < bestReserveID) {
				bestReserveID = member.AuthID
				bestReserveRemaining = remaining
			}
		}
		if bestReserveID == "" {
			break
		}

		auth := s.poolCandidate(bestReserveID)
		if auth == nil {
			s.removePoolMember(bestReserveID, "", "reserve_missing")
			break
		}

		probeCtx := WithDispositionSource(ctx, "pool_probe")
		beforeProbe := auth.Clone()
		probedAuth, result := poolProbeAuthFunc(probeCtx, s.cfg, auth.Clone())
		if probedAuth != nil {
			s.storePoolCandidate(probedAuth)
		}
		s.recordPoolProbe(PoolStateReserve, beforeProbe, probedAuth, result, "")

		reason := ""
		if result.Error != nil {
			reason = result.Error.Code
		}
		reserveInterval := time.Duration(s.cfg.PoolManager.ReserveScanIntervalSeconds) * time.Second
		if reserveInterval <= 0 {
			reserveInterval = time.Minute
		}
		s.poolManager.MarkProbe(bestReserveID, now, now.Add(reserveInterval), result.Success, reason)

		if !result.Success {
			s.handlePoolProbeResult(bestReserveID, auth.Provider, result)
			break
		}
		if probedAuth == nil {
			break
		}
		if authIsLowQuota(probedAuth, s.poolManager.LowQuotaThresholdPercent()) {
			s.setPoolMemberState(s.poolMemberForAuth(probedAuth, PoolStateLowQuota), PoolStateLowQuota, "reserve_probe_low_quota")
			log.Infof("pool-manager: low quota auth=%s remaining_percent=%d threshold=%d", probedAuth.ID, mustWeeklyRemainingPercent(probedAuth), s.poolManager.LowQuotaThresholdPercent())
			continue
		}
		if !s.replaceFallbackActive(ctx, probedAuth) {
			break
		}
		replaced++
	}

	return replaced
}

func (s *Service) nextColdCandidateIDs(limit int) []string {
	if s == nil || limit <= 0 {
		return nil
	}
	s.resortPoolCandidateOrder()
	s.poolCandidateMu.Lock()
	defer s.poolCandidateMu.Unlock()
	if len(s.poolCandidateOrder) == 0 {
		return nil
	}
	ids := make([]string, 0, limit)
	total := len(s.poolCandidateOrder)
	cursor := s.poolCandidateCursor
	visited := 0
	for visited < total && len(ids) < limit {
		id := s.poolCandidateOrder[cursor]
		cursor = (cursor + 1) % total
		visited++
		if id == "" || s.poolManager.HasTrackedState(id) {
			continue
		}
		if _, ok := s.poolCandidateIndex[id]; !ok && s.poolCandidates[id] == nil {
			continue
		}
		ids = append(ids, id)
	}
	s.poolCandidateCursor = cursor
	return ids
}

func (s *Service) handlePoolProbeResult(authID, provider string, result coreauth.Result) {
	if s == nil || s.poolManager == nil {
		return
	}
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return
	}
	member, ok := s.poolManager.LastSeenMember(authID)
	if !ok {
		member = PoolMember{AuthID: authID, Provider: provider}
	}
	member.Provider = provider
	if result.Success {
		s.setPoolMemberState(member, PoolStateReserve, "probe_success")
		return
	}
	if result.Error != nil {
		switch result.Error.HTTPStatus {
		case http.StatusTooManyRequests, http.StatusPaymentRequired:
			if s.poolMetrics != nil {
				s.poolMetrics.RecordMovedToLimit()
			}
			s.setPoolMemberState(member, PoolStateLimit, "probe_limit")
			return
		}
	}
	s.setPoolMemberState(member, PoolStateReserve, "probe_failure")
}

func (s *Service) syncPoolActiveToRuntime(ctx context.Context) {
	if s == nil || s.poolManager == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ctx = coreauth.WithSkipPersist(ctx)
	s.trimActiveOverflow()

	added, modified, removed := s.poolManager.ActiveDiff(s.publishedActive)
	modified = nil
	for _, id := range added {
		if auth := s.poolCandidate(id); auth != nil {
			log.Infof("pool-publish: add auth=%s provider=%s", id, auth.Provider)
			s.emitAuthUpdate(ctx, watcher.AuthUpdate{
				Action: watcher.AuthUpdateActionAdd,
				ID:     id,
				Auth:   auth,
			})
		}
	}
	for _, id := range removed {
		provider := ""
		if member, ok := s.poolManager.LastSeenMember(id); ok {
			provider = member.Provider
		}
		log.Infof("pool-publish: delete auth=%s provider=%s", id, provider)
		s.emitAuthUpdate(ctx, watcher.AuthUpdate{
			Action: watcher.AuthUpdateActionDelete,
			ID:     id,
		})
	}
	if s.poolMetrics != nil {
		s.poolMetrics.RecordPublish(len(added), len(modified), len(removed))
	}

	published := make(map[string]time.Time, len(s.poolManager.ActiveIDs()))
	for _, id := range s.poolManager.ActiveIDs() {
		published[id] = time.Now()
	}
	s.publishedActive = published
	if len(added) > 0 || len(modified) > 0 || len(removed) > 0 {
		log.Infof(
			"pool-publish: completed add=%d modify=%d delete=%d active_size=%d",
			len(added),
			len(modified),
			len(removed),
			len(published),
		)
		s.logPoolEvaluation()
	}
}

func (s *Service) handleAuthDisposition(ctx context.Context, disposition AuthDisposition) {
	if s == nil || s.poolManager == nil {
		return
	}
	authID := strings.TrimSpace(disposition.AuthID)
	if authID == "" {
		return
	}
	log.Infof(
		"auth-disposition: auth=%s provider=%s source=%s healthy=%t pool_eligible=%t deleted=%t moved_to_limit=%t refreshed=%t quota=%t",
		authID,
		disposition.Provider,
		disposition.Source,
		disposition.Healthy,
		disposition.PoolEligible,
		disposition.Deleted,
		disposition.MovedToLimit,
		disposition.Refreshed,
		disposition.QuotaExceeded,
	)
	if !s.poolManager.IsActive(authID) && !disposition.Deleted && !disposition.MovedToLimit && disposition.PoolEligible {
		return
	}

	wasActive := s.poolManager.IsActive(authID)
	if disposition.Deleted {
		if wasActive {
			log.Infof("pool-manager: active removed auth=%s reason=deleted source=%s", authID, disposition.Source)
			if s.poolMetrics != nil {
				s.poolMetrics.RecordActiveRemoval()
			}
		}
		if s.poolMetrics != nil {
			s.poolMetrics.RecordDeleted()
		}
		s.removePoolMember(authID, disposition.Provider, "deleted")
		s.deletePoolCandidate(authID)
	} else if disposition.MovedToLimit || !disposition.PoolEligible {
		reason := "ineligible"
		if disposition.MovedToLimit {
			reason = "limit"
		}
		if wasActive {
			log.Infof("pool-manager: active removed auth=%s reason=%s source=%s", authID, reason, disposition.Source)
			if s.poolMetrics != nil {
				s.poolMetrics.RecordActiveRemoval()
			}
		}
		if disposition.MovedToLimit && s.poolMetrics != nil {
			s.poolMetrics.RecordMovedToLimit()
		}
		if member, ok := s.poolManager.LastSeenMember(authID); ok {
			member.Provider = disposition.Provider
			s.setPoolMemberState(member, PoolStateLimit, reason)
		} else {
			s.removePoolMember(authID, disposition.Provider, reason)
		}
	} else {
		if member, ok := s.poolManager.LastSeenMember(authID); ok {
			member.Provider = disposition.Provider
			s.setPoolMemberState(member, PoolStateActive, "eligible")
		}
	}
	if disposition.Refreshed && s.poolMetrics != nil {
		s.poolMetrics.RecordRefreshed()
	}

	s.syncPoolActiveToRuntime(ctx)
	if wasActive || s.poolManager.Snapshot().Underfilled {
		s.schedulePoolRebalance()
	}
}

func (s *Service) fillActiveFromReserve(ctx context.Context) {
	if s == nil || s.poolManager == nil {
		return
	}
	for s.poolManager.Snapshot().ActiveCount < s.poolManager.TargetSize() {
		promoted := false
		for _, authID := range s.poolManager.ReserveIDs() {
			if !s.consumeBackgroundProbeBudget(time.Now()) {
				return
			}
			auth := s.poolCandidate(authID)
			if auth == nil {
				s.removePoolMember(authID, "", "reserve_missing")
				continue
			}
			beforeProbe := auth.Clone()
			probedAuth, result := poolProbeAuthFunc(ctx, s.cfg, auth.Clone())
			if probedAuth != nil {
				s.storePoolCandidate(probedAuth)
			}
			s.recordPoolProbe(PoolStateReserve, beforeProbe, probedAuth, result, "")
			if result.Success && probedAuth != nil {
				if authIsLowQuota(probedAuth, s.poolManager.LowQuotaThresholdPercent()) {
					s.setPoolMemberState(s.poolMemberForAuth(probedAuth, PoolStateLowQuota), PoolStateLowQuota, "reserve_probe_low_quota")
					log.Infof("pool-manager: low quota auth=%s remaining_percent=%d threshold=%d", probedAuth.ID, mustWeeklyRemainingPercent(probedAuth), s.poolManager.LowQuotaThresholdPercent())
					continue
				}
				s.setPoolMemberState(s.poolMemberForAuth(probedAuth, PoolStateActive), PoolStateActive, "promote_reserve")
				s.applyCoreAuthAddOrUpdate(ctx, probedAuth)
				if s.poolMetrics != nil {
					s.poolMetrics.RecordPromotion()
				}
				log.Infof("pool-manager: promoted auth=%s from=reserve to=active", probedAuth.ID)
				promoted = true
				break
			}
			s.handlePoolProbeResult(authID, auth.Provider, result)
		}
		if !promoted {
			snapshot := s.poolManager.Snapshot()
			s.logPoolUnderfilled(time.Now(), snapshot.ReserveCount == 0)
			break
		}
	}
	s.clearPoolUnderfilled(time.Now())
}

func (s *Service) reserveRefillLowWatermark() int {
	if s == nil || s.poolManager == nil {
		return 0
	}
	var cfg config.PoolManagerConfig
	if s.cfg != nil {
		cfg = s.cfg.PoolManager
	}
	return reserveRefillLowWatermark(cfg, s.poolManager.ReserveTargetSize())
}

func (s *Service) reserveRefillHighWatermark() int {
	if s == nil || s.poolManager == nil {
		return 0
	}
	var cfg config.PoolManagerConfig
	if s.cfg != nil {
		cfg = s.cfg.PoolManager
	}
	return reserveRefillHighWatermark(cfg, s.poolManager.ReserveTargetSize())
}

func (s *Service) coldBatchLoadSize() int {
	if s == nil || s.poolManager == nil {
		return 0
	}
	var cfg config.PoolManagerConfig
	if s.cfg != nil {
		cfg = s.cfg.PoolManager
	}
	return coldBatchLoadSize(cfg, s.poolManager.TargetSize())
}

func (s *Service) activeQuotaRefreshSampleSize() int {
	if s == nil || s.poolManager == nil {
		return 0
	}
	var cfg config.PoolManagerConfig
	if s.cfg != nil {
		cfg = s.cfg.PoolManager
	}
	return activeQuotaRefreshSampleSize(cfg, s.poolManager.TargetSize())
}

func (s *Service) poolNeedsRebalanceAtReserveThreshold(reserveThreshold int) bool {
	if s == nil || s.poolManager == nil {
		return false
	}
	snapshot := s.poolManager.Snapshot()
	if snapshot.ActiveCount < snapshot.TargetSize {
		return true
	}
	if reserveThreshold > 0 && snapshot.ReserveCount < reserveThreshold {
		return true
	}
	return len(s.poolManager.ActiveFallbackIDs(s.poolManager.LowQuotaThresholdPercent())) > 0
}

func (s *Service) poolNeedsRebalance() bool {
	return s.poolNeedsRebalanceAtReserveThreshold(s.reserveRefillLowWatermark())
}

func (s *Service) shouldDemoteLowQuotaActive() bool {
	if s == nil || s.poolManager == nil {
		return false
	}
	snapshot := s.poolManager.Snapshot()
	if snapshot.ReserveCount > 0 {
		return true
	}
	threshold := s.poolManager.LowQuotaThresholdPercent()
	fallbackCount := len(s.poolManager.ActiveFallbackIDs(threshold))
	if fallbackCount == 0 {
		return false
	}
	return snapshot.ActiveCount-fallbackCount >= snapshot.TargetSize
}

func (s *Service) fillWarmReserveFromColdCandidates(ctx context.Context) {
	if s == nil || s.poolManager == nil || s.cfg == nil {
		return
	}
	if !s.poolNeedsRebalance() {
		return
	}

	snapshot := s.poolManager.Snapshot()
	activeDeficit := snapshot.TargetSize - snapshot.ActiveCount
	if activeDeficit < 0 {
		activeDeficit = 0
	}
	reserveLowWatermark := s.reserveRefillLowWatermark()
	reserveTarget := s.reserveRefillHighWatermark()
	reserveDeficit := reserveTarget - snapshot.ReserveCount
	if reserveDeficit < 0 {
		reserveDeficit = 0
	}
	fallbackCount := len(s.poolManager.ActiveFallbackIDs(s.poolManager.LowQuotaThresholdPercent()))
	budget := s.coldBatchLoadSize()
	if budget <= 0 {
		budget = 20
	}
	maxBudget := budget
	if activeDeficit > 0 || (reserveTarget > reserveLowWatermark && reserveDeficit > 0) || fallbackCount > 0 {
		budget += activeDeficit + reserveDeficit + fallbackCount
		maxBudget = budget * 5
		if maxBudget < budget {
			maxBudget = budget
		}
	}

	sampled := 0
	healthy := 0
	unhealthy := 0
	for sampled < maxBudget && s.poolNeedsRebalanceAtReserveThreshold(reserveTarget) {
		batchLimit := s.coldBatchLoadSize()
		if batchLimit <= 0 {
			batchLimit = 20
		}
		remainingBudget := maxBudget - sampled
		if batchLimit > remainingBudget {
			batchLimit = remainingBudget
		}
		ids := s.nextColdCandidateIDs(batchLimit)
		if len(ids) == 0 {
			break
		}
		for _, authID := range ids {
			if !s.consumeBackgroundProbeBudget(time.Now()) {
				break
			}
			auth := s.poolCandidate(authID)
			if auth == nil {
				continue
			}
			sampled++
			beforeProbe := auth.Clone()
			probedAuth, result := poolProbeAuthFunc(WithDispositionSource(ctx, "pool_probe"), s.cfg, auth.Clone())
			if probedAuth != nil {
				s.storePoolCandidate(probedAuth)
			}
			s.recordPoolProbe(PoolStateReserve, beforeProbe, probedAuth, result, "")
			if result.Success && probedAuth != nil {
				if s.placeProbedAuth(ctx, probedAuth, true) {
					healthy++
				} else {
					healthy++
				}
			} else {
				unhealthy++
				if result.Error != nil {
					switch result.Error.HTTPStatus {
					case http.StatusTooManyRequests, http.StatusPaymentRequired:
						s.setPoolMemberState(s.poolMemberForAuth(auth, PoolStateLimit), PoolStateLimit, "cold_candidate_limit")
					}
				}
			}
			if !s.poolNeedsRebalanceAtReserveThreshold(reserveTarget) || sampled >= maxBudget {
				break
			}
		}
	}
	s.replaceLowQualityActiveFromReserve(ctx, s.poolManager.ReserveTargetSize())
	if sampled > 0 {
		candidateCount, coldCandidateCount := s.poolCandidateCounts()
		log.Infof("pool-manager: cold scan sampled=%d healthy=%d unhealthy=%d candidate_size=%d cold_candidate_size=%d", sampled, healthy, unhealthy, candidateCount, coldCandidateCount)
	}
}

func (s *Service) handleSelectedAuth(ctx context.Context, authID string) {
	if s == nil || s.poolManager == nil {
		return
	}
	s.poolManager.BeginUse(authID, time.Now(), poolSelectedAuthLeaseDuration)
}

func (s *Service) handlePoolResult(ctx context.Context, result coreauth.Result) {
	if s == nil || s.poolManager == nil {
		return
	}
	if DispositionSource(ctx) != "request" {
		return
	}
	if strings.TrimSpace(result.AuthID) == "" {
		return
	}
	s.poolManager.RecordRequest(result.AuthID, result.Success, time.Now())
}

func (s *Service) startPoolRebalanceWorker(parent context.Context) {
	if s == nil || s.poolManager == nil || !s.poolManager.Enabled() {
		return
	}
	if s.poolRebalanceStop != nil {
		return
	}
	if s.poolRebalanceQueue == nil {
		s.poolRebalanceQueue = make(chan struct{}, 1)
	}
	ctx, cancel := context.WithCancel(parent)
	s.poolRebalanceStop = cancel
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-s.poolRebalanceQueue:
				s.runPoolRebalanceNow(ctx)
			}
		}
	}()
}

func (s *Service) schedulePoolRebalance() {
	if s == nil || s.poolManager == nil || !s.poolManager.Enabled() {
		return
	}
	if s.poolRebalanceQueue == nil {
		s.runPoolRebalanceNow(context.Background())
		return
	}
	select {
	case s.poolRebalanceQueue <- struct{}{}:
	default:
	}
}

func (s *Service) runPoolRebalanceNow(ctx context.Context) {
	if s == nil || s.poolManager == nil {
		return
	}
	s.poolRebalanceMu.Lock()
	defer s.poolRebalanceMu.Unlock()
	s.fillActiveFromReserve(ctx)
	s.fillWarmReserveFromColdCandidates(ctx)
	s.fillActiveFromReserve(ctx)
	s.syncPoolActiveToRuntime(ctx)
}

func (s *Service) startPoolProbeLoops(parent context.Context) {
	if s == nil || s.poolManager == nil || s.cfg == nil || !s.poolManager.Enabled() {
		return
	}
	if s.poolProbeStop != nil {
		return
	}
	ctx, cancel := context.WithCancel(parent)
	s.poolProbeStop = cancel
	s.startPoolRebalanceWorker(parent)
	s.schedulePoolRebalance()

	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		s.runActiveProbeCycle(ctx, time.Now())
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-ticker.C:
				s.runActiveProbeCycle(ctx, now)
			}
		}
	}()

	if interval := time.Duration(s.cfg.PoolManager.ActiveQuotaRefreshIntervalSeconds) * time.Second; interval > 0 && s.activeQuotaRefreshSampleSize() > 0 {
		go func() {
			ticker := time.NewTicker(interval)
			defer ticker.Stop()
			s.runActiveQuotaRefreshCycle(ctx, time.Now())
			for {
				select {
				case <-ctx.Done():
					return
				case now := <-ticker.C:
					s.runActiveQuotaRefreshCycle(ctx, now)
				}
			}
		}()
	}

	if interval := time.Duration(s.cfg.PoolManager.ReserveScanIntervalSeconds) * time.Second; interval > 0 {
		go func() {
			ticker := time.NewTicker(interval)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case now := <-ticker.C:
					s.runReserveProbeCycle(ctx, now)
				}
			}
		}()
	}

	if interval := time.Duration(s.cfg.PoolManager.LimitScanIntervalSeconds) * time.Second; interval > 0 {
		go func() {
			ticker := time.NewTicker(interval)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case now := <-ticker.C:
					s.runLimitProbeCycle(ctx, now)
				}
			}
		}()
	}

	go func() {
		ticker := time.NewTicker(poolEvalLogInterval)
		defer ticker.Stop()
		s.runPoolEvalCycle(time.Now())
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-ticker.C:
				s.runPoolEvalCycle(now)
			}
		}
	}()
}

func (s *Service) runActiveProbeCycle(ctx context.Context, now time.Time) {
	if s == nil || s.poolManager == nil || s.cfg == nil || !s.poolManager.Enabled() {
		return
	}
	interval := time.Duration(s.cfg.PoolManager.ActiveIdleScanIntervalSeconds) * time.Second
	if interval <= 0 {
		return
	}
	sampled := 0
	healthy := 0
	unhealthy := 0
	for _, authID := range s.poolManager.DueActiveProbeIDs(now, interval) {
		if !s.consumeBackgroundProbeBudget(now) {
			break
		}
		sampled++
		auth, ok := s.coreManager.GetByID(authID)
		if !ok || auth == nil {
			s.removePoolMember(authID, auth.Provider, "active_missing")
			continue
		}
		probeCtx := WithDispositionSource(ctx, "pool_probe")
		beforeProbe := auth.Clone()
		probedAuth, result := poolProbeAuthFunc(probeCtx, s.cfg, auth)
		if probedAuth != nil {
			s.storePoolCandidate(probedAuth)
			if result.Success {
				s.applyCoreAuthAddOrUpdate(coreauth.WithSkipPersist(probeCtx), probedAuth)
			}
		}
		s.recordPoolProbe(PoolStateActive, beforeProbe, probedAuth, result, "")
		next := now.Add(interval)
		reason := ""
		if result.Error != nil {
			reason = result.Error.Code
		}
		s.poolManager.MarkProbe(authID, now, next, result.Success, reason)
		if s.coreManager != nil {
			s.coreManager.MarkResult(coreauth.WithSkipPersist(probeCtx), result)
		}
		if result.Success && probedAuth != nil {
			s.storePoolCandidate(probedAuth)
			s.setPoolMemberState(s.poolMemberForAuth(probedAuth, PoolStateActive), PoolStateActive, "active_probe_ok")
		}
		if result.Success && probedAuth != nil && authIsLowQuota(probedAuth, s.poolManager.LowQuotaThresholdPercent()) && s.shouldDemoteLowQuotaActive() {
			if s.poolMetrics != nil {
				s.poolMetrics.RecordActiveRemoval()
			}
			s.setPoolMemberState(s.poolMemberForAuth(probedAuth, PoolStateLowQuota), PoolStateLowQuota, "active_probe_low_quota")
			log.Infof("pool-manager: active demoted auth=%s reason=low_quota remaining_percent=%d threshold=%d", probedAuth.ID, mustWeeklyRemainingPercent(probedAuth), s.poolManager.LowQuotaThresholdPercent())
			s.syncPoolActiveToRuntime(ctx)
			s.schedulePoolRebalance()
		}
		if result.Success {
			healthy++
		} else {
			unhealthy++
		}
	}
	if sampled > 0 {
		log.Infof("pool-manager: active probe sampled=%d healthy=%d unhealthy=%d", sampled, healthy, unhealthy)
	}
	if s.poolNeedsRebalance() {
		s.schedulePoolRebalance()
	}
}

func (s *Service) runActiveQuotaRefreshCycle(ctx context.Context, now time.Time) {
	if s == nil || s.poolManager == nil || s.cfg == nil || !s.poolManager.Enabled() {
		return
	}
	interval := time.Duration(s.cfg.PoolManager.ActiveQuotaRefreshIntervalSeconds) * time.Second
	if interval <= 0 {
		return
	}
	sampleSize := s.activeQuotaRefreshSampleSize()
	if sampleSize <= 0 {
		return
	}

	sampled := 0
	healthy := 0
	unhealthy := 0
	for _, authID := range s.poolManager.DueActiveQuotaProbeIDs(now, interval, sampleSize) {
		if !s.consumeBackgroundProbeBudget(now) {
			break
		}
		sampled++
		auth, ok := s.coreManager.GetByID(authID)
		if !ok || auth == nil {
			provider := ""
			if member, okMember := s.poolManager.LastSeenMember(authID); okMember {
				provider = member.Provider
			}
			s.removePoolMember(authID, provider, "active_missing")
			continue
		}
		probeCtx := WithDispositionSource(ctx, "pool_probe")
		beforeProbe := auth.Clone()
		probedAuth, result := poolProbeAuthFunc(probeCtx, s.cfg, auth)
		if probedAuth != nil {
			s.storePoolCandidate(probedAuth)
			if result.Success {
				s.applyCoreAuthAddOrUpdate(coreauth.WithSkipPersist(probeCtx), probedAuth)
			}
		}
		s.recordPoolProbe(PoolStateActive, beforeProbe, probedAuth, result, "")
		reason := ""
		if result.Error != nil {
			reason = result.Error.Code
		}
		s.poolManager.MarkQuotaProbe(authID, now, now.Add(interval), result.Success, reason)
		if s.coreManager != nil {
			s.coreManager.MarkResult(coreauth.WithSkipPersist(probeCtx), result)
		}
		if result.Success && probedAuth != nil {
			s.storePoolCandidate(probedAuth)
			s.setPoolMemberState(s.poolMemberForAuth(probedAuth, PoolStateActive), PoolStateActive, "active_quota_refresh_ok")
		}
		if result.Success && probedAuth != nil && authIsLowQuota(probedAuth, s.poolManager.LowQuotaThresholdPercent()) && s.shouldDemoteLowQuotaActive() {
			if s.poolMetrics != nil {
				s.poolMetrics.RecordActiveRemoval()
			}
			s.setPoolMemberState(s.poolMemberForAuth(probedAuth, PoolStateLowQuota), PoolStateLowQuota, "active_quota_refresh_low_quota")
			log.Infof("pool-manager: active demoted auth=%s reason=low_quota remaining_percent=%d threshold=%d", probedAuth.ID, mustWeeklyRemainingPercent(probedAuth), s.poolManager.LowQuotaThresholdPercent())
			s.syncPoolActiveToRuntime(ctx)
			s.schedulePoolRebalance()
		}
		if result.Success {
			healthy++
		} else {
			unhealthy++
		}
	}

	if sampled > 0 {
		s.replaceLowQualityActiveFromReserve(ctx, sampleSize)
		log.Infof("pool-manager: active quota refresh sampled=%d healthy=%d unhealthy=%d", sampled, healthy, unhealthy)
	}
	if s.poolNeedsRebalance() {
		s.schedulePoolRebalance()
	}
	s.syncPoolActiveToRuntime(ctx)
}

func (s *Service) runReserveProbeCycle(ctx context.Context, now time.Time) {
	if s == nil || s.poolManager == nil || s.cfg == nil || !s.poolManager.Enabled() {
		return
	}
	interval := time.Duration(s.cfg.PoolManager.ReserveScanIntervalSeconds) * time.Second
	if interval <= 0 {
		return
	}
	sampleSize := s.cfg.PoolManager.ReserveSampleSize
	if sampleSize <= 0 {
		return
	}
	sampled := 0
	healthy := 0
	unhealthy := 0
	skipped := 0
	for _, authID := range s.poolManager.DueReserveProbeIDs(now, interval, sampleSize) {
		if !s.consumeBackgroundProbeBudget(now) {
			break
		}
		sampled++
		auth := s.poolCandidate(authID)
		if auth == nil {
			s.removePoolMember(authID, "", "reserve_missing")
			skipped++
			continue
		}
		beforeProbe := auth.Clone()
		probedAuth, result := poolProbeAuthFunc(WithDispositionSource(ctx, "pool_probe"), s.cfg, auth.Clone())
		if probedAuth != nil {
			s.storePoolCandidate(probedAuth)
		}
		s.recordPoolProbe(PoolStateReserve, beforeProbe, probedAuth, result, "")
		reason := ""
		if result.Error != nil {
			reason = result.Error.Code
		}
		s.poolManager.MarkProbe(authID, now, now.Add(interval), result.Success, reason)
		if result.Success {
			if probedAuth != nil && authIsLowQuota(probedAuth, s.poolManager.LowQuotaThresholdPercent()) {
				if s.poolManager.Snapshot().ActiveCount < s.poolManager.TargetSize() {
					s.placeProbedAuth(ctx, probedAuth, true)
					healthy++
					continue
				}
				s.setPoolMemberState(s.poolMemberForAuth(probedAuth, PoolStateLowQuota), PoolStateLowQuota, "reserve_probe_low_quota")
				log.Infof("pool-manager: low quota auth=%s remaining_percent=%d threshold=%d", probedAuth.ID, mustWeeklyRemainingPercent(probedAuth), s.poolManager.LowQuotaThresholdPercent())
				healthy++
				continue
			}
			if probedAuth != nil && s.replaceFallbackActive(ctx, probedAuth) {
				healthy++
				continue
			}
			if probedAuth != nil {
				s.setPoolMemberState(s.poolMemberForAuth(probedAuth, PoolStateReserve), PoolStateReserve, "reserve_probe_ok")
			}
			healthy++
			continue
		}
		unhealthy++
		s.handlePoolProbeResult(authID, auth.Provider, result)
	}
	if sampled > 0 {
		log.Infof("pool-manager: reserve probe sampled=%d healthy=%d unhealthy=%d skipped=%d", sampled, healthy, unhealthy, skipped)
	}
	if s.poolNeedsRebalance() {
		s.schedulePoolRebalance()
	}
}

func (s *Service) runLimitProbeCycle(ctx context.Context, now time.Time) {
	if s == nil || s.poolManager == nil || s.cfg == nil || !s.poolManager.Enabled() {
		return
	}
	interval := time.Duration(s.cfg.PoolManager.LimitScanIntervalSeconds) * time.Second
	if interval <= 0 {
		return
	}
	sampled := 0
	restored := 0
	stillLimited := 0
	skipped := 0
	for _, authID := range s.poolManager.DueLimitProbeIDs(now, interval) {
		if !s.consumeBackgroundProbeBudget(now) {
			break
		}
		sampled++
		auth := s.poolCandidate(authID)
		if auth == nil {
			s.removePoolMember(authID, "", "limit_missing")
			skipped++
			continue
		}
		beforeProbe := auth.Clone()
		probedAuth, result := poolProbeAuthFunc(WithDispositionSource(ctx, "pool_probe"), s.cfg, auth.Clone())
		if probedAuth != nil {
			s.storePoolCandidate(probedAuth)
		}
		disposition := ""
		if result.Success {
			disposition = "restore_to_reserve"
		}
		s.recordPoolProbe(PoolStateLimit, beforeProbe, probedAuth, result, disposition)
		reason := ""
		if result.Error != nil {
			reason = result.Error.Code
		}
		s.poolManager.MarkProbe(authID, now, now.Add(interval), result.Success, reason)
		if result.Success && probedAuth != nil {
			s.placeProbedAuth(ctx, probedAuth, true)
			if s.poolMetrics != nil {
				s.poolMetrics.RecordRestoredFromLimit()
			}
			restored++
			continue
		}
		stillLimited++
		s.handlePoolProbeResult(authID, auth.Provider, result)
	}
	if sampled > 0 {
		log.Infof("pool-manager: limit probe sampled=%d restored=%d still_limited=%d skipped=%d", sampled, restored, stillLimited, skipped)
	}
	s.fillActiveFromReserve(ctx)
	s.syncPoolActiveToRuntime(ctx)
}

func (s *Service) poolMetricsSnapshot() any {
	if s == nil || s.poolManager == nil || s.poolMetrics == nil {
		return nil
	}
	return s.poolMetricsSnapshotCurrent()
}

func (s *Service) recordPoolProbe(bucket PoolState, original, probed *coreauth.Auth, result coreauth.Result, successDisposition string) {
	if s == nil {
		return
	}
	if s.poolMetrics != nil {
		s.poolMetrics.RecordProbe(bucket, result.Success)
	}
	authID := result.AuthID
	if authID == "" && original != nil {
		authID = original.ID
	}
	if authID == "" && probed != nil {
		authID = probed.ID
	}
	refreshed := false
	if original != nil && probed != nil {
		beforeToken := codexProbeToken(original)
		afterToken := codexProbeToken(probed)
		refreshed = beforeToken != "" && afterToken != "" && beforeToken != afterToken
	}
	if refreshed && s.poolMetrics != nil {
		s.poolMetrics.RecordRefreshed()
	}

	resultLabel := "ok"
	if refreshed && result.Success {
		resultLabel = "401"
	} else if !result.Success {
		resultLabel = poolResultLabel(result)
	}

	var fields []string
	fields = append(fields, "pool-probe: auth="+authID)
	fields = append(fields, "bucket="+string(bucket))
	fields = append(fields, "result="+resultLabel)
	if refreshed && result.Success {
		fields = append(fields, "refresh=success")
		fields = append(fields, "verify=ok")
	}
	disposition := successDisposition
	if disposition == "" && !result.Success {
		disposition = poolProbeDisposition(result)
	}
	if disposition != "" {
		fields = append(fields, "disposition="+disposition)
	}
	log.Info(strings.Join(fields, " "))
}

func poolResultLabel(result coreauth.Result) string {
	if result.Error == nil {
		if result.Success {
			return "ok"
		}
		return "error"
	}
	if status := result.Error.HTTPStatus; status > 0 {
		return strconv.Itoa(status)
	}
	if code := strings.TrimSpace(result.Error.Code); code != "" {
		return code
	}
	return "error"
}

func poolProbeDisposition(result coreauth.Result) string {
	if result.Error == nil {
		return ""
	}
	switch result.Error.HTTPStatus {
	case http.StatusTooManyRequests, http.StatusPaymentRequired:
		return "limit"
	}
	return ""
}

func (s *Service) logPoolEvaluation() {
	if s == nil || s.poolManager == nil || s.poolMetrics == nil {
		return
	}
	poolSnapshot := s.poolMetricsSnapshotCurrent()
	usageSnapshot := s.usageStatistics().Snapshot()
	successRate := 0.0
	if usageSnapshot.TotalRequests > 0 {
		successRate = float64(usageSnapshot.SuccessCount) * 100 / float64(usageSnapshot.TotalRequests)
	}
	log.Infof(
		"pool-eval: total_requests=%d success=%d failure=%d success_rate=%.2f%% active_size=%d reserve_size=%d low_quota_size=%d limit_size=%d candidate_size=%d cold_candidate_size=%d promoted=%d active_removed=%d moved_to_limit=%d deleted=%d",
		usageSnapshot.TotalRequests,
		usageSnapshot.SuccessCount,
		usageSnapshot.FailureCount,
		successRate,
		poolSnapshot.ActiveCount,
		poolSnapshot.ReserveCount,
		poolSnapshot.LowQuotaCount,
		poolSnapshot.LimitCount,
		poolSnapshot.CandidateCount,
		poolSnapshot.ColdCandidateCount,
		poolSnapshot.PromotionsTotal,
		poolSnapshot.ActiveRemovedTotal,
		poolSnapshot.MovedToLimitTotal,
		poolSnapshot.DeletedTotal,
	)
}

func (s *Service) usageStatistics() *internalusage.RequestStatistics {
	if s != nil && s.usageStats != nil {
		return s.usageStats
	}
	return internalusage.GetRequestStatistics()
}

func (s *Service) runPoolEvalCycle(now time.Time) {
	if s == nil || s.poolManager == nil || s.poolMetrics == nil || !s.poolManager.Enabled() {
		return
	}
	if now.IsZero() {
		now = time.Now()
	}

	poolSnapshot := s.poolMetricsSnapshotCurrent()
	usageSnapshot := s.usageStatistics().Snapshot()
	current := &poolEvalWindowSnapshot{
		At:                 now,
		TotalRequests:      usageSnapshot.TotalRequests,
		SuccessCount:       usageSnapshot.SuccessCount,
		FailureCount:       usageSnapshot.FailureCount,
		PromotionsTotal:    poolSnapshot.PromotionsTotal,
		ActiveRemovedTotal: poolSnapshot.ActiveRemovedTotal,
		RefreshedTotal:     poolSnapshot.RefreshedTotal,
		MovedToLimitTotal:  poolSnapshot.MovedToLimitTotal,
		DeletedTotal:       poolSnapshot.DeletedTotal,
		RestoredTotal:      poolSnapshot.RestoredFromLimit,
	}

	s.poolEvalMu.Lock()
	previous := s.poolEvalLast
	s.poolEvalLast = current
	s.poolEvalMu.Unlock()
	if previous == nil {
		successRate := 0.0
		if current.TotalRequests > 0 {
			successRate = float64(current.SuccessCount) * 100 / float64(current.TotalRequests)
		}
		log.Infof(
			"pool-eval: baseline interval=%s total_requests=%d success=%d failure=%d success_rate=%.2f%% active_size=%d reserve_size=%d low_quota_size=%d limit_size=%d candidate_size=%d cold_candidate_size=%d",
			poolEvalLogInterval,
			current.TotalRequests,
			current.SuccessCount,
			current.FailureCount,
			successRate,
			poolSnapshot.ActiveCount,
			poolSnapshot.ReserveCount,
			poolSnapshot.LowQuotaCount,
			poolSnapshot.LimitCount,
			poolSnapshot.CandidateCount,
			poolSnapshot.ColdCandidateCount,
		)
		return
	}
	if !now.After(previous.At) {
		return
	}

	window := now.Sub(previous.At)
	totalRequests := current.TotalRequests - previous.TotalRequests
	successCount := current.SuccessCount - previous.SuccessCount
	failureCount := current.FailureCount - previous.FailureCount
	successRate := 0.0
	if totalRequests > 0 {
		successRate = float64(successCount) * 100 / float64(totalRequests)
	}

	log.Infof(
		"pool-eval: window=%s total_requests=%d success=%d failure=%d success_rate=%.2f%% active_size=%d reserve_size=%d low_quota_size=%d limit_size=%d candidate_size=%d cold_candidate_size=%d",
		window,
		totalRequests,
		successCount,
		failureCount,
		successRate,
		poolSnapshot.ActiveCount,
		poolSnapshot.ReserveCount,
		poolSnapshot.LowQuotaCount,
		poolSnapshot.LimitCount,
		poolSnapshot.CandidateCount,
		poolSnapshot.ColdCandidateCount,
	)
	log.Infof(
		"pool-eval: window=%s active_removed=%d promoted=%d refreshed=%d moved_to_limit=%d deleted=%d restored=%d",
		window,
		current.ActiveRemovedTotal-previous.ActiveRemovedTotal,
		current.PromotionsTotal-previous.PromotionsTotal,
		current.RefreshedTotal-previous.RefreshedTotal,
		current.MovedToLimitTotal-previous.MovedToLimitTotal,
		current.DeletedTotal-previous.DeletedTotal,
		current.RestoredTotal-previous.RestoredTotal,
	)
	s.logPoolLowSuccess(window, totalRequests, successCount, failureCount, successRate)
}

func (s *Service) logPoolLowSuccess(window time.Duration, totalRequests, successCount, failureCount int64, successRate float64) {
	if s == nil {
		return
	}
	if totalRequests <= 0 {
		return
	}

	s.poolLowSuccessMu.Lock()
	defer s.poolLowSuccessMu.Unlock()

	if successRate < poolEvalLowSuccessThreshold {
		s.poolLowSuccessStreak++
		if s.poolLowSuccessStreak >= poolEvalLowSuccessConsecutiveWindows && !s.poolLowSuccessWarned {
			s.poolLowSuccessWarned = true
			log.Warnf(
				"pool-eval: low_success_rate warning threshold=%.2f%% consecutive_windows=%d current_rate=%.2f%% total_requests=%d failure=%d window=%s",
				poolEvalLowSuccessThreshold,
				s.poolLowSuccessStreak,
				successRate,
				totalRequests,
				failureCount,
				window,
			)
		}
		return
	}

	if s.poolLowSuccessWarned {
		log.Infof(
			"pool-eval: success_rate recovered threshold=%.2f%% previous_streak=%d current_rate=%.2f%% total_requests=%d success=%d failure=%d window=%s",
			poolEvalLowSuccessThreshold,
			s.poolLowSuccessStreak,
			successRate,
			totalRequests,
			successCount,
			failureCount,
			window,
		)
	}
	s.poolLowSuccessStreak = 0
	s.poolLowSuccessWarned = false
}

func (s *Service) logPoolUnderfilled(now time.Time, reserveExhausted bool) {
	if s == nil || s.poolManager == nil {
		return
	}
	if now.IsZero() {
		now = time.Now()
	}
	snapshot := s.poolManager.Snapshot()
	if !snapshot.Underfilled {
		return
	}

	s.poolUnderfilledMu.Lock()
	if s.poolUnderfilledSince.IsZero() {
		s.poolUnderfilledSince = now
	}
	underfilledFor := now.Sub(s.poolUnderfilledSince)
	s.poolUnderfilledMu.Unlock()

	log.Warnf(
		"pool-manager: underfilled active target=%d actual=%d reserve_exhausted=%t underfilled_for=%s",
		snapshot.TargetSize,
		snapshot.ActiveCount,
		reserveExhausted,
		underfilledFor.Round(time.Second),
	)
}

func (s *Service) clearPoolUnderfilled(now time.Time) {
	if s == nil || s.poolManager == nil {
		return
	}
	snapshot := s.poolManager.Snapshot()
	if snapshot.Underfilled {
		return
	}
	if now.IsZero() {
		now = time.Now()
	}

	s.poolUnderfilledMu.Lock()
	startedAt := s.poolUnderfilledSince
	s.poolUnderfilledSince = time.Time{}
	s.poolUnderfilledMu.Unlock()

	if startedAt.IsZero() {
		return
	}
	log.Infof(
		"pool-manager: active recovered target=%d actual=%d underfilled_for=%s",
		snapshot.TargetSize,
		snapshot.ActiveCount,
		now.Sub(startedAt).Round(time.Second),
	)
}

func (s *Service) handleAuthUpdate(ctx context.Context, update watcher.AuthUpdate) {
	if s == nil {
		return
	}
	s.cfgMu.RLock()
	cfg := s.cfg
	s.cfgMu.RUnlock()
	if cfg == nil || s.coreManager == nil {
		return
	}
	if update.Runtime {
		switch update.Action {
		case watcher.AuthUpdateActionAdd, watcher.AuthUpdateActionModify:
			if update.Auth == nil || update.Auth.ID == "" {
				return
			}
			s.applyCoreAuthAddOrUpdate(ctx, update.Auth)
		case watcher.AuthUpdateActionDelete:
			id := update.ID
			if id == "" && update.Auth != nil {
				id = update.Auth.ID
			}
			if id == "" {
				return
			}
			s.applyCoreAuthRemoval(ctx, id)
		default:
			log.Debugf("received unknown runtime auth update action: %v", update.Action)
		}
		return
	}
	switch update.Action {
	case watcher.AuthUpdateActionAdd, watcher.AuthUpdateActionModify:
		if update.Auth == nil || update.Auth.ID == "" {
			return
		}
		if s.poolManager != nil {
			s.indexPoolCandidate(update.Auth)
			if s.keepPoolCandidateHot(update.Auth.ID) {
				s.storePoolCandidate(update.Auth)
			} else {
				s.evictPoolCandidateIfIndexed(update.Auth.ID)
			}
			if s.poolManager.IsActive(update.Auth.ID) {
				s.applyCoreAuthAddOrUpdate(ctx, update.Auth)
			}
			return
		}
		s.applyCoreAuthAddOrUpdate(ctx, update.Auth)
	case watcher.AuthUpdateActionDelete:
		id := update.ID
		if id == "" && update.Auth != nil {
			id = update.Auth.ID
		}
		if id == "" {
			return
		}
		if s.poolManager != nil {
			provider := ""
			if member, ok := s.poolManager.LastSeenMember(id); ok {
				provider = member.Provider
			}
			wasActive := s.poolManager.IsActive(id)
			if s.poolManager.HasTrackedState(id) {
				s.removePoolMember(id, provider, "file_delete")
			}
			s.deletePoolCandidate(id)
			if wasActive {
				s.applyCoreAuthRemoval(ctx, id)
			}
			if s.poolRebalanceQueue != nil && s.poolNeedsRebalance() {
				s.schedulePoolRebalance()
			}
			return
		}
		s.applyCoreAuthRemoval(ctx, id)
	default:
		log.Debugf("received unknown auth update action: %v", update.Action)
	}
}

func (s *Service) ensureWebsocketGateway() {
	if s == nil {
		return
	}
	if s.wsGateway != nil {
		return
	}
	opts := wsrelay.Options{
		Path:           "/v1/ws",
		OnConnected:    s.wsOnConnected,
		OnDisconnected: s.wsOnDisconnected,
		LogDebugf:      log.Debugf,
		LogInfof:       log.Infof,
		LogWarnf:       log.Warnf,
	}
	s.wsGateway = wsrelay.NewManager(opts)
}

func (s *Service) wsOnConnected(channelID string) {
	if s == nil || channelID == "" {
		return
	}
	if !strings.HasPrefix(strings.ToLower(channelID), "aistudio-") {
		return
	}
	if s.coreManager != nil {
		if existing, ok := s.coreManager.GetByID(channelID); ok && existing != nil {
			if !existing.Disabled && existing.Status == coreauth.StatusActive {
				return
			}
		}
	}
	now := time.Now().UTC()
	auth := &coreauth.Auth{
		ID:         channelID,  // keep channel identifier as ID
		Provider:   "aistudio", // logical provider for switch routing
		Label:      channelID,  // display original channel id
		Status:     coreauth.StatusActive,
		CreatedAt:  now,
		UpdatedAt:  now,
		Attributes: map[string]string{"runtime_only": "true"},
		Metadata:   map[string]any{"email": channelID}, // metadata drives logging and usage tracking
	}
	log.Infof("websocket provider connected: %s", channelID)
	s.emitAuthUpdate(context.Background(), watcher.AuthUpdate{
		Action: watcher.AuthUpdateActionAdd,
		ID:     auth.ID,
		Auth:   auth,
	})
}

func (s *Service) wsOnDisconnected(channelID string, reason error) {
	if s == nil || channelID == "" {
		return
	}
	if reason != nil {
		if strings.Contains(reason.Error(), "replaced by new connection") {
			log.Infof("websocket provider replaced: %s", channelID)
			return
		}
		log.Warnf("websocket provider disconnected: %s (%v)", channelID, reason)
	} else {
		log.Infof("websocket provider disconnected: %s", channelID)
	}
	ctx := context.Background()
	s.emitAuthUpdate(ctx, watcher.AuthUpdate{
		Action: watcher.AuthUpdateActionDelete,
		ID:     channelID,
	})
}

func (s *Service) applyCoreAuthAddOrUpdate(ctx context.Context, auth *coreauth.Auth) {
	if s == nil || s.coreManager == nil || auth == nil || auth.ID == "" {
		return
	}
	auth = auth.Clone()
	s.ensureExecutorsForAuth(auth)

	// IMPORTANT: Update coreManager FIRST, before model registration.
	// This ensures that configuration changes (proxy_url, prefix, etc.) take effect
	// immediately for API calls, rather than waiting for model registration to complete.
	op := "register"
	var err error
	if existing, ok := s.coreManager.GetByID(auth.ID); ok {
		auth.CreatedAt = existing.CreatedAt
		if !existing.Disabled && existing.Status != coreauth.StatusDisabled && !auth.Disabled && auth.Status != coreauth.StatusDisabled {
			auth.LastRefreshedAt = existing.LastRefreshedAt
			auth.NextRefreshAfter = existing.NextRefreshAfter
			if len(auth.ModelStates) == 0 && len(existing.ModelStates) > 0 {
				auth.ModelStates = existing.ModelStates
			}
		}
		op = "update"
		_, err = s.coreManager.Update(ctx, auth)
	} else {
		_, err = s.coreManager.Register(ctx, auth)
	}
	if err != nil {
		log.Errorf("failed to %s auth %s: %v", op, auth.ID, err)
		current, ok := s.coreManager.GetByID(auth.ID)
		if !ok || current.Disabled {
			GlobalModelRegistry().UnregisterClient(auth.ID)
			return
		}
		auth = current
	}

	// Register models after auth is updated in coreManager.
	// This operation may block on network calls, but the auth configuration
	// is already effective at this point.
	s.registerModelsForAuth(auth)
	s.coreManager.ReconcileRegistryModelStates(ctx, auth.ID)

	// Refresh the scheduler entry so that the auth's supportedModelSet is rebuilt
	// from the now-populated global model registry. Without this, newly added auths
	// have an empty supportedModelSet (because Register/Update upserts into the
	// scheduler before registerModelsForAuth runs) and are invisible to the scheduler.
	s.coreManager.RefreshSchedulerEntry(auth.ID)
}

func (s *Service) applyCoreAuthRemoval(ctx context.Context, id string) {
	if s == nil || id == "" {
		return
	}
	if s.coreManager == nil {
		return
	}
	GlobalModelRegistry().UnregisterClient(id)
	if existing, ok := s.coreManager.GetByID(id); ok && existing != nil {
		existing.Disabled = true
		existing.Status = coreauth.StatusDisabled
		if _, err := s.coreManager.Update(ctx, existing); err != nil {
			log.Errorf("failed to disable auth %s: %v", id, err)
		}
		if strings.EqualFold(strings.TrimSpace(existing.Provider), "codex") {
			executor.CloseCodexWebsocketSessionsForAuthID(existing.ID, "auth_removed")
			s.ensureExecutorsForAuth(existing)
		}
	}
}

func (s *Service) applyRetryConfig(cfg *config.Config) {
	if s == nil || s.coreManager == nil || cfg == nil {
		return
	}
	maxInterval := time.Duration(cfg.MaxRetryInterval) * time.Second
	s.coreManager.SetRetryConfig(cfg.RequestRetry, maxInterval, cfg.MaxRetryCredentials)
}

func openAICompatInfoFromAuth(a *coreauth.Auth) (providerKey string, compatName string, ok bool) {
	if a == nil {
		return "", "", false
	}
	if len(a.Attributes) > 0 {
		providerKey = strings.TrimSpace(a.Attributes["provider_key"])
		compatName = strings.TrimSpace(a.Attributes["compat_name"])
		if compatName != "" {
			if providerKey == "" {
				providerKey = compatName
			}
			return strings.ToLower(providerKey), compatName, true
		}
	}
	if strings.EqualFold(strings.TrimSpace(a.Provider), "openai-compatibility") {
		return "openai-compatibility", strings.TrimSpace(a.Label), true
	}
	return "", "", false
}

func (s *Service) ensureExecutorsForAuth(a *coreauth.Auth) {
	s.ensureExecutorsForAuthWithMode(a, false)
}

func (s *Service) ensureExecutorsForAuthWithMode(a *coreauth.Auth, forceReplace bool) {
	if s == nil || s.coreManager == nil || a == nil {
		return
	}
	if strings.EqualFold(strings.TrimSpace(a.Provider), "codex") {
		if !forceReplace {
			existingExecutor, hasExecutor := s.coreManager.Executor("codex")
			if hasExecutor {
				_, isCodexAutoExecutor := existingExecutor.(*executor.CodexAutoExecutor)
				if isCodexAutoExecutor {
					return
				}
			}
		}
		s.coreManager.RegisterExecutor(executor.NewCodexAutoExecutor(s.cfg))
		return
	}
	// Skip disabled auth entries when (re)binding executors.
	// Disabled auths can linger during config reloads (e.g., removed OpenAI-compat entries)
	// and must not override active provider executors.
	if a.Disabled {
		return
	}
	if compatProviderKey, _, isCompat := openAICompatInfoFromAuth(a); isCompat {
		if compatProviderKey == "" {
			compatProviderKey = strings.ToLower(strings.TrimSpace(a.Provider))
		}
		if compatProviderKey == "" {
			compatProviderKey = "openai-compatibility"
		}
		s.coreManager.RegisterExecutor(executor.NewOpenAICompatExecutor(compatProviderKey, s.cfg))
		return
	}
	switch strings.ToLower(a.Provider) {
	case "gemini":
		s.coreManager.RegisterExecutor(executor.NewGeminiExecutor(s.cfg))
	case "vertex":
		s.coreManager.RegisterExecutor(executor.NewGeminiVertexExecutor(s.cfg))
	case "gemini-cli":
		s.coreManager.RegisterExecutor(executor.NewGeminiCLIExecutor(s.cfg))
	case "aistudio":
		if s.wsGateway != nil {
			s.coreManager.RegisterExecutor(executor.NewAIStudioExecutor(s.cfg, a.ID, s.wsGateway))
		}
		return
	case "antigravity":
		s.coreManager.RegisterExecutor(executor.NewAntigravityExecutor(s.cfg))
	case "claude":
		s.coreManager.RegisterExecutor(executor.NewClaudeExecutor(s.cfg))
	case "kimi":
		s.coreManager.RegisterExecutor(executor.NewKimiExecutor(s.cfg))
	case "kiro":
		s.coreManager.RegisterExecutor(executor.NewKiroExecutor(s.cfg))
	case "kilo":
		s.coreManager.RegisterExecutor(executor.NewKiloExecutor(s.cfg))
	case "cursor":
		s.coreManager.RegisterExecutor(executor.NewCursorExecutor(s.cfg))
	case "github-copilot":
		s.coreManager.RegisterExecutor(executor.NewGitHubCopilotExecutor(s.cfg))
	case "codebuddy":
		s.coreManager.RegisterExecutor(executor.NewCodeBuddyExecutor(s.cfg))
	case "gitlab":
		s.coreManager.RegisterExecutor(executor.NewGitLabExecutor(s.cfg))
	default:
		providerKey := strings.ToLower(strings.TrimSpace(a.Provider))
		if providerKey == "" {
			providerKey = "openai-compatibility"
		}
		s.coreManager.RegisterExecutor(executor.NewOpenAICompatExecutor(providerKey, s.cfg))
	}
}

func (s *Service) registerResolvedModelsForAuth(a *coreauth.Auth, providerKey string, models []*ModelInfo) {
	if a == nil || a.ID == "" {
		return
	}
	if len(models) == 0 {
		GlobalModelRegistry().UnregisterClient(a.ID)
		return
	}
	GlobalModelRegistry().RegisterClient(a.ID, providerKey, models)
}

// rebindExecutors refreshes provider executors so they observe the latest configuration.
func (s *Service) rebindExecutors() {
	if s == nil || s.coreManager == nil {
		return
	}
	auths := s.coreManager.List()
	reboundCodex := false
	for _, auth := range auths {
		if auth != nil && strings.EqualFold(strings.TrimSpace(auth.Provider), "codex") {
			if reboundCodex {
				continue
			}
			reboundCodex = true
		}
		s.ensureExecutorsForAuthWithMode(auth, true)
	}
}

// Run starts the service and blocks until the context is cancelled or the server stops.
// It initializes all components including authentication, file watching, HTTP server,
// and starts processing requests. The method blocks until the context is cancelled.
//
// Parameters:
//   - ctx: The context for controlling the service lifecycle
//
// Returns:
//   - error: An error if the service fails to start or run
func (s *Service) Run(ctx context.Context) error {
	if s == nil {
		return fmt.Errorf("cliproxy: service is nil")
	}
	if ctx == nil {
		ctx = context.Background()
	}

	coreusage.StartDefault(ctx)

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer shutdownCancel()
	defer func() {
		if err := s.Shutdown(shutdownCtx); err != nil {
			log.Errorf("service shutdown returned error: %v", err)
		}
	}()

	if err := s.ensureAuthDir(); err != nil {
		return err
	}

	s.applyRetryConfig(s.cfg)

	if s.coreManager != nil {
		if s.cfg != nil && s.cfg.PoolManager.Size > 0 {
			s.coreManager.SetHook(&serviceAuthHook{service: s})
		}
		if errLoad := s.coreManager.Load(ctx); errLoad != nil {
			log.Warnf("failed to load auth store: %v", errLoad)
		}
		s.seedLoadedCoreAuthModels()
	}

	tokenResult, err := s.tokenProvider.Load(ctx, s.cfg)
	if err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	if tokenResult == nil {
		tokenResult = &TokenClientResult{}
	}

	apiKeyResult, err := s.apiKeyProvider.Load(ctx, s.cfg)
	if err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	if apiKeyResult == nil {
		apiKeyResult = &APIKeyClientResult{}
	}

	// legacy clients removed; no caches to refresh

	// handlers no longer depend on legacy clients; pass nil slice initially
	s.server = api.NewServer(s.cfg, s.coreManager, s.accessManager, s.configPath, s.serverOptions...)
	if s.server != nil {
		s.server.SetPoolStatisticsProvider(s.poolMetricsSnapshot)
		s.server.SetSelectedAuthObserver(s.handleSelectedAuth)
	}

	if s.authManager == nil {
		s.authManager = newDefaultAuthManager()
	}

	s.ensureWebsocketGateway()
	if s.server != nil && s.wsGateway != nil {
		s.server.AttachWebsocketRoute(s.wsGateway.Path(), s.wsGateway.Handler())
		s.server.SetWebsocketAuthChangeHandler(func(oldEnabled, newEnabled bool) {
			if oldEnabled == newEnabled {
				return
			}
			if !oldEnabled && newEnabled {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				if errStop := s.wsGateway.Stop(ctx); errStop != nil {
					log.Warnf("failed to reset websocket connections after ws-auth change %t -> %t: %v", oldEnabled, newEnabled, errStop)
					return
				}
				log.Debugf("ws-auth enabled; existing websocket sessions terminated to enforce authentication")
				return
			}
			log.Debugf("ws-auth disabled; existing websocket sessions remain connected")
		})
	}

	if s.hooks.OnBeforeStart != nil {
		s.hooks.OnBeforeStart(s.cfg)
	}

	// Register callback for startup and periodic model catalog refresh.
	// When remote model definitions change, re-register models for affected providers.
	// This intentionally rebuilds per-auth model availability from the latest catalog
	// snapshot instead of preserving prior registry suppression state.
	registry.SetModelRefreshCallback(func(changedProviders []string) {
		if s == nil || s.coreManager == nil || len(changedProviders) == 0 {
			return
		}

		providerSet := make(map[string]bool, len(changedProviders))
		for _, p := range changedProviders {
			providerSet[strings.ToLower(strings.TrimSpace(p))] = true
		}

		auths := s.coreManager.List()
		refreshed := 0
		for _, item := range auths {
			if item == nil || item.ID == "" {
				continue
			}
			auth, ok := s.coreManager.GetByID(item.ID)
			if !ok || auth == nil || auth.Disabled {
				continue
			}
			provider := strings.ToLower(strings.TrimSpace(auth.Provider))
			if !providerSet[provider] {
				continue
			}
			if s.refreshModelRegistrationForAuth(auth) {
				refreshed++
			}
		}

		if refreshed > 0 {
			log.Infof("re-registered models for %d auth(s) due to model catalog changes: %v", refreshed, changedProviders)
		}
	})

	s.serverErr = make(chan error, 1)
	go func() {
		if errStart := s.server.Start(); errStart != nil {
			s.serverErr <- errStart
		} else {
			s.serverErr <- nil
		}
	}()

	time.Sleep(100 * time.Millisecond)
	fmt.Printf("API server started successfully on: %s:%d\n", s.cfg.Host, s.cfg.Port)

	s.applyPprofConfig(s.cfg)

	if s.hooks.OnAfterStart != nil {
		s.hooks.OnAfterStart(s)
	}

	var watcherWrapper *WatcherWrapper
	reloadCallback := func(newCfg *config.Config) {
		previousStrategy := ""
		var previousSessionAffinity bool
		var previousSessionAffinityTTL string
		s.cfgMu.RLock()
		if s.cfg != nil {
			previousStrategy = strings.ToLower(strings.TrimSpace(s.cfg.Routing.Strategy))
			previousSessionAffinity = s.cfg.Routing.ClaudeCodeSessionAffinity || s.cfg.Routing.SessionAffinity
			previousSessionAffinityTTL = s.cfg.Routing.SessionAffinityTTL
		}
		s.cfgMu.RUnlock()

		if newCfg == nil {
			s.cfgMu.RLock()
			newCfg = s.cfg
			s.cfgMu.RUnlock()
		}
		if newCfg == nil {
			return
		}

		nextStrategy := strings.ToLower(strings.TrimSpace(newCfg.Routing.Strategy))
		normalizeStrategy := func(strategy string) string {
			switch strategy {
			case "fill-first", "fillfirst", "ff":
				return "fill-first"
			default:
				return "round-robin"
			}
		}
		previousStrategy = normalizeStrategy(previousStrategy)
		nextStrategy = normalizeStrategy(nextStrategy)

		nextSessionAffinity := newCfg.Routing.ClaudeCodeSessionAffinity || newCfg.Routing.SessionAffinity
		nextSessionAffinityTTL := newCfg.Routing.SessionAffinityTTL

		selectorChanged := previousStrategy != nextStrategy ||
			previousSessionAffinity != nextSessionAffinity ||
			previousSessionAffinityTTL != nextSessionAffinityTTL

		if s.coreManager != nil && selectorChanged {
			var selector coreauth.Selector
			switch nextStrategy {
			case "fill-first":
				selector = &coreauth.FillFirstSelector{}
			default:
				selector = &coreauth.RoundRobinSelector{}
			}

			if nextSessionAffinity {
				ttl := time.Hour
				if ttlStr := strings.TrimSpace(nextSessionAffinityTTL); ttlStr != "" {
					if parsed, err := time.ParseDuration(ttlStr); err == nil && parsed > 0 {
						ttl = parsed
					}
				}
				selector = coreauth.NewSessionAffinitySelectorWithConfig(coreauth.SessionAffinityConfig{
					Fallback: selector,
					TTL:      ttl,
				})
			}

			s.coreManager.SetSelector(selector)
		}

		// Propagate latency-aware scheduling config.
		if s.coreManager != nil {
			s.coreManager.SetLatencyAware(newCfg.Routing.LatencyAware)
		}

		s.applyRetryConfig(newCfg)
		s.applyPprofConfig(newCfg)
		if s.server != nil {
			s.server.UpdateClients(newCfg)
		}
		s.cfgMu.Lock()
		s.cfg = newCfg
		s.cfgMu.Unlock()
		if s.coreManager != nil {
			s.coreManager.SetConfig(newCfg)
			s.coreManager.SetOAuthModelAlias(newCfg.OAuthModelAlias)
		}
		s.rebindExecutors()
	}

	watcherWrapper, err = s.watcherFactory(s.configPath, s.cfg.AuthDir, reloadCallback)
	if err != nil {
		return fmt.Errorf("cliproxy: failed to create watcher: %w", err)
	}
	s.watcher = watcherWrapper
	s.ensureAuthUpdateQueue(ctx)
	if s.authUpdates != nil {
		watcherWrapper.SetAuthUpdateQueue(s.authUpdates)
	}
	watcherWrapper.SetConfig(s.cfg)
	s.bootstrapAuthSnapshot(ctx, watcherWrapper)

	// 方案 A: 连接 Kiro 后台刷新器回调到 Watcher
	// 当后台刷新器成功刷新 token 后，立即通知 Watcher 更新内存中的 Auth 对象
	// 这解决了后台刷新与内存 Auth 对象之间的时间差问题
	kiroauth.GetRefreshManager().SetOnTokenRefreshed(func(tokenID string, tokenData *kiroauth.KiroTokenData) {
		if tokenData == nil || watcherWrapper == nil {
			return
		}
		log.Debugf("kiro refresh callback: notifying watcher for token %s", tokenID)
		watcherWrapper.NotifyTokenRefreshed(tokenID, tokenData.AccessToken, tokenData.RefreshToken, tokenData.ExpiresAt)
	})
	log.Debug("kiro: connected background refresh callback to watcher")

	watcherCtx, watcherCancel := context.WithCancel(context.Background())
	s.watcherCancel = watcherCancel
	if err = watcherWrapper.Start(watcherCtx); err != nil {
		return fmt.Errorf("cliproxy: failed to start watcher: %w", err)
	}
	log.Info("file watcher started for config and auth directory changes")

	// Prefer core auth manager auto refresh if available.
	if s.coreManager != nil {
		interval := 15 * time.Minute
		s.coreManager.StartAutoRefresh(context.Background(), interval)
		log.Infof("core auth auto-refresh started (interval=%s)", interval)
	}
	s.startPoolProbeLoops(ctx)

	select {
	case <-ctx.Done():
		log.Debug("service context cancelled, shutting down...")
		return ctx.Err()
	case err = <-s.serverErr:
		return err
	}
}

// Shutdown gracefully stops background workers and the HTTP server.
// It ensures all resources are properly cleaned up and connections are closed.
// The shutdown is idempotent and can be called multiple times safely.
//
// Parameters:
//   - ctx: The context for controlling the shutdown timeout
//
// Returns:
//   - error: An error if shutdown fails
func (s *Service) Shutdown(ctx context.Context) error {
	if s == nil {
		return nil
	}
	var shutdownErr error
	s.shutdownOnce.Do(func() {
		if ctx == nil {
			ctx = context.Background()
		}

		// legacy refresh loop removed; only stopping core auth manager below

		if s.watcherCancel != nil {
			s.watcherCancel()
		}
		if s.coreManager != nil {
			s.coreManager.StopAutoRefresh()
		}
		if s.watcher != nil {
			if err := s.watcher.Stop(); err != nil {
				log.Errorf("failed to stop file watcher: %v", err)
				shutdownErr = err
			}
		}
		if s.wsGateway != nil {
			if err := s.wsGateway.Stop(ctx); err != nil {
				log.Errorf("failed to stop websocket gateway: %v", err)
				if shutdownErr == nil {
					shutdownErr = err
				}
			}
		}
		if s.authQueueStop != nil {
			s.authQueueStop()
			s.authQueueStop = nil
		}
		if s.poolProbeStop != nil {
			s.poolProbeStop()
			s.poolProbeStop = nil
		}
		if s.poolRebalanceStop != nil {
			s.poolRebalanceStop()
			s.poolRebalanceStop = nil
		}

		if errShutdownPprof := s.shutdownPprof(ctx); errShutdownPprof != nil {
			log.Errorf("failed to stop pprof server: %v", errShutdownPprof)
			if shutdownErr == nil {
				shutdownErr = errShutdownPprof
			}
		}

		// no legacy clients to persist

		if s.server != nil {
			shutdownCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()
			if err := s.server.Stop(shutdownCtx); err != nil {
				log.Errorf("error stopping API server: %v", err)
				if shutdownErr == nil {
					shutdownErr = err
				}
			}
		}

		coreusage.StopDefault()
	})
	return shutdownErr
}

func (s *Service) ensureAuthDir() error {
	info, err := os.Stat(s.cfg.AuthDir)
	if err != nil {
		if os.IsNotExist(err) {
			if mkErr := os.MkdirAll(s.cfg.AuthDir, 0o755); mkErr != nil {
				return fmt.Errorf("cliproxy: failed to create auth directory %s: %w", s.cfg.AuthDir, mkErr)
			}
			log.Infof("created missing auth directory: %s", s.cfg.AuthDir)
			return nil
		}
		return fmt.Errorf("cliproxy: error checking auth directory %s: %w", s.cfg.AuthDir, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("cliproxy: auth path exists but is not a directory: %s", s.cfg.AuthDir)
	}
	return nil
}

// registerModelsForAuth (re)binds provider models in the global registry using the core auth ID as client identifier.
func (s *Service) registerModelsForAuth(a *coreauth.Auth) {
	if a == nil || a.ID == "" {
		return
	}
	if a.Disabled {
		GlobalModelRegistry().UnregisterClient(a.ID)
		return
	}
	authKind := strings.ToLower(strings.TrimSpace(a.Attributes["auth_kind"]))
	if authKind == "" {
		if kind, _ := a.AccountInfo(); strings.EqualFold(kind, "api_key") {
			authKind = "apikey"
		}
	}
	if a.Attributes != nil {
		if v := strings.TrimSpace(a.Attributes["gemini_virtual_primary"]); strings.EqualFold(v, "true") {
			GlobalModelRegistry().UnregisterClient(a.ID)
			return
		}
	}
	// Unregister legacy client ID (if present) to avoid double counting
	if a.Runtime != nil {
		if idGetter, ok := a.Runtime.(interface{ GetClientID() string }); ok {
			if rid := idGetter.GetClientID(); rid != "" && rid != a.ID {
				GlobalModelRegistry().UnregisterClient(rid)
			}
		}
	}
	provider := strings.ToLower(strings.TrimSpace(a.Provider))
	compatProviderKey, compatDisplayName, compatDetected := openAICompatInfoFromAuth(a)
	if compatDetected {
		provider = "openai-compatibility"
	}
	excluded := s.oauthExcludedModels(provider, authKind)
	// The synthesizer pre-merges per-account and global exclusions into the "excluded_models" attribute.
	// If this attribute is present, it represents the complete list of exclusions and overrides the global config.
	if a.Attributes != nil {
		if val, ok := a.Attributes["excluded_models"]; ok && strings.TrimSpace(val) != "" {
			excluded = strings.Split(val, ",")
		}
	}
	var models []*ModelInfo
	switch provider {
	case "gemini":
		models = registry.GetGeminiModels()
		if entry := s.resolveConfigGeminiKey(a); entry != nil {
			if len(entry.Models) > 0 {
				models = buildGeminiConfigModels(entry)
			}
			if authKind == "apikey" {
				excluded = entry.ExcludedModels
			}
		}
		models = applyExcludedModels(models, excluded)
	case "vertex":
		// Vertex AI Gemini supports the same model identifiers as Gemini.
		models = registry.GetGeminiVertexModels()
		if entry := s.resolveConfigVertexCompatKey(a); entry != nil {
			if len(entry.Models) > 0 {
				models = buildVertexCompatConfigModels(entry)
			}
			if authKind == "apikey" {
				excluded = entry.ExcludedModels
			}
		}
		models = applyExcludedModels(models, excluded)
	case "gemini-cli":
		models = registry.GetGeminiCLIModels()
		models = applyExcludedModels(models, excluded)
	case "aistudio":
		models = registry.GetAIStudioModels()
		models = applyExcludedModels(models, excluded)
	case "antigravity":
		models = registry.GetAntigravityModels()
		models = applyExcludedModels(models, excluded)
	case "claude":
		models = registry.GetClaudeModels()
		if entry := s.resolveConfigClaudeKey(a); entry != nil {
			if len(entry.Models) > 0 {
				models = buildClaudeConfigModels(entry)
			}
			if authKind == "apikey" {
				excluded = entry.ExcludedModels
			}
		}
		models = applyExcludedModels(models, excluded)
	case "codex":
		codexPlanType := ""
		if a.Attributes != nil {
			codexPlanType = strings.TrimSpace(a.Attributes["plan_type"])
		}
		switch strings.ToLower(codexPlanType) {
		case "pro":
			models = registry.GetCodexProModels()
		case "plus":
			models = registry.GetCodexPlusModels()
		case "team", "business", "go":
			models = registry.GetCodexTeamModels()
		case "free":
			models = registry.GetCodexFreeModels()
		default:
			models = registry.GetCodexProModels()
		}
		if entry := s.resolveConfigCodexKey(a); entry != nil {
			if len(entry.Models) > 0 {
				models = buildCodexConfigModels(entry)
			}
			if authKind == "apikey" {
				excluded = entry.ExcludedModels
			}
		}
		models = applyExcludedModels(models, excluded)
	case "kimi":
		models = registry.GetKimiModels()
		models = applyExcludedModels(models, excluded)
	case "cursor":
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		models = executor.FetchCursorModels(ctx, a, s.cfg)
		models = applyExcludedModels(models, excluded)
	case "github-copilot":
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		models = executor.FetchGitHubCopilotModels(ctx, a, s.cfg)
		models = applyExcludedModels(models, excluded)
	case "kiro":
		models = s.fetchKiroModels(a)
		models = applyExcludedModels(models, excluded)
	case "kilo":
		models = executor.FetchKiloModels(context.Background(), a, s.cfg)
		models = applyExcludedModels(models, excluded)
	case "gitlab":
		models = executor.GitLabModelsFromAuth(a)
		models = applyExcludedModels(models, excluded)
	case "codebuddy":
		models = registry.GetCodeBuddyModels()
		models = applyExcludedModels(models, excluded)
	default:
		// Handle OpenAI-compatibility providers by name using config
		if s.cfg != nil {
			providerKey := provider
			compatName := strings.TrimSpace(a.Provider)
			isCompatAuth := false
			if compatDetected {
				if compatProviderKey != "" {
					providerKey = compatProviderKey
				}
				if compatDisplayName != "" {
					compatName = compatDisplayName
				}
				isCompatAuth = true
			}
			if strings.EqualFold(providerKey, "openai-compatibility") {
				isCompatAuth = true
				if a.Attributes != nil {
					if v := strings.TrimSpace(a.Attributes["compat_name"]); v != "" {
						compatName = v
					}
					if v := strings.TrimSpace(a.Attributes["provider_key"]); v != "" {
						providerKey = strings.ToLower(v)
						isCompatAuth = true
					}
				}
				if providerKey == "openai-compatibility" && compatName != "" {
					providerKey = strings.ToLower(compatName)
				}
			} else if a.Attributes != nil {
				if v := strings.TrimSpace(a.Attributes["compat_name"]); v != "" {
					compatName = v
					isCompatAuth = true
				}
				if v := strings.TrimSpace(a.Attributes["provider_key"]); v != "" {
					providerKey = strings.ToLower(v)
					isCompatAuth = true
				}
			}
			for i := range s.cfg.OpenAICompatibility {
				compat := &s.cfg.OpenAICompatibility[i]
				if strings.EqualFold(compat.Name, compatName) {
					isCompatAuth = true
					// Convert compatibility models to registry models
					ms := make([]*ModelInfo, 0, len(compat.Models))
					seenModelIDs := make(map[string]struct{}, len(compat.Models)*2)
					for j := range compat.Models {
						m := compat.Models[j]
						// Use alias as model ID, fallback to name if alias is empty
						modelID := m.Alias
						if modelID == "" {
							modelID = m.Name
						}
						thinking := m.Thinking
						if thinking == nil {
							thinking = &registry.ThinkingSupport{Levels: []string{"low", "medium", "high"}}
						}
						for _, candidateID := range []string{
							strings.TrimSpace(modelID),
							internalconfig.ImplicitOpenAICompatAlias(m.Name, m.Alias),
						} {
							if candidateID == "" {
								continue
							}
							key := strings.ToLower(candidateID)
							if _, exists := seenModelIDs[key]; exists {
								continue
							}
							seenModelIDs[key] = struct{}{}
							ms = append(ms, &ModelInfo{
								ID:          candidateID,
								Object:      "model",
								Created:     time.Now().Unix(),
								OwnedBy:     compat.Name,
								Type:        "openai-compatibility",
								DisplayName: candidateID,
								UserDefined: true,
								Thinking:    thinking,
							})
						}
					}
					// Register and return
					if len(ms) > 0 {
						if providerKey == "" {
							providerKey = "openai-compatibility"
						}
						s.registerResolvedModelsForAuth(a, providerKey, applyModelPrefixes(ms, a.Prefix, s.cfg.ForceModelPrefix))
					} else {
						// Ensure stale registrations are cleared when model list becomes empty.
						GlobalModelRegistry().UnregisterClient(a.ID)
					}
					return
				}
			}
			if isCompatAuth {
				// No matching provider found or models removed entirely; drop any prior registration.
				GlobalModelRegistry().UnregisterClient(a.ID)
				return
			}
		}
	}
	models = applyOAuthModelAlias(s.cfg, provider, authKind, models)
	if len(models) > 0 {
		key := provider
		if key == "" {
			key = strings.ToLower(strings.TrimSpace(a.Provider))
		}
		s.registerResolvedModelsForAuth(a, key, applyModelPrefixes(models, a.Prefix, s.cfg != nil && s.cfg.ForceModelPrefix))
		return
	}

	GlobalModelRegistry().UnregisterClient(a.ID)
}

// refreshModelRegistrationForAuth re-applies the latest model registration for
// one auth and reconciles any concurrent auth changes that race with the
// refresh. Callers are expected to pre-filter provider membership.
//
// Re-registration is deliberate: registry cooldown/suspension state is treated
// as part of the previous registration snapshot and is cleared when the auth is
// rebound to the refreshed model catalog.
func (s *Service) refreshModelRegistrationForAuth(current *coreauth.Auth) bool {
	if s == nil || s.coreManager == nil || current == nil || current.ID == "" {
		return false
	}

	if !current.Disabled {
		s.ensureExecutorsForAuth(current)
	}
	s.registerModelsForAuth(current)
	s.coreManager.ReconcileRegistryModelStates(context.Background(), current.ID)

	latest, ok := s.latestAuthForModelRegistration(current.ID)
	if !ok || latest.Disabled {
		GlobalModelRegistry().UnregisterClient(current.ID)
		s.coreManager.RefreshSchedulerEntry(current.ID)
		return false
	}

	// Re-apply the latest auth snapshot so concurrent auth updates cannot leave
	// stale model registrations behind. This may duplicate registration work when
	// no auth fields changed, but keeps the refresh path simple and correct.
	s.ensureExecutorsForAuth(latest)
	s.registerModelsForAuth(latest)
	s.coreManager.ReconcileRegistryModelStates(context.Background(), latest.ID)
	s.coreManager.RefreshSchedulerEntry(current.ID)
	return true
}

// latestAuthForModelRegistration returns the latest auth snapshot regardless of
// provider membership. Callers use this after a registration attempt to restore
// whichever state currently owns the client ID in the global registry.
func (s *Service) latestAuthForModelRegistration(authID string) (*coreauth.Auth, bool) {
	if s == nil || s.coreManager == nil || authID == "" {
		return nil, false
	}
	auth, ok := s.coreManager.GetByID(authID)
	if !ok || auth == nil || auth.ID == "" {
		return nil, false
	}
	return auth, true
}

func (s *Service) resolveConfigClaudeKey(auth *coreauth.Auth) *config.ClaudeKey {
	if auth == nil || s.cfg == nil {
		return nil
	}
	var attrKey, attrBase string
	if auth.Attributes != nil {
		attrKey = strings.TrimSpace(auth.Attributes["api_key"])
		attrBase = strings.TrimSpace(auth.Attributes["base_url"])
	}
	for i := range s.cfg.ClaudeKey {
		entry := &s.cfg.ClaudeKey[i]
		cfgKey := strings.TrimSpace(entry.GetAPIKey())
		cfgBase := strings.TrimSpace(entry.BaseURL)
		if attrKey != "" && attrBase != "" {
			if strings.EqualFold(cfgKey, attrKey) && strings.EqualFold(cfgBase, attrBase) {
				return entry
			}
			continue
		}
		if attrKey != "" && strings.EqualFold(cfgKey, attrKey) {
			if cfgBase == "" || strings.EqualFold(cfgBase, attrBase) {
				return entry
			}
		}
		if attrKey == "" && attrBase != "" && strings.EqualFold(cfgBase, attrBase) {
			return entry
		}
	}
	if attrKey != "" {
		for i := range s.cfg.ClaudeKey {
			entry := &s.cfg.ClaudeKey[i]
			if strings.EqualFold(strings.TrimSpace(entry.GetAPIKey()), attrKey) {
				return entry
			}
		}
	}
	return nil
}

func (s *Service) resolveConfigGeminiKey(auth *coreauth.Auth) *config.GeminiKey {
	if auth == nil || s.cfg == nil {
		return nil
	}
	var attrKey, attrBase string
	if auth.Attributes != nil {
		attrKey = strings.TrimSpace(auth.Attributes["api_key"])
		attrBase = strings.TrimSpace(auth.Attributes["base_url"])
	}
	for i := range s.cfg.GeminiKey {
		entry := &s.cfg.GeminiKey[i]
		cfgKey := strings.TrimSpace(entry.APIKey)
		cfgBase := strings.TrimSpace(entry.BaseURL)
		if attrKey != "" && strings.EqualFold(cfgKey, attrKey) {
			if cfgBase == "" || strings.EqualFold(cfgBase, attrBase) {
				return entry
			}
			continue
		}
		if attrKey == "" && attrBase != "" && strings.EqualFold(cfgBase, attrBase) {
			return entry
		}
	}
	return nil
}

func (s *Service) resolveConfigVertexCompatKey(auth *coreauth.Auth) *config.VertexCompatKey {
	if auth == nil || s.cfg == nil {
		return nil
	}
	var attrKey, attrBase string
	if auth.Attributes != nil {
		attrKey = strings.TrimSpace(auth.Attributes["api_key"])
		attrBase = strings.TrimSpace(auth.Attributes["base_url"])
	}
	for i := range s.cfg.VertexCompatAPIKey {
		entry := &s.cfg.VertexCompatAPIKey[i]
		cfgKey := strings.TrimSpace(entry.APIKey)
		cfgBase := strings.TrimSpace(entry.BaseURL)
		if attrKey != "" && strings.EqualFold(cfgKey, attrKey) {
			if cfgBase == "" || strings.EqualFold(cfgBase, attrBase) {
				return entry
			}
			continue
		}
		if attrKey == "" && attrBase != "" && strings.EqualFold(cfgBase, attrBase) {
			return entry
		}
	}
	if attrKey != "" {
		for i := range s.cfg.VertexCompatAPIKey {
			entry := &s.cfg.VertexCompatAPIKey[i]
			if strings.EqualFold(strings.TrimSpace(entry.GetAPIKey()), attrKey) {
				return entry
			}
		}
	}
	return nil
}

func (s *Service) resolveConfigCodexKey(auth *coreauth.Auth) *config.CodexKey {
	if auth == nil || s.cfg == nil {
		return nil
	}
	var attrKey, attrBase string
	if auth.Attributes != nil {
		attrKey = strings.TrimSpace(auth.Attributes["api_key"])
		attrBase = strings.TrimSpace(auth.Attributes["base_url"])
	}
	for i := range s.cfg.CodexKey {
		entry := &s.cfg.CodexKey[i]
		cfgKey := strings.TrimSpace(entry.APIKey)
		cfgBase := strings.TrimSpace(entry.BaseURL)
		if attrKey != "" && strings.EqualFold(cfgKey, attrKey) {
			if cfgBase == "" || strings.EqualFold(cfgBase, attrBase) {
				return entry
			}
			continue
		}
		if attrKey == "" && attrBase != "" && strings.EqualFold(cfgBase, attrBase) {
			return entry
		}
	}
	return nil
}

func (s *Service) oauthExcludedModels(provider, authKind string) []string {
	cfg := s.cfg
	if cfg == nil {
		return nil
	}
	authKindKey := strings.ToLower(strings.TrimSpace(authKind))
	providerKey := strings.ToLower(strings.TrimSpace(provider))
	if authKindKey == "apikey" {
		return nil
	}
	return cfg.OAuthExcludedModels[providerKey]
}

func applyExcludedModels(models []*ModelInfo, excluded []string) []*ModelInfo {
	if len(models) == 0 || len(excluded) == 0 {
		return models
	}

	patterns := make([]string, 0, len(excluded))
	for _, item := range excluded {
		if trimmed := strings.TrimSpace(item); trimmed != "" {
			patterns = append(patterns, strings.ToLower(trimmed))
		}
	}
	if len(patterns) == 0 {
		return models
	}

	filtered := make([]*ModelInfo, 0, len(models))
	for _, model := range models {
		if model == nil {
			continue
		}
		modelID := strings.ToLower(strings.TrimSpace(model.ID))
		blocked := false
		for _, pattern := range patterns {
			if matchWildcard(pattern, modelID) {
				blocked = true
				break
			}
		}
		if !blocked {
			filtered = append(filtered, model)
		}
	}
	return filtered
}

func applyModelPrefixes(models []*ModelInfo, prefix string, forceModelPrefix bool) []*ModelInfo {
	trimmedPrefix := strings.TrimSpace(prefix)
	if trimmedPrefix == "" || len(models) == 0 {
		return models
	}

	out := make([]*ModelInfo, 0, len(models)*2)
	seen := make(map[string]struct{}, len(models)*2)

	addModel := func(model *ModelInfo) {
		if model == nil {
			return
		}
		id := strings.TrimSpace(model.ID)
		if id == "" {
			return
		}
		if _, exists := seen[id]; exists {
			return
		}
		seen[id] = struct{}{}
		out = append(out, model)
	}

	for _, model := range models {
		if model == nil {
			continue
		}
		baseID := strings.TrimSpace(model.ID)
		if baseID == "" {
			continue
		}
		if !forceModelPrefix || trimmedPrefix == baseID {
			addModel(model)
		}
		clone := *model
		clone.ID = trimmedPrefix + "/" + baseID
		addModel(&clone)
	}
	return out
}

// matchWildcard performs case-insensitive wildcard matching where '*' matches any substring.
func matchWildcard(pattern, value string) bool {
	if pattern == "" {
		return false
	}

	// Fast path for exact match (no wildcard present).
	if !strings.Contains(pattern, "*") {
		return pattern == value
	}

	parts := strings.Split(pattern, "*")
	// Handle prefix.
	if prefix := parts[0]; prefix != "" {
		if !strings.HasPrefix(value, prefix) {
			return false
		}
		value = value[len(prefix):]
	}

	// Handle suffix.
	if suffix := parts[len(parts)-1]; suffix != "" {
		if !strings.HasSuffix(value, suffix) {
			return false
		}
		value = value[:len(value)-len(suffix)]
	}

	// Handle middle segments in order.
	for i := 1; i < len(parts)-1; i++ {
		segment := parts[i]
		if segment == "" {
			continue
		}
		idx := strings.Index(value, segment)
		if idx < 0 {
			return false
		}
		value = value[idx+len(segment):]
	}

	return true
}

type modelEntry interface {
	GetName() string
	GetAlias() string
}

func buildConfigModels[T modelEntry](models []T, ownedBy, modelType string) []*ModelInfo {
	if len(models) == 0 {
		return nil
	}
	now := time.Now().Unix()
	out := make([]*ModelInfo, 0, len(models))
	seen := make(map[string]struct{}, len(models))
	for i := range models {
		model := models[i]
		name := strings.TrimSpace(model.GetName())
		alias := strings.TrimSpace(model.GetAlias())
		if alias == "" {
			alias = name
		}
		if alias == "" {
			continue
		}
		key := strings.ToLower(alias)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		display := name
		if display == "" {
			display = alias
		}
		info := &ModelInfo{
			ID:          alias,
			Object:      "model",
			Created:     now,
			OwnedBy:     ownedBy,
			Type:        modelType,
			DisplayName: display,
			UserDefined: true,
		}
		if name != "" {
			if upstream := registry.LookupStaticModelInfo(name); upstream != nil && upstream.Thinking != nil {
				info.Thinking = upstream.Thinking
			}
		}
		out = append(out, info)
	}
	return out
}

func buildVertexCompatConfigModels(entry *config.VertexCompatKey) []*ModelInfo {
	if entry == nil {
		return nil
	}
	return buildConfigModels(entry.Models, "google", "vertex")
}

func buildGeminiConfigModels(entry *config.GeminiKey) []*ModelInfo {
	if entry == nil {
		return nil
	}
	return buildConfigModels(entry.Models, "google", "gemini")
}

func buildClaudeConfigModels(entry *config.ClaudeKey) []*ModelInfo {
	if entry == nil {
		return nil
	}
	return buildConfigModels(entry.Models, "anthropic", "claude")
}

func buildCodexConfigModels(entry *config.CodexKey) []*ModelInfo {
	if entry == nil {
		return nil
	}
	return registry.WithCodexBuiltins(buildConfigModels(entry.Models, "openai", "openai"))
}

func rewriteModelInfoName(name, oldID, newID string) string {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return name
	}
	oldID = strings.TrimSpace(oldID)
	newID = strings.TrimSpace(newID)
	if oldID == "" || newID == "" {
		return name
	}
	if strings.EqualFold(oldID, newID) {
		return name
	}
	if strings.EqualFold(trimmed, oldID) {
		return newID
	}
	if strings.HasSuffix(trimmed, "/"+oldID) {
		prefix := strings.TrimSuffix(trimmed, oldID)
		return prefix + newID
	}
	if trimmed == "models/"+oldID {
		return "models/" + newID
	}
	return name
}

func applyOAuthModelAlias(cfg *config.Config, provider, authKind string, models []*ModelInfo) []*ModelInfo {
	if cfg == nil || len(models) == 0 {
		return models
	}
	channel := coreauth.OAuthModelAliasChannel(provider, authKind)
	if channel == "" || len(cfg.OAuthModelAlias) == 0 {
		return models
	}
	aliases := cfg.OAuthModelAlias[channel]
	if len(aliases) == 0 {
		return models
	}

	type aliasEntry struct {
		alias string
		fork  bool
	}

	forward := make(map[string][]aliasEntry, len(aliases))
	for i := range aliases {
		name := strings.TrimSpace(aliases[i].Name)
		alias := strings.TrimSpace(aliases[i].Alias)
		if name == "" || alias == "" {
			continue
		}
		if strings.EqualFold(name, alias) {
			continue
		}
		key := strings.ToLower(name)
		forward[key] = append(forward[key], aliasEntry{alias: alias, fork: aliases[i].Fork})
	}
	if len(forward) == 0 {
		return models
	}

	out := make([]*ModelInfo, 0, len(models))
	seen := make(map[string]struct{}, len(models))
	for _, model := range models {
		if model == nil {
			continue
		}
		id := strings.TrimSpace(model.ID)
		if id == "" {
			continue
		}
		key := strings.ToLower(id)
		entries := forward[key]
		if len(entries) == 0 {
			if _, exists := seen[key]; exists {
				continue
			}
			seen[key] = struct{}{}
			out = append(out, model)
			continue
		}

		keepOriginal := false
		for _, entry := range entries {
			if entry.fork {
				keepOriginal = true
				break
			}
		}
		if keepOriginal {
			if _, exists := seen[key]; !exists {
				seen[key] = struct{}{}
				out = append(out, model)
			}
		}

		addedAlias := false
		for _, entry := range entries {
			mappedID := strings.TrimSpace(entry.alias)
			if mappedID == "" {
				continue
			}
			if strings.EqualFold(mappedID, id) {
				continue
			}
			aliasKey := strings.ToLower(mappedID)
			if _, exists := seen[aliasKey]; exists {
				continue
			}
			seen[aliasKey] = struct{}{}
			clone := *model
			clone.ID = mappedID
			if clone.Name != "" {
				clone.Name = rewriteModelInfoName(clone.Name, id, mappedID)
			}
			out = append(out, &clone)
			addedAlias = true
		}

		if !keepOriginal && !addedAlias {
			if _, exists := seen[key]; exists {
				continue
			}
			seen[key] = struct{}{}
			out = append(out, model)
		}
	}
	return out
}

// fetchKiroModels attempts to dynamically fetch Kiro models from the API.
// If dynamic fetch fails, it falls back to static registry.GetKiroModels().
func (s *Service) fetchKiroModels(a *coreauth.Auth) []*ModelInfo {
	if a == nil {
		log.Debug("kiro: auth is nil, using static models")
		return registry.GetKiroModels()
	}

	// Extract token data from auth attributes
	tokenData := s.extractKiroTokenData(a)
	if tokenData == nil || tokenData.AccessToken == "" {
		log.Debug("kiro: no valid token data in auth, using static models")
		return registry.GetKiroModels()
	}

	// Create KiroAuth instance
	kAuth := kiroauth.NewKiroAuth(s.cfg)
	if kAuth == nil {
		log.Warn("kiro: failed to create KiroAuth instance, using static models")
		return registry.GetKiroModels()
	}

	// Use timeout context for API call
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// Attempt to fetch dynamic models
	apiModels, err := kAuth.ListAvailableModels(ctx, tokenData)
	if err != nil {
		log.Warnf("kiro: failed to fetch dynamic models: %v, using static models", err)
		return registry.GetKiroModels()
	}

	if len(apiModels) == 0 {
		log.Debug("kiro: API returned no models, using static models")
		return registry.GetKiroModels()
	}

	// Convert API models to ModelInfo
	models := convertKiroAPIModels(apiModels)

	// Generate agentic variants
	models = generateKiroAgenticVariants(models)

	log.Infof("kiro: successfully fetched %d models from API (including agentic variants)", len(models))
	return models
}

// extractKiroTokenData extracts KiroTokenData from auth attributes and metadata.
// It supports both config-based tokens (stored in Attributes) and file-based tokens (stored in Metadata).
func (s *Service) extractKiroTokenData(a *coreauth.Auth) *kiroauth.KiroTokenData {
	if a == nil {
		return nil
	}

	var accessToken, profileArn, refreshToken string

	// Priority 1: Try to get from Attributes (config.yaml source)
	if a.Attributes != nil {
		accessToken = strings.TrimSpace(a.Attributes["access_token"])
		profileArn = strings.TrimSpace(a.Attributes["profile_arn"])
		refreshToken = strings.TrimSpace(a.Attributes["refresh_token"])
	}

	// Priority 2: If not found in Attributes, try Metadata (JSON file source)
	if accessToken == "" && a.Metadata != nil {
		if at, ok := a.Metadata["access_token"].(string); ok {
			accessToken = strings.TrimSpace(at)
		}
		if pa, ok := a.Metadata["profile_arn"].(string); ok {
			profileArn = strings.TrimSpace(pa)
		}
		if rt, ok := a.Metadata["refresh_token"].(string); ok {
			refreshToken = strings.TrimSpace(rt)
		}
	}

	// access_token is required
	if accessToken == "" {
		return nil
	}

	return &kiroauth.KiroTokenData{
		AccessToken:  accessToken,
		ProfileArn:   profileArn,
		RefreshToken: refreshToken,
	}
}

// convertKiroAPIModels converts Kiro API models to ModelInfo slice.
func convertKiroAPIModels(apiModels []*kiroauth.KiroModel) []*ModelInfo {
	if len(apiModels) == 0 {
		return nil
	}

	now := time.Now().Unix()
	models := make([]*ModelInfo, 0, len(apiModels))

	for _, m := range apiModels {
		if m == nil || m.ModelID == "" {
			continue
		}

		// Create model ID with kiro- prefix
		modelID := "kiro-" + normalizeKiroModelID(m.ModelID)

		info := &ModelInfo{
			ID:                  modelID,
			Object:              "model",
			Created:             now,
			OwnedBy:             "aws",
			Type:                "kiro",
			DisplayName:         formatKiroDisplayName(m.ModelName, m.RateMultiplier),
			Description:         m.Description,
			ContextLength:       200000,
			MaxCompletionTokens: 64000,
			Thinking:            &registry.ThinkingSupport{Min: 1024, Max: 32000, ZeroAllowed: true, DynamicAllowed: true},
		}

		if m.MaxInputTokens > 0 {
			info.ContextLength = m.MaxInputTokens
		}

		models = append(models, info)
	}

	return models
}

// normalizeKiroModelID normalizes a Kiro model ID by converting dots to dashes
// and removing common prefixes.
func normalizeKiroModelID(modelID string) string {
	// Remove common prefixes
	modelID = strings.TrimPrefix(modelID, "anthropic.")
	modelID = strings.TrimPrefix(modelID, "amazon.")

	// Replace dots with dashes for consistency
	modelID = strings.ReplaceAll(modelID, ".", "-")

	// Replace underscores with dashes
	modelID = strings.ReplaceAll(modelID, "_", "-")

	return strings.ToLower(modelID)
}

// formatKiroDisplayName formats the display name with rate multiplier info.
func formatKiroDisplayName(modelName string, rateMultiplier float64) string {
	if modelName == "" {
		return ""
	}

	displayName := "Kiro " + modelName
	if rateMultiplier > 0 && rateMultiplier != 1.0 {
		displayName += fmt.Sprintf(" (%.1fx credit)", rateMultiplier)
	}

	return displayName
}

// generateKiroAgenticVariants generates agentic variants for Kiro models.
// Agentic variants have optimized system prompts for coding agents.
func generateKiroAgenticVariants(models []*ModelInfo) []*ModelInfo {
	if len(models) == 0 {
		return models
	}

	result := make([]*ModelInfo, 0, len(models)*2)
	result = append(result, models...)

	for _, m := range models {
		if m == nil {
			continue
		}

		// Skip if already an agentic variant
		if strings.HasSuffix(m.ID, "-agentic") {
			continue
		}

		// Skip auto models from agentic variant generation
		if strings.Contains(m.ID, "-auto") {
			continue
		}

		// Create agentic variant
		agentic := &ModelInfo{
			ID:                  m.ID + "-agentic",
			Object:              m.Object,
			Created:             m.Created,
			OwnedBy:             m.OwnedBy,
			Type:                m.Type,
			DisplayName:         m.DisplayName + " (Agentic)",
			Description:         m.Description + " - Optimized for coding agents (chunked writes)",
			ContextLength:       m.ContextLength,
			MaxCompletionTokens: m.MaxCompletionTokens,
		}

		// Copy thinking support if present
		if m.Thinking != nil {
			agentic.Thinking = &registry.ThinkingSupport{
				Min:            m.Thinking.Min,
				Max:            m.Thinking.Max,
				ZeroAllowed:    m.Thinking.ZeroAllowed,
				DynamicAllowed: m.Thinking.DynamicAllowed,
			}
		}

		result = append(result, agentic)
	}

	return result
}
