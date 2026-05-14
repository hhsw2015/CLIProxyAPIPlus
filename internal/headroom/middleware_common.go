// Package headroom: types + non-FFI helpers shared between the cgo-backed
// FFI implementation (build tag headroom_ffi) and the stub fallback.
package headroom

import (
	"bytes"
	"context"
	"path"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
)

type AuthMode uint8

const (
	AuthPayg         AuthMode = 0
	AuthOAuth        AuthMode = 1
	AuthSubscription AuthMode = 2
	AuthUnknown      AuthMode = 3
)

type authModeContextKey struct{}

func WithAuthMode(ctx context.Context, mode AuthMode) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, authModeContextKey{}, mode)
}

func AuthModeFromContext(ctx context.Context) AuthMode {
	if ctx == nil {
		return AuthPayg
	}
	if v, ok := ctx.Value(authModeContextKey{}).(AuthMode); ok {
		return v
	}
	return AuthPayg
}

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

func AnthropicFrozenCount() int {
	filtersMu.RLock()
	defer filtersMu.RUnlock()
	return anthropicFrozenCount
}

func SetConfig(c Config) {
	enabled.Store(c.Enabled)
	filtersMu.Lock()
	minBytes = c.MinBytes
	allowGlobs = append(allowGlobs[:0:0], c.Allow...)
	denyGlobs = append(denyGlobs[:0:0], c.Deny...)
	anthropicFrozenCount = c.AnthropicFrozenCount
	filtersMu.Unlock()
}

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

type Result struct {
	Modified         bool
	CompressedBody   []byte
	TokensBefore     int
	TokensAfter      int
	TokensSaved      int
	CompressionRatio float64
	Error            error
}

type NormalizeResult struct {
	Modified  bool
	Body      []byte
	E1Applied bool
	E2Applied bool
	Error     error
}

var ccrMarkerRe = regexp.MustCompile(`<<ccr:([a-f0-9]{24})>>`)

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

type StreamExpander struct {
	buf bytes.Buffer
}

func NewStreamExpander() *StreamExpander { return &StreamExpander{} }

func (e *StreamExpander) Write(chunk []byte) []byte {
	if len(chunk) == 0 {
		return nil
	}
	e.buf.Write(chunk)
	data := e.buf.Bytes()

	expanded := ccrMarkerRe.ReplaceAllFunc(data, func(match []byte) []byte {
		hash := string(match[len("<<ccr:") : len(match)-len(">>")])
		content, found, err := CcrGet(hash)
		if err != nil || !found {
			return match
		}
		return []byte(content)
	})

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

func (e *StreamExpander) Flush() []byte {
	if e.buf.Len() == 0 {
		return nil
	}
	out := append([]byte(nil), e.buf.Bytes()...)
	e.buf.Reset()
	return out
}
