// Package config loads the agent's YAML config, CA file, and token file.
package config

import (
	"crypto/x509"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/jhyoong/KumaBoard/proto"
)

// Command is one named command. Run is an argv array, never a shell string.
type Command struct {
	Description      string   `yaml:"description"`
	Run              []string `yaml:"run"`
	TimeoutS         int      `yaml:"timeout_s"`
	ExpectDisconnect bool     `yaml:"expect_disconnect"`
}

// Config is /etc/kuma-agent/config.yaml plus the loaded secrets.
type Config struct {
	Device struct {
		Name string `yaml:"name"`
	} `yaml:"device"`
	Server struct {
		URL    string `yaml:"url"`
		CAFile string `yaml:"ca_file"`
	} `yaml:"server"`
	TokenFile    string             `yaml:"token_file"`
	Capabilities []string           `yaml:"capabilities"`
	Commands     map[string]Command `yaml:"commands"`

	// Loaded, not parsed from YAML. Path is the absolute config file path.
	Token  string         `yaml:"-"`
	CAPool *x509.CertPool `yaml:"-"`
	Path   string         `yaml:"-"`
}

var nameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// Load reads and validates the config and its referenced files.
func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg Config
	if err := yaml.Unmarshal(b, &cfg); err != nil {
		return nil, fmt.Errorf("config: parse: %w", err)
	}
	if !nameRe.MatchString(cfg.Device.Name) {
		return nil, errors.New("config: device.name must be lowercase letters, digits, and hyphens")
	}
	if !strings.HasPrefix(cfg.Server.URL, "wss://") {
		return nil, errors.New("config: server.url must start with wss://")
	}
	caPEM, err := os.ReadFile(cfg.Server.CAFile)
	if err != nil {
		return nil, fmt.Errorf("config: read ca_file: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, errors.New("config: ca_file contains no certificate")
	}
	cfg.CAPool = pool
	tok, err := os.ReadFile(cfg.TokenFile)
	if err != nil {
		return nil, fmt.Errorf("config: read token_file: %w", err)
	}
	cfg.Token = strings.TrimSpace(string(tok))
	if cfg.Token == "" {
		return nil, errors.New("config: token file is empty")
	}
	if cfg.Capabilities == nil {
		cfg.Capabilities = []string{}
	}
	for name, c := range cfg.Commands {
		if !nameRe.MatchString(name) {
			return nil, fmt.Errorf("config: command %q: bad name", name)
		}
		if len(c.Run) == 0 || c.Run[0] == "" {
			return nil, fmt.Errorf("config: command %q: run must be a non-empty argv array", name)
		}
		if c.TimeoutS <= 0 {
			return nil, fmt.Errorf("config: command %q: timeout_s must be positive", name)
		}
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("config: resolve path: %w", err)
	}
	cfg.Path = abs
	return &cfg, nil
}

// CommandDefs is what the agent declares in hello, sorted by name.
func (c *Config) CommandDefs() []proto.CommandDef {
	out := make([]proto.CommandDef, 0, len(c.Commands))
	for name, cmd := range c.Commands {
		out = append(out, proto.CommandDef{
			Name: name, Description: cmd.Description, TimeoutS: cmd.TimeoutS, ExpectDisconnect: cmd.ExpectDisconnect,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
