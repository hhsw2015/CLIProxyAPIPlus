package usage

import (
	"math"
	"runtime"
	"time"
)

// HealthMetrics contains the inputs for health score computation.
type HealthMetrics struct {
	TotalRequests int64
	SuccessCount  int64
	FailureCount  int64
	AvgLatencyMs  float64 // average response latency
	Uptime        time.Duration
}

// ComputeHealthScore returns a 0-100 health score.
// Formula adapted from sub2api's ops_health_score.go.
//
// Business Health (70%):
//   - Error rate score (60%): linear 1%-10% maps to 100->0
//   - Latency score (40%): linear 1s-5s maps to 100->0
//
// Infrastructure Health (30%):
//   - Memory score (50%): warn if alloc > 512MB, degrade to 1024MB
//   - Goroutine score (50%): warn if > 1000, degrade to 10000
func ComputeHealthScore(m HealthMetrics) int {
	// Idle/no-data: avoid showing a bad score when there is no traffic.
	if m.TotalRequests <= 0 && m.FailureCount <= 0 {
		return 100
	}

	business := computeBusinessHealth(m)
	infra := computeInfraHealth()
	score := business*0.70 + infra*0.30
	return int(math.Round(clampFloat(score, 0, 100)))
}

func computeBusinessHealth(m HealthMetrics) float64 {
	// Error rate score: 1% -> 100, 10% -> 0 (linear)
	errorRateScore := 100.0
	if m.TotalRequests > 0 {
		errorRate := float64(m.FailureCount) / float64(m.TotalRequests)
		errorPct := clampFloat(errorRate*100, 0, 100)
		if errorPct > 1.0 {
			if errorPct <= 10.0 {
				errorRateScore = (10.0 - errorPct) / 9.0 * 100
			} else {
				errorRateScore = 0
			}
		}
	}

	// Latency score: 1000ms -> 100, 5000ms -> 0 (linear)
	latencyScore := 100.0
	if m.AvgLatencyMs > 1000 {
		if m.AvgLatencyMs <= 5000 {
			latencyScore = (5000 - m.AvgLatencyMs) / 4000 * 100
		} else {
			latencyScore = 0
		}
	}

	return errorRateScore*0.60 + latencyScore*0.40
}

func computeInfraHealth() float64 {
	// Memory: degrade linearly from 512MB to 1024MB alloc
	var memStats runtime.MemStats
	runtime.ReadMemStats(&memStats)
	memScore := 100.0
	allocMB := float64(memStats.Alloc) / (1024 * 1024)
	if allocMB > 512 {
		if allocMB <= 1024 {
			memScore = (1024 - allocMB) / 512 * 100
		} else {
			memScore = 0
		}
	}

	// Goroutines: degrade linearly from 1000 to 10000
	goroutineScore := 100.0
	numGoroutines := runtime.NumGoroutine()
	if numGoroutines > 1000 {
		if numGoroutines <= 10000 {
			goroutineScore = float64(10000-numGoroutines) / 9000 * 100
		} else {
			goroutineScore = 0
		}
	}

	return memScore*0.50 + goroutineScore*0.50
}

func clampFloat(v, min, max float64) float64 {
	if v < min {
		return min
	}
	if v > max {
		return max
	}
	return v
}

// HealthMetricsFromSnapshot builds HealthMetrics from a StatisticsSnapshot
// by computing average latency across all recorded request details.
func HealthMetricsFromSnapshot(snap StatisticsSnapshot) HealthMetrics {
	var totalLatency int64
	var detailCount int64
	for _, api := range snap.APIs {
		for _, model := range api.Models {
			for _, d := range model.Details {
				totalLatency += d.LatencyMs
				detailCount++
			}
		}
	}
	var avgLatency float64
	if detailCount > 0 {
		avgLatency = float64(totalLatency) / float64(detailCount)
	}
	return HealthMetrics{
		TotalRequests: snap.TotalRequests,
		SuccessCount:  snap.SuccessCount,
		FailureCount:  snap.FailureCount,
		AvgLatencyMs:  avgLatency,
	}
}
