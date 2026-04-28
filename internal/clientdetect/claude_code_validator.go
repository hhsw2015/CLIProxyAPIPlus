package clientdetect

import (
	"context"
	"net/http"
	"regexp"
	"strconv"
	"strings"
)

type contextKey string

const (
	ctxKeyIsClaudeCode    contextKey = "clientdetect.isClaudeCode"
	ctxKeyClaudeCodeVersion contextKey = "clientdetect.claudeCodeVersion"
)

// ClaudeCodeValidator validates whether a request comes from a Claude Code client.
type ClaudeCodeValidator struct{}

var (
	// User-Agent match: claude-cli/x.x.x (official CLI only, case-insensitive)
	claudeCodeUAPattern = regexp.MustCompile(`(?i)^claude-cli/\d+\.\d+\.\d+`)

	// System prompt similarity threshold (0.5, matching claude-relay-service)
	systemPromptThreshold = 0.5
)

// Claude Code official system prompt templates.
var claudeCodeSystemPrompts = []string{
	// claudeOtherSystemPrompt1 - Primary
	"You are Claude Code, Anthropic's official CLI for Claude.",

	// claudeOtherSystemPrompt3 - Agent SDK
	"You are a Claude agent, built on Anthropic's Claude Agent SDK.",

	// claudeOtherSystemPrompt4 - Compact Agent SDK
	"You are Claude Code, Anthropic's official CLI for Claude, running within the Claude Agent SDK.",

	// exploreAgentSystemPrompt
	"You are a file search specialist for Claude Code, Anthropic's official CLI for Claude.",

	// claudeOtherSystemPromptCompact - Compact (conversation summary)
	"You are a helpful AI assistant tasked with summarizing conversations.",

	// claudeOtherSystemPrompt2 - Secondary (key part of long prompt)
	"You are an interactive CLI tool that helps users",
}

// NewClaudeCodeValidator creates a validator instance.
func NewClaudeCodeValidator() *ClaudeCodeValidator {
	return &ClaudeCodeValidator{}
}

// Validate checks whether a request comes from the Claude Code CLI.
//
//	Step 1: User-Agent check (required) - must be claude-cli/x.x.x
//	Step 2: For non-messages paths, UA match is sufficient
//	Step 3: Check max_tokens=1 + haiku probe request bypass (UA already verified)
//	Step 4: For messages paths, strict validation:
//	        - System prompt similarity check
//	        - X-App header check
//	        - anthropic-beta header check
//	        - anthropic-version header check
//	        - metadata.user_id format validation
func (v *ClaudeCodeValidator) Validate(r *http.Request, body map[string]any) bool {
	// Step 1: User-Agent check
	ua := r.Header.Get("User-Agent")
	if !claudeCodeUAPattern.MatchString(ua) {
		return false
	}

	// Step 2: Non-messages path - UA match is sufficient
	path := r.URL.Path
	if !strings.Contains(path, "messages") {
		return true
	}

	// Step 3: Check max_tokens=1 + haiku probe request bypass
	if isMaxTokensOneHaikuRequest(body) {
		return true
	}

	// Step 4: messages path - strict validation

	// 4.1 Check system prompt similarity
	if !v.hasClaudeCodeSystemPrompt(body) {
		return false
	}

	// 4.2 Check required headers (non-empty)
	xApp := r.Header.Get("X-App")
	if xApp == "" {
		return false
	}

	anthropicBeta := r.Header.Get("anthropic-beta")
	if anthropicBeta == "" {
		return false
	}

	anthropicVersion := r.Header.Get("anthropic-version")
	if anthropicVersion == "" {
		return false
	}

	// 4.3 Validate metadata.user_id
	if body == nil {
		return false
	}

	metadata, ok := body["metadata"].(map[string]any)
	if !ok {
		return false
	}

	userID, ok := metadata["user_id"].(string)
	if !ok || userID == "" {
		return false
	}

	if ParseMetadataUserID(userID) == nil {
		return false
	}

	return true
}

// isMaxTokensOneHaikuRequest checks if the body represents a max_tokens=1 haiku
// probe request used by Claude Code to verify API connectivity.
func isMaxTokensOneHaikuRequest(body map[string]any) bool {
	if body == nil {
		return false
	}
	maxTokens, ok := body["max_tokens"].(float64)
	if !ok || maxTokens != 1 {
		return false
	}
	model, _ := body["model"].(string)
	return strings.Contains(strings.ToLower(model), "haiku")
}

// hasClaudeCodeSystemPrompt checks whether the request contains a Claude Code system prompt.
// Uses string similarity matching (Dice coefficient).
func (v *ClaudeCodeValidator) hasClaudeCodeSystemPrompt(body map[string]any) bool {
	if body == nil {
		return false
	}

	// Check model field
	if _, ok := body["model"].(string); !ok {
		return false
	}

	// Get system field
	systemEntries, ok := body["system"].([]any)
	if !ok {
		return false
	}

	// Check each system entry
	for _, entry := range systemEntries {
		entryMap, ok := entry.(map[string]any)
		if !ok {
			continue
		}

		text, ok := entryMap["text"].(string)
		if !ok || text == "" {
			continue
		}

		// Compute best similarity against all templates
		bestScore := v.bestSimilarityScore(text)
		if bestScore >= systemPromptThreshold {
			return true
		}
	}

	return false
}

