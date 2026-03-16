package auth

import (
	"sort"
	"strings"
)

// skyworkModelCapability describes a model's capability for Skywork smart fallback.
type skyworkModelCapability struct {
	Name       string
	Family     string // "claude" or "gpt"
	Tier       int    // 1 = strongest, 2 = mid, 3 = weakest
	CodingRank int    // global coding rank (lower = better), used as fallback priority
	MaxEffort  string // max reasoning effort level for this model
}

// skyworkModelTable is the built-in capability table for Skywork-hosted models.
// Ordered by CodingRank (strongest first).
var skyworkModelTable = []skyworkModelCapability{
	{Name: "claude-opus-4.6", Family: "claude", Tier: 1, CodingRank: 1, MaxEffort: "high"},
	{Name: "gpt-5.4", Family: "gpt", Tier: 1, CodingRank: 2, MaxEffort: "xhigh"},
	{Name: "claude-sonnet-4.6", Family: "claude", Tier: 2, CodingRank: 3, MaxEffort: "high"},
	{Name: "gpt-5.3-codex", Family: "gpt", Tier: 2, CodingRank: 4, MaxEffort: "high"},
	{Name: "claude-opus-4.5", Family: "claude", Tier: 3, CodingRank: 5, MaxEffort: "high"},
	{Name: "claude-sonnet-4.5", Family: "claude", Tier: 3, CodingRank: 6, MaxEffort: "high"},
	{Name: "gpt-5.2", Family: "gpt", Tier: 3, CodingRank: 7, MaxEffort: "high"},
}

// skyworkModelIndex provides fast lookup by model name.
var skyworkModelIndex = func() map[string]skyworkModelCapability {
	m := make(map[string]skyworkModelCapability, len(skyworkModelTable))
	for _, cap := range skyworkModelTable {
		m[cap.Name] = cap
	}
	return m
}()

// skyworkModelFamily returns the family for a known Skywork model, or "" if unknown.
func skyworkModelFamily(model string) string {
	if cap, ok := skyworkModelIndex[model]; ok {
		return cap.Family
	}
	return ""
}

// SkyworkModelFamily is the exported version of skyworkModelFamily.
func SkyworkModelFamily(model string) string {
	return skyworkModelFamily(model)
}

// heavyRequestThreshold is the payload size in bytes above which a request is considered heavy.
const heavyRequestThreshold = 100 * 1024 // 100KB

// IsHeavySkyworkRequest estimates whether a request payload is heavy (large context).
// Uses a simple byte-size heuristic — no tokenization.
func IsHeavySkyworkRequest(payload []byte) bool {
	return len(payload) > heavyRequestThreshold
}

// PlanSkyworkFallbackChain returns an ordered list of models to try for a Skywork request.
// The requested model is always first. If availableModels is nil/empty, returns only the
// requested model (fallback disabled). Unknown models not in the capability table also
// return only the requested model.
//
// For light requests: same-family models first (by CodingRank), then cross-family.
// For heavy requests: same-tier cross-family model first, then remaining by CodingRank.
func PlanSkyworkFallbackChain(requestedModel string, availableModels []string, heavy bool) []string {
	if len(availableModels) == 0 {
		return []string{requestedModel}
	}

	reqCap, known := skyworkModelIndex[requestedModel]
	if !known {
		return []string{requestedModel}
	}

	// Build set of available models for O(1) lookup.
	availSet := make(map[string]bool, len(availableModels))
	for _, m := range availableModels {
		availSet[m] = true
	}

	// Collect eligible candidates (in capability table AND in available list), excluding requested.
	var candidates []skyworkModelCapability
	for _, cap := range skyworkModelTable {
		if cap.Name == requestedModel {
			continue
		}
		if !availSet[cap.Name] {
			continue
		}
		candidates = append(candidates, cap)
	}

	if heavy {
		// Heavy: same-tier cross-family first, then remaining by CodingRank.
		sort.SliceStable(candidates, func(i, j int) bool {
			ci, cj := candidates[i], candidates[j]
			iSameTierCross := ci.Tier == reqCap.Tier && ci.Family != reqCap.Family
			jSameTierCross := cj.Tier == reqCap.Tier && cj.Family != reqCap.Family
			if iSameTierCross != jSameTierCross {
				return iSameTierCross
			}
			return ci.CodingRank < cj.CodingRank
		})
	} else {
		// Light: same-family first by CodingRank, then cross-family by CodingRank.
		sort.SliceStable(candidates, func(i, j int) bool {
			ci, cj := candidates[i], candidates[j]
			iSame := ci.Family == reqCap.Family
			jSame := cj.Family == reqCap.Family
			if iSame != jSame {
				return iSame
			}
			return ci.CodingRank < cj.CodingRank
		})
	}

	result := make([]string, 0, len(candidates)+1)
	result = append(result, requestedModel)
	for _, c := range candidates {
		result = append(result, c.Name)
	}
	return result
}

// MapCrossFamilyReasoningEffort maps a reasoning_effort value when falling back across
// model families. The principle is relative effort: max on source = max on target.
//
// Claude max = "high", GPT 5.4 max = "xhigh".
// When fromFamily == toFamily, returns the input unchanged.
func MapCrossFamilyReasoningEffort(effort, fromFamily, toFamily string) string {
	effort = strings.ToLower(strings.TrimSpace(effort))
	if fromFamily == toFamily || effort == "" || effort == "none" || effort == "off" || effort == "disabled" {
		return effort
	}

	if fromFamily == "claude" && toFamily == "gpt" {
		// Claude levels shift up one notch on GPT scale.
		switch effort {
		case "high", "max":
			return "xhigh"
		case "medium":
			return "high"
		case "low":
			return "medium"
		case "minimal":
			return "low"
		default:
			return effort
		}
	}

	if fromFamily == "gpt" && toFamily == "claude" {
		// GPT levels shift down one notch on Claude scale, capped at "high".
		switch effort {
		case "xhigh", "max", "high":
			return "high"
		case "medium":
			return "medium"
		case "low":
			return "low"
		case "minimal":
			return "minimal"
		default:
			return effort
		}
	}

	return effort
}

// IsSkyworkFallbackAuth checks if an auth entry belongs to the Skywork provider
// (skywork, skyclaw, or singularity with desktop-llm.skywork.ai base URL).
func IsSkyworkFallbackAuth(a *Auth) bool {
	if a == nil {
		return false
	}
	provider := strings.ToLower(strings.TrimSpace(a.Provider))
	if strings.Contains(provider, "skywork") || strings.Contains(provider, "skyclaw") {
		return true
	}
	if baseURL, ok := a.Attributes["base_url"]; ok {
		if strings.Contains(strings.ToLower(baseURL), "desktop-llm.skywork.ai") {
			return true
		}
	}
	if compatName, ok := a.Attributes["compat_name"]; ok {
		lower := strings.ToLower(strings.TrimSpace(compatName))
		if strings.Contains(lower, "skywork") || strings.Contains(lower, "skyclaw") {
			return true
		}
	}
	return false
}
