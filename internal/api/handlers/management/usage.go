package management

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
)

type usageExportPayload struct {
	Version    int                      `json:"version"`
	ExportedAt time.Time                `json:"exported_at"`
	Usage      usage.StatisticsSnapshot `json:"usage"`
}

type usageImportPayload struct {
	Version int                      `json:"version"`
	Usage   usage.StatisticsSnapshot `json:"usage"`
}

// GetUsageStatistics returns the in-memory request statistics snapshot.
func (h *Handler) GetUsageStatistics(c *gin.Context) {
	var snapshot usage.StatisticsSnapshot
	if h != nil && h.usageStats != nil {
		snapshot = h.usageStats.Snapshot()
	}
	payload := gin.H{
		"usage":           snapshot,
		"failed_requests": snapshot.FailureCount,
	}
	if h != nil && h.poolStatsProvider != nil {
		if pool := h.poolStatsProvider(); pool != nil {
			payload["pool"] = pool
		}
	}
	c.JSON(http.StatusOK, payload)
}

// ExportUsageStatistics returns a complete usage snapshot for backup/migration.
func (h *Handler) ExportUsageStatistics(c *gin.Context) {
	var snapshot usage.StatisticsSnapshot
	if h != nil && h.usageStats != nil {
		snapshot = h.usageStats.Snapshot()
	}
	c.JSON(http.StatusOK, usageExportPayload{
		Version:    1,
		ExportedAt: time.Now().UTC(),
		Usage:      snapshot,
	})
}

// GetUsageHistory returns historical usage data from SQLite persistence store.
func (h *Handler) GetUsageHistory(c *gin.Context) {
	store := usage.GetSQLiteStore()
	if store == nil {
		c.JSON(http.StatusOK, gin.H{
			"enabled": false,
			"message": "usage persistence not enabled",
		})
		return
	}

	// Default: last 30 days
	days := 30
	if d := c.Query("days"); d != "" {
		if parsed, err := time.ParseDuration(d + "h"); err == nil {
			days = int(parsed.Hours()) / 24
		} else {
			// try as integer
			var n int
			if _, err := fmt.Sscanf(d, "%d", &n); err == nil && n > 0 {
				days = n
			}
		}
	}
	since := time.Now().AddDate(0, 0, -days)

	c.JSON(http.StatusOK, gin.H{
		"enabled":      true,
		"record_count": store.RecordCount(),
		"period_days":  days,
		"summary":      store.QuerySummary(since),
		"by_model":     store.QueryByModel(since, 30),
		"daily":        store.QueryDaily(since),
	})
}

// ImportUsageStatistics merges a previously exported usage snapshot into memory.
func (h *Handler) ImportUsageStatistics(c *gin.Context) {
	if h == nil || h.usageStats == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "usage statistics unavailable"})
		return
	}

	data, err := c.GetRawData()
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "failed to read request body"})
		return
	}

	var payload usageImportPayload
	if err := json.Unmarshal(data, &payload); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid json"})
		return
	}
	if payload.Version != 0 && payload.Version != 1 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "unsupported version"})
		return
	}

	result := h.usageStats.MergeSnapshot(payload.Usage)
	snapshot := h.usageStats.Snapshot()
	c.JSON(http.StatusOK, gin.H{
		"added":           result.Added,
		"skipped":         result.Skipped,
		"total_requests":  snapshot.TotalRequests,
		"failed_requests": snapshot.FailureCount,
	})
}
