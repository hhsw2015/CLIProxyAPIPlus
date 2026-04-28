// Package errorpass implements a config-driven rules engine for error passthrough.
// It decides whether upstream error responses should be forwarded to the client
// as-is (or with a custom status/body) instead of being retried or masked.
//
// The matching algorithm is adapted from sub2api's ErrorPassthroughService.
package errorpass

import (
	"sort"
	"strings"
)

// Match modes.
const (
	MatchModeAny = "any"
	MatchModeAll = "all"
)

// maxBodyMatchLen caps the portion of the response body inspected for keyword
// matching. Error details never appear past 8 KB, and limiting the search
// avoids allocating large lowercased copies.
const maxBodyMatchLen = 8 << 10 // 8 KB

// Rule defines an error passthrough rule loaded from config.
type Rule struct {
	Name            string   `yaml:"name" json:"name"`
	Enabled         bool     `yaml:"enabled" json:"enabled"`
	Priority        int      `yaml:"priority" json:"priority"`
	ErrorCodes      []int    `yaml:"error-codes" json:"error_codes"`
	Keywords        []string `yaml:"keywords" json:"keywords"`
	MatchMode       string   `yaml:"match-mode" json:"match_mode"`       // "any" | "all"
	Platforms       []string `yaml:"platforms" json:"platforms"`
	PassthroughCode bool     `yaml:"passthrough-code" json:"passthrough_code"`
	ResponseCode    *int     `yaml:"response-code" json:"response_code"`
	PassthroughBody bool     `yaml:"passthrough-body" json:"passthrough_body"`
	CustomMessage   *string  `yaml:"custom-message" json:"custom_message"`
}

// MatchResult contains the matched rule and the response to send.
type MatchResult struct {
	Rule        *Rule
	StatusCode  int
	Body        string
	Passthrough bool // true = pass original response unchanged
}

// Matcher holds pre-computed rules for efficient matching.
type Matcher struct {
	rules []cachedRule
}

type cachedRule struct {
	rule           Rule
	errorCodeSet   map[int]struct{}
	lowerKeywords  []string
	lowerPlatforms []string
}

// NewMatcher creates a Matcher from the given rules.
// Only enabled rules are retained. Rules are sorted by priority ascending
// (lower number = higher priority). Keywords, platforms, and error codes are
// pre-computed into efficient lookup structures.
func NewMatcher(rules []Rule) *Matcher {
	var cached []cachedRule
	for _, r := range rules {
		if !r.Enabled {
			continue
		}
		cr := cachedRule{rule: r}

		if len(r.ErrorCodes) > 0 {
			cr.errorCodeSet = make(map[int]struct{}, len(r.ErrorCodes))
			for _, code := range r.ErrorCodes {
				cr.errorCodeSet[code] = struct{}{}
			}
		}
		if len(r.Keywords) > 0 {
			cr.lowerKeywords = make([]string, len(r.Keywords))
			for i, kw := range r.Keywords {
				cr.lowerKeywords[i] = strings.ToLower(kw)
			}
		}
		if len(r.Platforms) > 0 {
			cr.lowerPlatforms = make([]string, len(r.Platforms))
			for i, p := range r.Platforms {
				cr.lowerPlatforms[i] = strings.ToLower(p)
			}
		}

		cached = append(cached, cr)
	}

	sort.Slice(cached, func(i, j int) bool {
		return cached[i].rule.Priority < cached[j].rule.Priority
	})

	return &Matcher{rules: cached}
}

// LoadRules creates a Matcher from a slice of rules (typically from YAML config).
func LoadRules(rules []Rule) *Matcher {
	return NewMatcher(rules)
}

// Match finds the first rule that matches the given platform, status code, and
// response body. Returns nil when no rule matches.
//
// The body is lazily lowercased and truncated to 8 KB for keyword matching.
// Rules are evaluated in priority order; the first match wins.
func (m *Matcher) Match(platform string, statusCode int, body []byte) *MatchResult {
	if len(m.rules) == 0 {
		return nil
	}

	lowerPlatform := strings.ToLower(platform)

	// Lazy body lowering -- only computed when a rule with keywords is reached.
	var bodyLower string
	var bodyLowerDone bool

	for i := range m.rules {
		cr := &m.rules[i]

		if !cr.matchesPlatform(lowerPlatform) {
			continue
		}
		if cr.matches(statusCode, body, &bodyLower, &bodyLowerDone) {
			return cr.buildResult(statusCode, body)
		}
	}

	return nil
}

