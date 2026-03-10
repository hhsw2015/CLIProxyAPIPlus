package auth

import (
	"context"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
)

type countingStore struct {
	saveCount atomic.Int32
}

func (s *countingStore) List(context.Context) ([]*Auth, error) { return nil, nil }

func (s *countingStore) Save(context.Context, *Auth) (string, error) {
	s.saveCount.Add(1)
	return "", nil
}

func (s *countingStore) Delete(context.Context, string) error { return nil }

type pathResolvingStore struct {
	baseDir string
}

func (s *pathResolvingStore) List(context.Context) ([]*Auth, error) { return nil, nil }

func (s *pathResolvingStore) Save(_ context.Context, auth *Auth) (string, error) {
	if auth == nil {
		return "", nil
	}
	if auth.FileName == "" {
		return filepath.Join(s.baseDir, auth.ID), nil
	}
	if filepath.IsAbs(auth.FileName) {
		return auth.FileName, nil
	}
	return filepath.Join(s.baseDir, auth.FileName), nil
}

func (s *pathResolvingStore) Delete(context.Context, string) error { return nil }

func TestWithSkipPersist_DisablesUpdatePersistence(t *testing.T) {
	store := &countingStore{}
	mgr := NewManager(store, nil, nil)
	auth := &Auth{
		ID:       "auth-1",
		Provider: "antigravity",
		Metadata: map[string]any{"type": "antigravity"},
	}

	if _, err := mgr.Update(context.Background(), auth); err != nil {
		t.Fatalf("Update returned error: %v", err)
	}
	if got := store.saveCount.Load(); got != 1 {
		t.Fatalf("expected 1 Save call, got %d", got)
	}

	ctxSkip := WithSkipPersist(context.Background())
	if _, err := mgr.Update(ctxSkip, auth); err != nil {
		t.Fatalf("Update(skipPersist) returned error: %v", err)
	}
	if got := store.saveCount.Load(); got != 1 {
		t.Fatalf("expected Save call count to remain 1, got %d", got)
	}
}

func TestWithSkipPersist_DisablesRegisterPersistence(t *testing.T) {
	store := &countingStore{}
	mgr := NewManager(store, nil, nil)
	auth := &Auth{
		ID:       "auth-1",
		Provider: "antigravity",
		Metadata: map[string]any{"type": "antigravity"},
	}

	if _, err := mgr.Register(WithSkipPersist(context.Background()), auth); err != nil {
		t.Fatalf("Register(skipPersist) returned error: %v", err)
	}
	if got := store.saveCount.Load(); got != 0 {
		t.Fatalf("expected 0 Save calls, got %d", got)
	}
}

func TestPersistSuppressesWatcherEchoWhenUsingFileName(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	store := &pathResolvingStore{baseDir: tmpDir}

	mgr := NewManager(store, nil, nil)
	auth := &Auth{
		ID:       "refresh.json",
		FileName: "refresh.json",
		Provider: "codex",
		Metadata: map[string]any{"type": "codex", "access_token": "access-token"},
	}

	if err := mgr.persist(context.Background(), auth); err != nil {
		t.Fatalf("persist returned error: %v", err)
	}

	path := filepath.Join(tmpDir, "refresh.json")
	if !util.ShouldSuppressAuthPathEvent(path) {
		t.Fatalf("expected persist to suppress watcher echo for %s", path)
	}
}
