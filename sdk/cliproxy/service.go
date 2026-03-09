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

	// poolCandidates stores auth snapshots known to the pool manager for promotion publishing.
	poolCandidates map[string]*coreauth.Auth

	// poolMetrics stores pool observability counters.
	poolMetrics *PoolMetrics

	poolRebalanceMu    sync.Mutex
	poolRebalanceQueue chan struct{}
	poolEvalMu          sync.Mutex
	poolEvalLast        *poolEvalWindowSnapshot
	poolUnderfilledMu   sync.Mutex
	poolUnderfilledSince time.Time
	poolLowSuccessMu     sync.Mutex
	poolLowSuccessStreak int
	poolLowSuccessWarned bool
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

// RegisterUsagePlugin registers a usage plugin on the global usage manager.
// This allows external code to monitor API usage and token consumption.
//
// Parameters:
//   - plugin: The usage plugin to register
func (s *Service) RegisterUsagePlugin(plugin coreusage.Plugin) {
	coreusage.RegisterPlugin(plugin)
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
		sdkAuth.NewQwenAuthenticator(),
	)
}

type serviceAuthHook struct {
	coreauth.NoopHook
	service *Service
}

func (h *serviceAuthHook) OnAuthDisposition(ctx context.Context, disposition coreauth.AuthDisposition) {
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
	s.handleAuthUpdate(ctx, update)
}

func (s *Service) bootstrapAuthSnapshot(ctx context.Context, watcherWrapper *WatcherWrapper) {
	if s == nil || watcherWrapper == nil {
		return
	}
	if s.cfg != nil && s.cfg.PoolManager.Size > 0 {
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

	sort.Slice(rootAuths, func(i, j int) bool {
		if rootAuths[i] == nil {
			return false
		}
		if rootAuths[j] == nil {
			return true
		}
		return rootAuths[i].ID < rootAuths[j].ID
	})
	sort.Slice(limitAuths, func(i, j int) bool {
		if limitAuths[i] == nil {
			return false
		}
		if limitAuths[j] == nil {
			return true
		}
		return limitAuths[i].ID < limitAuths[j].ID
	})

	if ctx == nil {
		ctx = context.Background()
	}
	ctx = coreauth.WithSkipPersist(ctx)
	s.poolCandidates = make(map[string]*coreauth.Auth, len(rootAuths)+len(limitAuths))
	for _, auth := range rootAuths {
		if auth == nil || strings.TrimSpace(auth.ID) == "" {
			continue
		}
		s.poolCandidates[auth.ID] = auth.Clone()
	}
	for _, auth := range limitAuths {
		if auth == nil || strings.TrimSpace(auth.ID) == "" {
			continue
		}
		s.poolCandidates[auth.ID] = auth.Clone()
	}

	activeCount := 0
	for _, auth := range rootAuths {
		if auth == nil || strings.TrimSpace(auth.ID) == "" {
			continue
		}
		if activeCount < pm.TargetSize() {
			beforeProbe := auth.Clone()
			probedAuth, result := poolProbeAuthFunc(ctx, s.cfg, auth.Clone())
			if probedAuth != nil {
				s.poolCandidates[probedAuth.ID] = probedAuth.Clone()
			}
			s.recordPoolProbe(PoolStateReserve, beforeProbe, probedAuth, result, "")
			if result.Success && probedAuth != nil {
				pm.SetActive(PoolMember{AuthID: probedAuth.ID, Provider: probedAuth.Provider})
				s.applyCoreAuthAddOrUpdate(ctx, probedAuth)
				activeCount++
				continue
			}
			s.handlePoolProbeResult(auth.ID, auth.Provider, result)
			continue
		}
		pm.SetReserve(PoolMember{AuthID: auth.ID, Provider: auth.Provider})
	}
	for _, auth := range limitAuths {
		if auth == nil || strings.TrimSpace(auth.ID) == "" {
			continue
		}
		pm.SetLimit(PoolMember{AuthID: auth.ID, Provider: auth.Provider})
	}
	s.syncPoolActiveToRuntime(ctx)
	log.Infof(
		"pool-manager: startup active target=%d selected=%d reserve=%d limit=%d",
		pm.TargetSize(),
		pm.Snapshot().ActiveCount,
		pm.Snapshot().ReserveCount,
		pm.Snapshot().LimitCount,
	)
	s.logPoolEvaluation()
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
		s.poolManager.SetReserve(member)
		return
	}
	if result.Error != nil {
		switch result.Error.HTTPStatus {
		case http.StatusTooManyRequests, http.StatusPaymentRequired:
			if s.poolMetrics != nil {
				s.poolMetrics.RecordMovedToLimit()
			}
			s.poolManager.SetLimit(member)
			return
		}
	}
	s.poolManager.SetReserve(member)
}

