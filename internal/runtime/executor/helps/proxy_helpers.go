package helps

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/headroom"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/proxypool"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/proxyutil"
	log "github.com/sirupsen/logrus"
)

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
		log.Debugf("failed to setup proxy from URL: %s, falling back to context transport", proxyURL)
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

// shouldCompress returns true only for endpoints whose body is OpenAI-format
// chat-completions JSON, which is the only schema headroom-ffi currently
// understands. Anthropic Messages (/v1/messages, /v1/messages/count_tokens),
// Google Gemini (:generateContent, :streamGenerateContent), token
// validation, model listing and OAuth refresh all pass through unchanged.
func shouldCompress(req *http.Request) bool {
	if req == nil || req.URL == nil {
		return false
	}
	if req.Method != http.MethodPost && req.Method != http.MethodPut {
		return false
	}
	p := req.URL.Path
	return strings.Contains(p, "/chat/completions") ||
		strings.HasSuffix(p, "/responses")
}

// HeadroomDo wraps httpClient.Do with headroom FFI compression.
// Falls back to the original body on compression failure or when disabled.
// Safe for nil/empty bodies and non-chat endpoints (passes through).
func HeadroomDo(httpClient *http.Client, req *http.Request) (*http.Response, error) {
	if req == nil || req.Body == nil || !shouldCompress(req) {
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
	result := headroom.CompressBytes(body, modelStr)
	if result.Error != nil {
		log.Warnf("[headroom] compress error: %v", result.Error)
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
