package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadConfigOptional_PoolManagerConfigAndDefaults(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	content := `
port: 8080
pool-manager:
  size: 100
`
	if err := os.WriteFile(configPath, []byte(content), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := LoadConfigOptional(configPath, false)
	if err != nil {
		t.Fatalf("LoadConfigOptional() error = %v", err)
	}

	if cfg.PoolManager.Size != 100 {
		t.Fatalf("PoolManager.Size = %d, want 100", cfg.PoolManager.Size)
	}
	if cfg.PoolManager.Provider != "codex" {
		t.Fatalf("PoolManager.Provider = %q, want %q", cfg.PoolManager.Provider, "codex")
	}
	if cfg.PoolManager.ActiveIdleScanIntervalSeconds != 1800 {
		t.Fatalf("PoolManager.ActiveIdleScanIntervalSeconds = %d, want 1800", cfg.PoolManager.ActiveIdleScanIntervalSeconds)
	}
	if cfg.PoolManager.ReserveScanIntervalSeconds != 300 {
		t.Fatalf("PoolManager.ReserveScanIntervalSeconds = %d, want 300", cfg.PoolManager.ReserveScanIntervalSeconds)
	}
	if cfg.PoolManager.LimitScanIntervalSeconds != 21600 {
		t.Fatalf("PoolManager.LimitScanIntervalSeconds = %d, want 21600", cfg.PoolManager.LimitScanIntervalSeconds)
	}
	if cfg.PoolManager.ReserveSampleSize != 20 {
		t.Fatalf("PoolManager.ReserveSampleSize = %d, want 20", cfg.PoolManager.ReserveSampleSize)
	}
}

func TestLoadConfigOptional_PoolManagerSanitizesInvalidValues(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	content := `
port: 8080
pool-manager:
  size: -1
  provider: "  "
  active-idle-scan-interval-seconds: -1
  reserve-scan-interval-seconds: -2
  limit-scan-interval-seconds: -3
  reserve-sample-size: -4
`
	if err := os.WriteFile(configPath, []byte(content), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := LoadConfigOptional(configPath, false)
	if err != nil {
		t.Fatalf("LoadConfigOptional() error = %v", err)
	}

	if cfg.PoolManager.Size != 0 {
		t.Fatalf("PoolManager.Size = %d, want 0", cfg.PoolManager.Size)
	}
	if cfg.PoolManager.Provider != "codex" {
		t.Fatalf("PoolManager.Provider = %q, want %q", cfg.PoolManager.Provider, "codex")
	}
	if cfg.PoolManager.ActiveIdleScanIntervalSeconds != 0 {
		t.Fatalf("PoolManager.ActiveIdleScanIntervalSeconds = %d, want 0", cfg.PoolManager.ActiveIdleScanIntervalSeconds)
	}
	if cfg.PoolManager.ReserveScanIntervalSeconds != 0 {
		t.Fatalf("PoolManager.ReserveScanIntervalSeconds = %d, want 0", cfg.PoolManager.ReserveScanIntervalSeconds)
	}
	if cfg.PoolManager.LimitScanIntervalSeconds != 0 {
		t.Fatalf("PoolManager.LimitScanIntervalSeconds = %d, want 0", cfg.PoolManager.LimitScanIntervalSeconds)
	}
	if cfg.PoolManager.ReserveSampleSize != 0 {
		t.Fatalf("PoolManager.ReserveSampleSize = %d, want 0", cfg.PoolManager.ReserveSampleSize)
	}
}
