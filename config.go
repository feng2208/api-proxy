package main

import (
	_ "embed"
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

//go:embed config-template.yaml
var ConfigTemplate string

// Config represents the top-level configuration.
type Config struct {
	Listen          string        `yaml:"listen"`
	MaxBodySize     int64         `yaml:"max_body_size"`
	BasePath        string        `yaml:"base_path"`
	ClientRateLimit int           `yaml:"client_rate_limit"`
	APIKeys         []string      `yaml:"api_keys"`
	Proxy           ProxyConfig   `yaml:"proxy"`
	Models          []ModelConfig `yaml:"models"`
	Auth            AuthConfig    `yaml:"auth"`
}

// ProxyConfig holds proxy connection settings.
type ProxyConfig struct {
	URL string `yaml:"url"`
}

// ModelConfig represents a model exposed to the client.
type ModelConfig struct {
	Name      string           `yaml:"name"`
	Providers []ProviderConfig `yaml:"providers"`
}

// ProviderConfig represents a backend provider for a model.
type ProviderConfig struct {
	Name           string      `yaml:"name"`
	Upstream       string      `yaml:"upstream"`
	Model          string      `yaml:"model"`
	Timeout        string      `yaml:"timeout"`
	ModelRateLimit int         `yaml:"model_rate_limit"`
	Proxy          ProxyConfig `yaml:"proxy"`

	// Parsed fields
	TimeoutDuration time.Duration `yaml:"-"`
}

// AuthConfig holds authentication settings for backend providers.
type AuthConfig struct {
	Providers []AuthProviderConfig `yaml:"providers"`
}

// AuthProviderConfig holds keys for a specific provider.
type AuthProviderConfig struct {
	Name      string   `yaml:"name"`
	RateLimit int      `yaml:"rate_limit"`
	Keys      []string `yaml:"keys"`
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
	if cfg.BasePath == "" {
		cfg.BasePath = "/v1"
	}
	if cfg.ClientRateLimit <= 0 {
		cfg.ClientRateLimit = 10
	}

	for i := range cfg.Models {
		for j := range cfg.Models[i].Providers {
			p := &cfg.Models[i].Providers[j]

			if p.Timeout == "" {
				p.Timeout = "30s"
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

	for i := range cfg.Auth.Providers {
		p := &cfg.Auth.Providers[i]
		if p.RateLimit <= 0 {
			p.RateLimit = 10
		}
	}
}

// validateConfig checks that all required fields are present and valid.
func validateConfig(cfg *Config) error {
	if len(cfg.APIKeys) == 0 {
		return fmt.Errorf("api_keys must not be empty")
	}

	if len(cfg.Models) == 0 {
		return fmt.Errorf("models must not be empty")
	}

	if len(cfg.Auth.Providers) == 0 {
		return fmt.Errorf("auth.providers must not be empty")
	}

	authProvidersMap := make(map[string]bool)
	for i, ap := range cfg.Auth.Providers {
		if ap.Name == "" {
			return fmt.Errorf("auth.providers[%d]: name must not be empty", i)
		}
		if len(ap.Keys) == 0 {
			return fmt.Errorf("auth.providers[%d] %q: keys must not be empty", i, ap.Name)
		}
		authProvidersMap[ap.Name] = true
	}

	for i, m := range cfg.Models {
		if m.Name == "" {
			return fmt.Errorf("models[%d]: name must not be empty", i)
		}
		if len(m.Providers) == 0 {
			return fmt.Errorf("models[%d] %q: providers must not be empty", i, m.Name)
		}

		for j, p := range m.Providers {
			if p.Name == "" {
				return fmt.Errorf("models[%d].providers[%d]: name must not be empty", i, j)
			}
			if p.Upstream == "" {
				return fmt.Errorf("models[%d].providers[%d] %q: upstream must not be empty", i, j, p.Name)
			}
			if p.Model == "" {
				return fmt.Errorf("models[%d].providers[%d] %q: model must not be empty", i, j, p.Name)
			}

			if !authProvidersMap[p.Name] {
				return fmt.Errorf("models[%d].providers[%d] %q: provider not found in auth.providers", i, j, p.Name)
			}
		}
	}

	return nil
}
