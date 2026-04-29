//go:build commercial

package commercial

import (
	"context"
	"log"

	"github.com/gin-gonic/gin"

	sub2api "github.com/Wei-Shaw/sub2api/pkg/types"
	coreusage "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/usage"
)

// BillingPlugin bridges CPA's usage reporting to sub2api's billing engine.
type BillingPlugin struct {
	billingSvc *sub2api.BillingService
	usageSvc   *sub2api.UsageService
}

// HandleUsage is called asynchronously by CPA after each request completes.
func (p *BillingPlugin) HandleUsage(ctx context.Context, record coreusage.Record) {
	if record.Failed {
		return
	}

	// Read commercial values from gin context (stored there by WrapAuthMiddleware).
	// The handler context is derived from context.Background(), so values on
	// c.Request.Context() are lost. The gin context is propagated via ctx.Value("gin").
	var userID int64
	var apiKeyID int64
	var rateMultiplier float64

	if ginCtx, ok := ctx.Value("gin").(*gin.Context); ok {
		if v, exists := ginCtx.Get(string(commercialUserIDKey)); exists {
			userID, _ = v.(int64)
		}
		if v, exists := ginCtx.Get(string(commercialAPIKeyIDKey)); exists {
			apiKeyID, _ = v.(int64)
		}
		if v, exists := ginCtx.Get(string(commercialRateMultiplierKey)); exists {
			rateMultiplier, _ = v.(float64)
		}
	}

	if userID == 0 {
		return
	}
	if rateMultiplier <= 0 {
		rateMultiplier = 1.0
	}

	tokens := sub2api.UsageTokens{
		InputTokens:     int(record.Detail.InputTokens),
		OutputTokens:    int(record.Detail.OutputTokens),
		CacheReadTokens: int(record.Detail.CachedTokens),
	}

	cost, err := p.billingSvc.CalculateCost(record.Model, tokens, rateMultiplier)
	if err != nil {
		log.Printf("[commercial billing] cost calculation failed for model=%s: %v", record.Model, err)
		return
	}

	if p.usageSvc != nil {
		durationMs := int(record.Latency.Milliseconds())
		_, err := p.usageSvc.Create(ctx, sub2api.CreateUsageLogRequest{
			UserID:          userID,
			APIKeyID:        apiKeyID,
			Model:           record.Model,
			InputTokens:     int(record.Detail.InputTokens),
			OutputTokens:    int(record.Detail.OutputTokens),
			CacheReadTokens: int(record.Detail.CachedTokens),
			InputCost:       cost.InputCost,
			OutputCost:      cost.OutputCost,
			CacheReadCost:   cost.CacheReadCost,
			TotalCost:       cost.TotalCost,
			ActualCost:      cost.ActualCost,
			RateMultiplier:  rateMultiplier,
			DurationMs:      &durationMs,
		})
		if err != nil {
			log.Printf("[commercial billing] usage log failed for user=%d model=%s: %v", userID, record.Model, err)
		}
	}
}

type ctxKey string

const (
	commercialUserIDKey         ctxKey = "commercial.userID"
	commercialAPIKeyIDKey       ctxKey = "commercial.apiKeyID"
	commercialRateMultiplierKey ctxKey = "commercial.rateMultiplier"
)
