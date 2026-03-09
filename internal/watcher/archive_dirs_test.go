package watcher

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
)

func TestLoadFileClientsSkipsArchivedDirectories(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	files := map[string]string{
		filepath.Join(tmpDir, "active.json"):         `{"type":"claude"}`,
		filepath.Join(tmpDir, "invalid", "bad.json"): `{"type":"claude"}`,
		filepath.Join(tmpDir, "limit", "cap.json"):   `{"type":"gemini"}`,
	}
	for path, content := range files {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}

	cfg := &config.Config{AuthDir: tmpDir}
	w := &Watcher{}
	w.SetConfig(cfg)

	if count := w.loadFileClients(cfg); count != 1 {
		t.Fatalf("expected only one active auth file, got %d", count)
	}
}

func TestSnapshotFileAuthsSplitRootAndLimit(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	files := map[string]string{
		filepath.Join(tmpDir, "active.json"):       `{"type":"claude","email":"active@example.com"}`,
		filepath.Join(tmpDir, "limit", "cap.json"): `{"type":"codex","email":"limit@example.com"}`,
	}
	for path, content := range files {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}

	cfg := &config.Config{AuthDir: tmpDir}
	w := &Watcher{authDir: tmpDir}
	w.SetConfig(cfg)

	rootAuths := w.SnapshotRootFileAuths()
	if len(rootAuths) != 1 {
		t.Fatalf("expected 1 root auth, got %d", len(rootAuths))
	}
	if rootAuths[0].ID != "active.json" {
		t.Fatalf("root auth ID = %q, want %q", rootAuths[0].ID, "active.json")
	}

	limitAuths := w.SnapshotLimitAuths()
	if len(limitAuths) != 1 {
		t.Fatalf("expected 1 limit auth, got %d", len(limitAuths))
	}
	if limitAuths[0].ID != filepath.Join("limit", "cap.json") {
		t.Fatalf("limit auth ID = %q, want %q", limitAuths[0].ID, filepath.Join("limit", "cap.json"))
	}
}
