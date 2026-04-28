package management

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// GetCommercialStatus returns whether the commercial layer is active.
// Used by the management frontend to decide whether to show commercial tabs.
func (h *Handler) GetCommercialStatus(c *gin.Context) {
	layer := h.commercialLayer
	enabled := layer != nil && layer.AuthMiddleware() != nil
	c.JSON(http.StatusOK, gin.H{
		"enabled":   enabled,
		"admin_url": "/admin/",
	})
}
