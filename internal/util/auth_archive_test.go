package util

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMoveAuthToArchiveRejectsDeleteKind(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	sourcePath := filepath.Join(tmpDir, "codex.json")
	if err := os.WriteFile(sourcePath, []byte(`{"type":"codex"}`), 0o600); err != nil {
		t.Fatalf("write auth file: %v", err)
	}

	if _, err := MoveAuthToArchive(tmpDir, sourcePath, FailedAuthArchiveDelete); err == nil {
		t.Fatal("expected delete kind to be rejected as an archive destination")
	}
}
