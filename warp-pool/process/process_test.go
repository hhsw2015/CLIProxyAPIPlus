package process

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeFakeWarp(t *testing.T, dir string, ignoreTerm bool) string {
	t.Helper()
	path := filepath.Join(dir, "warp")
	runBody := "trap 'exit 0' TERM INT\n    while :; do\n      sleep 0.1\n    done"
	if ignoreTerm {
		runBody = "trap '' TERM INT\n    while :; do\n      sleep 0.1\n    done"
	}
	script := `#!/bin/sh
set -eu
cmd="$1"
shift || true
data_dir=""
while [ "$#" -gt 0 ]; do
  if [ "$1" = "--data-dir" ]; then
    data_dir="$2"
    shift 2
    continue
  fi
  shift
done
case "$cmd" in
  generate)
    mkdir -p "$data_dir"
    printf '{}' > "$data_dir/wgcf-identity.json"
    ;;
  run)
    ` + runBody + `
    ;;
  update)
    exit 0
    ;;
  *)
    exit 1
    ;;
esac
`
	if err := os.WriteFile(path, []byte(script), 0755); err != nil {
		t.Fatalf("write fake warp: %v", err)
	}
	return path
}

func TestProcessRestartReturnsPromptly(t *testing.T) {
	tmpDir := t.TempDir()
	warpBin := writeFakeWarp(t, tmpDir, false)
	proc := New(0, warpBin, tmpDir, 10001, 11001)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := proc.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}

	restartDone := make(chan error, 1)
	go func() {
		restartDone <- proc.Restart(ctx)
	}()

	select {
	case err := <-restartDone:
		if err != nil {
			t.Fatalf("restart: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("restart timed out")
	}
}

func TestProcessRestartReturnsPromptlyWhenContextCancellationKillsProcess(t *testing.T) {
	tmpDir := t.TempDir()
	warpBin := writeFakeWarp(t, tmpDir, true)
	proc := New(0, warpBin, tmpDir, 10001, 11001)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := proc.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}

	restartDone := make(chan error, 1)
	go func() {
		restartDone <- proc.Restart(ctx)
	}()

	select {
	case err := <-restartDone:
		if err != nil {
			t.Fatalf("restart: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("restart timed out")
	}
}
