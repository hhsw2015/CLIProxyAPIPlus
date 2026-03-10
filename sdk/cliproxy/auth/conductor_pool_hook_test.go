package auth

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v6/internal/config"
)

type captureDispositionHook struct {
	NoopHook
	dispositions []AuthDisposition
}

func (h *captureDispositionHook) OnAuthDisposition(_ context.Context, disposition AuthDisposition) {
	h.dispositions = append(h.dispositions, disposition)
}

func TestMarkResultEmitsDeletedDisposition(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	sourcePath := filepath.Join(tmpDir, "codex.json")
	if err := os.WriteFile(sourcePath, []byte(`{"type":"codex","email":"demo@example.com"}`), 0o600); err != nil {
		t.Fatalf("write auth file: %v", err)
	}

	hook := &captureDispositionHook{}
	m := NewManager(nil, nil, hook)
	m.SetConfig(&internalconfig.Config{AuthDir: tmpDir, ArchiveFailedAuth: true})
	if _, err := m.Register(context.Background(), &Auth{
		ID:         "codex.json",
		Provider:   "codex",
		Metadata:   map[string]any{"type": "codex"},
		Attributes: map[string]string{"path": sourcePath},
	}); err != nil {
		t.Fatalf("register auth: %v", err)
	}

	ctx := WithDispositionSource(context.Background(), "pool_probe")
	m.MarkResult(ctx, Result{
		AuthID:  "codex.json",
		Success: false,
		Error: &Error{
			HTTPStatus: 401,
			Message:    "unauthorized",
		},
	})

	if len(hook.dispositions) != 1 {
		t.Fatalf("expected 1 disposition, got %d", len(hook.dispositions))
	}
	got := hook.dispositions[0]
	if !got.Deleted {
		t.Fatalf("expected Deleted=true, got %+v", got)
	}
	if got.Source != "pool_probe" {
		t.Fatalf("expected Source=pool_probe, got %q", got.Source)
	}
	if got.PoolEligible {
		t.Fatalf("expected PoolEligible=false, got %+v", got)
	}

	waitForCondition(t, 2*time.Second, func() bool {
		_, err := os.Stat(sourcePath)
		return os.IsNotExist(err)
	}, "source auth file removal after deleted disposition")
}

func TestMarkResultEmitsLimitDisposition(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	sourcePath := filepath.Join(tmpDir, "codex.json")
	if err := os.WriteFile(sourcePath, []byte(`{"type":"codex","email":"demo@example.com"}`), 0o600); err != nil {
		t.Fatalf("write auth file: %v", err)
	}

	hook := &captureDispositionHook{}
	m := NewManager(nil, nil, hook)
	m.SetConfig(&internalconfig.Config{AuthDir: tmpDir, ArchiveFailedAuth: true})
	if _, err := m.Register(context.Background(), &Auth{
		ID:         "codex.json",
		Provider:   "codex",
		Metadata:   map[string]any{"type": "codex"},
		Attributes: map[string]string{"path": sourcePath},
	}); err != nil {
		t.Fatalf("register auth: %v", err)
	}

	m.MarkResult(context.Background(), Result{
		AuthID:  "codex.json",
		Success: false,
		Error: &Error{
			HTTPStatus: 429,
			Message:    "quota exceeded",
		},
	})

	if len(hook.dispositions) != 1 {
		t.Fatalf("expected 1 disposition, got %d", len(hook.dispositions))
	}
	got := hook.dispositions[0]
	if !got.MovedToLimit {
		t.Fatalf("expected MovedToLimit=true, got %+v", got)
	}
	if !got.QuotaExceeded {
		t.Fatalf("expected QuotaExceeded=true, got %+v", got)
	}
	if got.PoolEligible {
		t.Fatalf("expected PoolEligible=false, got %+v", got)
	}

	waitForCondition(t, 2*time.Second, func() bool {
		_, err := os.Stat(filepath.Join(tmpDir, "limit", "codex.json"))
		return err == nil
	}, "limit archive creation after limit disposition")
}
