package chat_completions

import (
	"context"
	"testing"

	"github.com/tidwall/gjson"
)

// Interactions model_output steps can carry inline media (e.g. gemini-omni returns
// {"type":"video","data":...,"mime_type":"video/mp4"}). The egress converter must not
// drop it: OpenAI clients receive media as choices[].message.images[] data URLs.
func TestInteractionsResponseToOpenAINonStreamCarriesVideo(t *testing.T) {
	raw := []byte(`{"id":"ix_1","model":"gemini-omni-1.1-flash","status":"completed",` +
		`"steps":[{"type":"model_output","content":[` +
		`{"type":"text","text":"here is the clip"},` +
		`{"type":"video","data":"QUFB","mime_type":"video/mp4"}]}],` +
		`"usage":{"total_input_tokens":5,"total_output_tokens":7,"total_tokens":12}}`)

	out := ConvertInteractionsResponseToOpenAINonStream(context.Background(), "gemini-omni-1.1-flash", nil, nil, raw, nil)
	root := gjson.ParseBytes(out)

	if got := root.Get("choices.0.message.content").String(); got != "here is the clip" {
		t.Fatalf("content = %q, want %q", got, "here is the clip")
	}
	if n := root.Get("choices.0.message.images.#").Int(); n != 1 {
		t.Fatalf("images count = %d, want 1", n)
	}
	if got := root.Get("choices.0.message.images.0.image_url.url").String(); got != "data:video/mp4;base64,QUFB" {
		t.Fatalf("image url = %q, want data:video/mp4;base64,QUFB", got)
	}
}

// A media-only model_output (no text) must still surface the media, not an empty message.
func TestInteractionsResponseToOpenAINonStreamMediaOnly(t *testing.T) {
	raw := []byte(`{"id":"ix_2","model":"gemini-omni-1.1-flash",` +
		`"steps":[{"type":"model_output","content":[` +
		`{"type":"image","data":"SU1H","mime_type":"image/png"}]}]}`)

	out := ConvertInteractionsResponseToOpenAINonStream(context.Background(), "gemini-omni-1.1-flash", nil, nil, raw, nil)
	root := gjson.ParseBytes(out)

	if got := root.Get("choices.0.message.images.0.image_url.url").String(); got != "data:image/png;base64,SU1H" {
		t.Fatalf("image url = %q, want data:image/png;base64,SU1H", got)
	}
}
