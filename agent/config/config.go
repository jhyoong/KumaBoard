// Package config loads the agent's YAML config, CA file, and token file.
package config

import (
	"crypto/x509"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/jhyoong/KumaBoard/proto"
)

// Command is one named command. Run is an argv array, never a shell string.
// Script is the absolute path of a script file; argv is Run + [Script], or
// the script alone when Run is empty. At least one of the two is required.
// Commands take no parameters: nothing outside this file adds to argv.
type Command struct {
	Description      string   `yaml:"description"`
	Run              []string `yaml:"run"`
	Script           string   `yaml:"script"`
	TimeoutS         int      `yaml:"timeout_s"`
	ExpectDisconnect bool     `yaml:"expect_disconnect"`
	// Confirm makes the dashboard ask before starting. It guards against
	// misclicks only; it is enforced in the browser, not here.
	Confirm bool `yaml:"confirm"`
}

// Argv is what the command executes: Run with Script appended when set.
func (c Command) Argv() []string {
	argv := append([]string(nil), c.Run...)
	if c.Script != "" {
		argv = append(argv, c.Script)
	}
	return argv
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
	if err := validateCommands(cfg.Commands); err != nil {
		return nil, err
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("config: resolve path: %w", err)
	}
	cfg.Path = abs
	return &cfg, nil
}

func validateCommands(cmds map[string]Command) error {
	for name, c := range cmds {
		if !nameRe.MatchString(name) {
			return fmt.Errorf("config: command %q: bad name", name)
		}
		if len(c.Run) == 0 && c.Script == "" {
			return fmt.Errorf("config: command %q: needs run (an argv array), script, or both", name)
		}
		if len(c.Run) > 0 && c.Run[0] == "" {
			return fmt.Errorf("config: command %q: run must be a non-empty argv array", name)
		}
		if c.Script != "" && !filepath.IsAbs(c.Script) {
			return fmt.Errorf("config: command %q: script must be an absolute path", name)
		}
		if c.TimeoutS <= 0 {
			return fmt.Errorf("config: command %q: timeout_s must be positive", name)
		}
	}
	return nil
}

// ParseCommands parses and validates only the commands: block of a config
// file's content. Nothing else in the file is validated or loaded, so it is
// safe to call on every edit while the agent runs.
func ParseCommands(b []byte) (map[string]Command, error) {
	var f struct {
		Commands map[string]Command `yaml:"commands"`
	}
	if err := yaml.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("config: parse: %w", err)
	}
	if err := validateCommands(f.Commands); err != nil {
		return nil, err
	}
	if f.Commands == nil {
		f.Commands = map[string]Command{}
	}
	return f.Commands, nil
}

// LoadCommands is ParseCommands on the file at path.
func LoadCommands(path string) (map[string]Command, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ParseCommands(b)
}

// RestartOnlyChanges names the settings in b, a newer version of this
// config's file, that differ from the running config and only take effect
// after a restart. Content that does not parse reports nothing.
func (c *Config) RestartOnlyChanges(b []byte) []string {
	var f Config
	if err := yaml.Unmarshal(b, &f); err != nil {
		return nil
	}
	var out []string
	if f.Device.Name != c.Device.Name {
		out = append(out, "device")
	}
	if f.Server != c.Server {
		out = append(out, "server")
	}
	if f.TokenFile != c.TokenFile {
		out = append(out, "token_file")
	}
	if !slices.Equal(f.Capabilities, c.Capabilities) {
		out = append(out, "capabilities")
	}
	return out
}

// CommandDefs converts commands to what the agent declares, sorted by name.
func CommandDefs(cmds map[string]Command) []proto.CommandDef {
	out := make([]proto.CommandDef, 0, len(cmds))
	for name, cmd := range cmds {
		out = append(out, proto.CommandDef{
			Name: name, Description: cmd.Description, TimeoutS: cmd.TimeoutS,
			ExpectDisconnect: cmd.ExpectDisconnect, Confirm: cmd.Confirm,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// CommandDefs is every command in the config as a declaration, sorted by name.
func (c *Config) CommandDefs() []proto.CommandDef { return CommandDefs(c.Commands) }
