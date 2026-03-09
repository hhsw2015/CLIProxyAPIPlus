package management

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
)

func TestGetUsageStatistics_IncludesPoolMetrics(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	stats := usage.NewRequestStatistics()
	h := &Handler{usageStats: stats}
	h.SetPoolStatisticsProvider(func() any {
		return map[string]any{
			"enabled":       true,
			"provider":      "codex",
			"target_size":   100,
			"active_count":  96,
			"reserve_count": 2048,
			"limit_count":   31,
			"underfilled":   true,
		}
	})

	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/usage", nil)

	h.GetUsageStatistics(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d body=%s", rec.Code, rec.Body.String())
	}

	var payload map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	pool, ok := payload["pool"].(map[string]any)
	if !ok {
		t.Fatalf("expected pool metrics in response, got %#v", payload)
	}
	if enabled, ok := pool["enabled"].(bool); !ok || !enabled {
		t.Fatalf("expected pool.enabled=true, got %#v", pool["enabled"])
	}
	if activeCount, ok := pool["active_count"].(float64); !ok || int(activeCount) != 96 {
		t.Fatalf("expected pool.active_count=96, got %#v", pool["active_count"])
	}
	if reserveCount, ok := pool["reserve_count"].(float64); !ok || int(reserveCount) != 2048 {
		t.Fatalf("expected pool.reserve_count=2048, got %#v", pool["reserve_count"])
	}
}
