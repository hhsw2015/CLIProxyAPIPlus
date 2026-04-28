//go:build !commercial

package commercial

import (
	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
)

// Layer represents the commercial layer lifecycle.
type Layer struct{}

// Start is a no-op when built without the commercial tag.
func Start(_ *gin.Engine, _ config.CommercialConfig) (*Layer, error) {
	return &Layer{}, nil
}

// AuthMiddleware returns nil when commercial is not enabled.
func (l *Layer) AuthMiddleware() gin.HandlerFunc { return nil }

// Stop is a no-op.
func (l *Layer) Stop() {}
