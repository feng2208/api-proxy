package main

import (
	_ "embed"
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

//go:embed config.yaml
var ConfigTemplate string

// Config represents the top-level configuration.
type Config struct {
	Listen      string     `yaml:"listen"`
	MaxBodySize int64      `yaml:"max_body_size"`
	APIKeys     []string   `yaml:"api_keys"`
	Proxy       ProxyConfig `yaml:"proxy"`
	Providers   []Provider `yaml:"providers"`
}

// ProxyConfig holds proxy connection settings.
type ProxyConfig struct {
	URL string `yaml:"url"`
}

// Provider defines an upstream API provider.
type Provider struct {
	Name         string      `yaml:"name"`
	PathPrefix   string      `yaml:"path_prefix"`
	Upstream     string      `yaml:"upstream"`
	AuthHeader   string      `yaml:"auth_header"`
	AuthKeys     []string    `yaml:"auth_keys"`
	Timeout      string      `yaml:"timeout"`
	MaxRetries   int         `yaml:"max_retries"`
	Proxy        ProxyConfig `yaml:"proxy"`

	// Parsed fields (not from YAML)
	TimeoutDuration time.Duration `yaml:"-"`
}

// LoadConfig reads and parses the YAML configuration file.
func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config file: %w", err)
	}

	cfg := &Config{}
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parsing config file: %w", err)
	}

	applyDefaults(cfg)

	if err := validateConfig(cfg); err != nil {
		return nil, fmt.Errorf("validating config: %w", err)
	}

	return cfg, nil
}

// applyDefaults fills in default values for unset fields.
func applyDefaults(cfg *Config) {
	if cfg.Listen == "" {
		cfg.Listen = "0.0.0.0:3000"
	}

	for i := range cfg.Providers {
		p := &cfg.Providers[i]

		if p.Timeout == "" {
			p.Timeout = "30s"
		}

		// max_retries defaults to 2
		if p.MaxRetries == 0 {
			p.MaxRetries = 2
		}

		// Parse timeout duration
		d, err := time.ParseDuration(p.Timeout)
		if err != nil {
			d = 30 * time.Second
		}
		p.TimeoutDuration = d

		// Inherit global proxy if provider-level proxy is not set
		if p.Proxy.URL == "" && cfg.Proxy.URL != "" {
			p.Proxy.URL = cfg.Proxy.URL
		}
	}
}

// validateConfig checks that all required fields are present.
func validateConfig(cfg *Config) error {
	if len(cfg.APIKeys) == 0 {
		return fmt.Errorf("api_keys must not be empty")
	}

	if len(cfg.Providers) == 0 {
		return fmt.Errorf("providers must not be empty")
	}

	for i, p := range cfg.Providers {
		if p.Name == "" {
			return fmt.Errorf("provider[%d]: name must not be empty", i)
		}
		if p.PathPrefix == "" {
			return fmt.Errorf("provider[%d] %q: path_prefix must not be empty", i, p.Name)
		}
		if p.Upstream == "" {
			return fmt.Errorf("provider[%d] %q: upstream must not be empty", i, p.Name)
		}
		if p.AuthHeader == "" {
			return fmt.Errorf("provider[%d] %q: auth_header must not be empty", i, p.Name)
		}
		if len(p.AuthKeys) == 0 {
			return fmt.Errorf("provider[%d] %q: auth_keys must not be empty", i, p.Name)
		}
	}

	return nil
}
