package executor

import (
	"strings"
	"testing"
)

func TestSanitizeWebSearchQuery_StripsProactiveExpansion(t *testing.T) {
	q := "AliYunDun aegis bypass\n\n[Proactive Context Expansion - relevant]\n\n--- Expanded from earlier (57 items) ---\n" + strings.Repeat("X", 5000)
	got := sanitizeWebSearchQuery(q)
	if strings.Contains(got, "Proactive") {
		t.Fatalf("expansion marker should be stripped, got: %q", got)
	}
	if got != "AliYunDun aegis bypass" {
		t.Fatalf("unexpected sanitized query: %q", got)
	}
}

func TestSanitizeWebSearchQuery_ClipsLongQuery(t *testing.T) {
	q := strings.Repeat("a", 3000)
	got := sanitizeWebSearchQuery(q)
	if len(got) > 1900 {
		t.Fatalf("expected ≤1900 chars, got %d", len(got))
	}
}

func TestSanitizeWebSearchQuery_PreservesShortQuery(t *testing.T) {
	q := "AWS Bedrock Claude Opus 4.7 pricing"
	if got := sanitizeWebSearchQuery(q); got != q {
		t.Fatalf("short query should be unchanged, got %q", got)
	}
}

func TestSanitizeWebSearchQuery_ParagraphBreakCut(t *testing.T) {
	short := "first paragraph\n\n" + strings.Repeat("b", 2500)
	got := sanitizeWebSearchQuery(short)
	if got != "first paragraph" {
		t.Fatalf("paragraph cut failed, got %q", got)
	}
}
