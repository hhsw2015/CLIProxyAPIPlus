package middleware

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

func MaxBodySize(maxMB int) gin.HandlerFunc {
	if maxMB <= 0 {
		return func(c *gin.Context) { c.Next() }
	}
	maxBytes := int64(maxMB) * 1024 * 1024

	return func(c *gin.Context) {
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxBytes)
		c.Next()
	}
}
