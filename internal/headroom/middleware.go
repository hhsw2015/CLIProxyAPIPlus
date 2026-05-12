// Package headroom provides FFI bindings to headroom-core compression.
package headroom

// #cgo CFLAGS: -I${SRCDIR}
// #cgo LDFLAGS: -lheadroom_ffi
// #include <stdlib.h>
// #include <headroom.h>
import "C"

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"unsafe"
)

// AuthMode mirrors headroom-core's AuthMode enum. Selects the headroom
// pricing/compression policy variant.
type AuthMode uint8

const (
	AuthPayg         AuthMode = 0
	AuthOAuth        AuthMode = 1
	AuthSubscription AuthMode = 2
	AuthUnknown      AuthMode = 3
)

// authModeContextKey gates the headroom AuthMode override. Conductor calls
// WithAuthMode to attach the upstream auth tier; HeadroomDo reads it back.
type authModeContextKey struct{}

// WithAuthMode returns a child context carrying the headroom AuthMode.
func WithAuthMode(ctx context.Context, mode AuthMode) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, authModeContextKey{}, mode)
}

// AuthModeFromContext extracts the headroom AuthMode for the request.
// Defaults to Payg (most aggressive compression policy) when no override.
func AuthModeFromContext(ctx context.Context) AuthMode {
	if ctx == nil {
		return AuthPayg
	}
	if v, ok := ctx.Value(authModeContextKey{}).(AuthMode); ok {
		return v
	}
	return AuthPayg
}

// AuthModeFromAccountType maps the type string returned by Auth.AccountInfo()
// to an AuthMode. Unrecognised types fall back to Payg (safe default).
func AuthModeFromAccountType(t string) AuthMode {
	switch strings.ToLower(strings.TrimSpace(t)) {
	case "oauth":
		return AuthOAuth
	case "subscription":
		return AuthSubscription
	case "", "api_key", "personal_access_token", "service_account":
		return AuthPayg
	default:
		return AuthUnknown
	}
}

// Config controls runtime compression behavior. See sdk_config.HeadroomConfig
// for the YAML-facing equivalent and filter evaluation order.
type Config struct {
	Enabled              bool
	MinBytes             int
	Allow                []string
	Deny                 []string
	AnthropicFrozenCount int
}

var (
	enabled              atomic.Bool
	filtersMu            sync.RWMutex
	minBytes             int
	allowGlobs           []string
	denyGlobs            []string
	anthropicFrozenCount int
)

// AnthropicFrozenCount returns the configured Anthropic floor.
func AnthropicFrozenCount() int {
	filtersMu.RLock()
	defer filtersMu.RUnlock()
	return anthropicFrozenCount
}

// SetConfig updates headroom configuration. Safe to call concurrently with
// CompressBytes; new values become visible to subsequent calls.
func SetConfig(c Config) {
	enabled.Store(c.Enabled)
	filtersMu.Lock()
	minBytes = c.MinBytes
	// Three-arg reslice (cap 0) forces append to allocate a fresh backing
	// array, so any reader that snapshotted the old slice header continues
	// to observe the old contents until GC reclaims them.
	allowGlobs = append(allowGlobs[:0:0], c.Allow...)
	denyGlobs = append(denyGlobs[:0:0], c.Deny...)
	anthropicFrozenCount = c.AnthropicFrozenCount
	filtersMu.Unlock()
}

// shouldRun applies the body-size + allow/deny filters. Returns true when
// compression should proceed.
func shouldRun(body []byte, model string) bool {
	filtersMu.RLock()
	threshold := minBytes
	allow := allowGlobs
	deny := denyGlobs
	filtersMu.RUnlock()

	if threshold > 0 && len(body) < threshold {
		return false
	}
	probe := model
	if probe == "" {
		probe = "unknown"
	}
	for _, g := range deny {
		if matched, _ := path.Match(g, probe); matched {
			return false
		}
	}
	if len(allow) == 0 {
		return true
	}
	for _, g := range allow {
		if matched, _ := path.Match(g, probe); matched {
			return true
		}
	}
	return false
}

