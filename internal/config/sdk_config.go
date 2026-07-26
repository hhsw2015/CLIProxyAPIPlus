// Package config provides configuration management for the CLI Proxy API server.
// It handles loading and parsing YAML configuration files, and provides structured
// access to application settings including server port, authentication directory,
// debug settings, proxy configuration, and API keys.
package config

// SDKConfig represents the application's configuration, loaded from a YAML file.
// HeadroomConfig controls in-process compression via headroom-core FFI.
// Falls back to original body if compression fails or is disabled.
//
// Filters are evaluated in this order; failing any one skips compression:
//  1. Enabled must be true
//  2. body length must be >= MinBytes (when MinBytes > 0)
//  3. model must NOT match any Deny glob
//  4. when Allow is non-empty, model must match at least one Allow glob
//
// Globs use Go path.Match semantics: '*' / '?' / '[a-z]'. Note that '*' does
// NOT cross '/' (path.Match treats '/' as a separator), so for vendor/model
// style names match each segment explicitly (e.g. "vendor/*"). The empty
// model string (no "model" field in body) is treated as "unknown" for
// matching, so deny: ["unknown"] suppresses unrecognised payloads.
// WebSearchConfig controls gateway-level web search interception.
//
// Two configuration shapes are supported (one OR the other, or both):
//
//  1. Legacy (single provider):
//     provider: tinyfish
//     api-keys: [sk-...]
//
//  2. Provider catalog + selectable primary (preferred):
//     provider: tinyfish        # primary, just rename to switch
//     fallbacks: [anysearch]    # optional ordered fallback names
//     providers:
//     tinyfish:  { api-keys: [...] }
//     anysearch: { api-keys: [...] }
//
// When `providers` is set, top-level `api-keys` is treated as the keys for
// the primary provider only if that provider has no entry in the map.
type WebSearchConfig struct {
	Enabled  bool     `yaml:"enabled" json:"enabled"`
	Provider string   `yaml:"provider,omitempty" json:"provider,omitempty"`
	APIKeys  []string `yaml:"api-keys,omitempty" json:"api-keys,omitempty"`
	// Fallbacks is an ordered list of provider names from `Providers` to try
	// after the primary fails. Names not present in the map are skipped.
	Fallbacks []string `yaml:"fallbacks,omitempty" json:"fallbacks,omitempty"`
	// Providers is the catalog of all configured search backends keyed by
	// provider id (tinyfish, anysearch, bing-rss, ...). Each entry holds
	// that backend's credentials.
	Providers map[string]WebSearchProvider `yaml:"providers,omitempty" json:"providers,omitempty"`
}

// WebSearchProvider holds a backend's credentials.
type WebSearchProvider struct {
	APIKeys []string `yaml:"api-keys,omitempty" json:"api-keys,omitempty"`
}

// ThrottleConfig controls concurrent request limiting with backpressure.
type ThrottleConfig struct {
	MaxConcurrency int `yaml:"max-concurrency,omitempty" json:"max-concurrency,omitempty"`
	Backlog        int `yaml:"backlog,omitempty" json:"backlog,omitempty"`
	TimeoutSeconds int `yaml:"timeout-seconds,omitempty" json:"timeout-seconds,omitempty"`
}

