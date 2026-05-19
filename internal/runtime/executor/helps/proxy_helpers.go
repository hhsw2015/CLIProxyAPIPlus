package helps

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/headroom"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/proxypool"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/proxyutil"
	log "github.com/sirupsen/logrus"
)

var headroomDebugCounter atomic.Uint64

func dumpHeadroomDebug(format string, body []byte) {
	dir := os.Getenv("HEADROOM_DEBUG_DIR")
	if dir == "" {
		return
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	n := headroomDebugCounter.Add(1)
	if n > 5 {
		return
	}
	name := fmt.Sprintf("%s_%d_%d.json", format, time.Now().Unix(), n)
	_ = os.WriteFile(filepath.Join(dir, name), body, 0o644)
}

// httpClientCache caches HTTP clients by proxy URL to enable connection reuse
var (
	httpClientCache      = make(map[string]*http.Client)
	httpClientCacheMutex sync.RWMutex
)

// NewProxyAwareHTTPClient creates an HTTP client with proper proxy configuration priority:
// 1. Use auth.ProxyURL if configured (highest priority)
// 2. Use proxy pool (in-process ECH workers) if enabled and no auth proxy
// 3. Use cfg.ProxyURL if neither auth proxy nor pool is available
// 4. Fall back to direct connection (no proxy) if nothing is configured
//
// This function caches HTTP clients by proxy URL to enable TCP/TLS connection reuse.
//
// Parameters:
//   - ctx: The context containing optional RoundTripper
//   - cfg: The application configuration
//   - auth: The authentication information
//   - timeout: The client timeout (0 means no timeout)
//
// Returns:
//   - *http.Client: An HTTP client with configured proxy or transport
func NewProxyAwareHTTPClient(ctx context.Context, cfg *config.Config, auth *cliproxyauth.Auth, timeout time.Duration) *http.Client {
	// Priority 1: Use auth.ProxyURL if configured
	var proxyURL string
	if auth != nil {
		proxyURL = strings.TrimSpace(auth.ProxyURL)
	}

	// Priority 2: proxy pool (when enabled, replaces global proxy)
	if proxyURL == "" {
		if transport := proxypool.GetTransport(); transport != nil {
			client := &http.Client{Transport: transport}
			if timeout > 0 {
				client.Timeout = timeout
			}
			return client
		}
	}

	// Priority 3: Use cfg.ProxyURL if auth proxy is not configured
	if proxyURL == "" && cfg != nil {
		proxyURL = strings.TrimSpace(cfg.ProxyURL)
	}

	// If we have a proxy URL configured, try cache first to reuse TCP/TLS connections.
	if proxyURL != "" {
		httpClientCacheMutex.RLock()
		if cachedClient, ok := httpClientCache[proxyURL]; ok {
			httpClientCacheMutex.RUnlock()
			if timeout > 0 {
				return &http.Client{Transport: cachedClient.Transport, Timeout: timeout}
			}
			return cachedClient
		}
		httpClientCacheMutex.RUnlock()
	}

	// Create new client
	httpClient := &http.Client{}
	if timeout > 0 {
		httpClient.Timeout = timeout
	}

	// If we have a proxy URL configured, set up the transport
	if proxyURL != "" {
		transport := buildProxyTransport(proxyURL)
		if transport != nil {
			httpClient.Transport = transport
			// Cache the client
			httpClientCacheMutex.Lock()
			httpClientCache[proxyURL] = httpClient
			httpClientCacheMutex.Unlock()
			return httpClient
		}
		// If proxy setup failed, log and fall through to context RoundTripper
		log.Debugf("failed to setup proxy from URL: %s, falling back to context transport", proxyutil.Redact(proxyURL))
	}

	// Priority 4: Use RoundTripper from context (typically from RoundTripperFor)
	if rt, ok := ctx.Value("cliproxy.roundtripper").(http.RoundTripper); ok && rt != nil {
		httpClient.Transport = rt
	}

	return httpClient
}

// buildProxyTransport creates an HTTP transport configured for the given proxy URL.
// It supports SOCKS5, HTTP, and HTTPS proxy protocols.
//
// Parameters:
//   - proxyURL: The proxy URL string (e.g., "socks5://user:pass@host:port", "http://host:port")
//
// Returns:
//   - *http.Transport: A configured transport, or nil if the proxy URL is invalid
func buildProxyTransport(proxyURL string) *http.Transport {
	transport, _, errBuild := proxyutil.BuildHTTPTransport(proxyURL)
	if errBuild != nil {
		log.Errorf("%v", errBuild)
		return nil
	}
	return transport
}

// extractModel reads the "model" field from request body JSON bytes.
func extractModel(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	var j map[string]any
	if json.Unmarshal(body, &j) != nil {
		return ""
	}
	if m, ok := j["model"].(string); ok && m != "" {
		return m
	}
	return ""
}

// 8 MiB cap on compressible body size; bodies larger than this skip FFI to
// bound compression latency. The body itself is still forwarded unchanged.
const headroomMaxBodyBytes = 8 << 20

// bodyFormat indicates the JSON schema of a request body — used to dispatch
// to the matching FFI compressor.
type bodyFormat int

const (
	formatNone bodyFormat = iota
	formatOpenAIChat
	formatOpenAIResponses
	formatAnthropic
)


// classifyRequest returns the body format for endpoints headroom-ffi knows
// how to compress. Returns formatNone for token validation, model listing,
// OAuth refresh, count_tokens, Gemini, etc. — those pass through unchanged.
func classifyRequest(req *http.Request) bodyFormat {
	if req == nil || req.URL == nil {
		return formatNone
	}
	if req.Method != http.MethodPost && req.Method != http.MethodPut {
		return formatNone
	}
	p := req.URL.Path
	if strings.Contains(p, "/chat/completions") {
		return formatOpenAIChat
	}
	// OpenAI Responses API (Codex `/v1/responses`) — different schema than
	// chat/completions: top-level `input` array of items.
	if strings.HasSuffix(p, "/responses") {
		return formatOpenAIResponses
	}
	// Anthropic Messages: /v1/messages or /anthropic/v1/messages, but NOT
	// /messages/count_tokens (token introspection — must not compress).
	if strings.HasSuffix(p, "/v1/messages") {
		return formatAnthropic
	}
	return formatNone
}

// HeadroomDo wraps httpClient.Do with headroom FFI compression.
// Falls back to the original body on compression failure or when disabled.
// Safe for nil/empty bodies and non-chat endpoints (passes through).
func HeadroomDo(httpClient *http.Client, req *http.Request) (*http.Response, error) {
	format := classifyRequest(req)
	if req == nil || req.Body == nil || format == formatNone {
		return httpClient.Do(req)
	}
	// Read the full body — CPA executors have already serialized JSON in
	// memory before this point, so we are not introducing new allocations.
	// Truncating with LimitReader would silently drop bytes when the body
	// exceeds the size cap, producing invalid requests upstream.
	body, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}

	// Always set GetBody so retries (auth refresh, transient errors) can
	// reconstruct the request body from a stable snapshot, regardless of
	// whether compression runs.
	req.Body = io.NopCloser(bytes.NewReader(body))
	req.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(body)), nil
	}
	req.ContentLength = int64(len(body))

	if len(body) > headroomMaxBodyBytes {
		return httpClient.Do(req)
	}

	modelStr := extractModel(body)
	authMode := headroom.AuthModeFromContext(req.Context())
	var result *headroom.Result
	switch format {
	case formatAnthropic:
		// FFI auto-detects cache_control markers and raises the frozen
		// floor accordingly. The user-supplied floor (config + per-request
		// header X-Headroom-Frozen-Count) is passed through; FFI takes
		// max(user, computed) so cache_control always wins for safety.
		userFloor := headroom.AnthropicFrozenCount()
		if hdr := req.Header.Get("X-Headroom-Frozen-Count"); hdr != "" {
			if v, err := strconv.Atoi(hdr); err == nil && v >= 0 {
				userFloor = v
			}
		}
		dumpHeadroomDebug("anthropic", body)
		// PR-E1/E2: normalize tool definitions (sort + schema-key sort) BEFORE
		// compression. Stabilizes the tools-prefix bytes so upstream prompt
		// cache survives across reorderings. PAYG-only safe; FFI internally
		// gates on auth mode and passes through for OAuth/Subscription.
		if norm := headroom.NormalizeAnthropicTools(body, authMode); norm != nil && norm.Modified {
			body = norm.Body
			req.Body = io.NopCloser(bytes.NewReader(body))
			req.GetBody = func() (io.ReadCloser, error) {
				return io.NopCloser(bytes.NewReader(body)), nil
			}
			req.ContentLength = int64(len(body))
			log.Infof("[headroom] normalize anthropic tools: e1=%v e2=%v", norm.E1Applied, norm.E2Applied)
		}
		result = headroom.CompressAnthropicBytes(body, modelStr, userFloor, authMode)
	case formatOpenAIResponses:
		result = headroom.CompressResponsesBytes(body, modelStr, authMode)
	default:
		result = headroom.CompressBytes(body, modelStr, authMode)
	}
	if result.Error != nil {
		log.Warnf("[headroom] compress error: %v", result.Error)
	} else if !result.Modified {
		log.Infof("[headroom] no-change format=%d body=%d model=%q tokens=%d path=%s",
			format, len(body), modelStr, result.TokensBefore, req.URL.Path)
	} else if result.Modified && len(result.CompressedBody) > 0 {
		if modelStr == "" {
			modelStr = "unknown"
		}
		log.Infof("[headroom] %s: %d→%d tokens (saved %d, ratio %.2f) body %d→%d bytes",
			modelStr, result.TokensBefore, result.TokensAfter,
			result.TokensSaved, result.CompressionRatio,
			len(body), len(result.CompressedBody))
		compressed := result.CompressedBody
		req.Body = io.NopCloser(bytes.NewReader(compressed))
		req.GetBody = func() (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader(compressed)), nil
		}
		req.ContentLength = int64(len(compressed))
	}
	return httpClient.Do(req)
}
