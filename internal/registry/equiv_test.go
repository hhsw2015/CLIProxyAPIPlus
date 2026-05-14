package registry

import (
	"reflect"
	"testing"
)

func TestClaudeModelEquivalents(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"claude-opus-4.7", []string{"claude-opus-4.7", "claude-opus-4-7"}},
		{"claude-opus-4-7", []string{"claude-opus-4-7", "claude-opus-4.7"}},
		{"claude-haiku-4.5", []string{"claude-haiku-4.5", "claude-haiku-4-5"}},
		{"claude-sonnet-4-6", []string{"claude-sonnet-4-6", "claude-sonnet-4.6"}},
		{"claude-opus-4-7-20251019", []string{"claude-opus-4-7-20251019", "claude-opus-4.7-20251019"}},
		{"claude-opus-4.7-20251019", []string{"claude-opus-4.7-20251019", "claude-opus-4-7-20251019"}},
		{"gpt-4o", []string{"gpt-4o"}},
		{"", nil},
	}
	for _, c := range cases {
		got := ClaudeModelEquivalents(c.in)
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("ClaudeModelEquivalents(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}
