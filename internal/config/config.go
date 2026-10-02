// Package config loads and validates orch's config.yaml.
//
// The file names providers, models, roles and limits. It never holds
// secrets: credentials are read from files under SecretsDir at run time.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the parsed contents of config.yaml.
type Config struct {
	// DataDir holds the SQLite database, workspaces and run logs.
	DataDir string `yaml:"data_dir"`
	// SecretsDir holds one 0600 file per credential (tokens, never in YAML).
	SecretsDir string `yaml:"secrets_dir"`

	GitHub    GitHub              `yaml:"github"`
	Telegram  Telegram            `yaml:"telegram"`
	Panel     Panel               `yaml:"panel"`
	Providers map[string]Provider `yaml:"providers"`
	Models    map[string]Model    `yaml:"models"`
	Roles     map[string]Role     `yaml:"roles"`
	Limits    Limits              `yaml:"limits"`
}

// GitHub configures the poller and the repositories orch works on.
type GitHub struct {
	// Repos are "owner/name" pilot repositories.
	Repos []string `yaml:"repos"`
	// Trigger is the label that starts a task.
	Trigger string `yaml:"trigger_label"`
	// TrustedActor is the only login whose labels and issues orch accepts.
	TrustedActor string        `yaml:"trusted_actor"`
	PollInterval time.Duration `yaml:"poll_interval"`
}

// Telegram configures the bot. The bot token lives in SecretsDir.
type Telegram struct {
	Enabled bool  `yaml:"enabled"`
	UserID  int64 `yaml:"user_id"`
}

// Panel configures the LAN-only control panel.
type Panel struct {
	Enabled bool   `yaml:"enabled"`
	Listen  string `yaml:"listen"`
}

// Provider is one account / CLI.
type Provider struct {
	CLI           string `yaml:"cli"`
	Auth          string `yaml:"auth"`
	Endpoint      string `yaml:"endpoint"`
	MaxConcurrent int    `yaml:"max_concurrent"`
	Disabled      bool   `yaml:"disabled"`
}

// Model is a provider plus the model id passed to its CLI.
type Model struct {
	Provider     string `yaml:"provider"`
	Model        string `yaml:"model"`
	MaxRunsPer5h int    `yaml:"max_runs_per_5h"`
	Disabled     bool   `yaml:"disabled"`
}

// Role ranks the models it may use; the router takes the first usable one.
type Role struct {
	Pool []string `yaml:"pool"`
	// NotSameAs names a role whose model this role must never reuse on the
	// same task (the reviewer is never the developer's model).
	NotSameAs string        `yaml:"not_same_as"`
	Timeout   time.Duration `yaml:"timeout"`
	// Enabled defaults to true when omitted.
	Enabled *bool `yaml:"enabled"`
}

// IsEnabled reports whether the role takes part in the workflow.
func (r Role) IsEnabled() bool { return r.Enabled == nil || *r.Enabled }

// Limits bound every loop; hitting one moves the task to needs_human.
type Limits struct {
	CIAttempts   int `yaml:"ci_attempts"`
	ReviewCycles int `yaml:"review_cycles"`
	DevRuns      int `yaml:"dev_runs"`
	MaxDiffLines int `yaml:"max_diff_lines"`
}

var knownAuth = map[string]bool{
	"oauth_token": true,
	"auth_json":   true,
	"keyring":     true,
	"api_key":     true,
	"none":        true,
}

// Load reads, defaults and validates the config file at path.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	return Parse(raw)
}

