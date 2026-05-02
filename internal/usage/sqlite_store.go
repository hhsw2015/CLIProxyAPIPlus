package usage

import (
	"fmt"
	"path/filepath"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
	"gorm.io/driver/sqlite"
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
	CreatedAt       time.Time
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

	if err := db.AutoMigrate(&UsageRecord{}); err != nil {
		return nil, fmt.Errorf("migrate usage sqlite: %w", err)
	}

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

// Record persists a single usage event to SQLite.
func (s *SQLiteStore) Record(r UsageRecord) {
	s.mu.Lock()
	defer s.mu.Unlock()

	r.CreatedAt = time.Now()
	result := s.db.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "event_key"}},
		DoNothing: true,
	}).Create(&r)
	if result.Error != nil {
		log.Debugf("[usage-sqlite] write failed: %v", result.Error)
	}
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
