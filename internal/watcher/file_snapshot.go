package watcher

import (
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/watcher/synthesizer"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

// SnapshotRootFileAuths returns auths synthesized from JSON files directly under authDir.
func (w *Watcher) SnapshotRootFileAuths() []*coreauth.Auth {
	if w == nil {
		return nil
	}
	w.clientsMutex.RLock()
	cfg := w.config
	authDir := w.authDir
	w.clientsMutex.RUnlock()
	return snapshotFileAuthsInDir(cfg, authDir, authDir)
}

// SnapshotLimitAuths returns auths synthesized from JSON files under authDir/limit.
func (w *Watcher) SnapshotLimitAuths() []*coreauth.Auth {
	if w == nil {
		return nil
	}
	w.clientsMutex.RLock()
	cfg := w.config
	authDir := w.authDir
	w.clientsMutex.RUnlock()
	if strings.TrimSpace(authDir) == "" {
		return nil
	}
	return snapshotFileAuthsInDir(cfg, authDir, filepath.Join(authDir, "limit"))
}

func snapshotFileAuthsInDir(cfg *config.Config, authDir, targetDir string) []*coreauth.Auth {
	authDir = strings.TrimSpace(authDir)
	targetDir = strings.TrimSpace(targetDir)
	if cfg == nil || authDir == "" || targetDir == "" {
		return nil
	}
	entries, err := os.ReadDir(targetDir)
	if err != nil {
		return nil
	}

	ctx := &synthesizer.SynthesisContext{
		Config:      cfg,
		AuthDir:     authDir,
		Now:         time.Now(),
		IDGenerator: synthesizer.NewStableIDGenerator(),
	}

	out := make([]*coreauth.Auth, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasSuffix(strings.ToLower(name), ".json") {
			continue
		}
		fullPath := filepath.Join(targetDir, name)
		data, errRead := os.ReadFile(fullPath)
		if errRead != nil || len(data) == 0 {
			continue
		}
		auths := synthesizer.SynthesizeAuthFile(ctx, fullPath, data)
		if len(auths) == 0 {
			continue
		}
		out = append(out, auths...)
	}
	return out
}
