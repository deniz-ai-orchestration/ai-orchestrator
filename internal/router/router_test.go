package router

import (
	"errors"
	"strings"
	"testing"

	"github.com/deniz-ai-orchestration/ai-orchestrator/internal/config"
)

func testConfig(t *testing.T) *config.Config {
	t.Helper()
	cfg, err := config.Parse([]byte(`
github: { trusted_actor: d }
runner: { git_name: a, git_email: a@b }
providers:
  claude: { cli: claude }
  codex:  { cli: codex, disabled: true }
  gemini: { cli: agy }
  ollama: { endpoint: http://x }
models:
  sonnet: { provider: claude, model: sonnet }
  opus:   { provider: claude, model: opus, disabled: true }
  haiku:  { provider: claude, model: haiku }
  sol:    { provider: codex, model: sol }
  gem:    { provider: gemini, model: g }
  local:  { provider: ollama, model: q }
roles:
  developer: { pool: [opus, sol, sonnet, haiku, gem] }
  reviewer:  { pool: [sol, sonnet, gem], not_same_as: developer }
  helper:    { pool: [local] }
  tester:    { pool: [gem], enabled: false }
`))
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestPick(t *testing.T) {
	cfg := testConfig(t)
	if c, err := Pick(cfg, "developer", Options{}); err != nil || c.Name != "sonnet" || c.Provider.CLI != "claude" || c.Pinned {
		t.Fatalf("developer: %+v %v", c, err)
	}
	if c, err := Pick(cfg, "reviewer", Options{Avoid: "sonnet"}); err != nil || c.Name != "gem" {
		t.Fatalf("reviewer avoiding sonnet: %+v %v", c, err)
	}
	noAgy := func(p config.Provider) bool { return p.CLI != "agy" }
	_, err := Pick(cfg, "reviewer", Options{Avoid: "sonnet", Usable: noAgy})
	if !errors.Is(err, ErrNoModel) {
		t.Fatalf("expected ErrNoModel, got %v", err)
	}
	for _, want := range []string{"sol: disabled in the config", "sonnet: same model as the developer", "gem: cannot run in a container yet"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q lacks %q", err, want)
		}
	}
	if _, err := Pick(cfg, "helper", Options{}); !errors.Is(err, ErrNoModel) {
		t.Fatalf("endpoint-only provider must not be picked for a CLI run: %v", err)
	}
	if _, err := Pick(cfg, "tester", Options{}); err == nil {
		t.Fatal("disabled role picked a model")
	}
	if _, err := Pick(cfg, "nope", Options{}); err == nil {
		t.Fatal("unknown role picked a model")
	}
}

func TestPinsAndSkips(t *testing.T) {
	cfg := testConfig(t)
	cooling := func(name string) string {
		if name == "sonnet" {
			return "cooling down"
		}
		return ""
	}
	if c, err := Pick(cfg, "developer", Options{Skip: cooling}); err != nil || c.Name != "haiku" {
		t.Fatalf("fallback past a cooling model: %+v %v", c, err)
	}
	if c, err := Pick(cfg, "developer", Options{Pin: "gem"}); err != nil || c.Name != "gem" || !c.Pinned {
		t.Fatalf("pin: %+v %v", c, err)
	}
	// A pin outside the pool still works; a pin that cannot run falls back.
	if c, err := Pick(cfg, "reviewer", Options{Pin: "haiku"}); err != nil || c.Name != "haiku" {
		t.Fatalf("pin outside pool: %+v %v", c, err)
	}
	if c, err := Pick(cfg, "developer", Options{Pin: "sonnet", Skip: cooling}); err != nil || c.Name != "haiku" || c.Pinned {
		t.Fatalf("cooling pin: %+v %v", c, err)
	}
	if c, err := Pick(cfg, "reviewer", Options{Pin: "sonnet", Avoid: "sonnet"}); err != nil || c.Name != "gem" {
		t.Fatalf("a pin never overrides not_same_as: %+v %v", c, err)
	}
	if _, err := Pick(cfg, "developer", Options{Pin: "ghost", Skip: func(string) string { return "x" }}); !strings.Contains(err.Error(), "ghost: not in the config") {
		t.Fatalf("err %v", err)
	}
}