// RecoveryProbeConfig controls periodic probing of disabled auth entries.
type RecoveryProbeConfig struct {
	Enabled         bool `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	IntervalSeconds int  `yaml:"interval-seconds,omitempty" json:"interval-seconds,omitempty"`
	BackoffBaseMin  int  `yaml:"backoff-base-min,omitempty" json:"backoff-base-min,omitempty"`
	BackoffMaxMin   int  `yaml:"backoff-max-min,omitempty" json:"backoff-max-min,omitempty"`
	Concurrency     int  `yaml:"concurrency,omitempty" json:"concurrency,omitempty"`
}

// RTKConfig controls in-process tool_result compression. See RTK field doc on
// SDKConfig for behavior.
type RTKConfig struct {
	Enabled bool `yaml:"enabled" json:"enabled"`
	// MinSavingsPct is the minimum percentage reduction required to keep the
	// compressed output. Below this threshold the original is preserved.
	// Default 5 (i.e. compression must save at least 5%).
	MinSavingsPct int `yaml:"min-savings-pct,omitempty" json:"min-savings-pct,omitempty"`
}

type HeadroomConfig struct {
	Enabled  bool     `yaml:"enabled" json:"enabled"`
	MinBytes int      `yaml:"min-bytes,omitempty" json:"min-bytes,omitempty"`
	Allow    []string `yaml:"allow,omitempty" json:"allow,omitempty"`
	Deny     []string `yaml:"deny,omitempty" json:"deny,omitempty"`
	// CCRSqlitePath enables a persistent SQLite-backed CCR store at the
	// given path. Empty string keeps the default in-memory store. Mutually
	// exclusive with CCRRedisURL — Redis takes precedence.
	CCRSqlitePath string `yaml:"ccr-sqlite-path,omitempty" json:"ccr-sqlite-path,omitempty"`
	// CCRRedisURL enables a Redis-backed CCR store for fleet-wide sharing
	// (e.g. redis://127.0.0.1:6379). Takes precedence over CCRSqlitePath.
	CCRRedisURL string `yaml:"ccr-redis-url,omitempty" json:"ccr-redis-url,omitempty"`
	// CCRRedisKeyPrefix optionally namespaces Redis keys.
	CCRRedisKeyPrefix string `yaml:"ccr-redis-key-prefix,omitempty" json:"ccr-redis-key-prefix,omitempty"`
	// CCRTtlSeconds sets entry TTL for the chosen backend. 0 = headroom
	// default (300s).
	CCRTtlSeconds uint64 `yaml:"ccr-ttl-seconds,omitempty" json:"ccr-ttl-seconds,omitempty"`
	// AnthropicFrozenCount pins the first N Anthropic messages from
	// compression unconditionally. Useful for protecting few-shot examples
	// or system seed turns. The FFI also auto-detects cache_control
	// markers — the effective floor is max(this value, computed). 0 means
	// "rely entirely on cache_control auto-detect".
	AnthropicFrozenCount int `yaml:"anthropic-frozen-count,omitempty" json:"anthropic-frozen-count,omitempty"`
}

// ProxyPoolConfig controls the embedded ECH worker proxy pool.
// When enabled, CPA manages ECH worker processes directly and routes requests
// through persistent per-worker connections, eliminating the external warp-pool hop.
//
// In addition to ECH workers, in-process Cloudflare WARP MASQUE tunnels can be
// added via WARPInstances. Each tunnel exits at a unique Cloudflare WARP IP and
// participates in the same weighted round-robin pool as ECH workers.
type ProxyPoolConfig struct {
	Enabled       bool              `yaml:"enabled" json:"enabled"`
	Workers       []ECHWorkerConfig `yaml:"workers" json:"workers"`
	WARPInstances []WARPInstance    `yaml:"warp-instances,omitempty" json:"warp-instances,omitempty"`
	IncludeDirect bool              `yaml:"include-direct,omitempty" json:"include-direct,omitempty"`
	WeightECH     int               `yaml:"weight-ech,omitempty" json:"weight-ech,omitempty"`
	WeightWARP    int               `yaml:"weight-warp,omitempty" json:"weight-warp,omitempty"`
	WeightDirect  int               `yaml:"weight-direct,omitempty" json:"weight-direct,omitempty"`
}

// WARPInstance holds the credentials produced by `usque register` for one
// Cloudflare WARP account. Only the four required fields must be set; ipv6 /
// access-token / id / license are kept for future-proofing (token refresh,
// IPv6 mode) but are not required for outbound HTTP/3 SOCKS-equivalent use.
type WARPInstance struct {
	Name           string `yaml:"name" json:"name"`
	PrivateKey     string `yaml:"private-key" json:"private-key"`           // base64-DER ECDSA
	EndpointPubKey string `yaml:"endpoint-pub-key" json:"endpoint-pub-key"` // PEM
	EndpointV4     string `yaml:"endpoint-v4" json:"endpoint-v4"`           // 162.159.x
	IPv4           string `yaml:"ipv4" json:"ipv4"`                         // 172.16.x assigned to us
	IPv6           string `yaml:"ipv6,omitempty" json:"ipv6,omitempty"`
	AccessToken    string `yaml:"access-token,omitempty" json:"access-token,omitempty"`
	ID             string `yaml:"id,omitempty" json:"id,omitempty"`
}

// ECHWorkerConfig defines a single ECH worker instance.
type ECHWorkerConfig struct {
	Name   string `yaml:"name" json:"name"`
	Domain string `yaml:"domain" json:"domain"`
	IP     string `yaml:"ip,omitempty" json:"ip,omitempty"`
	Token  string `yaml:"token" json:"token"`
	Port   int    `yaml:"port" json:"port"`
}

type SDKConfig struct {
	// ProxyURL is the URL of an optional proxy server to use for outbound requests.
	ProxyURL string `yaml:"proxy-url" json:"proxy-url"`

	// ProxyPool embeds ECH worker management with per-worker connection pools.
	// Priority: per-auth proxy-url > proxy-pool > global proxy-url.
	ProxyPool ProxyPoolConfig `yaml:"proxy-pool" json:"proxy-pool"`

	// Headroom enables in-process compression via headroom-core FFI.
	Headroom HeadroomConfig `yaml:"headroom" json:"headroom"`

	// WebSearch enables gateway-level web search interception for providers
	// that don't support web_search tool natively (e.g. Bedrock). When
	// enabled, CPA intercepts the tool call, executes the search via the
	// configured provider, and injects results back into the conversation.
	WebSearch WebSearchConfig `yaml:"web-search" json:"web-search"`

	// Throttle controls concurrent request limiting with backpressure. When the
	// proxy goroutine count exceeds MaxConcurrency, additional requests queue (up
	// to Backlog); if they wait longer than TimeoutSeconds they get 503.
	Throttle ThrottleConfig `yaml:"throttle" json:"throttle"`

	// MaxRequestBodyMB limits incoming request body size. 0 disables the limit.
	MaxRequestBodyMB int `yaml:"max-request-body-mb,omitempty" json:"max-request-body-mb,omitempty"`

	// RecoveryProbe controls periodic probing of disabled/cooldown auth entries
	// to detect early recovery without waiting the full cooldown timer.
	RecoveryProbe RecoveryProbeConfig `yaml:"recovery-probe" json:"recovery-probe"`

	// RTK controls in-process tool_result compression using the vendored RTK port
	// (internal/rtk). When enabled, verbose tool outputs (git diff, grep, ls, tree,
	// build logs, etc.) are compressed before being forwarded upstream, saving
	// 30-90% tokens on common dev operations.
	//
	// Disabled by default because clients running RTK CLI locally (PreToolUse hook)
	// already compress output; enabling this would re-process already-compressed
	// content and waste CPU.
	RTK RTKConfig `yaml:"rtk" json:"rtk"`

	// DisableImageGeneration controls whether the built-in image_generation tool is injected/allowed.
	//
	// Supported values:
	//   - false (default): image_generation is enabled everywhere (normal behavior).
	//   - true: image_generation is disabled everywhere. The server stops injecting it, removes it from request payloads,
	//     and returns 404 for /v1/images/generations and /v1/images/edits.
	//   - "chat": disable image_generation injection for all non-images endpoints (e.g. /v1/responses, /v1/chat/completions),
	//     while keeping /v1/images/generations and /v1/images/edits enabled and preserving image_generation there.
	//   - "passthrough": do not modify the tool list on non-images endpoints — keep image_generation if the client
	//     sent it and do not inject it otherwise; on /v1/images/generations and /v1/images/edits behave like "chat".
	DisableImageGeneration DisableImageGenerationMode `yaml:"disable-image-generation" json:"disable-image-generation"`

	// GPTImage2BaseModel sets the base (mainline) model used when proxying GPT Image 2
	// requests via the hosted image_generation tool (e.g. Codex OAuth /v1/images/*).
	//
	// The value must start with "gpt-" (case-insensitive). If empty or invalid, the
	// default base model ("gpt-5.4-mini") is used.
	GPTImage2BaseModel string `yaml:"gpt-image-2-base-model,omitempty" json:"gpt-image-2-base-model,omitempty"`

	// VideoResultAuthCacheTTL controls how long video IDs stay pinned to the credential
	// that created them. Accepts duration strings like "30m" or "3h".
	// Empty or invalid values use the default 3h.
	VideoResultAuthCacheTTL string `yaml:"video-result-auth-cache-ttl,omitempty" json:"video-result-auth-cache-ttl,omitempty"`

	// EnableGeminiCLIEndpoint controls whether Gemini CLI internal endpoints (/v1internal:*) are enabled.
	// Default is false for safety; when false, /v1internal:* requests are rejected.
	EnableGeminiCLIEndpoint bool `yaml:"enable-gemini-cli-endpoint" json:"enable-gemini-cli-endpoint"`

	// ForceModelPrefix requires explicit model prefixes (e.g., "teamA/gemini-3-pro-preview")
	// to target prefixed credentials. When false, unprefixed model requests may use prefixed
	// credentials as well.
	ForceModelPrefix bool `yaml:"force-model-prefix" json:"force-model-prefix"`

	// RequestLog enables or disables detailed request logging functionality.
	RequestLog bool `yaml:"request-log" json:"request-log"`

	// CodexOptimizeMultiAgentV2 mirrors the provider-wide runtime setting for API handlers.
	CodexOptimizeMultiAgentV2 bool `yaml:"-" json:"-"`

	// APIKeys is a list of keys for authenticating clients to this proxy server.
	APIKeys []string `yaml:"api-keys" json:"api-keys"`

	// PassthroughHeaders controls whether upstream response headers are forwarded to downstream clients.
	// Default is false (disabled).
	PassthroughHeaders bool `yaml:"passthrough-headers" json:"passthrough-headers"`

	// Streaming configures server-side streaming behavior (keep-alives and safe bootstrap retries).
	Streaming StreamingConfig `yaml:"streaming" json:"streaming"`

	// NonStreamKeepAliveInterval controls how often blank lines are emitted for non-streaming responses.
	// <= 0 disables keep-alives. Value is in seconds.
	NonStreamKeepAliveInterval int `yaml:"nonstream-keepalive-interval,omitempty" json:"nonstream-keepalive-interval,omitempty"`
}

// StreamingConfig holds server streaming behavior configuration.
type StreamingConfig struct {
	// KeepAliveSeconds controls how often the server emits SSE heartbeats (": keep-alive\n\n").
	// <= 0 disables keep-alives. Default is 0.
	KeepAliveSeconds int `yaml:"keepalive-seconds,omitempty" json:"keepalive-seconds,omitempty"`

	// BootstrapRetries controls how many times the server may retry a streaming request before any bytes are sent,
	// to allow auth rotation / transient recovery.
	// <= 0 disables bootstrap retries. Default is 0.
	BootstrapRetries int `yaml:"bootstrap-retries,omitempty" json:"bootstrap-retries,omitempty"`

	// TTFBTimeoutSeconds controls the maximum time to wait for the first byte of a stream response
	// before triggering a retry/fallback.
	// <= 0 disables TTFB timeout. Default is 0.
	TTFBTimeoutSeconds int `yaml:"ttfb-timeout-seconds,omitempty" json:"ttfb-timeout-seconds,omitempty"`
}
