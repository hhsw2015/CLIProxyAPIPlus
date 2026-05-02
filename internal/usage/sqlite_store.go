package usage

import (
	"fmt"
	"path/filepath"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/logger"
)

// UsageRecord is the SQLite model for persisted usage events.
type UsageRecord struct {
	ID              uint      `gorm:"primaryKey"`
	EventKey        string    `gorm:"uniqueIndex:uniq_event_key"`
	Model           string    `gorm:"index:idx_model"`
	Timestamp       time.Time `gorm:"index:idx_timestamp"`
	Source          string    `gorm:"index:idx_source"`
	AuthIndex       string    `gorm:"index:idx_auth_index"`
	Provider        string
	Failed          bool `gorm:"index:idx_failed"`
	LatencyMS       int64
	InputTokens     int64
	OutputTokens    int64
	ReasoningTokens int64
	CachedTokens    int64
	TotalTokens     int64
	CostUSD         float64
	CreatedAt       time.Time
}

// ModelPrice stores per-model pricing for cost calculation.
type ModelPrice struct {
	ID                   uint    `gorm:"primaryKey"`
	Model                string  `gorm:"uniqueIndex:uniq_model_price"`
	InputPricePer1M      float64
	OutputPricePer1M     float64
	CacheReadPricePer1M  float64
	UpdatedAt            time.Time
}

// SQLiteStore handles persistent usage storage.
type SQLiteStore struct {
	db            *gorm.DB
	adminKeys     map[string]bool
	retentionDays int
	mu            sync.Mutex
}

// NewSQLiteStore opens the SQLite database and runs migrations.
func NewSQLiteStore(dbPath string, adminAPIKeys []string, retentionDays int) (*SQLiteStore, error) {
	dir := filepath.Dir(dbPath)
	if dir != "" && dir != "." {
		// Ensure directory exists (handled by caller or config)
	}

	dsn := dbPath + "?_busy_timeout=5000&_foreign_keys=on&_journal_mode=WAL"
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		return nil, fmt.Errorf("open usage sqlite %s: %w", dbPath, err)
	}

	sqlDB, err := db.DB()
	if err != nil {
		return nil, fmt.Errorf("configure usage sqlite: %w", err)
	}
	sqlDB.SetMaxOpenConns(1)
	sqlDB.SetMaxIdleConns(1)

	if err := db.AutoMigrate(&UsageRecord{}, &ModelPrice{}); err != nil {
		return nil, fmt.Errorf("migrate usage sqlite: %w", err)
	}

	seedDefaultPrices(db)

	keys := make(map[string]bool, len(adminAPIKeys))
	for _, k := range adminAPIKeys {
		if k != "" {
			keys[k] = true
		}
	}

	store := &SQLiteStore{
		db:            db,
		adminKeys:     keys,
		retentionDays: retentionDays,
	}

	go store.cleanupLoop()

	return store, nil
}

// IsAdminKey checks if the given API key is a CPA admin key.
func (s *SQLiteStore) IsAdminKey(apiKey string) bool {
	return s.adminKeys[apiKey]
}

// Record persists a single usage event to SQLite with cost calculation.
func (s *SQLiteStore) Record(r UsageRecord) {
	s.mu.Lock()
	defer s.mu.Unlock()

	r.CreatedAt = time.Now()
	r.CostUSD = s.calculateCost(r)

	result := s.db.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "event_key"}},
		DoNothing: true,
	}).Create(&r)
	if result.Error != nil {
		log.Debugf("[usage-sqlite] write failed: %v", result.Error)
	}
}

func (s *SQLiteStore) calculateCost(r UsageRecord) float64 {
	var price ModelPrice
	if err := s.db.Where("model = ?", r.Model).First(&price).Error; err != nil {
		return 0
	}
	inputCost := float64(r.InputTokens) * price.InputPricePer1M / 1_000_000
	outputCost := float64(r.OutputTokens+r.ReasoningTokens) * price.OutputPricePer1M / 1_000_000
	cacheCost := float64(r.CachedTokens) * price.CacheReadPricePer1M / 1_000_000
	return inputCost + outputCost + cacheCost
}

// GetModelPrices returns all configured model prices.
func (s *SQLiteStore) GetModelPrices() []ModelPrice {
	var prices []ModelPrice
	s.db.Order("model ASC").Find(&prices)
	return prices
}

// SetModelPrice upserts a model price entry.
func (s *SQLiteStore) SetModelPrice(model string, inputPer1M, outputPer1M, cacheReadPer1M float64) {
	s.db.Where("model = ?", model).Assign(ModelPrice{
		InputPricePer1M:     inputPer1M,
		OutputPricePer1M:    outputPer1M,
		CacheReadPricePer1M: cacheReadPer1M,
		UpdatedAt:           time.Now(),
	}).FirstOrCreate(&ModelPrice{Model: model})
}

