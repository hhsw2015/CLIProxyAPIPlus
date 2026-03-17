package config

import (
	"os"

	"gopkg.in/yaml.v3"
)

type ProxyConfig struct {
	SocksPort     int             `yaml:"socks_port"`
	HTTPPort      int             `yaml:"http_port"`
	IncludeDirect bool            `yaml:"include_direct"`
	ExtraBackends []ExtraBackend  `yaml:"extra_backends"`
}

// ExtraBackend is an external SOCKS5/HTTP proxy to include in the rotation pool.
type ExtraBackend struct {
	Name string `yaml:"name"`
	Addr string `yaml:"addr"` // e.g. "127.0.0.1:30004"
}

// ECHWorkerConfig defines an ech-workers instance to be managed by warp-pool.
type ECHWorkerConfig struct {
	Name   string `yaml:"name"`             // display name
	Domain string `yaml:"domain"`           // e.g. "ech.playingapi.tech:443"
	IP     string `yaml:"ip,omitempty"`     // optional: pin server IP
	Token  string `yaml:"token"`            // auth token
	Port   int    `yaml:"port"`             // local listen port (SOCKS5)
}

// ECHWorkersConfig holds all ech-workers instances.
type ECHWorkersConfig struct {
	Enabled  bool              `yaml:"enabled"`
	BinPath  string            `yaml:"bin_path"`  // path to ech-workers binary
	Workers  []ECHWorkerConfig `yaml:"workers"`
}

type APIConfig struct {
	Port  int    `yaml:"port"`
	Token string `yaml:"token"`
}

type RotationConfig struct {
	Enabled           bool `yaml:"enabled"`
	RequestsThreshold int  `yaml:"requests_threshold"`
	MinInterval       int  `yaml:"min_interval"`
	OnlyUsed          bool `yaml:"only_used"`
}

type DirectConfig struct {
	Enabled        bool `yaml:"enabled"`
	ExposeExternal bool `yaml:"expose_external"`
}

type UniqueIPv4Config struct {
	Enabled    bool `yaml:"enabled"`
	MaxRetries int  `yaml:"max_retries"`
	RetryDelay int  `yaml:"retry_delay"`
}

type ResourceLimitsConfig struct {
	// MemoryMB sets GOMEMLIMIT per warp instance (MiB). 0 = no limit.
	MemoryMB int `yaml:"memory_mb"`
	// MaxProcs sets GOMAXPROCS per warp instance. 0 = use system default.
	MaxProcs int `yaml:"max_procs"`
	// LogLevel overrides warp --loglevel (debug/info/warn/error/silent). Empty = default.
	LogLevel string `yaml:"log_level"`
}

type Config struct {
	PoolSize            int              `yaml:"pool_size"`
	WarpBin             string           `yaml:"warp_bin"`
	DataDir             string           `yaml:"data_dir"`
	SocksBasePort       int              `yaml:"socks_base_port"`
	HTTPBasePort        int              `yaml:"http_base_port"`
	Proxy               ProxyConfig      `yaml:"proxy"`
	API                 APIConfig        `yaml:"api"`
	HealthCheckInterval int              `yaml:"health_check_interval"`
	LicenseKeyURL       string           `yaml:"license_key_url"`
	Rotation            RotationConfig       `yaml:"rotation"`
	Direct              DirectConfig         `yaml:"direct"`
	UniqueIPv4          UniqueIPv4Config     `yaml:"unique_ipv4"`
	ResourceLimits      ResourceLimitsConfig `yaml:"resource_limits"`
	ECHWorkers          ECHWorkersConfig     `yaml:"ech_workers"`
}

func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	cfg := &Config{
		PoolSize:            3,
		WarpBin:             "./warp",
		DataDir:             "./data",
		SocksBasePort:       10001,
		HTTPBasePort:        11001,
		Proxy:               ProxyConfig{SocksPort: 1080, HTTPPort: 8118},
		API:                 APIConfig{Port: 9090},
		HealthCheckInterval: 30,
		Rotation: RotationConfig{
			Enabled:           false,
			RequestsThreshold: 0,
			MinInterval:       60,
			OnlyUsed:          true,
		},
		Direct: DirectConfig{
			Enabled:        true,
			ExposeExternal: true,
		},
		UniqueIPv4: UniqueIPv4Config{
			Enabled:    true,
			MaxRetries: 10,
			RetryDelay: 3,
		},
	}

	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, err
	}

	return cfg, nil
}
