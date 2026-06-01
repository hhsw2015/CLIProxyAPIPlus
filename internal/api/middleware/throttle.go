package middleware

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
)

func ThrottleBacklog(maxConcurrency, backlog int, timeout time.Duration) gin.HandlerFunc {
	if maxConcurrency <= 0 {
		return func(c *gin.Context) { c.Next() }
	}
	if backlog < 0 {
		backlog = 0
	}

	sem := make(chan struct{}, maxConcurrency)
	queue := make(chan struct{}, maxConcurrency+backlog)

	return func(c *gin.Context) {
		select {
		case queue <- struct{}{}:
		default:
			c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{
				"type": "error",
				"error": gin.H{
					"type":    "overloaded_error",
					"message": "server at capacity, try again later",
				},
			})
			return
		}
		defer func() { <-queue }()

		select {
		case sem <- struct{}{}:
		case <-time.After(timeout):
			c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{
				"type": "error",
				"error": gin.H{
					"type":    "overloaded_error",
					"message": "request queued too long, try again later",
				},
			})
			return
		case <-c.Request.Context().Done():
			c.Abort()
			return
		}
		defer func() { <-sem }()

		c.Next()
	}
}
