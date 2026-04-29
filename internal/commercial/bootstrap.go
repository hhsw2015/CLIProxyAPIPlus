//go:build commercial

package commercial

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/gin-gonic/gin"

	sub2apiEmbed "github.com/Wei-Shaw/sub2api/pkg/embed"
	sub2api "github.com/Wei-Shaw/sub2api/pkg/types"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	coreusage "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/usage"
)

// Layer represents the commercial layer lifecycle.
type Layer struct {
	cleanup        func()
	authMiddleware gin.HandlerFunc
	syncer         *DataSyncer
	configPath     string
	stopStatusSync chan struct{}
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
		syncer = NewDataSyncer(result.AdminService, cfg.SyncDryRun)
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
		cleanup:        result.Cleanup,
		authMiddleware: WrapAuthMiddleware(result.APIKeyAuthMiddleware),
		syncer:         syncer,
		configPath:     configPath,
	}, nil
}

// AuthMiddleware returns the sub2api API key auth middleware.
func (l *Layer) AuthMiddleware() gin.HandlerFunc {
	if l == nil {
		return nil
	}
	return l.authMiddleware
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
	configSyncTicker := time.NewTicker(5 * time.Minute)
	defer statusTicker.Stop()
	defer configSyncTicker.Stop()
	for {
		select {
		case <-statusTicker.C:
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			l.syncer.SyncAuthStatus(ctx, authMgr.List())
			cancel()
		case <-configSyncTicker.C:
			if l.configPath != "" {
				freshCfg, err := config.LoadConfig(l.configPath)
				if err != nil {
					log.Printf("[commercial] periodic config re-sync: load failed: %v", err)
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
