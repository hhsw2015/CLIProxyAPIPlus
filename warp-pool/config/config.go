package config

import (
	"os"

	"gopkg.in/yaml.v3"
)

type ProxyConfig struct {
	SocksPort int `yaml:"socks_port"`
	HTTPPort  int `yaml:"http_port"`
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
	Rotation            RotationConfig   `yaml:"rotation"`
	Direct              DirectConfig     `yaml:"direct"`
	UniqueIPv4          UniqueIPv4Config `yaml:"unique_ipv4"`
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