// Result holds compression output.
type Result struct {
	Modified         bool
	CompressedBody   []byte
	TokensBefore     int
	TokensAfter      int
	TokensSaved      int
	CompressionRatio float64
	Error            error
}

// ccrMarkerRe matches the `<<ccr:HASH>>` markers injected by headroom-core
// into compressed payloads. HASH is 24 lowercase hex chars (see
// headroom-core/src/ccr/mod.rs::marker_format_is_pinned).
var ccrMarkerRe = regexp.MustCompile(`<<ccr:([a-f0-9]{24})>>`)

// ExpandMarkers replaces every `<<ccr:HASH>>` marker in text with the original
// content from the CCR store. Markers whose hash is unknown or expired are
// left intact. Useful for post-processing model responses that quoted or
// echoed compressed payloads.
//
// Returns the rewritten text and the number of markers successfully expanded.
// On empty input or text without markers, returned unchanged with count 0.
func ExpandMarkers(text string) (string, int) {
	if text == "" {
		return text, 0
	}
	expanded := 0
	out := ccrMarkerRe.ReplaceAllStringFunc(text, func(match string) string {
		hash := match[len("<<ccr:") : len(match)-len(">>")]
		content, found, err := CcrGet(hash)
		if err != nil || !found {
			return match
		}
		expanded++
		return content
	})
	return out, expanded
}

// StreamExpander applies CCR marker expansion to a stream of byte chunks
// without losing markers split across chunk boundaries. The expander keeps
// at most one partial marker (~32 bytes) buffered at the trailing edge.
//
// Lifecycle: NewStreamExpander → Write per chunk → Flush at stream end.
// Not safe for concurrent use; create one per stream.
type StreamExpander struct {
	buf bytes.Buffer
}

// NewStreamExpander returns an expander ready to consume chunks.
func NewStreamExpander() *StreamExpander { return &StreamExpander{} }

// Write appends chunk to the internal buffer, expands any complete markers
// found, and returns the prefix of the buffer that is safe to forward.
// Trailing bytes that could be the start of an incomplete marker are
// retained for the next call.
func (e *StreamExpander) Write(chunk []byte) []byte {
	if len(chunk) == 0 {
		return nil
	}
	e.buf.Write(chunk)
	data := e.buf.Bytes()

	// Replace all complete markers in-place. Regex anchored to the full
	// 32-byte marker shape so partial trailing fragments stay raw.
	expanded := ccrMarkerRe.ReplaceAllFunc(data, func(match []byte) []byte {
		hash := string(match[len("<<ccr:") : len(match)-len(">>")])
		content, found, err := CcrGet(hash)
		if err != nil || !found {
			return match
		}
		return []byte(content)
	})

	// Keep enough trailing bytes to hold a partial marker. A complete marker
	// is 32 bytes (<<ccr: + 24 hex + >>). Buffering policy is tiered to
	// minimise delay on streams whose content happens to contain '<' or even
	// `<<...>>` (HTML, markdown, code templates):
	//   1. Look for the rightmost '<<' in the trailing window. If the bytes
	//      from there to end are a valid prefix of "<<ccr:" (or already
	//      start with it), buffer from there — could still complete.
	//   2. Otherwise, if the very last byte is '<', buffer just that byte
	//      so a marker split exactly at '<<' across chunks still matches.
	//   3. Otherwise flush the whole expansion. Crucially, `<<some_text>>`
	//      content the model echoes is NOT held back.
	const markerLen = 32
	markerPrefix := []byte("<<ccr:")
	keepWindow := len(expanded) - (markerLen - 1)
	if keepWindow < 0 {
		keepWindow = 0
	}
	keepStart := len(expanded)
	if idx := bytes.LastIndex(expanded[keepWindow:], []byte("<<")); idx >= 0 {
		abs := keepWindow + idx
		rest := expanded[abs:]
		// rest is a viable partial marker iff it could grow into "<<ccr:..."
		// — either rest is itself a prefix of the marker prefix (rest len
		// <= 6) or it already starts with the full marker prefix.
		viable := false
		if len(rest) <= len(markerPrefix) {
			viable = bytes.HasPrefix(markerPrefix, rest)
		} else {
			viable = bytes.HasPrefix(rest, markerPrefix)
		}
		if viable {
			keepStart = abs
		}
	}
	if keepStart == len(expanded) && len(expanded) > 0 && expanded[len(expanded)-1] == '<' {
		keepStart = len(expanded) - 1
	}

	flushable := append([]byte(nil), expanded[:keepStart]...)
	e.buf.Reset()
	e.buf.Write(expanded[keepStart:])
	return flushable
}

