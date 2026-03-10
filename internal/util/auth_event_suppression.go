package util

import (
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

var suppressedAuthPaths sync.Map

// SuppressAuthPathEvent marks an auth path as a short-lived self-write so watcher
// event handlers can ignore the corresponding fsnotify echo.
func SuppressAuthPathEvent(path string, ttl time.Duration) {
	normalized := normalizeSuppressedAuthPath(path)
	if normalized == "" {
		return
	}
	if ttl <= 0 {
		ttl = time.Second
	}
	suppressedAuthPaths.Store(normalized, time.Now().Add(ttl))
}

// ShouldSuppressAuthPathEvent returns true when the given path was recently
// marked as a self-write and should be ignored by watcher event handling.
func ShouldSuppressAuthPathEvent(path string) bool {
	normalized := normalizeSuppressedAuthPath(path)
	if normalized == "" {
		return false
	}
	raw, ok := suppressedAuthPaths.Load(normalized)
	if !ok {
		return false
	}
	until, ok := raw.(time.Time)
	if !ok {
		suppressedAuthPaths.Delete(normalized)
		return false
	}
	if time.Now().After(until) {
		suppressedAuthPaths.Delete(normalized)
		return false
	}
	return true
}

func normalizeSuppressedAuthPath(path string) string {
	trimmed := strings.TrimSpace(path)
	if trimmed == "" {
		return ""
	}
	cleaned := filepath.Clean(trimmed)
	if runtime.GOOS == "windows" {
		cleaned = strings.TrimPrefix(cleaned, `\\?\`)
		cleaned = strings.ToLower(cleaned)
	}
	return cleaned
}