func (s *Service) syncPoolActiveToRuntime(ctx context.Context) {
	if s == nil || s.poolManager == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ctx = coreauth.WithSkipPersist(ctx)

	added, modified, removed := s.poolManager.ActiveDiff(s.publishedActive)
	for _, id := range added {
		if auth := s.poolCandidates[id]; auth != nil {
			log.Infof("pool-publish: add auth=%s provider=%s", id, auth.Provider)
			s.emitAuthUpdate(ctx, watcher.AuthUpdate{
				Action: watcher.AuthUpdateActionAdd,
				ID:     id,
				Auth:   auth.Clone(),
			})
		}
	}
	for _, id := range modified {
		if auth := s.poolCandidates[id]; auth != nil {
			log.Infof("pool-publish: modify auth=%s provider=%s", id, auth.Provider)
			s.emitAuthUpdate(ctx, watcher.AuthUpdate{
				Action: watcher.AuthUpdateActionModify,
				ID:     id,
				Auth:   auth.Clone(),
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

func (s *Service) handleAuthDisposition(ctx context.Context, disposition coreauth.AuthDisposition) {
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
		s.poolManager.Remove(authID)
		delete(s.poolCandidates, authID)
	} else if disposition.MovedToLimit || !disposition.PoolEligible {
		if wasActive {
			reason := "ineligible"
			if disposition.MovedToLimit {
				reason = "limit"
			}
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
			s.poolManager.SetLimit(member)
		} else {
			s.poolManager.Remove(authID)
		}
	} else {
		if member, ok := s.poolManager.LastSeenMember(authID); ok {
			member.Provider = disposition.Provider
			s.poolManager.SetActive(member)
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
			auth := s.poolCandidates[authID]
			if auth == nil {
				s.poolManager.Remove(authID)
				continue
			}
			beforeProbe := auth.Clone()
			probedAuth, result := poolProbeAuthFunc(ctx, s.cfg, auth.Clone())
			if probedAuth != nil {
				s.poolCandidates[probedAuth.ID] = probedAuth.Clone()
			}
			s.recordPoolProbe(PoolStateReserve, beforeProbe, probedAuth, result, "")
			if result.Success && probedAuth != nil {
				s.poolManager.SetActive(PoolMember{AuthID: probedAuth.ID, Provider: probedAuth.Provider})
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
	if coreauth.DispositionSource(ctx) != "request" {
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
		sampled++
		auth, ok := s.coreManager.GetByID(authID)
		if !ok || auth == nil {
			s.poolManager.Remove(authID)
			continue
		}
		probeCtx := coreauth.WithDispositionSource(ctx, "pool_probe")
		beforeProbe := auth.Clone()
		probedAuth, result := poolProbeAuthFunc(probeCtx, s.cfg, auth)
		if probedAuth != nil {
			s.poolCandidates[probedAuth.ID] = probedAuth.Clone()
			if result.Success {
				s.applyCoreAuthAddOrUpdate(probeCtx, probedAuth)
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
			s.coreManager.MarkResult(probeCtx, result)
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
		sampled++
		auth := s.poolCandidates[authID]
		if auth == nil {
			s.poolManager.Remove(authID)
			skipped++
			continue
		}
		beforeProbe := auth.Clone()
		probedAuth, result := poolProbeAuthFunc(coreauth.WithDispositionSource(ctx, "pool_probe"), s.cfg, auth.Clone())
		if probedAuth != nil {
			s.poolCandidates[probedAuth.ID] = probedAuth.Clone()
		}
		s.recordPoolProbe(PoolStateReserve, beforeProbe, probedAuth, result, "")
		reason := ""
		if result.Error != nil {
			reason = result.Error.Code
		}
		s.poolManager.MarkProbe(authID, now, now.Add(interval), result.Success, reason)
		if result.Success {
			healthy++
			continue
		}
		unhealthy++
		s.handlePoolProbeResult(authID, auth.Provider, result)
	}
	if sampled > 0 {
		log.Infof("pool-manager: reserve probe sampled=%d healthy=%d unhealthy=%d skipped=%d", sampled, healthy, unhealthy, skipped)
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
		sampled++
		auth := s.poolCandidates[authID]
		if auth == nil {
			s.poolManager.Remove(authID)
			skipped++
			continue
		}
		beforeProbe := auth.Clone()
		probedAuth, result := poolProbeAuthFunc(coreauth.WithDispositionSource(ctx, "pool_probe"), s.cfg, auth.Clone())
		if probedAuth != nil {
			s.poolCandidates[probedAuth.ID] = probedAuth.Clone()
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
			s.poolManager.SetReserve(PoolMember{AuthID: probedAuth.ID, Provider: probedAuth.Provider})
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
	return s.poolMetrics.Snapshot(s.poolManager.Snapshot(), len(s.publishedActive))
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
	poolSnapshot := s.poolMetrics.Snapshot(s.poolManager.Snapshot(), len(s.publishedActive))
	usageSnapshot := s.usageStatistics().Snapshot()
	successRate := 0.0
	if usageSnapshot.TotalRequests > 0 {
		successRate = float64(usageSnapshot.SuccessCount) * 100 / float64(usageSnapshot.TotalRequests)
	}
	log.Infof(
		"pool-eval: total_requests=%d success=%d failure=%d success_rate=%.2f%% active_size=%d reserve_size=%d limit_size=%d promoted=%d active_removed=%d moved_to_limit=%d deleted=%d",
		usageSnapshot.TotalRequests,
		usageSnapshot.SuccessCount,
		usageSnapshot.FailureCount,
		successRate,
		poolSnapshot.ActiveCount,
		poolSnapshot.ReserveCount,
		poolSnapshot.LimitCount,
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

	poolSnapshot := s.poolMetrics.Snapshot(s.poolManager.Snapshot(), len(s.publishedActive))
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
			"pool-eval: baseline interval=%s total_requests=%d success=%d failure=%d success_rate=%.2f%% active_size=%d reserve_size=%d limit_size=%d",
			poolEvalLogInterval,
			current.TotalRequests,
			current.SuccessCount,
			current.FailureCount,
			successRate,
			poolSnapshot.ActiveCount,
			poolSnapshot.ReserveCount,
			poolSnapshot.LimitCount,
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
		"pool-eval: window=%s total_requests=%d success=%d failure=%d success_rate=%.2f%% active_size=%d reserve_size=%d limit_size=%d",
		window,
		totalRequests,
		successCount,
		failureCount,
		successRate,
		poolSnapshot.ActiveCount,
		poolSnapshot.ReserveCount,
		poolSnapshot.LimitCount,
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
	// Model registration may involve network calls (e.g., FetchAntigravityModels) that
	// could timeout if the new proxy_url is unreachable.
	op := "register"
	var err error
	if existing, ok := s.coreManager.GetByID(auth.ID); ok {
		auth.CreatedAt = existing.CreatedAt
		auth.LastRefreshedAt = existing.LastRefreshedAt
		auth.NextRefreshAfter = existing.NextRefreshAfter
		if len(auth.ModelStates) == 0 && len(existing.ModelStates) > 0 {
			auth.ModelStates = existing.ModelStates
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
	// and must not override active provider executors (such as iFlow OAuth accounts).
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
	case "qwen":
		s.coreManager.RegisterExecutor(executor.NewQwenExecutor(s.cfg))
	case "iflow":
		s.coreManager.RegisterExecutor(executor.NewIFlowExecutor(s.cfg))
	case "kimi":
		s.coreManager.RegisterExecutor(executor.NewKimiExecutor(s.cfg))
	case "kiro":
		s.coreManager.RegisterExecutor(executor.NewKiroExecutor(s.cfg))
	case "kilo":
		s.coreManager.RegisterExecutor(executor.NewKiloExecutor(s.cfg))
	case "github-copilot":
		s.coreManager.RegisterExecutor(executor.NewGitHubCopilotExecutor(s.cfg))
	default:
		providerKey := strings.ToLower(strings.TrimSpace(a.Provider))
		if providerKey == "" {
			providerKey = "openai-compatibility"
		}
		s.coreManager.RegisterExecutor(executor.NewOpenAICompatExecutor(providerKey, s.cfg))
	}
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
		s.cfgMu.RLock()
		if s.cfg != nil {
			previousStrategy = strings.ToLower(strings.TrimSpace(s.cfg.Routing.Strategy))
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
		if s.coreManager != nil && previousStrategy != nextStrategy {
			var selector coreauth.Selector
			switch nextStrategy {
			case "fill-first":
				selector = &coreauth.FillFirstSelector{}
			default:
				selector = &coreauth.RoundRobinSelector{}
			}
			s.coreManager.SetSelector(selector)
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
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		models = executor.FetchAntigravityModels(ctx, a, s.cfg)
		cancel()
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
		models = registry.GetOpenAIModels()
		if entry := s.resolveConfigCodexKey(a); entry != nil {
			if len(entry.Models) > 0 {
				models = buildCodexConfigModels(entry)
			}
			if authKind == "apikey" {
				excluded = entry.ExcludedModels
			}
		}
		models = applyExcludedModels(models, excluded)
	case "qwen":
		models = registry.GetQwenModels()
		models = applyExcludedModels(models, excluded)
	case "iflow":
		models = registry.GetIFlowModels()
		models = applyExcludedModels(models, excluded)
	case "kimi":
		models = registry.GetKimiModels()
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
					for j := range compat.Models {
						m := compat.Models[j]
						// Use alias as model ID, fallback to name if alias is empty
						modelID := m.Alias
						if modelID == "" {
							modelID = m.Name
						}
						ms = append(ms, &ModelInfo{
							ID:          modelID,
							Object:      "model",
							Created:     time.Now().Unix(),
							OwnedBy:     compat.Name,
							Type:        "openai-compatibility",
							DisplayName: modelID,
							UserDefined: true,
						})
					}
					// Register and return
					if len(ms) > 0 {
						if providerKey == "" {
							providerKey = "openai-compatibility"
						}
						GlobalModelRegistry().RegisterClient(a.ID, providerKey, applyModelPrefixes(ms, a.Prefix, s.cfg.ForceModelPrefix))
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
		GlobalModelRegistry().RegisterClient(a.ID, key, applyModelPrefixes(models, a.Prefix, s.cfg != nil && s.cfg.ForceModelPrefix))
		if provider == "antigravity" {
			s.backfillAntigravityModels(a, models)
		}
		return
	}

	GlobalModelRegistry().UnregisterClient(a.ID)
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
		cfgKey := strings.TrimSpace(entry.APIKey)
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
			if strings.EqualFold(strings.TrimSpace(entry.APIKey), attrKey) {
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
			if strings.EqualFold(strings.TrimSpace(entry.APIKey), attrKey) {
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

func (s *Service) backfillAntigravityModels(source *coreauth.Auth, primaryModels []*ModelInfo) {
	if s == nil || s.coreManager == nil || len(primaryModels) == 0 {
		return
	}

	sourceID := ""
	if source != nil {
		sourceID = strings.TrimSpace(source.ID)
	}

	reg := registry.GetGlobalRegistry()
	for _, candidate := range s.coreManager.List() {
		if candidate == nil || candidate.Disabled {
			continue
		}
		candidateID := strings.TrimSpace(candidate.ID)
		if candidateID == "" || candidateID == sourceID {
			continue
		}
		if !strings.EqualFold(strings.TrimSpace(candidate.Provider), "antigravity") {
			continue
		}
		if len(reg.GetModelsForClient(candidateID)) > 0 {
			continue
		}

		authKind := strings.ToLower(strings.TrimSpace(candidate.Attributes["auth_kind"]))
		if authKind == "" {
			if kind, _ := candidate.AccountInfo(); strings.EqualFold(kind, "api_key") {
				authKind = "apikey"
			}
		}
		excluded := s.oauthExcludedModels("antigravity", authKind)
		if candidate.Attributes != nil {
			if val, ok := candidate.Attributes["excluded_models"]; ok && strings.TrimSpace(val) != "" {
				excluded = strings.Split(val, ",")
			}
		}

		models := applyExcludedModels(primaryModels, excluded)
		models = applyOAuthModelAlias(s.cfg, "antigravity", authKind, models)
		if len(models) == 0 {
			continue
		}

		reg.RegisterClient(candidateID, "antigravity", applyModelPrefixes(models, candidate.Prefix, s.cfg != nil && s.cfg.ForceModelPrefix))
		log.Debugf("antigravity models backfilled for auth %s using primary model list", candidateID)
	}
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
	return buildConfigModels(entry.Models, "openai", "openai")
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
