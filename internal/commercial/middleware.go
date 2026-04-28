//go:build commercial

package commercial

import (
	"github.com/gin-gonic/gin"

	sub2apiMW "github.com/Wei-Shaw/sub2api/internal/server/middleware"
)

// WrapAuthMiddleware wraps sub2api's API key auth middleware to bridge
// the authenticated userID from gin context to request context, so the
// billing plugin can read it downstream.
func WrapAuthMiddleware(inner gin.HandlerFunc) gin.HandlerFunc {
	if inner == nil {
		return nil
	}
	return func(c *gin.Context) {
		inner(c)
		if c.IsAborted() {
			return
		}
		subject, ok := sub2apiMW.GetAuthSubjectFromContext(c)
		if ok && subject.UserID > 0 {
			ctx := SetUserID(c.Request.Context(), subject.UserID)
			c.Request = c.Request.WithContext(ctx)
		}
	}
}
