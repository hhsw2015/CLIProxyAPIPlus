package main

import (
	"context"
	"testing"
	"time"

	"warp-pool/config"
	"warp-pool/health"
	"warp-pool/pool"
)

func TestStartUniqueIPv4AsyncReturnsImmediately(t *testing.T) {
	oldFunc := ensureUniqueIPv4Func
	defer func() { ensureUniqueIPv4Func = oldFunc }()

	started := make(chan struct{})
	release := make(chan struct{})
	ensureUniqueIPv4Func = func(ctx context.Context, _ *pool.Pool, _ *health.Checker, _ *config.Config) error {
		close(started)
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	cfg := &config.Config{
		UniqueIPv4: config.UniqueIPv4Config{Enabled: true},
	}

	done := make(chan struct{})
	go func() {
		startUniqueIPv4Async(context.Background(), nil, nil, cfg)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("startUniqueIPv4Async blocked")
	}

	select {
	case <-started:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("ensureUniqueIPv4Func was not started asynchronously")
	}

	close(release)
}