// bestSimilarityScore computes the best similarity between text and all Claude Code templates.
func (v *ClaudeCodeValidator) bestSimilarityScore(text string) float64 {
	normalizedText := normalizePrompt(text)
	bestScore := 0.0

	for _, template := range claudeCodeSystemPrompts {
		normalizedTemplate := normalizePrompt(template)
		score := diceCoefficient(normalizedText, normalizedTemplate)
		if score > bestScore {
			bestScore = score
		}
	}

	return bestScore
}

// normalizePrompt normalizes prompt text (collapse whitespace).
func normalizePrompt(text string) string {
	return strings.Join(strings.Fields(text), " ")
}

// diceCoefficient computes the Dice coefficient (Sorensen-Dice coefficient) of two strings.
// Formula: 2 * |intersection| / (|bigrams(a)| + |bigrams(b)|)
func diceCoefficient(a, b string) float64 {
	if a == b {
		return 1.0
	}

	if len(a) < 2 || len(b) < 2 {
		return 0.0
	}

	bigramsA := getBigrams(a)
	bigramsB := getBigrams(b)

	if len(bigramsA) == 0 || len(bigramsB) == 0 {
		return 0.0
	}

	// Compute intersection size
	intersection := 0
	for bigram, countA := range bigramsA {
		if countB, exists := bigramsB[bigram]; exists {
			if countA < countB {
				intersection += countA
			} else {
				intersection += countB
			}
		}
	}

	// Compute total bigram counts
	totalA := 0
	for _, count := range bigramsA {
		totalA += count
	}
	totalB := 0
	for _, count := range bigramsB {
		totalB += count
	}

	return float64(2*intersection) / float64(totalA+totalB)
}

// getBigrams returns all bigrams (adjacent character pairs) of a string.
func getBigrams(s string) map[string]int {
	bigrams := make(map[string]int)
	runes := []rune(strings.ToLower(s))

	for i := 0; i < len(runes)-1; i++ {
		bigram := string(runes[i : i+2])
		bigrams[bigram]++
	}

	return bigrams
}

// ValidateUserAgent validates only the User-Agent (for scenarios that don't need body parsing).
func (v *ClaudeCodeValidator) ValidateUserAgent(ua string) bool {
	return claudeCodeUAPattern.MatchString(ua)
}

// IncludesClaudeCodeSystemPrompt checks if the request body contains a Claude Code system prompt.
// Returns true if any matching system prompt is found (for loose detection).
func (v *ClaudeCodeValidator) IncludesClaudeCodeSystemPrompt(body map[string]any) bool {
	return v.hasClaudeCodeSystemPrompt(body)
}

// IsClaudeCodeClient reads the Claude Code client flag from context.
func IsClaudeCodeClient(ctx context.Context) bool {
	if v, ok := ctx.Value(ctxKeyIsClaudeCode).(bool); ok {
		return v
	}
	return false
}

// SetClaudeCodeClient sets the Claude Code client flag in context.
func SetClaudeCodeClient(ctx context.Context, isClaudeCode bool) context.Context {
	return context.WithValue(ctx, ctxKeyIsClaudeCode, isClaudeCode)
}

// ExtractVersion extracts the Claude Code version from a User-Agent string.
// Returns a version like "2.1.22", or empty string if not matched.
func (v *ClaudeCodeValidator) ExtractVersion(ua string) string {
	return ExtractCLIVersion(ua)
}

// SetClaudeCodeVersion sets the Claude Code version in context.
func SetClaudeCodeVersion(ctx context.Context, version string) context.Context {
	return context.WithValue(ctx, ctxKeyClaudeCodeVersion, version)
}

// GetClaudeCodeVersion reads the Claude Code version from context.
func GetClaudeCodeVersion(ctx context.Context) string {
	if v, ok := ctx.Value(ctxKeyClaudeCodeVersion).(string); ok {
		return v
	}
	return ""
}

// CompareVersions compares two semver version strings.
// Returns: -1 (a < b), 0 (a == b), 1 (a > b)
func CompareVersions(a, b string) int {
	aParts := parseSemver(a)
	bParts := parseSemver(b)
	for i := 0; i < 3; i++ {
		if aParts[i] < bParts[i] {
			return -1
		}
		if aParts[i] > bParts[i] {
			return 1
		}
	}
	return 0
}

// parseSemver parses a semver string into [major, minor, patch].
func parseSemver(v string) [3]int {
	v = strings.TrimPrefix(v, "v")
	parts := strings.Split(v, ".")
	result := [3]int{0, 0, 0}
	for i := 0; i < len(parts) && i < 3; i++ {
		if parsed, err := strconv.Atoi(parts[i]); err == nil {
			result[i] = parsed
		}
	}
	return result
}
