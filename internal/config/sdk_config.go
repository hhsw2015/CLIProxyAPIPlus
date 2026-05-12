// Package config provides configuration management for the CLI Proxy API server.
// It handles loading and parsing YAML configuration files, and provides structured
// access to application settings including server port, authentication directory,
// debug settings, proxy configuration, and API keys.
package config

// SDKConfig represents the application's configuration, loaded from a YAML file.
// ProxyPoolConfig controls the embedded ECH worker proxy pool.
// When enabled, CPA manages ECH worker processes directly and routes requests
// through persistent per-worker connections, eliminating the external warp-pool hop.
type ProxyPoolConfig struct {
	Enabled       bool              `yaml:"enabled" json:"enabled"`
	ECHBin        string            `yaml:"ech-bin,omitempty" json:"ech-bin,omitempty"`
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
