package executor

import "testing"

const sampleAnySearchMarkdown = "## Search Results (3 results, 2483ms)\n\n" +
	"### 1. Introducing Claude Opus 4.8 - Anthropic\n" +
	"- **URL**: https://www.anthropic.com/news/claude-opus-4-8\n" +
	"- We're upgrading Claude Opus to a new version: Claude Opus 4.8.\n\n" +
	"### 2. What's new in Claude Opus 4.8\n" +
	"- **URL**: https://platform.claude.com/docs/en/about-claude/models/whats-new-claude-4-8\n" +
	"- Overview of new features and behavior changes in Claude Opus 4.8.\n\n" +
	"### 3. Claude Opus 4.8 release\n" +
	"- **URL**: https://thenewstack.io/claude-opus-48-release/\n" +
	"- Released May 28.\n"

func TestParseAnySearchMarkdown(t *testing.T) {
	results := parseAnySearchMarkdown(sampleAnySearchMarkdown)
	if got, want := len(results), 3; got != want {
		t.Fatalf("expected %d results, got %d", want, got)
	}

	if results[0].URL != "https://www.anthropic.com/news/claude-opus-4-8" {
		t.Errorf("result[0].URL = %q, want %q", results[0].URL, "https://www.anthropic.com/news/claude-opus-4-8")
	}
	if results[0].Title != "Introducing Claude Opus 4.8 - Anthropic" {
		t.Errorf("result[0].Title = %q", results[0].Title)
	}
	if results[0].Snippet == nil || *results[0].Snippet == "" {
		t.Errorf("result[0].Snippet missing")
	}
}

func TestParseAnySearchMarkdown_Empty(t *testing.T) {
	if got := parseAnySearchMarkdown(""); len(got) != 0 {
		t.Fatalf("expected 0 results from empty input, got %d", len(got))
	}
}

func TestParseAnySearchMarkdown_MissingURL(t *testing.T) {
	md := "### 1. Title only\n- snippet without URL\n\n### 2. Has URL\n- **URL**: https://example.com\n- snippet two\n"
	results := parseAnySearchMarkdown(md)
	if got, want := len(results), 1; got != want {
		t.Fatalf("expected 1 result (entries without URL skipped), got %d", got)
	}
	if results[0].URL != "https://example.com" {
		t.Errorf("expected example.com, got %q", results[0].URL)
	}
}
