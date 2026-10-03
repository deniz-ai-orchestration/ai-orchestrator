package provider

import (
	"strconv"
	"strings"
)

// Flags were checked against real runs on PC2 on 2026-10-02 (claude 2.1.287,
// codex-cli 0.160.0, agy 1.2.14, opencode 1.18.34); see testdata/fixtures.

func init() {
	register(claude{})
	register(codex{})
	register(agy{})
	register(opencode{})
}

// claude drives Claude Code with the Pro subscription's OAuth token.
type claude struct{}

func (claude) CLI() string { return "claude" }

func (claude) Build(r Request) (Command, error) {
	if err := check(r); err != nil {
		return Command{}, err
	}
	argv := []string{"claude", "-p", "--model", r.Model,
		"--output-format", "stream-json", "--verbose",
		"--permission-mode", "bypassPermissions",
		// Keep the account's claude.ai connectors, user skills and plugins
		// out of agent runs; only the repo's own settings apply.
		"--strict-mcp-config", "--setting-sources", "project"}
	if r.Schema != "" {
		argv = append(argv, "--json-schema", r.Schema) // inline JSON, not a path
	}
	if r.MaxTurns > 0 {
		argv = append(argv, "--max-turns", strconv.Itoa(r.MaxTurns))
	}
	if r.Session != "" {
		argv = append(argv, "--resume", r.Session)
	}
	if r.ReadOnly {
		// Not a sandbox: Bash stays available. The real guard is the
		// read-only GitHub token and the throwaway container.
		argv = append(argv, "--disallowedTools", "Edit,Write,NotebookEdit")
	}
	return Command{
		Argv:      argv,
		Stdin:     r.Prompt,
		SecretEnv: map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "claude_oauth_token"},
	}, nil
}

// codex drives Codex CLI with the ChatGPT plan's auth.json. OpenAI's CI
// guidance forbids sharing one auth.json across concurrent jobs, so runs are
// serialized and the auth lives in one named volume.
type codex struct{}

func (codex) CLI() string { return "codex" }

func (codex) Build(r Request) (Command, error) {
	if err := check(r); err != nil {
		return Command{}, err
	}
	if r.Session != "" {
		return Command{}, errNoResume
	}
	// Codex's own sandbox needs Linux namespaces, which the agent container
	// (all capabilities dropped, no-new-privileges) does not allow, so every
	// command would fail. The throwaway container is the sandbox, and a
	// reviewer is kept read-only the same way as Claude's: by its prompt and
	// its read-only GitHub token.
	argv := []string{"codex", "exec", "-m", r.Model, "--json", "--dangerously-bypass-approvals-and-sandbox"}
	if r.SchemaPath != "" {
		argv = append(argv, "--output-schema", r.SchemaPath)
	}
	if r.ResultPath != "" {
		argv = append(argv, "-o", r.ResultPath)
	}
	argv = append(argv, "-") // read the prompt from stdin
	return Command{
		Argv:      argv,
		Stdin:     r.Prompt,
		Env:       map[string]string{"CODEX_HOME": "/home/agent/.codex"},
		Mounts:    []string{"orch-codex-home"},
		Serialize: true,
	}, nil
}

// agy drives Google's Antigravity CLI (Google AI Pro). Its login lives in
// the OS keyring; how that reaches a container is decided in Phase 0. agy
// does not read stdin, so the prompt must be an argument (bounded by
// ARG_MAX and visible in ps inside the container).
type agy struct{}

func (agy) CLI() string { return "agy" }

func (agy) Build(r Request) (Command, error) {
	if err := check(r); err != nil {
		return Command{}, err
	}
	if r.Session != "" {
		return Command{}, errNoResume
	}
	argv := []string{"agy", "-p", r.Prompt, "--model", r.Model, "--output-format", "stream-json"}
	if r.Schema != "" {
		argv = append(argv, "--json-schema", r.Schema)
	}
	return Command{
		Argv:   argv,
		Mounts: []string{"orch-agy-keyring"},
	}, nil
}

// opencode drives OpenCode with the OpenCode Go plan's API key. Models are
// always fully qualified as opencode-go/<model>. Stored auth under
// XDG_DATA_HOME beats OPENCODE_API_KEY, so every run gets its own data dir.
type opencode struct{}

func (opencode) CLI() string { return "opencode" }

func (opencode) Build(r Request) (Command, error) {
	if err := check(r); err != nil {
		return Command{}, err
	}
	if r.Session != "" {
		return Command{}, errNoResume
	}
	model := r.Model
	if !strings.HasPrefix(model, "opencode-go/") {
		model = "opencode-go/" + model
	}
	argv := []string{"opencode", "run", "--format", "json", "--model", model}
	agent := r.Agent
	if agent == "" && r.ReadOnly {
		agent = "plan" // opencode's built-in read-only agent
	}
	if agent != "" {
		argv = append(argv, "--agent", agent)
	}
	return Command{
		Argv:      argv,
		Stdin:     r.Prompt,
		Env:       map[string]string{"XDG_DATA_HOME": "/home/agent/.local/share/orch-run"},
		SecretEnv: map[string]string{"OPENCODE_API_KEY": "opencode_api_key"},
	}, nil
}
