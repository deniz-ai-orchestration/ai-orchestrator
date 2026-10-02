package provider

import (
	"strconv"
	"strings"
)

// Flags follow the architecture doc and the CLIs' docs as of 2026-10-02.
// They are confirmed against real runs on PC2 before the parsers land.

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
		"--permission-mode", "bypassPermissions"}
	if r.SchemaPath != "" {
		argv = append(argv, "--json-schema", r.SchemaPath)
	}
	if r.MaxTurns > 0 {
		argv = append(argv, "--max-turns", strconv.Itoa(r.MaxTurns))
	}
	if r.ReadOnly {
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
	sandbox := "workspace-write"
	if r.ReadOnly {
		sandbox = "read-only"
	}
	argv := []string{"codex", "exec", "-m", r.Model, "--json", "--sandbox", sandbox}
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
// the OS keyring; how that reaches a container is decided in Phase 0.
type agy struct{}

func (agy) CLI() string { return "agy" }

func (agy) Build(r Request) (Command, error) {
	if err := check(r); err != nil {
		return Command{}, err
	}
	return Command{
		Argv:   []string{"agy", "-p", r.Prompt, "--model", r.Model, "--output-format", "stream-json"},
		Mounts: []string{"orch-agy-keyring"},
	}, nil
}

// opencode drives OpenCode with the OpenCode Go plan's API key. Models are
// always fully qualified as opencode-go/<model>.
type opencode struct{}

func (opencode) CLI() string { return "opencode" }

func (opencode) Build(r Request) (Command, error) {
	if err := check(r); err != nil {
		return Command{}, err
	}
	model := r.Model
	if !strings.HasPrefix(model, "opencode-go/") {
		model = "opencode-go/" + model
	}
	argv := []string{"opencode", "run", "--format", "json", "--model", model}
	if r.Agent != "" {
		argv = append(argv, "--agent", r.Agent)
	}
	argv = append(argv, r.Prompt)
	return Command{
		Argv:      argv,
		SecretEnv: map[string]string{"OPENCODE_API_KEY": "opencode_api_key"},
	}, nil
}
