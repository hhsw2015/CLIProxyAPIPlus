package auth

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
)

func waitForCondition(t *testing.T, timeout time.Duration, condition func() bool, description string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %s", description)
}

func TestMarkResultDeletesInvalidAuthFile(t *testing.T) {
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
	if _, err := os.Stat(filepath.Join(tmpDir, "invalid", "claude.json")); !os.IsNotExist(err) {
		t.Fatalf("expected no invalid archive file, got err=%v", err)
	}
	waitForCondition(t, 2*time.Second, func() bool {
		_, err := os.Stat(sourcePath)
		return os.IsNotExist(err)
	}, "source auth file removal")
}

func TestMarkResultArchivesLimitAuthAndSiblingEntries(t *testing.T) {
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
	waitForCondition(t, 2*time.Second, func() bool {
		_, err := os.Stat(filepath.Join(tmpDir, "limit", "gemini.json"))
		return err == nil
	}, "archived limit auth file")
}

func TestMarkResult_ArchivesAsyncAfterDisposition(t *testing.T) {
	tmpDir := t.TempDir()
	sourcePath := filepath.Join(tmpDir, "gemini.json")
	if err := os.WriteFile(sourcePath, []byte(`{"type":"gemini","email":"demo@example.com"}`), 0o600); err != nil {
		t.Fatalf("write auth file: %v", err)
	}

	hook := &captureDispositionHook{}
	m := NewManager(nil, nil, hook)
	m.SetConfig(&internalconfig.Config{AuthDir: tmpDir, ArchiveFailedAuth: true})
	if _, err := m.Register(context.Background(), &Auth{
		ID:         "gemini.json",
		Provider:   "gemini-cli",
		Metadata:   map[string]any{"type": "gemini"},
		Attributes: map[string]string{"path": sourcePath},
	}); err != nil {
		t.Fatalf("register auth: %v", err)
	}

	originalArchive := archiveAuthFileFunc
	archiveStarted := make(chan struct{})
	unblockArchive := make(chan struct{})
	archiveAuthFileFunc = func(m *Manager, sourcePath string, kind util.FailedAuthArchiveKind) (string, error) {
		close(archiveStarted)
		<-unblockArchive
		return sourcePath, nil
	}
	t.Cleanup(func() { archiveAuthFileFunc = originalArchive })

	done := make(chan struct{})
	go func() {
		m.MarkResult(context.Background(), Result{
			AuthID:  "gemini.json",
			Success: false,
			Error: &Error{
				HTTPStatus: 429,
				Message:    "quota exceeded",
			},
		})
		close(done)
	}()

	select {
	case <-archiveStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("expected archive worker to start")
	}

	select {
	case <-done:
	case <-time.After(200 * time.Millisecond):
		t.Fatal("expected MarkResult to return before archive file operation completes")
	}

	if len(hook.dispositions) != 1 || !hook.dispositions[0].MovedToLimit {
		t.Fatalf("expected disposition before async archive, got %+v", hook.dispositions)
	}

	if _, ok := m.GetByID("gemini.json"); ok {
		t.Fatal("expected auth removed from manager before async archive finishes")
	}
	if _, err := os.Stat(sourcePath); err != nil {
		t.Fatalf("expected source file to still exist before async archive finishes: %v", err)
	}

	close(unblockArchive)
}
