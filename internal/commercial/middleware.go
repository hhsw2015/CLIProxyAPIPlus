//go:build commercial

package commercial

import (
	"github.com/gin-gonic/gin"

	sub2api "github.com/Wei-Shaw/sub2api/pkg/types"
)

// WrapAuthMiddleware wraps sub2api's API key auth middleware to bridge
// the authenticated user info from gin context to request context.
func WrapAuthMiddleware(inner gin.HandlerFunc) gin.HandlerFunc {
	if inner == nil {
		return nil
	}
	return func(c *gin.Context) {
		inner(c)
		if c.IsAborted() {
			return
		}

		// Defensive: verify auth actually succeeded
		subject, ok := sub2api.GetAuthSubjectFromContext(c)
		if !ok || subject.UserID <= 0 {
			c.AbortWithStatusJSON(401, map[string]string{"error": "authentication required"})
			return
		}

		// Store in gin context (survives GetContextWithCancel which uses context.Background)
		c.Set(string(commercialUserIDKey), subject.UserID)

		apiKey, ok := sub2api.GetAPIKeyFromContext(c)
		if ok && apiKey != nil {
			c.Set(string(commercialAPIKeyIDKey), apiKey.ID)
			if apiKey.Group != nil && apiKey.Group.RateMultiplier > 0 {
				c.Set(string(commercialRateMultiplierKey), apiKey.Group.RateMultiplier)
			}
			c.Set("apiKey", apiKey.Key)
			c.Set("accessProvider", "sub2api")
		}
	}
}
