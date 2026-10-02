package provider

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/deniz-ai-orchestration/ai-orchestrator/internal/config"
	"github.com/deniz-ai-orchestration/ai-orchestrator/internal/engine"
)

func build(t *testing.T, cli string, r Request) Command {
	t.Helper()
	a, err := For(config.Provider{CLI: cli})
	if err != nil {
		t.Fatal(err)
	}
	c, err := a.Build(r)
	if err != nil {
		t.Fatal(err)
	}
	if c.Argv[0] != cli {
		t.Fatalf("argv[0] = %q, want %q", c.Argv[0], cli)
	}
	return c
}

func flag(argv []string, name string) string {
	i := slices.Index(argv, name)
	if i < 0 || i+1 >= len(argv) {
		return ""
	}
	return argv[i+1]
}

func TestClaude(t *testing.T) {
	c := build(t, "claude", Request{Model: "sonnet", Prompt: "fix #17", Schema: `{"type":"object"}`, MaxTurns: 40})
	if flag(c.Argv, "--model") != "sonnet" || flag(c.Argv, "--json-schema") != `{"type":"object"}` ||
		flag(c.Argv, "--max-turns") != "40" || flag(c.Argv, "--output-format") != "stream-json" {
		t.Fatalf("argv %v", c.Argv)
	}
	if c.Stdin != "fix #17" || slices.Contains(c.Argv, "fix #17") {
		t.Fatal("prompt must go on stdin, not argv")
	}
	if c.SecretEnv["CLAUDE_CODE_OAUTH_TOKEN"] != "claude_oauth_token" {
		t.Fatalf("secret env %v", c.SecretEnv)
	}
	ro := build(t, "claude", Request{Model: "sonnet", Prompt: "p", ReadOnly: true})
	if !strings.Contains(flag(ro.Argv, "--disallowedTools"), "Edit") {
		t.Fatalf("read-only argv %v", ro.Argv)
	}
}

func TestCodex(t *testing.T) {
	c := build(t, "codex", Request{Model: "gpt-6.1-sol", Prompt: "review", SchemaPath: "/r.json", ResultPath: "/out/verdict.json", ReadOnly: true})
	if flag(c.Argv, "-m") != "gpt-6.1-sol" || flag(c.Argv, "--sandbox") != "read-only" ||
		flag(c.Argv, "--output-schema") != "/r.json" || flag(c.Argv, "-o") != "/out/verdict.json" ||
		c.Argv[len(c.Argv)-1] != "-" {
		t.Fatalf("argv %v", c.Argv)
	}
	if !c.Serialize || len(c.Mounts) != 1 || c.Stdin != "review" {
		t.Fatalf("codex must serialize and mount one auth volume: %+v", c)
	}
	if w := build(t, "codex", Request{Model: "m", Prompt: "p"}); flag(w.Argv, "--sandbox") != "workspace-write" {
		t.Fatalf("developer sandbox %v", w.Argv)
	}
}

func TestOpenCodeQualifiesModel(t *testing.T) {
	for _, m := range []string{"kimi", "opencode-go/kimi"} {
		c := build(t, "opencode", Request{Model: m, Prompt: "test it", Agent: "qa"})
		if flag(c.Argv, "--model") != "opencode-go/kimi" || flag(c.Argv, "--agent") != "qa" || c.Stdin != "test it" {
			t.Fatalf("argv %v", c.Argv)
		}
		if slices.Contains(c.Argv, "test it") || c.Env["XDG_DATA_HOME"] == "" {
			t.Fatalf("prompt must be on stdin and the data dir isolated: %+v", c)
		}
	}
}

func TestOpenCodeReadOnlyUsesPlanAgent(t *testing.T) {
	c := build(t, "opencode", Request{Model: "kimi", Prompt: "p", ReadOnly: true})
	if flag(c.Argv, "--agent") != "plan" {
		t.Fatalf("argv %v", c.Argv)
	}
}

func TestClaudeIsolatesAccountSettings(t *testing.T) {
	c := build(t, "claude", Request{Model: "sonnet", Prompt: "p"})
	if !slices.Contains(c.Argv, "--strict-mcp-config") || flag(c.Argv, "--setting-sources") != "project" {
		t.Fatalf("argv %v", c.Argv)
	}
}

func TestAgy(t *testing.T) {
	c := build(t, "agy", Request{Model: "gemini-pro", Prompt: "p"})
	if flag(c.Argv, "--model") != "gemini-pro" || flag(c.Argv, "-p") != "p" {
		t.Fatalf("argv %v", c.Argv)
	}
}

func TestNoSecretValuesInCommands(t *testing.T) {
	for _, cli := range []string{"claude", "codex", "agy", "opencode"} {
		c := build(t, cli, Request{Model: "m", Prompt: "p"})
		for env, file := range c.SecretEnv {
			if file == "" || filepath.Base(file) != file {
				t.Errorf("%s: %s must name a bare file in secrets_dir, got %q", cli, env, file)
			}
		}
	}
}

func TestBuildValidates(t *testing.T) {
	for cli, a := range adapters {
		if _, err := a.Build(Request{Prompt: "p"}); err == nil {
			t.Errorf("%s: missing model accepted", cli)
		}
		if _, err := a.Build(Request{Model: "m"}); err == nil {
			t.Errorf("%s: missing prompt accepted", cli)
		}
	}
	if _, err := For(config.Provider{CLI: "gemini"}); err == nil {
		t.Error("unknown cli accepted")
	}
}

func TestExampleConfigProvidersHaveAdapters(t *testing.T) {
	cfg, err := config.Load("../../config.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for name, p := range cfg.Providers {
		if p.CLI == "" {
			continue // HTTP endpoint (Ollama), handled by the helper package
		}
		if _, err := For(p); err != nil {
			t.Errorf("provider %s: %v", name, err)
		}
	}
}

func TestOutcomeReason(t *testing.T) {
	cases := map[OutcomeKind]engine.Reason{
		OK: engine.ReasonNone, AuthFailed: engine.ReasonAuthFailed, RateLimited: engine.ReasonProviderUnavailable,
		Unavailable: engine.ReasonProviderUnavailable, Timeout: engine.ReasonTimeout, BadOutput: engine.ReasonBadOutput,
		Blocked: engine.ReasonRequirementsUnclear, CLIError: engine.ReasonCLIError,
	}
	for k, want := range cases {
		if got := (Outcome{Kind: k}).Reason(); got != want {
			t.Errorf("%s: got %q want %q", k, got, want)
		}
		if want != engine.ReasonNone && !want.Valid() {
			t.Errorf("%s maps to invalid reason %q", k, want)
		}
	}
}
