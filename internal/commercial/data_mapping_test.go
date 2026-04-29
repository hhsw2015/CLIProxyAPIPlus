//go:build commercial

package commercial

import (
	"testing"

	sub2api "github.com/Wei-Shaw/sub2api/pkg/types"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
)

func TestConvertPriority(t *testing.T) {
	tests := []struct {
		cpa    int
		expect int
	}{
		{0, 50},
		{1, 45},
		{5, 25},
		{8, 10},
		{9, 5},
		{10, 1},
		{11, 1},   // clamped to 1
		{20, 1},   // clamped to 1
		{-5, 75},  // negative CPA priority
		{-20, 100}, // clamped to 100
	}
	for _, tt := range tests {
		got := convertPriority(tt.cpa)
		if got != tt.expect {
			t.Errorf("convertPriority(%d) = %d, want %d", tt.cpa, got, tt.expect)
		}
	}
}

func TestConvertPriorityOrdering(t *testing.T) {
	// CPA: higher = better. Sub2API: lower = better.
	// So CPA 10 should map to a LOWER Sub2API value than CPA 5.
	high := convertPriority(10)
	mid := convertPriority(5)
	low := convertPriority(0)

	if high >= mid {
		t.Errorf("CPA priority 10 (%d) should map to lower Sub2API value than CPA 5 (%d)", high, mid)
	}
	if mid >= low {
		t.Errorf("CPA priority 5 (%d) should map to lower Sub2API value than CPA 0 (%d)", mid, low)
	}
}

func TestStableID_Deterministic(t *testing.T) {
	id1 := stableID("claude-key", "sk-ant-abc123")
	id2 := stableID("claude-key", "sk-ant-abc123")
	if id1 != id2 {
		t.Errorf("stableID not deterministic: %s != %s", id1, id2)
	}
	if len(id1) != 16 {
		t.Errorf("stableID length = %d, want 16", len(id1))
	}
}

func TestStableID_DifferentInputs(t *testing.T) {
	id1 := stableID("claude-key", "sk-ant-abc123")
	id2 := stableID("claude-key", "sk-ant-xyz789")
	id3 := stableID("gemini-key", "sk-ant-abc123")

	if id1 == id2 {
		t.Error("different fingerprints should produce different stableIDs")
	}
	if id1 == id3 {
		t.Error("different provider types should produce different stableIDs")
	}
}

func TestGroupKey(t *testing.T) {
	got := groupKey("anthropic", "bedrock", 10)
	if got != "CPA-anthropic-bedrock-P10" {
		t.Errorf("groupKey = %q, want %q", got, "CPA-anthropic-bedrock-P10")
	}
}

func TestSanitizeName(t *testing.T) {
	tests := []struct {
		input, expect string
	}{
		{"TaijiAI", "taijiai"},
		{"Cookie Pool", "cookie-pool"},
		{"A Very Long Name That Exceeds The Maximum Allowed Length For Names", "a-very-long-name-that-exceeds-the-maximu"},
	}
	for _, tt := range tests {
		got := sanitizeName(tt.input)
		if got != tt.expect {
			t.Errorf("sanitizeName(%q) = %q, want %q", tt.input, got, tt.expect)
		}
	}
}

func TestMapClaudeKeys_APIKey(t *testing.T) {
	keys := []config.ClaudeKey{
		{
			APIKey:   "sk-ant-test123",
			Priority: 9,
			BaseURL:  "https://api.anthropic.com",
			Prefix:   "team-a",
		},
	}

	mappings := MapClaudeKeys(keys)
	if len(mappings) != 1 {
		t.Fatalf("expected 1 mapping, got %d", len(mappings))
	}

	m := mappings[0]
	if m.Platform != "anthropic" {
		t.Errorf("platform = %q, want anthropic", m.Platform)
	}
	if m.CreateInput.Type != "apikey" {
		t.Errorf("type = %q, want apikey", m.CreateInput.Type)
	}
	if m.CreateInput.Priority != convertPriority(9) {
		t.Errorf("priority = %d, want %d", m.CreateInput.Priority, convertPriority(9))
	}
	if m.CreateInput.Credentials["api_key"] != "sk-ant-test123" {
		t.Errorf("credentials.api_key = %v, want sk-ant-test123", m.CreateInput.Credentials["api_key"])
	}
	if m.CreateInput.Credentials["base_url"] != "https://api.anthropic.com" {
		t.Errorf("credentials.base_url = %v", m.CreateInput.Credentials["base_url"])
	}
	if m.GroupKey != "CPA-anthropic-apikey-P9" {
		t.Errorf("groupKey = %q, want CPA-anthropic-apikey-P9", m.GroupKey)
	}
	if m.CreateInput.Extra["prefix"] != "team-a" {
		t.Errorf("extra.prefix = %v, want team-a", m.CreateInput.Extra["prefix"])
	}
	if m.CreateInput.Extra[extraKeyCPASource] != true {
		t.Error("extra.cpa_source should be true")
	}
	if m.StableID == "" {
		t.Error("stableID should not be empty")
	}
}

