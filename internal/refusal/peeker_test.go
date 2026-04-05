package refusal

import (
	"context"
	"fmt"
	"testing"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

func makeSSEChunk(content string) cliproxyexecutor.StreamChunk {
	data := fmt.Sprintf(`data: {"choices":[{"delta":{"content":"%s"}}]}`, content)
	return cliproxyexecutor.StreamChunk{Payload: []byte(data + "\n")}
}

func makeResponsesChunk(content string) cliproxyexecutor.StreamChunk {
	data := fmt.Sprintf(`data: {"type":"response.output_text.delta","delta":"%s"}`, content)
	return cliproxyexecutor.StreamChunk{Payload: []byte(data + "\n")}
}

func TestPeekStream_DetectsRefusal(t *testing.T) {
	d := NewDetector(nil, nil)
	ch := make(chan cliproxyexecutor.StreamChunk, 3)
	ch <- makeSSEChunk("I'm sorry, but I cannot assist with this request.")
	close(ch)

	result := PeekStream(context.Background(), d, ch, 256)
	if result.Refusal == nil {
		t.Fatal("expected refusal to be detected")
	}
	if len(result.Buffered) == 0 {
		t.Error("buffered chunks should not be empty")
	}
}

func TestPeekStream_PassthroughNormal(t *testing.T) {
	d := NewDetector(nil, nil)
	ch := make(chan cliproxyexecutor.StreamChunk, 5)
	ch <- makeSSEChunk("Sure! Here is the implementation")
	ch <- makeSSEChunk(" you requested:")
	close(ch)

	result := PeekStream(context.Background(), d, ch, 256)
	if result.Refusal != nil {
		t.Fatalf("unexpected refusal: %v", result.Refusal)
	}
	if len(result.Buffered) != 2 {
		t.Errorf("expected 2 buffered chunks, got %d", len(result.Buffered))
	}
	if !result.Closed {
		t.Error("expected closed=true since channel was drained")
	}
}

func TestPeekStream_StopsAtPeekBytes(t *testing.T) {
	d := NewDetector(nil, nil)
	ch := make(chan cliproxyexecutor.StreamChunk, 100)

	// Send many small chunks; peeker should stop after peekBytes threshold.
	for i := 0; i < 50; i++ {
		ch <- makeSSEChunk(fmt.Sprintf("chunk-%d ", i))
	}
	// Don't close — peeker should return after peekBytes without blocking.

	result := PeekStream(context.Background(), d, ch, 64)
	if result.Refusal != nil {
		t.Fatalf("unexpected refusal: %v", result.Refusal)
	}
	if len(result.Buffered) == 0 {
		t.Error("expected at least one buffered chunk")
	}
}

func TestPeekStream_RespondsToContextCancel(t *testing.T) {
	d := NewDetector(nil, nil)
	ch := make(chan cliproxyexecutor.StreamChunk) // unbuffered, will block

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	result := PeekStream(ctx, d, ch, 256)
	// Should return without hanging.
	if result.Refusal != nil {
		t.Error("cancelled context should not produce refusal")
	}
}

func TestPeekStream_UpstreamError(t *testing.T) {
	d := NewDetector(nil, nil)
	ch := make(chan cliproxyexecutor.StreamChunk, 2)
	ch <- cliproxyexecutor.StreamChunk{Err: fmt.Errorf("upstream timeout")}
	close(ch)

	result := PeekStream(context.Background(), d, ch, 256)
	if result.Refusal != nil {
		t.Error("upstream error should not trigger refusal detection")
	}
	if len(result.Buffered) != 1 {
		t.Errorf("expected 1 buffered chunk (the error), got %d", len(result.Buffered))
	}
}

func TestPeekStream_ResponsesAPIFormat(t *testing.T) {
	d := NewDetector(nil, nil)
	ch := make(chan cliproxyexecutor.StreamChunk, 3)
	ch <- makeResponsesChunk("I cannot assist with this request as it violates my guidelines.")
	close(ch)

	result := PeekStream(context.Background(), d, ch, 256)
	if result.Refusal == nil {
		t.Fatal("expected refusal to be detected in Responses API format")
	}
}

func TestPeekStream_NilInputs(t *testing.T) {
	result := PeekStream(context.Background(), nil, nil, 256)
	if result.Refusal != nil {
		t.Error("nil inputs should return empty result")
	}
}
