package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoadExample(t *testing.T) {
	c, err := Load(filepath.Join("..", "..", "config.example.yaml"))
	if err != nil {
		t.Fatalf("example config should load: %v", err)
	}
	if got := c.Roles["reviewer"].NotSameAs; got != "developer" {
		t.Errorf("reviewer.not_same_as = %q, want developer", got)
	}
	if got := c.Roles["developer"].Timeout; got != 45*time.Minute {
		t.Errorf("developer.timeout = %s, want 45m", got)
	}
	if c.Roles["functional_tester"].IsEnabled() {
		t.Error("functional_tester should be disabled")
	}
	if !c.Roles["developer"].IsEnabled() {
		t.Error("developer should default to enabled")
	}
	if got := c.Providers["codex"].MaxConcurrent; got != 1 {
		t.Errorf("codex.max_concurrent = %d, want 1", got)
	}
	if strings.HasPrefix(c.DataDir, "~") || strings.HasPrefix(c.SecretsDir, "~") {
		t.Errorf("home not expanded: %q, %q", c.DataDir, c.SecretsDir)
	}
}

const minimal = `
github: { trusted_actor: denizekinci }
providers: { claude: { cli: claude, auth: oauth_token } }
models: { sonnet: { provider: claude, model: sonnet } }
roles: { developer: { pool: [sonnet] } }
`

func TestDefaults(t *testing.T) {
	c, err := Parse([]byte(minimal))
	if err != nil {
		t.Fatal(err)
	}
	want := Limits{CIAttempts: 3, ReviewCycles: 3, DevRuns: 8, MaxDiffLines: 800}
	if c.Limits != want {
		t.Errorf("limits = %+v, want %+v", c.Limits, want)
	}
	if c.GitHub.Trigger != "agent:ready" || c.GitHub.PollInterval != time.Minute {
		t.Errorf("github defaults = %+v", c.GitHub)
	}
	home, _ := os.UserHomeDir()
	if c.SecretsDir != filepath.Join(home, ".config", "orch", "secrets") {
		t.Errorf("secrets_dir = %q", c.SecretsDir)
	}
}

func TestInvalid(t *testing.T) {
	cases := []struct {
		name, yaml, wantErr string
	}{
		{"unknown key", minimal + "bogus: 1\n", "field bogus not found"},
		{"no trusted actor", strings.Replace(minimal, "trusted_actor: denizekinci", "trigger_label: x", 1), "trusted_actor is required"},
		{"unknown provider", `
github: { trusted_actor: d }
providers: { claude: { cli: claude } }
models: { m: { provider: nope, model: x } }
roles: { developer: { pool: [m] } }
`, `unknown provider "nope"`},
		{"unknown model in pool", `
github: { trusted_actor: d }
providers: { claude: { cli: claude } }
models: { m: { provider: claude, model: x } }
roles: { developer: { pool: [m, ghost] } }
`, `unknown model "ghost"`},
		{"not_same_as itself", `
github: { trusted_actor: d }
providers: { claude: { cli: claude } }
models: { m: { provider: claude, model: x } }
roles: { reviewer: { pool: [m], not_same_as: reviewer } }
`, "cannot name itself"},
		{"not_same_as unknown", `
github: { trusted_actor: d }
providers: { claude: { cli: claude } }
models: { m: { provider: claude, model: x } }
roles: { reviewer: { pool: [m], not_same_as: developer } }
`, `unknown role "developer"`},
		{"unknown auth", `
github: { trusted_actor: d }
providers: { claude: { cli: claude, auth: password } }
models: { m: { provider: claude, model: x } }
roles: { developer: { pool: [m] } }
`, `unknown auth "password"`},
		{"dev_runs too low", minimal + "limits: { ci_attempts: 3, review_cycles: 3, dev_runs: 2, max_diff_lines: 800 }\n", "must cover"},
		{"zero limit", minimal + "limits: { ci_attempts: 0, review_cycles: 3, dev_runs: 8, max_diff_lines: 800 }\n", "at least 1"},
		{"telegram without user", minimal + "telegram: { enabled: true }\n", "telegram.user_id"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(tc.yaml))
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %v, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}

func TestBadRepo(t *testing.T) {
	c := `
github: { trusted_actor: d, repos: [justname] }
providers: { claude: { cli: claude } }
models: { m: { provider: claude, model: x } }
roles: { developer: { pool: [m] } }
`
	if _, err := Parse([]byte(c)); err == nil || !strings.Contains(err.Error(), "not owner/name") {
		t.Fatalf("error = %v", err)
	}
}
