//go:build commercial

package commercial

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	sub2api "github.com/Wei-Shaw/sub2api/pkg/types"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
)

const (
	cpaPlatformAnthropic = "anthropic"
	cpaPlatformOpenAI    = "openai"
	cpaPlatformGemini    = "gemini"

	cpaAccountTypeAPIKey  = "apikey"
	cpaAccountTypeBedrock = "bedrock"

	extraKeyCPASource    = "cpa_source"
	extraKeyCPAStableID  = "cpa_stable_id"
	extraKeyCPAImported  = "cpa_imported_at"
	extraKeyCPAProvider  = "cpa_provider"
)

// AccountMapping holds the converted Sub2API input alongside CPA metadata.
type AccountMapping struct {
	StableID    string
	Platform    string
	GroupKey    string // "<platform>-<provider_type>-P<priority>"
	CreateInput sub2api.CreateAccountInput
	CPAPriority int
}

// convertPriority maps CPA priority (higher=better) to Sub2API (lower=better).
func convertPriority(cpaPriority int) int {
	p := 50 - (cpaPriority * 5)
	if p < 1 {
		return 1
	}
	if p > 100 {
		return 100
	}
	return p
}

// stableID generates a deterministic identifier for a CPA credential.
func stableID(providerType, fingerprint string) string {
	h := sha256.Sum256([]byte(providerType + ":" + fingerprint))
	return hex.EncodeToString(h[:8])
}

func makeExtra(provider, stableID string, extra map[string]any) map[string]any {
	m := map[string]any{}
	for k, v := range extra {
		m[k] = v
	}
	// CPA metadata written last to prevent caller override
	m[extraKeyCPASource] = true
	m[extraKeyCPAStableID] = stableID
	m[extraKeyCPAImported] = time.Now().UTC().Format(time.RFC3339)
	m[extraKeyCPAProvider] = provider
	return m
}

func groupKey(platform, providerType string, cpaPriority int) string {
	return fmt.Sprintf("CPA-%s-%s-P%d", platform, providerType, cpaPriority)
}

// MapClaudeKeys converts CPA ClaudeKey entries to AccountMappings.
func MapClaudeKeys(keys []config.ClaudeKey) []AccountMapping {
	var result []AccountMapping
	for _, k := range keys {
		if k.Disabled {
			continue
		}

		isBedrock := k.AWSAccessKeyID != ""
		isVertexAI := k.GCPCredentialsFile != ""

		// Skip Vertex AI Claude (Sub2API has no vertex type)
		if isVertexAI {
			continue
		}

		// Skip entries with no usable credentials
		if !isBedrock && k.APIKey == "" {
			continue
		}

		var accountType, fingerprint string
		creds := map[string]any{}
		providerSubtype := "apikey"

		if isBedrock {
			accountType = cpaAccountTypeBedrock
			providerSubtype = "bedrock"
			fingerprint = k.AWSAccessKeyID + ":" + k.AWSRegion
			creds["auth_mode"] = "sigv4"
			creds["aws_access_key_id"] = k.AWSAccessKeyID
			creds["aws_secret_access_key"] = k.AWSSecretAccessKey
			if k.AWSRegion != "" {
				creds["aws_region"] = k.AWSRegion
			}
		} else {
			accountType = cpaAccountTypeAPIKey
			fingerprint = k.APIKey
			creds["api_key"] = k.APIKey
			if k.BaseURL != "" {
				creds["base_url"] = k.BaseURL
			}
		}

		sid := stableID("claude-key", fingerprint)
		name := fmt.Sprintf("claude-%s-%s", providerSubtype, sid[:8])

		extra := map[string]any{}
		if len(k.ExcludedModels) > 0 {
			extra["excluded_models"] = k.ExcludedModels
		}
		if k.Prefix != "" {
			extra["prefix"] = k.Prefix
		}
		if k.AuthStyle != "" {
			extra["auth_style"] = k.AuthStyle
		}

		gk := groupKey(cpaPlatformAnthropic, providerSubtype, k.Priority)
		extra["cpa_group_key"] = gk

		result = append(result, AccountMapping{
			StableID:    sid,
			Platform:    cpaPlatformAnthropic,
			GroupKey:    gk,
			CPAPriority: k.Priority,
			CreateInput: sub2api.CreateAccountInput{
				Name:                  name,
				Platform:              cpaPlatformAnthropic,
				Type:                  accountType,
				Credentials:           creds,
				Extra:                 makeExtra("claude-key", sid, extra),
				Concurrency:           3,
				Priority:              convertPriority(k.Priority),
				SkipDefaultGroupBind:  true,
				SkipMixedChannelCheck: true,
			},
		})
	}
	return result
}

// MapGeminiKeys converts CPA GeminiKey entries to AccountMappings.
func MapGeminiKeys(keys []config.GeminiKey) []AccountMapping {
	var result []AccountMapping
	for _, k := range keys {
		if k.APIKey == "" {
			continue
		}
		sid := stableID("gemini-key", k.APIKey)
		name := fmt.Sprintf("gemini-%s", sid[:8])

		creds := map[string]any{"api_key": k.APIKey}
		if k.BaseURL != "" {
			creds["base_url"] = k.BaseURL
		}

		extra := map[string]any{}
		if len(k.ExcludedModels) > 0 {
			extra["excluded_models"] = k.ExcludedModels
		}
		if k.Prefix != "" {
			extra["prefix"] = k.Prefix
		}

		gk := groupKey(cpaPlatformGemini, "apikey", k.Priority)
		extra["cpa_group_key"] = gk

		result = append(result, AccountMapping{
			StableID:    sid,
			Platform:    cpaPlatformGemini,
			GroupKey:    gk,
			CPAPriority: k.Priority,
			CreateInput: sub2api.CreateAccountInput{
				Name:                  name,
				Platform:              cpaPlatformGemini,
				Type:                  cpaAccountTypeAPIKey,
				Credentials:           creds,
				Extra:                 makeExtra("gemini-key", sid, extra),
				Concurrency:           3,
				Priority:              convertPriority(k.Priority),
				SkipDefaultGroupBind:  true,
				SkipMixedChannelCheck: true,
			},
		})
	}
	return result
}

