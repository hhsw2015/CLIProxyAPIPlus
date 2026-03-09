package tui

import (
	"strings"
	"testing"
)

func TestUsageTabRenderContent_IncludesPoolMetrics(t *testing.T) {
	m := newUsageTabModel(nil)
	m.width = 120
	m.height = 40
	m.usage = map[string]any{
		"usage": map[string]any{
			"total_requests": 10.0,
			"success_count":  8.0,
			"failure_count":  2.0,
			"total_tokens":   1234.0,
		},
		"pool": map[string]any{
			"enabled":                  true,
			"provider":                 "codex",
			"target_size":              100.0,
			"active_count":             97.0,
			"reserve_count":            4096.0,
			"limit_count":              33.0,
			"underfilled":              true,
			"promotions_total":         12.0,
			"active_removed_total":     9.0,
			"moved_to_limit_total":     7.0,
			"restored_from_limit_total": 3.0,
		},
	}

	content := m.renderContent()
	if !strings.Contains(content, "Pool Manager") {
		t.Fatalf("expected pool section title, got %q", content)
	}
	if !strings.Contains(content, "97/100") {
		t.Fatalf("expected active pool ratio in content, got %q", content)
	}
	if !strings.Contains(content, "4096") {
		t.Fatalf("expected reserve count in content, got %q", content)
	}
}
