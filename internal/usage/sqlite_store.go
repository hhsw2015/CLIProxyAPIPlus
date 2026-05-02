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
