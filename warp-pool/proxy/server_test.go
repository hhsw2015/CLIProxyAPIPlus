package proxy

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"
)

func TestServeSocksExitsWhenListenerClosed(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	s := &Server{socksLn: ln}
	var wg sync.WaitGroup
	s.wg = wg
	s.wg.Add(1)

	done := make(chan struct{})
	go func() {
		defer close(done)
		s.serveSocks(context.Background())
	}()

	time.Sleep(100 * time.Millisecond)
	if err := ln.Close(); err != nil {
		t.Fatalf("close listener: %v", err)
	}

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("serveSocks did not exit after listener close")
	}
}
