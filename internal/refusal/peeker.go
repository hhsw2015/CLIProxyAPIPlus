package refusal

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

// ErrRefusalDetected is returned when the stream peeker detects a refusal
// in the initial response bytes. The caller should rewrite the conversation
// history and retry the request through the normal credential rotation loop.
type ErrRefusalDetected struct {
	// Text is the extracted refusal text (for use in history rewriting).
	Text string
}

func (e *ErrRefusalDetected) Error() string {
	return "refusal detected in stream response"
}

// PeekResult holds the outcome of inspecting the first N bytes of a stream.
type PeekResult struct {
	// Refusal is non-nil when the peeked content was classified as a refusal.
	Refusal *ErrRefusalDetected
	// Buffered contains the chunks consumed during peeking (must be re-emitted on passthrough).
	Buffered []cliproxyexecutor.StreamChunk
	// Closed is true when the upstream channel was fully drained during peeking.
	Closed bool
}

// PeekStream reads up to peekBytes worth of payload from the chunk channel,
// extracts text content from SSE data lines, and runs refusal detection.
//
// The function returns as soon as one of the following is true:
//   - Enough payload bytes have been accumulated (peekBytes threshold).
//   - The upstream channel is closed.
//   - The context is cancelled.
//
// If no refusal is found, the caller should prepend Buffered chunks to the
// remaining channel for transparent passthrough — the user sees zero delay.
func PeekStream(ctx context.Context, detector *Detector, chunks <-chan cliproxyexecutor.StreamChunk, peekBytes int) PeekResult {
	if chunks == nil || detector == nil {
		return PeekResult{}
	}

	var (
		buffered  []cliproxyexecutor.StreamChunk
		totalRead int
	)

	for totalRead < peekBytes {
		var (
			chunk cliproxyexecutor.StreamChunk
			ok    bool
		)
		select {
		case <-ctx.Done():
			return PeekResult{Buffered: buffered}
		case chunk, ok = <-chunks:
			if !ok {
				// Channel closed — inspect what we have so far.
				text := ExtractTextFromChunks(buffered)
				if detector.IsRefusal(text) {
					return PeekResult{
						Refusal:  &ErrRefusalDetected{Text: text},
						Buffered: buffered,
						Closed:   true,
					}
				}
				return PeekResult{Buffered: buffered, Closed: true}
			}
		}

		// Terminal error from upstream — return it as-is, no detection needed.
		if chunk.Err != nil {
			buffered = append(buffered, chunk)
			return PeekResult{Buffered: buffered}
		}

		buffered = append(buffered, chunk)
		totalRead += len(chunk.Payload)
	}

	// We've accumulated enough bytes. Run detection.
	text := ExtractTextFromChunks(buffered)
	if detector.IsRefusal(text) {
		return PeekResult{
			Refusal:  &ErrRefusalDetected{Text: text},
			Buffered: buffered,
		}
	}

	return PeekResult{Buffered: buffered}
}

// ExtractTextFromChunks concatenates text content from SSE data payloads.
// It handles both raw text chunks and SSE "data: {...}" JSON lines containing
// delta content from OpenAI, Anthropic, and Responses API formats.
func ExtractTextFromChunks(chunks []cliproxyexecutor.StreamChunk) string {
	var sb strings.Builder
	for _, chunk := range chunks {
		if len(chunk.Payload) == 0 {
			continue
		}
		// Try to extract text from SSE data lines.
		lines := bytes.Split(chunk.Payload, []byte("\n"))
		for _, line := range lines {
			line = bytes.TrimSpace(line)
			if !bytes.HasPrefix(line, []byte("data: ")) {
				continue
			}
			data := bytes.TrimPrefix(line, []byte("data: "))
			if bytes.Equal(data, []byte("[DONE]")) {
				continue
			}
			sb.WriteString(extractTextFromJSON(data))
		}
		// If no SSE data lines were found, use raw payload as fallback.
		if sb.Len() == 0 {
			sb.Write(chunk.Payload)
		}
	}
	return sb.String()
}

// extractTextFromJSON extracts text content from a JSON SSE event payload.
// Supports OpenAI chat completions (choices[].delta.content), OpenAI responses
// (delta field, text field), and Anthropic messages (delta.text).
func extractTextFromJSON(data []byte) string {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return ""
	}

	// OpenAI Responses API: {"type":"response.output_text.delta","delta":"..."}
	if deltaRaw, ok := raw["delta"]; ok {
		var s string
		if json.Unmarshal(deltaRaw, &s) == nil {
			return s
		}
	}

	// OpenAI Responses API done: {"type":"response.output_text.done","text":"..."}
	if textRaw, ok := raw["text"]; ok {
		var s string
		if json.Unmarshal(textRaw, &s) == nil {
			return s
		}
	}

	// OpenAI Chat Completions: {"choices":[{"delta":{"content":"..."}}]}
	if choicesRaw, ok := raw["choices"]; ok {
		var choices []struct {
			Delta struct {
				Content string `json:"content"`
			} `json:"delta"`
		}
		if json.Unmarshal(choicesRaw, &choices) == nil {
			var sb strings.Builder
			for _, c := range choices {
				sb.WriteString(c.Delta.Content)
			}
			return sb.String()
		}
	}

	return ""
}
