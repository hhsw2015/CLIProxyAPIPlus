package util

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

type FailedAuthArchiveKind string

const (
	FailedAuthArchiveInvalid FailedAuthArchiveKind = "invalid"
	FailedAuthArchiveLimit   FailedAuthArchiveKind = "limit"
)

func FailedAuthArchiveDirName(kind FailedAuthArchiveKind) string {
	switch kind {
	case FailedAuthArchiveLimit:
		return string(FailedAuthArchiveLimit)
	default:
		return string(FailedAuthArchiveInvalid)
	}
}

func IsFailedAuthArchiveDirName(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case string(FailedAuthArchiveInvalid), string(FailedAuthArchiveLimit):
		return true
	default:
		return false
	}
}

func IsArchivedAuthPath(authDir, path string) bool {
	authDir = strings.TrimSpace(authDir)
	path = strings.TrimSpace(path)
	if authDir == "" || path == "" {
		return false
	}
	authDir = filepath.Clean(authDir)
	if !filepath.IsAbs(path) {
		path = filepath.Join(authDir, path)
	}
	cleanPath := filepath.Clean(path)
	rel, err := filepath.Rel(authDir, cleanPath)
	if err != nil || rel == "" || rel == "." {
		return false
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return false
	}
	parts := strings.Split(filepath.ToSlash(rel), "/")
	for _, part := range parts[:len(parts)-1] {
		if IsFailedAuthArchiveDirName(part) {
			return true
		}
	}
	return false
}

func MoveAuthToArchive(authDir, sourcePath string, kind FailedAuthArchiveKind) (string, error) {
	authDir = strings.TrimSpace(authDir)
	sourcePath = strings.TrimSpace(sourcePath)
	if authDir == "" {
		return "", fmt.Errorf("auth archive: auth directory is empty")
	}
	if sourcePath == "" {
		return "", fmt.Errorf("auth archive: source path is empty")
	}

	authDir = filepath.Clean(authDir)
	if !filepath.IsAbs(sourcePath) {
		sourcePath = filepath.Join(authDir, sourcePath)
	}
	sourcePath = filepath.Clean(sourcePath)

	rel, err := filepath.Rel(authDir, sourcePath)
	if err != nil {
		return "", fmt.Errorf("auth archive: compute relative path: %w", err)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return "", fmt.Errorf("auth archive: source path %s outside auth directory", sourcePath)
	}
	if IsArchivedAuthPath(authDir, sourcePath) {
		return sourcePath, nil
	}

	destination := filepath.Join(authDir, FailedAuthArchiveDirName(kind), filepath.FromSlash(filepath.ToSlash(rel)))
	destination, err = uniqueArchivePath(destination)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		return "", fmt.Errorf("auth archive: create destination dir: %w", err)
	}
	if err := moveFile(sourcePath, destination); err != nil {
		return "", err
	}
	return destination, nil
}

func uniqueArchivePath(path string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("auth archive: destination path is empty")
	}
	if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
		return path, nil
	} else if err != nil {
		return "", fmt.Errorf("auth archive: stat destination: %w", err)
	}

	ext := filepath.Ext(path)
	base := strings.TrimSuffix(path, ext)
	for i := 0; i < 1000; i++ {
		candidate := fmt.Sprintf("%s-%d%s", base, time.Now().UnixNano(), ext)
		if _, err := os.Stat(candidate); errors.Is(err, fs.ErrNotExist) {
			return candidate, nil
		} else if err != nil {
			return "", fmt.Errorf("auth archive: stat candidate: %w", err)
		}
		time.Sleep(time.Microsecond)
	}
	return "", fmt.Errorf("auth archive: failed to allocate unique destination for %s", path)
}

func moveFile(source, destination string) error {
	if err := os.Rename(source, destination); err == nil {
		return nil
	} else if !isCrossDeviceRename(err) {
		return fmt.Errorf("auth archive: rename %s -> %s: %w", source, destination, err)
	}

	if err := copyFile(source, destination); err != nil {
		return err
	}
	if err := os.Remove(source); err != nil {
		return fmt.Errorf("auth archive: remove source %s after copy: %w", source, err)
	}
	return nil
}

func copyFile(source, destination string) error {
	src, err := os.Open(source)
	if err != nil {
		return fmt.Errorf("auth archive: open source %s: %w", source, err)
	}
	defer func() { _ = src.Close() }()

	info, err := src.Stat()
	if err != nil {
		return fmt.Errorf("auth archive: stat source %s: %w", source, err)
	}

	dst, err := os.OpenFile(destination, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, info.Mode().Perm())
	if err != nil {
		return fmt.Errorf("auth archive: open destination %s: %w", destination, err)
	}
	defer func() { _ = dst.Close() }()

	if _, err := io.Copy(dst, src); err != nil {
		return fmt.Errorf("auth archive: copy %s -> %s: %w", source, destination, err)
	}
	if err := dst.Sync(); err != nil {
		return fmt.Errorf("auth archive: sync destination %s: %w", destination, err)
	}
	return nil
}

func isCrossDeviceRename(err error) bool {
	if err == nil {
		return false
	}
	if linkErr, ok := err.(*os.LinkError); ok && linkErr != nil {
		return isCrossDeviceRename(linkErr.Err)
	}
	if runtime.GOOS == "windows" {
		return strings.Contains(strings.ToLower(err.Error()), "not same device")
	}
	return errors.Is(err, fs.ErrInvalid) || strings.Contains(strings.ToLower(err.Error()), "cross-device link")
}
