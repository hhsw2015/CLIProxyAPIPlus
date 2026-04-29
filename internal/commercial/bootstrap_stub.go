//go:build !commercial

package commercial

import (
	"github.com/gin-gonic/gin"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
)

// Layer represents the commercial layer lifecycle.
type Layer struct{}

// Start is a no-op when built without the commercial tag.
func Start(_ *gin.Engine, _ config.CommercialConfig, _ *config.Config, _ string) (*Layer, error) {
	return &Layer{}, nil
}

// AuthMiddleware returns nil when commercial is not enabled.
func (l *Layer) AuthMiddleware() gin.HandlerFunc { return nil }

// StartStatusSync is a no-op when commercial is not enabled.
func (l *Layer) StartStatusSync(_ *coreauth.Manager) {}

// JWTValidator returns nil when commercial is not enabled.
func (l *Layer) JWTValidator() func(string) bool { return nil }

// Stop is a no-op.
func (l *Layer) Stop() {}