// DeleteModelPrice removes a model price entry.
func (s *SQLiteStore) DeleteModelPrice(model string) {
	s.db.Where("model = ?", model).Delete(&ModelPrice{})
}

// Close closes the SQLite database.
func (s *SQLiteStore) Close() {
	if s.db != nil {
		sqlDB, _ := s.db.DB()
		if sqlDB != nil {
			sqlDB.Close()
		}
	}
}

func (s *SQLiteStore) cleanupLoop() {
	if s.retentionDays <= 0 {
		return
	}
	ticker := time.NewTicker(24 * time.Hour)
	defer ticker.Stop()

	// Run once at startup after 1 minute
	time.Sleep(time.Minute)
	s.cleanup()

	for range ticker.C {
		s.cleanup()
	}
}

// QuerySummary returns aggregated usage statistics from SQLite.
func (s *SQLiteStore) QuerySummary(since time.Time) map[string]any {
	if s.db == nil {
		return nil
	}

	type summaryRow struct {
		TotalRequests   int64
		FailedRequests  int64
		TotalInput      int64
		TotalOutput     int64
		TotalReasoning  int64
		TotalCached     int64
		TotalTokens     int64
		TotalCost       float64
		AvgLatency      float64
	}

	var row summaryRow
	s.db.Model(&UsageRecord{}).
		Where("timestamp >= ?", since).
		Select(`
			COUNT(*) as total_requests,
			SUM(CASE WHEN failed = 1 THEN 1 ELSE 0 END) as failed_requests,
			COALESCE(SUM(input_tokens), 0) as total_input,
			COALESCE(SUM(output_tokens), 0) as total_output,
			COALESCE(SUM(reasoning_tokens), 0) as total_reasoning,
			COALESCE(SUM(cached_tokens), 0) as total_cached,
			COALESCE(SUM(total_tokens), 0) as total_tokens,
			COALESCE(SUM(cost_usd), 0) as total_cost,
			COALESCE(AVG(latency_ms), 0) as avg_latency
		`).Scan(&row)

	return map[string]any{
		"total_requests":   row.TotalRequests,
		"failed_requests":  row.FailedRequests,
		"total_input":      row.TotalInput,
		"total_output":     row.TotalOutput,
		"total_reasoning":  row.TotalReasoning,
		"total_cached":     row.TotalCached,
		"total_tokens":     row.TotalTokens,
		"total_cost_usd":   row.TotalCost,
		"avg_latency_ms":   row.AvgLatency,
	}
}

// QueryByModel returns per-model aggregated stats.
func (s *SQLiteStore) QueryByModel(since time.Time, limit int) []map[string]any {
	if s.db == nil {
		return nil
	}
	if limit <= 0 {
		limit = 20
	}

	type modelRow struct {
		Model        string
		Requests     int64
		Failed       int64
		InputTokens  int64
		OutputTokens int64
		TotalTokens  int64
		AvgLatency   float64
	}

	var rows []modelRow
	s.db.Model(&UsageRecord{}).
		Where("timestamp >= ?", since).
		Select(`
			model,
			COUNT(*) as requests,
			SUM(CASE WHEN failed = 1 THEN 1 ELSE 0 END) as failed,
			COALESCE(SUM(input_tokens), 0) as input_tokens,
			COALESCE(SUM(output_tokens), 0) as output_tokens,
			COALESCE(SUM(total_tokens), 0) as total_tokens,
			COALESCE(AVG(latency_ms), 0) as avg_latency
		`).
		Group("model").
		Order("requests DESC").
		Limit(limit).
		Scan(&rows)

	result := make([]map[string]any, len(rows))
	for i, r := range rows {
		result[i] = map[string]any{
			"model":         r.Model,
			"requests":      r.Requests,
			"failed":        r.Failed,
			"input_tokens":  r.InputTokens,
			"output_tokens": r.OutputTokens,
			"total_tokens":  r.TotalTokens,
			"avg_latency_ms": r.AvgLatency,
		}
	}
	return result
}

// QueryDaily returns daily aggregated stats.
func (s *SQLiteStore) QueryDaily(since time.Time) []map[string]any {
	if s.db == nil {
		return nil
	}

	type dailyRow struct {
		Day          string
		Requests     int64
		Failed       int64
		TotalTokens  int64
	}

	var rows []dailyRow
	s.db.Model(&UsageRecord{}).
		Where("timestamp >= ?", since).
		Select(`
			DATE(timestamp) as day,
			COUNT(*) as requests,
			SUM(CASE WHEN failed = 1 THEN 1 ELSE 0 END) as failed,
			COALESCE(SUM(total_tokens), 0) as total_tokens
		`).
		Group("day").
		Order("day ASC").
		Scan(&rows)

	result := make([]map[string]any, len(rows))
	for i, r := range rows {
		result[i] = map[string]any{
			"day":          r.Day,
			"requests":     r.Requests,
			"failed":       r.Failed,
			"total_tokens": r.TotalTokens,
		}
	}
	return result
}