func TestMapClaudeKeys_Bedrock(t *testing.T) {
	keys := []config.ClaudeKey{
		{
			AWSAccessKeyID:     "AKIA_TEST",
			AWSSecretAccessKey: "secret123",
			AWSRegion:          "us-east-1",
			Priority:           10,
		},
	}

	mappings := MapClaudeKeys(keys)
	if len(mappings) != 1 {
		t.Fatalf("expected 1 mapping, got %d", len(mappings))
	}

	m := mappings[0]
	if m.CreateInput.Type != "bedrock" {
		t.Errorf("type = %q, want bedrock", m.CreateInput.Type)
	}
	if m.CreateInput.Credentials["auth_mode"] != "sigv4" {
		t.Errorf("credentials.auth_mode = %v, want sigv4", m.CreateInput.Credentials["auth_mode"])
	}
	if m.CreateInput.Credentials["aws_access_key_id"] != "AKIA_TEST" {
		t.Error("credentials.aws_access_key_id mismatch")
	}
	if m.CreateInput.Credentials["aws_region"] != "us-east-1" {
		t.Error("credentials.aws_region mismatch")
	}
	if m.GroupKey != "CPA-anthropic-bedrock-P10" {
		t.Errorf("groupKey = %q, want CPA-anthropic-bedrock-P10", m.GroupKey)
	}
}

func TestMapClaudeKeys_SkipsEmptyCredentials(t *testing.T) {
	keys := []config.ClaudeKey{
		{APIKey: "", Priority: 5}, // no API key and no AWS key
	}

	mappings := MapClaudeKeys(keys)
	if len(mappings) != 0 {
		t.Fatalf("expected 0 mappings (empty credentials), got %d", len(mappings))
	}
}

func TestMapClaudeKeys_SkipsDisabled(t *testing.T) {
	keys := []config.ClaudeKey{
		{APIKey: "sk-active", Priority: 5},
		{APIKey: "sk-disabled", Priority: 5, Disabled: true},
	}

	mappings := MapClaudeKeys(keys)
	if len(mappings) != 1 {
		t.Fatalf("expected 1 mapping (skip disabled), got %d", len(mappings))
	}
}

func TestMapClaudeKeys_SkipsVertexAI(t *testing.T) {
	keys := []config.ClaudeKey{
		{
			GCPCredentialsFile: "/path/to/creds.json",
			Priority:           5,
		},
	}

	mappings := MapClaudeKeys(keys)
	if len(mappings) != 0 {
		t.Fatalf("expected 0 mappings (skip vertex), got %d", len(mappings))
	}
}

func TestMapGeminiKeys(t *testing.T) {
	keys := []config.GeminiKey{
		{APIKey: "AIza-test123", Priority: 6, BaseURL: "https://custom.gemini.api"},
	}

	mappings := MapGeminiKeys(keys)
	if len(mappings) != 1 {
		t.Fatalf("expected 1 mapping, got %d", len(mappings))
	}

	m := mappings[0]
	if m.Platform != "gemini" {
		t.Errorf("platform = %q, want gemini", m.Platform)
	}
	if m.CreateInput.Type != "apikey" {
		t.Errorf("type = %q, want apikey", m.CreateInput.Type)
	}
	if m.CreateInput.Credentials["api_key"] != "AIza-test123" {
		t.Error("credentials.api_key mismatch")
	}
	if m.GroupKey != "CPA-gemini-apikey-P6" {
		t.Errorf("groupKey = %q", m.GroupKey)
	}
}

func TestMapCodexKeys(t *testing.T) {
	keys := []config.CodexKey{
		{APIKey: "sk-codex-test", Priority: 7, BaseURL: "https://api.openai.com"},
	}

	mappings := MapCodexKeys(keys)
	if len(mappings) != 1 {
		t.Fatalf("expected 1 mapping, got %d", len(mappings))
	}

	m := mappings[0]
	if m.Platform != "openai" {
		t.Errorf("platform = %q, want openai", m.Platform)
	}
	if m.GroupKey != "CPA-openai-codex-P7" {
		t.Errorf("groupKey = %q", m.GroupKey)
	}
}

