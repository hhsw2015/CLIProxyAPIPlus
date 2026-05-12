package headroom

import (
	"bytes"
	"strings"
	"testing"
)

// TestStreamExpanderPassthrough — chunks without markers flow through unchanged
// once the trailing-buffer settles.
func TestStreamExpanderPassthrough(t *testing.T) {
	e := NewStreamExpander()
	out1 := e.Write([]byte("hello "))
	out2 := e.Write([]byte("world"))
	tail := e.Flush()

	got := string(out1) + string(out2) + string(tail)
	if got != "hello world" {
		t.Fatalf("passthrough mismatch: got %q", got)
	}
}

// TestStreamExpanderBufferPartialMarker — a marker straddling chunk boundaries
// should be recognized and held until complete.
func TestStreamExpanderBufferPartialMarker(t *testing.T) {
	// Pre-load a known CCR entry by enabling + registering an in-memory
	// store and stuffing it via the public CcrGet path. We can't call the
	// FFI directly without an active backend, so we exercise the buffering
	// logic against a *missing* hash — expander must leave the marker
	// untouched (it does CcrGet, gets nothing, returns the original match).
	SetConfig(Config{Enabled: true})

	e := NewStreamExpander()
	chunk1 := e.Write([]byte("Prefix <<ccr:abcdef0123456789abcdef0"))
	chunk2 := e.Write([]byte("1>> tail"))
	tail := e.Flush()

	combined := append(append(append([]byte{}, chunk1...), chunk2...), tail...)
	got := string(combined)

	want := "Prefix <<ccr:abcdef0123456789abcdef01>> tail"
	if got != want {
		t.Fatalf("partial-marker reassembly failed:\n  got:  %q\n  want: %q", got, want)
	}
	// Marker survives intact when CcrGet misses — this is the contract.
	if !strings.Contains(got, "<<ccr:abcdef0123456789abcdef01>>") {
		t.Fatalf("marker not preserved on CCR miss: %q", got)
	}
}

// TestStreamExpanderUserContentBracketsNotBuffered — `<<bracketed>>` user text
// must NOT be held back; only valid `<<ccr:` prefixes buffer.
func TestStreamExpanderUserContentBrackets(t *testing.T) {
	e := NewStreamExpander()
	out := e.Write([]byte("see <<some_text>> here"))
	tail := e.Flush()

	combined := string(out) + string(tail)
	if combined != "see <<some_text>> here" {
		t.Fatalf("non-marker `<<...>>` mishandled: got %q", combined)
	}
	// The buffer should be empty after Write — user content flushes immediately
	// because `<<some` is not a viable prefix of `<<ccr:`.
	if got := string(out); !strings.Contains(got, "<<some_text>>") {
		t.Fatalf("non-marker content was buffered when it should have flushed: out=%q tail=%q", out, tail)
	}
}

// TestStreamExpanderTrailingPartial — single trailing `<` is kept so a marker
// that splits exactly at `<<` across chunks still matches on the next call.
func TestStreamExpanderTrailingPartial(t *testing.T) {
	e := NewStreamExpander()
	// Chunk 1 ends with a single `<` — could be the start of a marker.
	out1 := e.Write([]byte("hello <"))
	// Buffered: at minimum the trailing `<`. The non-`<` prefix must flush.
	if !bytes.HasPrefix(out1, []byte("hello")) {
		t.Fatalf("expected `hello` to flush, got %q", out1)
	}

	// Chunk 2: `<ccr:` continues — should be recognised as marker prefix.
	out2 := e.Write([]byte("<ccr:0123456789abcdef01234567>> done"))
	tail := e.Flush()
	combined := string(out1) + string(out2) + string(tail)

	if !strings.HasSuffix(combined, " done") {
		t.Fatalf("trailing text not flushed: %q", combined)
	}
	if !strings.Contains(combined, "<<ccr:0123456789abcdef01234567>>") {
		t.Fatalf("marker not reassembled across chunk boundary: %q", combined)
	}
}

// TestStreamExpanderMultipleMarkers — several markers in one chunk all get
// processed (regex ReplaceAllFunc).
func TestStreamExpanderMultipleMarkers(t *testing.T) {
	e := NewStreamExpander()
	out := e.Write([]byte("a <<ccr:111111111111111111111111>> b <<ccr:222222222222222222222222>> c"))
	tail := e.Flush()
	combined := string(out) + string(tail)

	// Both markers preserved (CCR miss) — regex matched both, expander
	// returned original text for each unknown hash.
	count := strings.Count(combined, "<<ccr:")
	if count != 2 {
		t.Fatalf("expected 2 markers in output, got %d: %q", count, combined)
	}
}

// TestExpandMarkersEmpty — empty input is a no-op.
func TestExpandMarkersEmpty(t *testing.T) {
	got, n := ExpandMarkers("")
	if got != "" || n != 0 {
		t.Fatalf("ExpandMarkers(\"\") = (%q, %d)", got, n)
	}
}

// TestExpandMarkersNoMarker — text without markers is returned unchanged.
func TestExpandMarkersNoMarker(t *testing.T) {
	in := "no markers here, just text with <html>&<<not_a_marker>>"
	got, n := ExpandMarkers(in)
	if got != in || n != 0 {
		t.Fatalf("ExpandMarkers no-op failed: got=%q n=%d", got, n)
	}
}
