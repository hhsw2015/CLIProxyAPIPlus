//go:build commercial

package commercial

import (
	"fmt"

	"github.com/gin-gonic/gin"

	sub2apiEmbed "github.com/Wei-Shaw/sub2api/internal/embed"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	coreusage "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/usage"
)

// Layer represents the commercial layer lifecycle.
type Layer struct {
	cleanup        func()
	authMiddleware gin.HandlerFunc
}

// Start initializes the commercial layer and mounts routes on the engine.
func Start(engine *gin.Engine, cfg config.CommercialConfig) (*Layer, error) {
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
		billingSvc:      result.BillingService,
		billingCacheSvc: result.BillingCacheService,
	})

	return &Layer{
		cleanup:        result.Cleanup,
		authMiddleware: WrapAuthMiddleware(result.APIKeyAuthMiddleware),
	}, nil
}

// AuthMiddleware returns the sub2api API key auth middleware.
func (l *Layer) AuthMiddleware() gin.HandlerFunc {
	if l == nil {
		return nil
	}
	return l.authMiddleware
}

// Stop shuts down the commercial layer.
func (l *Layer) Stop() {
	if l != nil && l.cleanup != nil {
		l.cleanup()
	}
}
