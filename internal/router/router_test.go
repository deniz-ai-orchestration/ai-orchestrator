package router

import (
	"errors"
	"testing"

	"github.com/deniz-ai-orchestration/ai-orchestrator/internal/config"
)

func TestPick(t *testing.T) {
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
  sol:    { provider: codex, model: sol }
  gem:    { provider: gemini, model: g }
  local:  { provider: ollama, model: q }
roles:
  developer: { pool: [opus, sol, sonnet, gem] }
  reviewer:  { pool: [sol, sonnet, gem], not_same_as: developer }
  helper:    { pool: [local] }
  tester:    { pool: [gem], enabled: false }
`))
	if err != nil {
		t.Fatal(err)
	}
	if c, err := Pick(cfg, "developer", "", nil); err != nil || c.Name != "sonnet" || c.Provider.CLI != "claude" {
		t.Fatalf("developer: %+v %v", c, err)
	}
	if c, err := Pick(cfg, "reviewer", "sonnet", nil); err != nil || c.Name != "gem" {
		t.Fatalf("reviewer avoiding sonnet: %+v %v", c, err)
	}
	noAgy := func(p config.Provider) bool { return p.CLI != "agy" }
	if _, err := Pick(cfg, "reviewer", "sonnet", noAgy); !errors.Is(err, ErrNoModel) {
		t.Fatalf("expected ErrNoModel, got %v", err)
	}
	if _, err := Pick(cfg, "helper", "", nil); !errors.Is(err, ErrNoModel) {
		t.Fatalf("endpoint-only provider must not be picked for a CLI run: %v", err)
	}
	if _, err := Pick(cfg, "tester", "", nil); err == nil {
		t.Fatal("disabled role picked a model")
	}
	if _, err := Pick(cfg, "nope", "", nil); err == nil {
		t.Fatal("unknown role picked a model")
	}
}
