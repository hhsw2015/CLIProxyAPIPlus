package management

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// GetCommercialStatus returns whether the commercial layer is active.
// Uses the config's commercial.enabled flag instead of runtime state.
func (h *Handler) GetCommercialStatus(c *gin.Context) {
	enabled := h.cfg != nil && h.cfg.Commercial.Enabled
	c.JSON(http.StatusOK, gin.H{
		"enabled":   enabled,
		"admin_url": "/admin/",
	})
}
