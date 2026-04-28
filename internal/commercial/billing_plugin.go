//go:build commercial

package commercial

import (
	"context"
	"log"

	sub2api "github.com/Wei-Shaw/sub2api/pkg/types"
	coreusage "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/usage"
)

// BillingPlugin bridges CPA's usage reporting to sub2api's billing engine.
type BillingPlugin struct {
	billingSvc      *sub2api.BillingService
	billingCacheSvc *sub2api.BillingCacheService
}

// HandleUsage is called asynchronously by CPA after each request completes.
func (p *BillingPlugin) HandleUsage(ctx context.Context, record coreusage.Record) {
	if record.Failed {
		return
	}

	userID, _ := ctx.Value(commercialUserIDKey).(int64)
	if userID == 0 {
		return
	}

	rateMultiplier, _ := ctx.Value(commercialRateMultiplierKey).(float64)
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

	if cost.ActualCost > 0 {
		p.billingCacheSvc.QueueDeductBalance(userID, cost.ActualCost)
	}
}

type ctxKey string

const (
	commercialUserIDKey         ctxKey = "commercial.userID"
	commercialRateMultiplierKey ctxKey = "commercial.rateMultiplier"
)

func SetUserID(ctx context.Context, userID int64) context.Context {
	return context.WithValue(ctx, commercialUserIDKey, userID)
}

func SetRateMultiplier(ctx context.Context, rate float64) context.Context {
	return context.WithValue(ctx, commercialRateMultiplierKey, rate)
}
