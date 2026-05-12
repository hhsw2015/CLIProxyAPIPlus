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
//   1. Enabled must be true
//   2. body length must be >= MinBytes (when MinBytes > 0)
//   3. model must NOT match any Deny glob
//   4. when Allow is non-empty, model must match at least one Allow glob
//
// Globs use Go path.Match semantics: '*' / '?' / '[a-z]'. Note that '*' does
// NOT cross '/' (path.Match treats '/' as a separator), so for vendor/model
// style names match each segment explicitly (e.g. "vendor/*"). The empty
// model string (no "model" field in body) is treated as "unknown" for
// matching, so deny: ["unknown"] suppresses unrecognised payloads.
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
type ProxyPoolConfig struct {
	Enabled       bool              `yaml:"enabled" json:"enabled"`
	Workers       []ECHWorkerConfig `yaml:"workers" json:"workers"`
	IncludeDirect bool              `yaml:"include-direct,omitempty" json:"include-direct,omitempty"`
	WeightECH     int               `yaml:"weight-ech,omitempty" json:"weight-ech,omitempty"`
	WeightDirect  int               `yaml:"weight-direct,omitempty" json:"weight-direct,omitempty"`
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

	// DisableImageGeneration controls whether the built-in image_generation tool is injected/allowed.
	//
	// Supported values:
	//   - false (default): image_generation is enabled everywhere (normal behavior).
	//   - true: image_generation is disabled everywhere. The server stops injecting it, removes it from request payloads,
	//     and returns 404 for /v1/images/generations and /v1/images/edits.
	//   - "chat": disable image_generation injection for all non-images endpoints (e.g. /v1/responses, /v1/chat/completions),
	//     while keeping /v1/images/generations and /v1/images/edits enabled and preserving image_generation there.
	DisableImageGeneration DisableImageGenerationMode `yaml:"disable-image-generation" json:"disable-image-generation"`

	// EnableGeminiCLIEndpoint controls whether Gemini CLI internal endpoints (/v1internal:*) are enabled.
	// Default is false for safety; when false, /v1internal:* requests are rejected.
	EnableGeminiCLIEndpoint bool `yaml:"enable-gemini-cli-endpoint" json:"enable-gemini-cli-endpoint"`

	// ForceModelPrefix requires explicit model prefixes (e.g., "teamA/gemini-3-pro-preview")
	// to target prefixed credentials. When false, unprefixed model requests may use prefixed
	// credentials as well.
	ForceModelPrefix bool `yaml:"force-model-prefix" json:"force-model-prefix"`

	// RequestLog enables or disables detailed request logging functionality.
	RequestLog bool `yaml:"request-log" json:"request-log"`

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