func TestMapCodexKeys_SkipsEmptyAPIKey(t *testing.T) {
	keys := []config.CodexKey{
		{APIKey: "", Priority: 5},
	}

	mappings := MapCodexKeys(keys)
	if len(mappings) != 0 {
		t.Fatalf("expected 0 mappings (empty api key), got %d", len(mappings))
	}
}

func TestMapOpenAICompat_BearerPlatform(t *testing.T) {
	entries := []config.OpenAICompatibility{
		{
			Name:     "CookiePool",
			Priority: 8,
			BaseURL:  "https://cookie.example.com",
			APIKeyEntries: []config.OpenAICompatibilityAPIKey{
				{APIKey: "key-1"},
				{APIKey: "key-2"},
			},
		},
	}

	mappings := MapOpenAICompat(entries)
	if len(mappings) != 2 {
		t.Fatalf("expected 2 mappings (one per api key entry), got %d", len(mappings))
	}

	for _, m := range mappings {
		if m.Platform != "openai" {
			t.Errorf("platform = %q, want openai (bearer auth style)", m.Platform)
		}
		if m.CreateInput.Extra["compat_name"] != "CookiePool" {
			t.Errorf("extra.compat_name = %v", m.CreateInput.Extra["compat_name"])
		}
	}
}

func TestMapOpenAICompat_AnthropicAuthStyle(t *testing.T) {
	entries := []config.OpenAICompatibility{
		{
			Name:      "TaijiAI",
			Priority:  9,
			BaseURL:   "https://api.taijiaicloud.com",
			AuthStyle: "anthropic",
			APIKeyEntries: []config.OpenAICompatibilityAPIKey{
				{APIKey: "taiji-key-1"},
			},
		},
	}

	mappings := MapOpenAICompat(entries)
	if len(mappings) != 1 {
		t.Fatalf("expected 1 mapping, got %d", len(mappings))
	}

	m := mappings[0]
	if m.Platform != "anthropic" {
		t.Errorf("platform = %q, want anthropic (auth-style=anthropic)", m.Platform)
	}
	if m.GroupKey != "CPA-anthropic-compat-P9" {
		t.Errorf("groupKey = %q", m.GroupKey)
	}
}

func TestMapOpenAICompat_SkipsDisabled(t *testing.T) {
	entries := []config.OpenAICompatibility{
		{
			Name:     "disabled-provider",
			Disabled: true,
			APIKeyEntries: []config.OpenAICompatibilityAPIKey{
				{APIKey: "key-1"},
			},
		},
	}

	mappings := MapOpenAICompat(entries)
	if len(mappings) != 0 {
		t.Fatalf("expected 0 mappings (disabled), got %d", len(mappings))
	}
}

func TestCollectAllMappings(t *testing.T) {
	cfg := &config.Config{}
	cfg.ClaudeKey = []config.ClaudeKey{
		{APIKey: "sk-claude-1", Priority: 10},
	}
	cfg.GeminiKey = []config.GeminiKey{
		{APIKey: "AIza-1", Priority: 6},
	}
	cfg.CodexKey = []config.CodexKey{
		{APIKey: "sk-codex-1", Priority: 7},
	}
	cfg.OpenAICompatibility = []config.OpenAICompatibility{
		{
			Name:    "test",
			BaseURL: "https://example.com",
			APIKeyEntries: []config.OpenAICompatibilityAPIKey{
				{APIKey: "key-1"},
			},
		},
	}

	mappings := CollectAllMappings(cfg)
	if len(mappings) != 4 {
		t.Errorf("expected 4 mappings, got %d", len(mappings))
	}
}

func TestDeriveGroups(t *testing.T) {
	mappings := []AccountMapping{
		{GroupKey: "CPA-anthropic-apikey-P10", Platform: "anthropic", CPAPriority: 10},
		{GroupKey: "CPA-anthropic-apikey-P10", Platform: "anthropic", CPAPriority: 10},
		{GroupKey: "CPA-gemini-apikey-P6", Platform: "gemini", CPAPriority: 6},
		{GroupKey: "CPA-openai-codex-P7", Platform: "openai", CPAPriority: 7},
	}

	groups := DeriveGroups(mappings)
	if len(groups) != 3 {
		t.Fatalf("expected 3 unique groups, got %d", len(groups))
	}
}

