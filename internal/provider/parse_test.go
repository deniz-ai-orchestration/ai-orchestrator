package provider

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/deniz-ai-orchestration/ai-orchestrator/internal/config"
)

// Every recorded case from PC2 and the outcome orch must derive from it.
var fixtureCases = map[string]OutcomeKind{
	"claude/success":     OK,
	"claude/json_schema": OK,
	"claude/read_only":   OK,
	"claude/question":    OK, // no schema: a question is just text
	"claude/bad_model":   CLIError,
	"claude/auth_fail":   AuthFailed,
	"claude/max_turns":   CLIError,
	"claude/timeout":     Timeout,

	"codex/read_only":         OK,
	"codex/workspace_write":   OK,
	"codex/no_schema":         OK,
	"codex/question":          Blocked,
	"codex/edit_in_read_only": Blocked,
	"codex/bad_model":         CLIError,
	"codex/auth_fail":         AuthFailed,
	"codex/timeout":           Timeout,

	"agy/success":                 OK,
	"agy/json_schema":             OK,
	"agy/read_only":               OK,
	"agy/question":                OK,
	"agy/no_skip_perms":           OK,
	"agy/stdin_dash":              OK,
	"agy/empty_home_still_authed": OK,
	"agy/bad_model":               CLIError,
	"agy/print_timeout":           Timeout,
	"agy/timeout":                 Timeout,

	"opencode/success":                         OK,
	"opencode/read_only":                       OK,
	"opencode/question":                        OK,
	"opencode/stdin_prompt":                    OK,
	"opencode/plan_agent_edit":                 OK,
	"opencode/env_key_invalid_but_stored_auth": OK,
	"opencode/bad_model":                       CLIError,
	"opencode/bad_model_unprefixed":            CLIError,
	"opencode/no_auth":                         CLIError, // opencode reports this as an opaque UnknownError
	"opencode/auth_fail":                       AuthFailed,
	"opencode/timeout":                         Timeout,
}

const fixtures = "../../testdata/fixtures"

func loadCase(t *testing.T, name string) (Request, RunOutput) {
	t.Helper()
	dir := filepath.Join(fixtures, name)
	read := func(f string) []byte {
		b, err := os.ReadFile(filepath.Join(dir, f))
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		return b
	}
	code, err := strconv.Atoi(strings.TrimSpace(string(read("exit_code"))))
	if err != nil {
		t.Fatalf("%s: exit_code: %v", name, err)
	}
	var req Request
	argv := string(read("argv.txt"))
	if strings.Contains(argv, "--json-schema") || strings.Contains(argv, "--output-schema") {
		req.SchemaPath = "schema.json"
	}
	return req, RunOutput{ExitCode: code, Stdout: read("stdout"), Stderr: read("stderr")}
}

func TestFixtures(t *testing.T) {
	for name, want := range fixtureCases {
		t.Run(name, func(t *testing.T) {
			cli := strings.SplitN(name, "/", 2)[0]
			a, err := For(config.Provider{CLI: cli})
			if err != nil {
				t.Fatal(err)
			}
			req, run := loadCase(t, name)
			got := a.Parse(req, run)
			if got.Kind != want {
				t.Fatalf("kind = %s (%s), want %s", got.Kind, got.Detail, want)
			}
			if want == OK && req.SchemaPath != "" && len(got.Result) == 0 {
				t.Fatal("schema run should carry the structured result")
			}
			if want == Blocked && got.Detail == "" {
				t.Fatal("blocked outcome should carry the agent's summary")
			}
			if name == "codex/question" && !strings.Contains(got.Detail, "package name") {
				t.Fatalf("question should be surfaced, got %q", got.Detail)
			}
			if want == OK && got.InputTokens+got.OutputTokens == 0 {
				t.Fatal("successful run should report token usage")
			}
		})
	}
}

// Every case directory on disk must be listed above, so new fixtures get an
// expectation.
func TestEveryFixtureHasAnExpectation(t *testing.T) {
	dirs, err := filepath.Glob(filepath.Join(fixtures, "*", "*", "exit_code"))
	if err != nil || len(dirs) == 0 {
		t.Fatalf("no fixtures found: %v", err)
	}
	for _, d := range dirs {
		rel, _ := filepath.Rel(fixtures, filepath.Dir(d))
		if _, ok := fixtureCases[filepath.ToSlash(rel)]; !ok {
			t.Errorf("fixture %s has no expected outcome", rel)
		}
	}
	if len(dirs) != len(fixtureCases) {
		t.Errorf("%d fixtures on disk, %d expectations", len(dirs), len(fixtureCases))
	}
}

func TestClaudeReportsQuotaReset(t *testing.T) {
	req, run := loadCase(t, "claude/success")
	if o := (claude{}).Parse(req, run); o.ResetAt.IsZero() {
		t.Fatal("rate_limit_event resetsAt should be parsed")
	}
}

func TestCodexAuthFailingEarly(t *testing.T) {
	_, run := loadCase(t, "codex/auth_fail")
	lines := strings.SplitN(string(run.Stdout), "\n", 4) // first reconnect error only
	if !(codex{}).AuthFailing([]byte(strings.Join(lines[:3], "\n"))) {
		t.Fatal("first 401 retry should be detected")
	}
	_, ok := loadCase(t, "codex/read_only")
	if (codex{}).AuthFailing(ok.Stdout) {
		t.Fatal("healthy run flagged")
	}
}

func TestSchemaRunWithoutJSONIsBadOutput(t *testing.T) {
	_, run := loadCase(t, "codex/no_schema")
	if o := (codex{}).Parse(Request{SchemaPath: "s.json"}, run); o.Kind != BadOutput {
		t.Fatalf("kind = %s", o.Kind)
	}
}

func TestClaudeRateLimitAndKill(t *testing.T) {
	stream := `{"type":"rate_limit_event","rate_limit_info":{"status":"rejected","resetsAt":1790934600}}
{"type":"assistant","error":"rate_limit","message":{}}
{"type":"result","subtype":"success","is_error":true,"terminal_reason":"api_error","result":"You've hit your limit"}`
	o := (claude{}).Parse(Request{}, RunOutput{ExitCode: 1, Stdout: []byte(stream)})
	if o.Kind != RateLimited || o.ResetAt.Unix() != 1790934600 {
		t.Fatalf("got %+v", o)
	}
	if o := (opencode{}).Parse(Request{}, RunOutput{Killed: true}); o.Kind != Timeout {
		t.Fatalf("killed opencode run: %s", o.Kind)
	}
}

func TestFencedJSON(t *testing.T) {
	o := structured(Request{SchemaPath: "s"}, nil, "```json\n{\"status\":\"blocked\",\"summary\":\"Which DB?\"}\n```", Outcome{})
	if o.Kind != Blocked || o.Detail != "Which DB?" {
		t.Fatalf("got %+v", o)
	}
}
