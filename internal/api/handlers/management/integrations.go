package management

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/integration"
)

type IntegrationResponse struct {
	Product         string `json:"product"`
	Name            string `json:"name"`
	State           string `json:"state"`
	TargetPath      string `json:"target_path"`
	BackupAvailable bool   `json:"backup_available"`
	Warning         string `json:"warning,omitempty"`
	CurrentContent  string `json:"current_content,omitempty"`
	PlannedContent  string `json:"planned_content,omitempty"`
}

type IntegrationActionResponse struct {
	Message string              `json:"message"`
	Product string              `json:"product"`
	Status  IntegrationResponse `json:"status"`
}

func toIntegrationResponse(product integration.ProductID, status integration.Status, preview integration.Preview) IntegrationResponse {
	return IntegrationResponse{
		Product:         string(product),
		Name:            integration.ProductName(product),
		State:           string(status.State),
		TargetPath:      status.TargetPath,
		BackupAvailable: status.BackupAvailable,
		Warning:         status.Warning,
		CurrentContent:  preview.CurrentContent,
		PlannedContent:  preview.PlannedContent,
	}
}

func isValidProduct(product integration.ProductID) bool {
	for _, p := range integration.SupportedProducts() {
		if p == product {
			return true
		}
	}
	return false
}

func (h *Handler) GetIntegrations(c *gin.Context) {
	if h.integrationMgr == nil {
		c.JSON(http.StatusOK, []IntegrationResponse{})
		return
	}

	resp := make([]IntegrationResponse, 0, len(integration.SupportedProducts()))
	for _, product := range integration.SupportedProducts() {
		status, err := h.integrationMgr.Status(product)
		if err != nil {
			resp = append(resp, IntegrationResponse{
				Product: string(product),
				Name:    integration.ProductName(product),
				State:   string(integration.StateError),
				Warning: err.Error(),
			})
			continue
		}
		preview, _ := h.integrationMgr.Preview(product)
		resp = append(resp, toIntegrationResponse(product, status, preview))
	}
	c.JSON(http.StatusOK, resp)
}

func (h *Handler) ApplyIntegration(c *gin.Context) {
	if h.integrationMgr == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "integration manager not available"})
		return
	}

	product := integration.ProductID(c.Param("product"))
	if !isValidProduct(product) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "unknown product: " + string(product)})
		return
	}
	result, err := h.integrationMgr.Apply(product)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	preview, err := h.integrationMgr.Preview(result.Product)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, IntegrationActionResponse{
		Message: result.Message,
		Product: string(result.Product),
		Status:  toIntegrationResponse(result.Product, result.Status, preview),
	})
}

func (h *Handler) RollbackIntegration(c *gin.Context) {
	if h.integrationMgr == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "integration manager not available"})
		return
	}

	product := integration.ProductID(c.Param("product"))
	if !isValidProduct(product) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "unknown product: " + string(product)})
		return
	}
	result, err := h.integrationMgr.Rollback(product)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	preview, err := h.integrationMgr.Preview(result.Product)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, IntegrationActionResponse{
		Message: result.Message,
		Product: string(result.Product),
		Status:  toIntegrationResponse(result.Product, result.Status, preview),
	})
}
