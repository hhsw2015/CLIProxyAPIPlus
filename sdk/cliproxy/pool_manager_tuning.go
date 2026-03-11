package cliproxy

import (
	"math"

	"github.com/router-for-me/CLIProxyAPI/v6/sdk/config"
)

func scaledPoolCount(total int, ratio float64, fallback int) int {
	if total <= 0 {
		if fallback > 0 {
			return fallback
		}
		return 0
	}
	if ratio > 0 {
		count := int(math.Ceil(float64(total) * ratio))
		if count < 1 {
			count = 1
		}
		if count > total {
			count = total
		}
		return count
	}
	if fallback <= 0 {
		return 0
	}
	if fallback > total {
		return total
	}
	return fallback
}

func scaledPoolSampleCount(total int, ratio float64, fallback int) int {
	if total <= 0 {
		if fallback > 0 {
			return fallback
		}
		return 0
	}
	if ratio > 0 {
		count := int(math.Ceil(float64(total) * ratio))
		if count < 1 {
			count = 1
		}
		return count
	}
	if fallback <= 0 {
		return 0
	}
	return fallback
}

func reserveRefillLowWatermark(cfg config.PoolManagerConfig, reserveTarget int) int {
	return scaledPoolCount(reserveTarget, cfg.ReserveRefillLowRatio, reserveTarget)
}

func reserveRefillHighWatermark(cfg config.PoolManagerConfig, reserveTarget int) int {
	return scaledPoolCount(reserveTarget, cfg.ReserveRefillHighRatio, reserveTarget)
}

func coldBatchLoadSize(cfg config.PoolManagerConfig, poolSize int) int {
	return scaledPoolSampleCount(poolSize, cfg.ColdBatchLoadRatio, cfg.ReserveSampleSize)
}

func activeQuotaRefreshSampleSize(cfg config.PoolManagerConfig, activeTarget int) int {
	return scaledPoolSampleCount(activeTarget, cfg.ActiveQuotaRefreshSampleRatio, cfg.ActiveQuotaRefreshSampleSize)
}