// RecordCount returns total record count in SQLite.
func (s *SQLiteStore) RecordCount() int64 {
	if s.db == nil {
		return 0
	}
	var count int64
	s.db.Model(&UsageRecord{}).Count(&count)
	return count
}

func seedDefaultPrices(db *gorm.DB) {
	// Official pricing per 1M tokens (USD)
	defaults := []ModelPrice{
		// Anthropic Claude
		{Model: "claude-opus-4.7", InputPricePer1M: 5, OutputPricePer1M: 25, CacheReadPricePer1M: 0.5},
		{Model: "claude-opus-4.6", InputPricePer1M: 5, OutputPricePer1M: 25, CacheReadPricePer1M: 0.5},
		{Model: "claude-opus-4.5", InputPricePer1M: 5, OutputPricePer1M: 25, CacheReadPricePer1M: 0.5},
		{Model: "claude-opus-4.1", InputPricePer1M: 5, OutputPricePer1M: 25, CacheReadPricePer1M: 0.5},
		{Model: "claude-sonnet-4.6", InputPricePer1M: 3, OutputPricePer1M: 15, CacheReadPricePer1M: 0.3},
		{Model: "claude-sonnet-4.5", InputPricePer1M: 3, OutputPricePer1M: 15, CacheReadPricePer1M: 0.3},
		{Model: "claude-4-sonnet", InputPricePer1M: 3, OutputPricePer1M: 15, CacheReadPricePer1M: 0.3},
		{Model: "claude-haiku-4.5", InputPricePer1M: 1, OutputPricePer1M: 5, CacheReadPricePer1M: 0.1},
		// OpenAI GPT
		{Model: "gpt-5.5", InputPricePer1M: 2.5, OutputPricePer1M: 15, CacheReadPricePer1M: 0.25},
		{Model: "gpt-5.4", InputPricePer1M: 2.5, OutputPricePer1M: 15, CacheReadPricePer1M: 0.25},
		{Model: "gpt-5.4-pro", InputPricePer1M: 5, OutputPricePer1M: 30, CacheReadPricePer1M: 0.5},
		{Model: "gpt-5.4-mini", InputPricePer1M: 0.75, OutputPricePer1M: 4.5, CacheReadPricePer1M: 0.075},
		{Model: "gpt-5.4-nano", InputPricePer1M: 0.2, OutputPricePer1M: 1.25, CacheReadPricePer1M: 0.02},
		{Model: "gpt-5.3-codex", InputPricePer1M: 1.5, OutputPricePer1M: 12, CacheReadPricePer1M: 0.15},
		{Model: "gpt-5", InputPricePer1M: 2, OutputPricePer1M: 8, CacheReadPricePer1M: 0.2},
		{Model: "gpt-5-mini", InputPricePer1M: 0.4, OutputPricePer1M: 1.6, CacheReadPricePer1M: 0.04},
		{Model: "o3-pro", InputPricePer1M: 20, OutputPricePer1M: 80, CacheReadPricePer1M: 2},
		// Google Gemini
		{Model: "gemini-2.5-pro", InputPricePer1M: 1.25, OutputPricePer1M: 10, CacheReadPricePer1M: 0.3},
		{Model: "gemini-3-flash-preview", InputPricePer1M: 0.15, OutputPricePer1M: 0.6, CacheReadPricePer1M: 0.04},
		{Model: "gemini-3.1-pro-preview", InputPricePer1M: 2.5, OutputPricePer1M: 15, CacheReadPricePer1M: 0.5},
		// Image generation (per image, stored as per-1M-token equivalent)
		{Model: "gpt-image-2", InputPricePer1M: 40, OutputPricePer1M: 0, CacheReadPricePer1M: 0},
	}
	for _, p := range defaults {
		db.Where("model = ?", p.Model).FirstOrCreate(&p)
	}
}

func (s *SQLiteStore) cleanup() {
	if s.retentionDays <= 0 {
		return
	}
	cutoff := time.Now().AddDate(0, 0, -s.retentionDays)
	result := s.db.Where("timestamp < ?", cutoff).Delete(&UsageRecord{})
	if result.Error != nil {
		log.Warnf("[usage-sqlite] cleanup failed: %v", result.Error)
	} else if result.RowsAffected > 0 {
		log.Infof("[usage-sqlite] cleaned up %d records older than %d days", result.RowsAffected, s.retentionDays)
		s.db.Exec("VACUUM")
	}
}
