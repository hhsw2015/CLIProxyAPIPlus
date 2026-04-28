//go:build commercial

package commercial

import (
	"fmt"

	"github.com/gin-gonic/gin"

	sub2apiEmbed "github.com/Wei-Shaw/sub2api/internal/embed"
	coreusage "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/usage"
)

// CommercialConfig holds configuration for the commercial layer.
type CommercialConfig struct {
	Enabled    bool   `yaml:"enabled" json:"enabled"`
	ConfigPath string `yaml:"config-path" json:"config_path"`
}

// Layer represents the commercial layer lifecycle.
type Layer struct {
	cleanup func()
}

// Start initializes the commercial layer and mounts routes on the engine.
func Start(engine *gin.Engine, cfg CommercialConfig) (*Layer, error) {
	if !cfg.Enabled {
		return &Layer{}, nil
	}

	configDir := cfg.ConfigPath
	if configDir == "" {
		configDir = "."
	}

	result, err := sub2apiEmbed.Init(engine, configDir)
	if err != nil {
		return nil, fmt.Errorf("commercial layer init: %w", err)
	}

	coreusage.RegisterPlugin(&BillingPlugin{
		billingSvc:      result.BillingService,
		billingCacheSvc: result.BillingCacheService,
	})

	return &Layer{cleanup: result.Cleanup}, nil
}

// Stop shuts down the commercial layer.
func (l *Layer) Stop() {
	if l != nil && l.cleanup != nil {
		l.cleanup()
	}
}
