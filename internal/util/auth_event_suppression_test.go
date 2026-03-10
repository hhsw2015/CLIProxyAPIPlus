package util

import (
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestSuppressAuthPathEventUsesNormalizedPathLookup(t *testing.T) {
	t.Parallel()

	path := filepath.Join("tmp", "Auth.json")
	SuppressAuthPathEvent(path, time.Second)

	lookup := filepath.Clean(path)
	if runtime.GOOS == "windows" {
		lookup = filepath.Join("TMP", "AUTH.JSON")
	}

	if !ShouldSuppressAuthPathEvent(lookup) {
		t.Fatalf("expected suppressed lookup for %s", lookup)
	}
}