// ---------------------------------------------------------------------------
// cachedRule helpers
// ---------------------------------------------------------------------------

// matchesPlatform returns true if the rule applies to the given platform.
// An empty platforms list means the rule applies to all platforms.
func (r *cachedRule) matchesPlatform(lowerPlatform string) bool {
	if len(r.lowerPlatforms) == 0 {
		return true
	}
	for _, p := range r.lowerPlatforms {
		if p == lowerPlatform {
			return true
		}
	}
	return false
}

// matchesErrorCode checks whether statusCode is in the pre-computed set. O(1).
func (r *cachedRule) matchesErrorCode(statusCode int) bool {
	_, ok := r.errorCodeSet[statusCode]
	return ok
}

// matchesKeyword checks whether any pre-computed keyword is a substring of
// the lowercased body.
func (r *cachedRule) matchesKeyword(bodyLower string) bool {
	for _, kw := range r.lowerKeywords {
		if strings.Contains(bodyLower, kw) {
			return true
		}
	}
	return false
}

// matches implements the core matching logic adapted from sub2api's
// ruleMatchesOptimized. It supports both "any" and "all" match modes with
// short-circuit evaluation and lazy body lowering.
func (r *cachedRule) matches(statusCode int, body []byte, bodyLower *string, bodyLowerDone *bool) bool {
	hasErrorCodes := len(r.errorCodeSet) > 0
	hasKeywords := len(r.lowerKeywords) > 0

	// A rule with no conditions never matches.
	if !hasErrorCodes && !hasKeywords {
		return false
	}

	codeMatch := !hasErrorCodes || r.matchesErrorCode(statusCode)

	if r.rule.MatchMode == MatchModeAll {
		// "all": every configured condition must be satisfied.
		if hasErrorCodes && !codeMatch {
			return false
		}
		if hasKeywords {
			return r.matchesKeyword(ensureBodyLower(body, bodyLower, bodyLowerDone))
		}
		return codeMatch
	}

	// "any": any single condition is sufficient.
	if hasErrorCodes && hasKeywords {
		if codeMatch {
			return true
		}
		return r.matchesKeyword(ensureBodyLower(body, bodyLower, bodyLowerDone))
	}
	if hasKeywords {
		return r.matchesKeyword(ensureBodyLower(body, bodyLower, bodyLowerDone))
	}
	return codeMatch
}

// buildResult constructs a MatchResult from the matched rule and the original
// upstream response.
func (r *cachedRule) buildResult(statusCode int, body []byte) *MatchResult {
	res := &MatchResult{
		Rule: &r.rule,
	}

	// If both code and body are passed through, the caller can forward the
	// upstream response unchanged.
	if r.rule.PassthroughCode && r.rule.PassthroughBody {
		res.Passthrough = true
		res.StatusCode = statusCode
		res.Body = string(body)
		return res
	}

	// Status code.
	if r.rule.PassthroughCode {
		res.StatusCode = statusCode
	} else if r.rule.ResponseCode != nil {
		res.StatusCode = *r.rule.ResponseCode
	} else {
		// Fallback -- should not happen with validated rules.
		res.StatusCode = statusCode
	}

	// Body.
	if r.rule.PassthroughBody {
		res.Body = string(body)
	} else if r.rule.CustomMessage != nil {
		res.Body = *r.rule.CustomMessage
	}

	return res
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// ensureBodyLower lazily computes the lowercased, truncated body string.
// The result is cached across rule evaluations for the same request.
func ensureBodyLower(body []byte, bodyLower *string, done *bool) string {
	if *done {
		return *bodyLower
	}
	b := body
	if len(b) > maxBodyMatchLen {
		b = b[:maxBodyMatchLen]
	}
	*bodyLower = strings.ToLower(string(b))
	*done = true
	return *bodyLower
}
