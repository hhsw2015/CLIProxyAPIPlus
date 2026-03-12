package config

import "strings"

// ImplicitOpenAICompatAlias returns a client-facing fallback alias for OpenAI-compatible
// models when the config entry does not define one explicitly.
//
// Today this only covers Claude-style dotted IDs because Anthropic clients commonly use
// hyphenated variants such as "claude-opus-4-5" while OpenAI-compatible providers often
// expose "claude-opus-4.5" upstream.
func ImplicitOpenAICompatAlias(name, alias string) string {
	name = strings.TrimSpace(name)
	alias = strings.TrimSpace(alias)
	if name == "" || alias != "" {
		return ""
	}
	lowerName := strings.ToLower(name)
	if !strings.HasPrefix(lowerName, "claude-") || !strings.Contains(name, ".") {
		return ""
	}
	implicit := strings.ReplaceAll(name, ".", "-")
	if strings.EqualFold(implicit, name) {
		return ""
	}
	return implicit
}
