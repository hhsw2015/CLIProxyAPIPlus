//go:build commercial

package commercial

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/gin-gonic/gin"

	sub2apiEmbed "github.com/Wei-Shaw/sub2api/pkg/embed"
	sub2api "github.com/Wei-Shaw/sub2api/pkg/types"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	coreusage "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/usage"
)

// Layer represents the commercial layer lifecycle.
type Layer struct {
	cleanup          func()
	authMiddleware   gin.HandlerFunc
	syncer           *DataSyncer
	configPath       string
	stopStatusSync   chan struct{}
	validateAdminJWT func(token string) bool
}

// Start initializes the commercial layer and mounts routes on the engine.
// cpaConfig is used for initial data sync; configPath enables periodic re-sync on hot-reload.
func Start(engine *gin.Engine, cfg config.CommercialConfig, cpaConfig *config.Config, configPath string) (*Layer, error) {
	if !cfg.Enabled {
		return &Layer{}, nil
	}

	if cfg.Sub2API == nil || len(cfg.Sub2API) == 0 {
		return nil, fmt.Errorf("commercial layer enabled but sub2api config is empty")
	}

	result, err := sub2apiEmbed.InitFromMap(engine, cfg.Sub2API)
	if err != nil {
		return nil, fmt.Errorf("commercial layer init: %w", err)
	}

	coreusage.RegisterPlugin(&BillingPlugin{
		billingSvc: result.BillingService,
		usageSvc:   result.UsageService,
	})

	BridgeLogrusToZap(sub2api.GetLogger())

	// Sync CPA provider/auth data to Sub2API database
	var syncer *DataSyncer
	if result.AdminService != nil {
		syncer = NewDataSyncer(result.AdminService, result.ChannelService, cfg.SyncDryRun)
		if cpaConfig != nil {
			syncCtx, syncCancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer syncCancel()
			report := syncer.Sync(syncCtx, cpaConfig)
			if len(report.Errors) > 0 {
				log.Printf("[commercial] data sync completed with %d errors", len(report.Errors))
				for _, e := range report.Errors {
					log.Printf("[commercial] sync error: %s", e)
				}
			}
		}
	}

	return &Layer{
		cleanup:          result.Cleanup,
		authMiddleware:   WrapAuthMiddleware(result.APIKeyAuthMiddleware),
		syncer:           syncer,
		configPath:       configPath,
		validateAdminJWT: result.ValidateAdminJWT,
	}, nil
}

// AuthMiddleware returns the sub2api API key auth middleware.
func (l *Layer) AuthMiddleware() gin.HandlerFunc {
	if l == nil {
		return nil
	}
	return l.authMiddleware
}

// JWTValidator returns a function that validates Sub2API admin JWTs.
func (l *Layer) JWTValidator() func(string) bool {
	if l == nil {
		return nil
	}
	return l.validateAdminJWT
}

// StartStatusSync begins periodic sync of CPA auth status to Sub2API database.
func (l *Layer) StartStatusSync(authMgr *coreauth.Manager) {
	if l == nil || l.syncer == nil || authMgr == nil {
		return
	}
	l.stopStatusSync = make(chan struct{})
	go l.statusSyncLoop(authMgr)
}

func (l *Layer) statusSyncLoop(authMgr *coreauth.Manager) {
	statusTicker := time.NewTicker(60 * time.Second)
	defer statusTicker.Stop()

	// Watch config file for changes (event-driven, not polling)
	var configEvents <-chan struct{}
	if l.configPath != "" {
		configEvents = l.watchConfigFile()
	}

	for {
		select {
		case <-statusTicker.C:
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			l.syncer.SyncAuthStatus(ctx, authMgr.List())
			cancel()
		case <-configEvents:
			if l.configPath != "" {
				freshCfg, err := config.LoadConfig(l.configPath)
				if err != nil {
					log.Printf("[commercial] config change sync: load failed: %v", err)
				} else {
					ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
					l.syncer.Sync(ctx, freshCfg)
					cancel()
				}
			}
		case <-l.stopStatusSync:
			return
		}
	}
}

func (l *Layer) watchConfigFile() <-chan struct{} {
	ch := make(chan struct{}, 1)
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		log.Printf("[commercial] fsnotify init failed, config watch disabled: %v", err)
		return ch
	}
	if err := watcher.Add(l.configPath); err != nil {
		log.Printf("[commercial] fsnotify watch failed for %s: %v", l.configPath, err)
		watcher.Close()
		return ch
	}
	go func() {
		defer watcher.Close()
		var debounce *time.Timer
		for {
			select {
			case event, ok := <-watcher.Events:
				if !ok {
					return
				}
				if event.Op&(fsnotify.Write|fsnotify.Create) == 0 {
					continue
				}
				if debounce != nil {
					debounce.Stop()
				}
				debounce = time.AfterFunc(2*time.Second, func() {
					select {
					case ch <- struct{}{}:
					default:
					}
				})
			case <-watcher.Errors:
			case <-l.stopStatusSync:
				return
			}
		}
	}()
	return ch
}

// Stop shuts down the commercial layer.
func (l *Layer) Stop() {
	if l == nil {
		return
	}
	if l.stopStatusSync != nil {
		close(l.stopStatusSync)
	}
	if l.cleanup != nil {
		l.cleanup()
	}
}