func TestAccountNeedsUpdate_NoChange(t *testing.T) {
	existing := &sub2api.Account{
		Name:     "claude-apikey-abc123",
		Type:     "apikey",
		Priority: 5,
		Status:   "active",
		Extra: map[string]any{
			extraKeyCPAImported: "2099-01-01T00:00:00Z",
		},
	}
	mapping := &AccountMapping{
		CreateInput: sub2api.CreateAccountInput{
			Name:     "claude-apikey-abc123",
			Type:     "apikey",
			Priority: 5,
		},
	}
	if accountNeedsUpdate(existing, mapping) {
		t.Error("should not need update when fields match")
	}
}

func TestAccountNeedsUpdate_PriorityChanged(t *testing.T) {
	existing := &sub2api.Account{
		Name:     "claude-apikey-abc123",
		Type:     "apikey",
		Priority: 5,
		Status:   "active",
		Extra: map[string]any{
			extraKeyCPAImported: "2099-01-01T00:00:00Z",
		},
	}
	mapping := &AccountMapping{
		CreateInput: sub2api.CreateAccountInput{
			Name:     "claude-apikey-abc123",
			Type:     "apikey",
			Priority: 10,
		},
	}
	if !accountNeedsUpdate(existing, mapping) {
		t.Error("should need update when priority changed")
	}
}

func TestAccountNeedsUpdate_DisabledAccount(t *testing.T) {
	existing := &sub2api.Account{
		Name:     "claude-apikey-abc123",
		Type:     "apikey",
		Priority: 5,
		Status:   "disabled",
	}
	mapping := &AccountMapping{
		CreateInput: sub2api.CreateAccountInput{
			Name:     "claude-apikey-abc123",
			Type:     "apikey",
			Priority: 5,
		},
	}
	if !accountNeedsUpdate(existing, mapping) {
		t.Error("should need update when account is disabled (re-enable)")
	}
}

func TestMapGeminiKeys_SkipsEmptyAPIKey(t *testing.T) {
	keys := []config.GeminiKey{
		{APIKey: "", Priority: 5},
	}

	mappings := MapGeminiKeys(keys)
	if len(mappings) != 0 {
		t.Fatalf("expected 0 mappings (empty api key), got %d", len(mappings))
	}
}

func TestCredentialsChanged_SameKeys(t *testing.T) {
	old := map[string]any{"api_key": "sk-123", "base_url": "https://api.example.com"}
	new := map[string]any{"api_key": "sk-123", "base_url": "https://api.example.com"}
	if credentialsChanged(old, new) {
		t.Error("identical credentials should not be detected as changed")
	}
}

func TestCredentialsChanged_SecretRotated(t *testing.T) {
	old := map[string]any{
		"auth_mode":             "sigv4",
		"aws_access_key_id":     "AKIA_TEST",
		"aws_secret_access_key": "old-secret",
		"aws_region":            "us-east-1",
	}
	new := map[string]any{
		"auth_mode":             "sigv4",
		"aws_access_key_id":     "AKIA_TEST",
		"aws_secret_access_key": "new-secret",
		"aws_region":            "us-east-1",
	}
	if !credentialsChanged(old, new) {
		t.Error("rotated secret key should be detected as changed")
	}
}

func TestCredentialsChanged_IgnoresExtraFields(t *testing.T) {
	old := map[string]any{"api_key": "sk-123", "some_metadata": "old"}
	new := map[string]any{"api_key": "sk-123", "some_metadata": "new"}
	if credentialsChanged(old, new) {
		t.Error("non-credential fields should be ignored")
	}
}

func TestMakeExtra_CPAKeysCannotBeOverridden(t *testing.T) {
	extra := makeExtra("claude-key", "abc123", map[string]any{
		extraKeyCPASource: false,
		"prefix":          "team-a",
	})
	if extra[extraKeyCPASource] != true {
		t.Error("caller should not be able to override cpa_source")
	}
}

func TestMakeExtra(t *testing.T) {
	extra := makeExtra("claude-key", "abc123", map[string]any{
		"prefix": "team-a",
	})

	if extra[extraKeyCPASource] != true {
		t.Error("cpa_source should be true")
	}
	if extra[extraKeyCPAStableID] != "abc123" {
		t.Errorf("cpa_stable_id = %v, want abc123", extra[extraKeyCPAStableID])
	}
	if extra[extraKeyCPAProvider] != "claude-key" {
		t.Errorf("cpa_provider = %v", extra[extraKeyCPAProvider])
	}
	if extra["prefix"] != "team-a" {
		t.Errorf("prefix = %v", extra["prefix"])
	}
	if _, ok := extra[extraKeyCPAImported]; !ok {
		t.Error("cpa_imported_at should be set")
	}
}