// MapCodexKeys converts CPA CodexKey entries to AccountMappings.
func MapCodexKeys(keys []config.CodexKey) []AccountMapping {
	var result []AccountMapping
	for _, k := range keys {
		if k.APIKey == "" {
			continue
		}
		sid := stableID("codex-key", k.APIKey)
		name := fmt.Sprintf("codex-%s", sid[:8])

		creds := map[string]any{"api_key": k.APIKey}
		if k.BaseURL != "" {
			creds["base_url"] = k.BaseURL
		}

		extra := map[string]any{}
		if len(k.ExcludedModels) > 0 {
			extra["excluded_models"] = k.ExcludedModels
		}
		if k.Prefix != "" {
			extra["prefix"] = k.Prefix
		}

		gk := groupKey(cpaPlatformOpenAI, "codex", k.Priority)
		extra["cpa_group_key"] = gk

		result = append(result, AccountMapping{
			StableID:    sid,
			Platform:    cpaPlatformOpenAI,
			GroupKey:    gk,
			CPAPriority: k.Priority,
			CreateInput: sub2api.CreateAccountInput{
				Name:                  name,
				Platform:              cpaPlatformOpenAI,
				Type:                  cpaAccountTypeAPIKey,
				Credentials:           creds,
				Extra:                 makeExtra("codex-key", sid, extra),
				Concurrency:           3,
				Priority:              convertPriority(k.Priority),
				SkipDefaultGroupBind:  true,
				SkipMixedChannelCheck: true,
			},
		})
	}
	return result
}

// MapOpenAICompat converts CPA OpenAICompatibility entries to AccountMappings.
// Each api-key-entry becomes a separate Account.
func MapOpenAICompat(entries []config.OpenAICompatibility) []AccountMapping {
	var result []AccountMapping
	for _, entry := range entries {
		if entry.Disabled {
			continue
		}

		platform := cpaPlatformOpenAI
		if strings.EqualFold(entry.AuthStyle, "anthropic") {
			platform = cpaPlatformAnthropic
		}

		providerSubtype := "compat"

		for _, apiKeyEntry := range entry.APIKeyEntries {
			if apiKeyEntry.APIKey == "" {
				continue
			}

			fingerprint := entry.Name + ":" + entry.BaseURL + ":" + apiKeyEntry.APIKey
			sid := stableID("openai-compat", fingerprint)

			name := fmt.Sprintf("%s-%s", sanitizeName(entry.Name), sid[:8])

			creds := map[string]any{
				"api_key":  apiKeyEntry.APIKey,
				"base_url": entry.BaseURL,
			}

			extra := map[string]any{
				"compat_name": entry.Name,
			}
			if entry.EndpointPath != "" {
				extra["endpoint_path"] = entry.EndpointPath
			}
			if entry.ResponsesFormat {
				extra["responses_format"] = true
			}
			if entry.AuthStyle != "" {
				extra["auth_style"] = entry.AuthStyle
			}
			if len(entry.ExcludedModels) > 0 {
				extra["excluded_models"] = entry.ExcludedModels
			}
			if entry.Prefix != "" {
				extra["prefix"] = entry.Prefix
			}

			gk := groupKey(platform, providerSubtype, entry.Priority)
			extra["cpa_group_key"] = gk

			result = append(result, AccountMapping{
				StableID:    sid,
				Platform:    platform,
				GroupKey:    gk,
				CPAPriority: entry.Priority,
				CreateInput: sub2api.CreateAccountInput{
					Name:                  name,
					Platform:              platform,
					Type:                  cpaAccountTypeAPIKey,
					Credentials:           creds,
					Extra:                 makeExtra("openai-compat", sid, extra),
					Concurrency:           3,
					Priority:              convertPriority(entry.Priority),
					SkipDefaultGroupBind:  true,
					SkipMixedChannelCheck: true,
				},
			})
		}
	}
	return result
}

// CollectAllMappings gathers Account mappings from all CPA config key types.
func CollectAllMappings(cfg *config.Config) []AccountMapping {
	var all []AccountMapping
	all = append(all, MapClaudeKeys(cfg.ClaudeKey)...)
	all = append(all, MapGeminiKeys(cfg.GeminiKey)...)
	all = append(all, MapCodexKeys(cfg.CodexKey)...)
	all = append(all, MapOpenAICompat(cfg.OpenAICompatibility)...)
	return all
}

// GroupSpec describes a Sub2API Group to be created.
type GroupSpec struct {
	Name        string
	Platform    string
	CPAPriority int
}

// DeriveGroups extracts unique Group specs from Account mappings.
func DeriveGroups(mappings []AccountMapping) []GroupSpec {
	seen := map[string]bool{}
	var groups []GroupSpec
	for _, m := range mappings {
		if seen[m.GroupKey] {
			continue
		}
		seen[m.GroupKey] = true
		groups = append(groups, GroupSpec{
			Name:        m.GroupKey,
			Platform:    m.Platform,
			CPAPriority: m.CPAPriority,
		})
	}
	return groups
}

func sanitizeName(s string) string {
	s = strings.ToLower(s)
	s = strings.ReplaceAll(s, " ", "-")
	if len(s) > 40 {
		s = s[:40]
	}
	return s
}
