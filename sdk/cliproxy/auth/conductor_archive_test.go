package auth

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v6/internal/config"
)

func TestMarkResultArchivesInvalidAuthFile(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	sourcePath := filepath.Join(tmpDir, "claude.json")
	if err := os.WriteFile(sourcePath, []byte(`{"type":"claude","email":"demo@example.com"}`), 0o600); err != nil {
		t.Fatalf("write auth file: %v", err)
	}

	m := NewManager(nil, nil, nil)
	m.SetConfig(&internalconfig.Config{AuthDir: tmpDir, ArchiveFailedAuth: true})
	if _, err := m.Register(context.Background(), &Auth{
		ID:         "claude.json",
		Provider:   "claude",
		Metadata:   map[string]any{"type": "claude"},
		Attributes: map[string]string{"path": sourcePath},
	}); err != nil {
		t.Fatalf("register auth: %v", err)
	}

	m.MarkResult(context.Background(), Result{
		AuthID:  "claude.json",
		Success: false,
		Error: &Error{
			HTTPStatus: 401,
			Message:    "unauthorized",
		},
	})

	if _, ok := m.GetByID("claude.json"); ok {
		t.Fatal("expected invalid auth to be removed from manager")
	}
	if _, err := os.Stat(filepath.Join(tmpDir, "invalid", "claude.json")); err != nil {
		t.Fatalf("expected archived invalid auth file: %v", err)
	}
	if _, err := os.Stat(sourcePath); !os.IsNotExist(err) {
		t.Fatalf("expected source auth file removed, got err=%v", err)
	}
}

func TestMarkResultArchivesLimitAuthAndSiblingEntries(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	sourcePath := filepath.Join(tmpDir, "gemini.json")
	if err := os.WriteFile(sourcePath, []byte(`{"type":"gemini","email":"demo@example.com"}`), 0o600); err != nil {
		t.Fatalf("write auth file: %v", err)
	}

	m := NewManager(nil, nil, nil)
	m.SetConfig(&internalconfig.Config{AuthDir: tmpDir, ArchiveFailedAuth: true})
	entries := []*Auth{
		{
			ID:         "gemini.json",
			Provider:   "gemini-cli",
			Metadata:   map[string]any{"type": "gemini"},
			Attributes: map[string]string{"path": sourcePath},
		},
		{
			ID:       "gemini.json::project-a",
			Provider: "gemini-cli",
			Metadata: map[string]any{"type": "gemini", "virtual": true},
			Attributes: map[string]string{
				"path":         sourcePath,
				"runtime_only": "true",
			},
		},
	}
	for _, entry := range entries {
		if _, err := m.Register(context.Background(), entry); err != nil {
			t.Fatalf("register auth %s: %v", entry.ID, err)
		}
	}

	m.MarkResult(context.Background(), Result{
		AuthID:  "gemini.json::project-a",
		Success: false,
		Error: &Error{
			HTTPStatus: 429,
			Message:    "quota exceeded",
		},
	})

	for _, id := range []string{"gemini.json", "gemini.json::project-a"} {
		if _, ok := m.GetByID(id); ok {
			t.Fatalf("expected auth %s to be removed after archive", id)
		}
	}
	if _, err := os.Stat(filepath.Join(tmpDir, "limit", "gemini.json")); err != nil {
		t.Fatalf("expected archived limit auth file: %v", err)
	}
}
