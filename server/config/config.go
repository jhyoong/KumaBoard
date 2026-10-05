// Package config loads the server configuration file.
package config

import (
	"errors"
	"fmt"
	"net"
	"os"

	"gopkg.in/yaml.v3"
)

// WOL is the magic packet sender configuration.
type WOL struct {
	Interface string `yaml:"interface"`
	Broadcast string `yaml:"broadcast"`
}

// Config is /etc/kumaboard/config.yaml.
type Config struct {
	ListenAddrs        []string `yaml:"listen_addrs"`
	Hostnames          []string `yaml:"hostnames"`
	DataDir            string   `yaml:"data_dir"`
	WOL                WOL      `yaml:"wol"`
	MetricsIntervalS   int      `yaml:"metrics_interval_s"`
	HeartbeatIntervalS int      `yaml:"heartbeat_interval_s"`
}

// Load reads, defaults, and validates the config.
func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	cfg := &Config{
		DataDir:            "/var/lib/kumaboard",
		MetricsIntervalS:   30,
		HeartbeatIntervalS: 15,
	}
	if err := yaml.Unmarshal(b, cfg); err != nil {
		return nil, fmt.Errorf("config: parse %s: %w", path, err)
	}
	if len(cfg.ListenAddrs) == 0 {
		return nil, errors.New("config: listen_addrs must list at least one host:port")
	}
	for _, a := range cfg.ListenAddrs {
		host, _, err := net.SplitHostPort(a)
		if err != nil {
			return nil, fmt.Errorf("config: listen_addrs %q: %w", a, err)
		}
		ip := net.ParseIP(host)
		if host == "" || (ip != nil && ip.IsUnspecified()) {
			return nil, fmt.Errorf("config: listen_addrs %q binds all interfaces; use a specific IP", a)
		}
	}
	if cfg.MetricsIntervalS < 5 || cfg.HeartbeatIntervalS < 5 {
		return nil, errors.New("config: intervals must be at least 5 seconds")
	}
	return cfg, nil
}

// SANs are the certificate subject alternative names: listen hosts plus hostnames.
func (c *Config) SANs() []string {
	seen := map[string]bool{}
	var out []string
	for _, a := range c.ListenAddrs {
		host, _, _ := net.SplitHostPort(a)
		if !seen[host] {
			seen[host] = true
			out = append(out, host)
		}
	}
	for _, h := range c.Hostnames {
		if !seen[h] {
			seen[h] = true
			out = append(out, h)
		}
	}
	return out
}

// AllowedHosts is the set of acceptable Host header values.
func (c *Config) AllowedHosts() map[string]bool {
	out := map[string]bool{}
	ports := map[string]bool{}
	for _, a := range c.ListenAddrs {
		out[a] = true
		_, port, _ := net.SplitHostPort(a)
		ports[port] = true
	}
	for _, h := range c.Hostnames {
		out[h] = true
		for p := range ports {
			out[net.JoinHostPort(h, p)] = true
		}
	}
	return out
}
