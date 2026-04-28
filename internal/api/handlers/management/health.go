package management

import (
	"net/http"
	"runtime"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
)

// GetHealthScore returns a 0-100 health score computed from current usage
// statistics and infrastructure metrics.
func (h *Handler) GetHealthScore(c *gin.Context) {
	var snap usage.StatisticsSnapshot
	if h != nil && h.usageStats != nil {
		snap = h.usageStats.Snapshot()
	}

	metrics := usage.HealthMetricsFromSnapshot(snap)
	score := usage.ComputeHealthScore(metrics)

	var memStats runtime.MemStats
	runtime.ReadMemStats(&memStats)

	c.JSON(http.StatusOK, gin.H{
		"score": score,
		"metrics": gin.H{
			"total_requests": metrics.TotalRequests,
			"success_count":  metrics.SuccessCount,
			"failure_count":  metrics.FailureCount,
			"avg_latency_ms": metrics.AvgLatencyMs,
		},
		"infra": gin.H{
			"alloc_mb":   float64(memStats.Alloc) / (1024 * 1024),
			"goroutines": runtime.NumGoroutine(),
		},
	})
}