// Flush returns any bytes still buffered. Call once at stream end. Any
// trailing partial marker is emitted unchanged because the upstream is
// done — the partial cannot be completed.
func (e *StreamExpander) Flush() []byte {
	if e.buf.Len() == 0 {
		return nil
	}
	out := append([]byte(nil), e.buf.Bytes()...)
	e.buf.Reset()
	return out
}

// CcrGet retrieves original content stored under hash. CCR is a data layer
// independent of the Enabled flag — once a backend is registered, lookups
// continue to work even if compression is later disabled, so previously
// compressed payloads remain dereferenceable. Returns the content and a
// bool indicating presence. Errors propagate from the FFI layer (e.g. bad
// UTF-8, panic).
func CcrGet(hash string) (string, bool, error) {
	cHash := C.CString(hash)
	defer C.free(unsafe.Pointer(cHash))
	cResult := C.headroom_ccr_get(cHash)
	defer C.headroom_result_free(cResult)
	if cResult == nil {
		return "", false, errors.New("nil ccr result")
	}
	var got struct {
		Found   bool    `json:"found"`
		Content string  `json:"content"`
		Error   *string `json:"error"`
	}
	if err := json.Unmarshal([]byte(C.GoString(cResult)), &got); err != nil {
		return "", false, fmt.Errorf("ccr parse: %w", err)
	}
	if got.Error != nil && *got.Error != "" {
		return "", false, errors.New(*got.Error)
	}
	return got.Content, got.Found, nil
}

// RegisterSqliteCCRStore swaps the in-memory CCR store for a persistent
// SQLite-backed one. The store is independent of the Enabled flag — registry
// only governs WHERE compression artefacts live, not WHETHER compression
// runs. Pass ttlSeconds=0 to use the headroom default (300s). Safe to call
// after SetConfig; idempotent on the same dbPath.
func RegisterSqliteCCRStore(dbPath string, ttlSeconds uint64) error {
	cPath := C.CString(dbPath)
	defer C.free(unsafe.Pointer(cPath))
	cErr := C.headroom_ccr_init_sqlite(cPath, C.ulonglong(ttlSeconds))
	if cErr == nil {
		return nil
	}
	defer C.headroom_result_free(cErr)
	return errors.New(C.GoString(cErr))
}

// CompressBytes compresses raw OpenAI-format chat-completions body bytes.
func CompressBytes(body []byte, model string, auth AuthMode) *Result {
	if !enabled.Load() || len(body) == 0 || !shouldRun(body, model) {
		return &Result{Modified: false, CompressedBody: body}
	}
	cBody := C.CBytes(body)
	defer C.free(unsafe.Pointer(cBody))
	bodyLen := C.size_t(len(body))
	cModel := C.CString(model)
	defer C.free(unsafe.Pointer(cModel))

	cResult := C.headroom_compress_openai(
		(*C.uchar)(cBody), bodyLen, cModel, C.uint8_t(auth))
	defer C.headroom_result_free(cResult)
	return parseResult(cResult, body)
}