// Parse decodes YAML, rejecting unknown keys, then applies defaults and validates.
func Parse(raw []byte) (*Config, error) {
	var c Config
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	c.applyDefaults()
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

func (c *Config) applyDefaults() {
	home, _ := os.UserHomeDir()
	if c.DataDir == "" {
		c.DataDir = filepath.Join(home, ".local", "share", "orch")
	}
	if c.SecretsDir == "" {
		c.SecretsDir = filepath.Join(home, ".config", "orch", "secrets")
	}
	c.DataDir = expandHome(c.DataDir, home)
	c.SecretsDir = expandHome(c.SecretsDir, home)
	if c.GitHub.Trigger == "" {
		c.GitHub.Trigger = "agent:ready"
	}
	if c.GitHub.PollInterval == 0 {
		c.GitHub.PollInterval = 60 * time.Second
	}
	if c.Limits == (Limits{}) {
		c.Limits = Limits{CIAttempts: 3, ReviewCycles: 3, DevRuns: 8, MaxDiffLines: 800}
	}
	for name, p := range c.Providers {
		if p.MaxConcurrent == 0 {
			p.MaxConcurrent = 1
			c.Providers[name] = p
		}
	}
}

// Validate checks cross references and bounds, reporting every problem at once.
func (c *Config) Validate() error {
	var errs []error
	add := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }

	for _, r := range c.GitHub.Repos {
		if parts := strings.Split(r, "/"); len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			add("github.repos: %q is not owner/name", r)
		}
	}
	if c.GitHub.TrustedActor == "" {
		add("github.trusted_actor is required")
	}
	if c.GitHub.PollInterval < 10*time.Second {
		add("github.poll_interval %s is below 10s", c.GitHub.PollInterval)
	}
	if c.Telegram.Enabled && c.Telegram.UserID == 0 {
		add("telegram.user_id is required when telegram is enabled")
	}
	if c.Panel.Enabled && c.Panel.Listen == "" {
		add("panel.listen is required when the panel is enabled")
	}

	if len(c.Providers) == 0 {
		add("providers: at least one is required")
	}
	for _, name := range sortedKeys(c.Providers) {
		p := c.Providers[name]
		if p.CLI == "" && p.Endpoint == "" {
			add("providers.%s: needs cli or endpoint", name)
		}
		if p.Auth != "" && !knownAuth[p.Auth] {
			add("providers.%s: unknown auth %q", name, p.Auth)
		}
		if p.MaxConcurrent < 0 {
			add("providers.%s: max_concurrent must not be negative", name)
		}
	}

	for _, name := range sortedKeys(c.Models) {
		m := c.Models[name]
		if _, ok := c.Providers[m.Provider]; !ok {
			add("models.%s: unknown provider %q", name, m.Provider)
		}
		if m.Model == "" {
			add("models.%s: model id is required", name)
		}
		if m.MaxRunsPer5h < 0 {
			add("models.%s: max_runs_per_5h must not be negative", name)
		}
	}

	if len(c.Roles) == 0 {
		add("roles: at least one is required")
	}
	for _, name := range sortedKeys(c.Roles) {
		r := c.Roles[name]
		if len(r.Pool) == 0 {
			add("roles.%s: pool is empty", name)
		}
		seen := map[string]bool{}
		for _, m := range r.Pool {
			if _, ok := c.Models[m]; !ok {
				add("roles.%s: unknown model %q in pool", name, m)
			}
			if seen[m] {
				add("roles.%s: model %q listed twice", name, m)
			}
			seen[m] = true
		}
		if r.NotSameAs != "" {
			if r.NotSameAs == name {
				add("roles.%s: not_same_as cannot name itself", name)
			} else if _, ok := c.Roles[r.NotSameAs]; !ok {
				add("roles.%s: not_same_as names unknown role %q", name, r.NotSameAs)
			}
		}
		if r.Timeout < 0 {
			add("roles.%s: timeout must not be negative", name)
		}
	}

	l := c.Limits
	if l.CIAttempts < 1 || l.ReviewCycles < 1 || l.DevRuns < 1 || l.MaxDiffLines < 1 {
		add("limits: every limit must be at least 1")
	}
	if l.DevRuns < l.CIAttempts || l.DevRuns < l.ReviewCycles {
		add("limits.dev_runs (%d) must cover ci_attempts and review_cycles", l.DevRuns)
	}

	return errors.Join(errs...)
}

// expandHome replaces a leading "~/" with the user's home directory.
func expandHome(p, home string) string {
	if p == "~" {
		return home
	}
	if strings.HasPrefix(p, "~/") {
		return filepath.Join(home, p[2:])
	}
	return p
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
