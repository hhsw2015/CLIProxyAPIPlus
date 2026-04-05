// Package refusal provides lightweight refusal detection for LLM streaming responses.
//
// The detector uses a two-tier matching strategy ported from codex-session-patcher
// (strong phrases + weak prefix keywords) combined with thinking-block stripping
// to achieve high precision with near-zero false positives.
package refusal

import (
	"regexp"
	"strings"
)

// strongPhrases are high-confidence indicators matched anywhere in the response text.
// A single hit is sufficient to classify the response as a refusal.
var strongPhrases = []string{
	// English — canonical refusal sentences
	"i cannot assist",
	"i can't assist",
	"i'm unable to assist",
	"i cannot help with",
	"i can't help with",
	"i'm unable to help",
	"i must decline",
	"i must refuse",
	"i have to decline",
	"against my guidelines",
	"against my safety policy",
	"against my policy",
	"violates my guidelines",
	"violates my policy",
	"i won't help",
	"i won't assist",
	"i'm not able to help",
	"i'm not able to assist",
	"i'm not allowed to",
	"not able to provide",
	"not able to assist",
	"i apologize, but i can't",
	"i'm sorry, but i can't",
	"i'm sorry, but i cannot",
	"my programming prevents",
	"cannot fulfill this request",
	"can't fulfill this request",

	// English — identity-based refusals
	"as an ai",
	"as a language model",
	"as an ai assistant",
	"as a helpful assistant",

	// Chinese — canonical refusal sentences
	"我无法协助",
	"我无法帮助",
	"我不能协助",
	"我不能帮助",
	"我不能做",
	"我无法做",
	"我不会帮",
	"我无法提供",
	"违反了我的",
	"违反我的",
	"这违反",
	"我拒绝",
	"我必须拒绝",
	"作为ai",
	"作为 ai",
	"作为ai助手",
	"作为 ai 助手",
	"作为语言模型",
	"作为 ai 语言模型",
}

// weakPrefixes are lower-confidence keywords that only indicate refusal when they
// appear near the very beginning of the response (within the first prefixWindow chars).
// This prevents false positives like "Sorry, I found a bug on line 10".
var weakPrefixes = []string{
	// English
	"i'm sorry",
	"i am sorry",
	"i apologize",
	"sorry,",
	"unfortunately,",
	"i cannot",
	"i can't",
	"i'm unable",
	"unable to",
	"not permitted",
	"not allowed",
	"refuse to",

	// Chinese
	"抱歉",
	"很抱歉",
	"对不起",
	"不好意思",
	"我无法",
	"我不能",
	"不允许",
	"不能帮你",
	"无法帮你",
	"无法协助",
	"不能为你",
	"无法为你",
	"很遗憾",
}

// safePassthroughPatterns indicate the response contains real content even if a weak
// keyword matched. If any of these appear in the inspected text, we skip detection.
var safePassthroughPatterns = []string{
	"```",     // code block — model is producing code
	"import ", // code statement
	"func ",   // Go function
	"def ",    // Python function
	"class ",  // class definition
}

const (
	// prefixWindow is the number of characters examined for weak-prefix matching.
	prefixWindow = 30
)

// thinkingStripRegex removes <thinking>...</thinking> blocks before detection.
var thinkingStripRegex = regexp.MustCompile(`(?si)<thinking>.*?</thinking>`)

// Detector provides refusal detection with configurable extra patterns.
type Detector struct {
	extraStrong []string
	extraWeak   []string
}

// NewDetector creates a Detector with optional extra patterns from config.
func NewDetector(extraStrong, extraWeak []string) *Detector {
	return &Detector{
		extraStrong: extraStrong,
		extraWeak:   extraWeak,
	}
}

// IsRefusal returns true if the given text is classified as a model refusal.
//
// The detection pipeline is:
//  1. Strip <thinking> blocks so internal reasoning doesn't cause false positives.
//  2. Check safe-passthrough patterns (code blocks, etc.) — if found, return false.
//  3. Strong phrase match — full-text scan for high-confidence indicators.
//  4. Weak prefix match — scan only the first 30 characters for lower-confidence keywords.
func (d *Detector) IsRefusal(text string) bool {
	if len(strings.TrimSpace(text)) == 0 {
		return false
	}

	// Step 1: strip thinking blocks.
	cleaned := thinkingStripRegex.ReplaceAllString(text, "")
	cleaned = strings.TrimSpace(cleaned)
	if len(cleaned) == 0 {
		return false
	}

	lower := strings.ToLower(cleaned)

	// Step 2: safe passthrough — if the text contains code/structured content, skip.
	for _, safe := range safePassthroughPatterns {
		if strings.Contains(lower, safe) {
			return false
		}
	}

	// Step 3: strong phrase match — anywhere in text.
	for _, phrase := range strongPhrases {
		if strings.Contains(lower, phrase) {
			return true
		}
	}
	for _, phrase := range d.extraStrong {
		if strings.Contains(lower, strings.ToLower(phrase)) {
			return true
		}
	}

	// Step 4: weak prefix match — the response must *start with* a weak keyword.
	// We trim leading whitespace and check if the lowered text begins with the keyword.
	// This prevents "I analyzed the code. Sorry, bug on line 10" from being a false positive.
	for _, kw := range weakPrefixes {
		if strings.HasPrefix(lower, kw) {
			return true
		}
	}
	for _, kw := range d.extraWeak {
		if strings.HasPrefix(lower, strings.ToLower(kw)) {
			return true
		}
	}

	return false
}
