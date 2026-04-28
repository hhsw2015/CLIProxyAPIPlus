//go:build !commercial

package commercial

import "github.com/gin-gonic/gin"

// CommercialConfig holds configuration for the commercial layer.
type CommercialConfig struct {
	Enabled bool `yaml:"enabled" json:"enabled"`
}

// Layer represents the commercial layer lifecycle.
type Layer struct{}

// Start is a no-op when built without the commercial tag.
func Start(_ *gin.Engine, _ CommercialConfig) (*Layer, error) {
	return &Layer{}, nil
}

// AuthMiddleware returns nil when commercial is not enabled.
func (l *Layer) AuthMiddleware() gin.HandlerFunc { return nil }

// Stop is a no-op.
func (l *Layer) Stop() {}