// CompressAnthropicBytes compresses raw Anthropic Messages-format body bytes.
// frozenCount is the number of leading messages pinned from compression
// (typically 0; set higher to preserve few-shot examples or seeded turns).
func CompressAnthropicBytes(body []byte, model string, frozenCount int, auth AuthMode) *Result {
	if !enabled.Load() || len(body) == 0 || !shouldRun(body, model) {
		return &Result{Modified: false, CompressedBody: body}
	}
	cBody := C.CBytes(body)
	defer C.free(unsafe.Pointer(cBody))
	bodyLen := C.size_t(len(body))
	cModel := C.CString(model)
	defer C.free(unsafe.Pointer(cModel))

	cResult := C.headroom_compress_anthropic(
		(*C.uchar)(cBody), bodyLen, C.size_t(frozenCount), cModel, C.uint8_t(auth))
	defer C.headroom_result_free(cResult)
	return parseResult(cResult, body)
}

// CompressResponsesBytes compresses raw OpenAI Responses-API body bytes
// (Codex /v1/responses endpoint — `input` array schema).
func CompressResponsesBytes(body []byte, model string, auth AuthMode) *Result {
	if !enabled.Load() || len(body) == 0 || !shouldRun(body, model) {
		return &Result{Modified: false, CompressedBody: body}
	}
	cBody := C.CBytes(body)
	defer C.free(unsafe.Pointer(cBody))
	bodyLen := C.size_t(len(body))
	cModel := C.CString(model)
	defer C.free(unsafe.Pointer(cModel))

	cResult := C.headroom_compress_openai_responses(
		(*C.uchar)(cBody), bodyLen, cModel, C.uint8_t(auth))
	defer C.headroom_result_free(cResult)
	return parseResult(cResult, body)
}

// RegisterRedisCCRStore swaps the active CCR store for a Redis-backed one.
// Pass keyPrefix="" to use the headroom default. ttlSeconds=0 uses the
// headroom default (300s). Suited for multi-instance deployments needing
// fleet-wide shared CCR.
func RegisterRedisCCRStore(url string, keyPrefix string, ttlSeconds uint64) error {
	cURL := C.CString(url)
	defer C.free(unsafe.Pointer(cURL))
	var cPrefix *C.char
	if keyPrefix != "" {
		cPrefix = C.CString(keyPrefix)
		defer C.free(unsafe.Pointer(cPrefix))
	}
	cErr := C.headroom_ccr_init_redis(cURL, cPrefix, C.ulonglong(ttlSeconds))
	if cErr == nil {
		return nil
	}
	defer C.headroom_result_free(cErr)
	return errors.New(C.GoString(cErr))
}

// --- internal ---

type crJSON struct {
	Modified         bool    `json:"modified"`
	Body             *string `json:"body"`
	TokensBefore     int     `json:"tokens_before"`
	TokensAfter      int     `json:"tokens_after"`
	TokensSaved      int     `json:"tokens_saved"`
	CompressionRatio float64 `json:"compression_ratio"`
	Error            *string `json:"error"`
}

func parseResult(cResult *C.char, originalBody []byte) *Result {
	if cResult == nil {
		return &Result{CompressedBody: originalBody, Error: errors.New("nil ffi result")}
	}
	resultStr := C.GoString(cResult)
	var cr crJSON
	if err := json.Unmarshal([]byte(resultStr), &cr); err != nil {
		return &Result{CompressedBody: originalBody, Error: fmt.Errorf("parse: %w", err)}
	}
	out := &Result{
		Modified:         cr.Modified && cr.Body != nil,
		CompressedBody:   originalBody,
		TokensBefore:     cr.TokensBefore,
		TokensAfter:      cr.TokensAfter,
		TokensSaved:      cr.TokensSaved,
		CompressionRatio: cr.CompressionRatio,
	}
	if cr.Error != nil && *cr.Error != "" {
		out.Modified = false
		out.Error = errors.New(*cr.Error)
		return out
	}
	if out.Modified {
		out.CompressedBody = []byte(*cr.Body)
	}
	return out
}
