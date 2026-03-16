package auth

import (
	"sort"
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
)

// skyworkGlobalCooldown tracks model failures across all Skywork accounts.
// Since all Skywork accounts share the same upstream domain (desktop-llm.skywork.ai),
// a model failure on one account means the same model will fail on all accounts.
// Uses exponential backoff: 2min → 5min → 10min (cap), reset on success.
var skyworkGlobalCooldown = struct {
	mu           sync.RWMutex
	entries      map[string]time.Time // model name -> cooldown expiry
	failureCounts map[string]int       // model name -> consecutive failure count
}{
	entries:      make(map[string]time.Time),
	failureCounts: make(map[string]int),
}

// skyworkCooldownSteps defines exponential backoff durations.
// Observed failure windows last 10-20 minutes, so the cap is 10 minutes.
var skyworkCooldownSteps = []time.Duration{
	2 * time.Minute,  // 1st failure
	5 * time.Minute,  // 2nd consecutive failure
	10 * time.Minute, // 3rd+ consecutive failure (cap)
}

// MarkSkyworkModelCooldown records that a model has failed and should be
// skipped across all Skywork accounts. Duration increases with consecutive failures.
func MarkSkyworkModelCooldown(model string) {
	skyworkGlobalCooldown.mu.Lock()
	count := skyworkGlobalCooldown.failureCounts[model]
	stepIdx := count
	if stepIdx >= len(skyworkCooldownSteps) {
		stepIdx = len(skyworkCooldownSteps) - 1
	}
	duration := skyworkCooldownSteps[stepIdx]
	skyworkGlobalCooldown.entries[model] = time.Now().Add(duration)
	skyworkGlobalCooldown.failureCounts[model] = count + 1
	skyworkGlobalCooldown.mu.Unlock()
	log.WithFields(log.Fields{
		"event":          "skywork-global-cooldown",
		"model":          model,
		"duration":       duration.String(),
		"failure_count":  count + 1,
	}).Infof("[skywork-fallback] %s globally cooled down for %s (failure #%d)", model, duration, count+1)
}

// ClearSkyworkModelCooldown clears the cooldown and resets the failure count
// for a model after it succeeds.
func ClearSkyworkModelCooldown(model string) {
	skyworkGlobalCooldown.mu.Lock()
	if _, had := skyworkGlobalCooldown.entries[model]; had {
		log.WithFields(log.Fields{
			"event": "skywork-global-cooldown-cleared",
			"model": model,
		}).Infof("[skywork-fallback] %s cooldown cleared (model recovered)", model)
	}
	delete(skyworkGlobalCooldown.entries, model)
	delete(skyworkGlobalCooldown.failureCounts, model)
	skyworkGlobalCooldown.mu.Unlock()
}

// IsSkyworkModelCooledDown returns true if the model is in global cooldown.
func IsSkyworkModelCooledDown(model string) bool {
	skyworkGlobalCooldown.mu.RLock()
	expiry, ok := skyworkGlobalCooldown.entries[model]
	skyworkGlobalCooldown.mu.RUnlock()
	if !ok {
		return false
	}
	if time.Now().After(expiry) {
		// Expired — clean up lazily but keep failure count for backoff escalation.
		skyworkGlobalCooldown.mu.Lock()
		if e, exists := skyworkGlobalCooldown.entries[model]; exists && time.Now().After(e) {
			delete(skyworkGlobalCooldown.entries, model)
		}
		skyworkGlobalCooldown.mu.Unlock()
		return false
	}
	return true
}

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

// IsHeavySkyworkRequest estimates whether a request is heavy (large context).
// A request is considered heavy if:
// - Payload exceeds the byte-size threshold (100KB), OR
// - The request uses 1M context mode (detected via Anthropic beta header or betas field)
func IsHeavySkyworkRequest(payload []byte, originalRequest []byte) bool {
	if len(payload) > heavyRequestThreshold {
		return true
	}
	// Check for 1M context mode in original request payload.
	// Claude CLI sends betas in the request body or Anthropic-Beta header.
	for _, src := range [][]byte{originalRequest, payload} {
		if len(src) == 0 {
			continue
		}
		betas := strings.ToLower(gjson.GetBytes(src, "betas").String())
		if strings.Contains(betas, "context-1m") {
			return true
		}
		// Also check anthropic_beta / anthropic-beta fields
		for _, field := range []string{"anthropic_beta", "anthropic-beta"} {
			v := strings.ToLower(gjson.GetBytes(src, field).String())
			if strings.Contains(v, "context-1m") {
				return true
			}
		}
	}
	return false
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
	// Only T1 and T2 models participate in fallback as the originator.
	// T3 models are too weak to justify fallback overhead.
	if reqCap.Tier > 2 {
		return []string{requestedModel}
	}

	// Build set of available models for O(1) lookup.
	availSet := make(map[string]bool, len(availableModels))
	for _, m := range availableModels {
		availSet[m] = true
	}

	// Collect eligible candidates (in capability table AND in available list), excluding requested.
	// Only T1 and T2 models participate in fallback to keep the chain short and avoid
	// blocking account rotation with too many slow retries on weak models.
	var candidates []skyworkModelCapability
	for _, cap := range skyworkModelTable {
		if cap.Name == requestedModel {
			continue
		}
		if !availSet[cap.Name] {
			continue
		}
		if cap.Tier > 2 {
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

// LogSkyworkFallbackEvent logs a structured entry when a Skywork model fails and
// the next fallback candidate is about to be tried. Designed for easy filtering
// with grep: all entries contain "[skywork-fallback]".
func LogSkyworkFallbackEvent(failedModel, nextModel, provider, authID string, httpStatus int, errMsg string) {
	log.WithFields(log.Fields{
		"event":        "skywork-fallback",
		"failed_model": failedModel,
		"next_model":   nextModel,
		"provider":     provider,
		"auth_id":      authID,
		"http_status":  httpStatus,
		"error":        errMsg,
	}).Warnf("[skywork-fallback] %s failed (HTTP %d), falling back to %s: %s",
		failedModel, httpStatus, nextModel, errMsg)
}

// LogSkyworkFallbackExhausted logs when all fallback models have been exhausted.
func LogSkyworkFallbackExhausted(requestedModel, provider, authID string, errMsg string) {
	log.WithFields(log.Fields{
		"event":           "skywork-fallback-exhausted",
		"requested_model": requestedModel,
		"provider":        provider,
		"auth_id":         authID,
		"error":           errMsg,
	}).Errorf("[skywork-fallback] all models exhausted for %s: %s", requestedModel, errMsg)
}

// LogSkyworkFallbackSuccess logs when a fallback model succeeds.
func LogSkyworkFallbackSuccess(requestedModel, fallbackModel, provider, authID string) {
	log.WithFields(log.Fields{
		"event":           "skywork-fallback-success",
		"requested_model": requestedModel,
		"fallback_model":  fallbackModel,
		"provider":        provider,
		"auth_id":         authID,
	}).Infof("[skywork-fallback] %s failed, recovered via %s", requestedModel, fallbackModel)
}
