package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/deniz-ai-orchestration/ai-orchestrator/internal/config"
	"github.com/deniz-ai-orchestration/ai-orchestrator/internal/engine"
	"github.com/deniz-ai-orchestration/ai-orchestrator/internal/github"
	"github.com/deniz-ai-orchestration/ai-orchestrator/internal/store"
)

// fakeAgent stands in for the container: it checks what it was given and
// prints a canned CLI stream.
type fakeAgent struct {
	t      *testing.T
	stdout string
	exit   int
	killed bool
	specs  []Spec
	stale  int
	// act runs inside the "container" against the workspace, like the CLI would.
	act func(ws string)
	// started, when set, is signalled once the run starts; the run then
	// blocks until its context ends, like a long agent run.
	started chan struct{}
}

func (f *fakeAgent) Run(ctx context.Context, s Spec, stdout, _ io.Writer) (Result, error) {
	f.specs = append(f.specs, s)
	if f.act != nil {
		f.act(s.Work)
	}
	if f.started != nil {
		close(f.started)
		<-ctx.Done()
		return Result{ExitCode: 137, Killed: true}, nil
	}
	_, _ = io.WriteString(stdout, f.stdout)
	return Result{ExitCode: f.exit, Killed: f.killed}, nil
}

func (f *fakeAgent) RemoveStale(context.Context) error { f.stale++; return nil }

type fakeGH struct {
	pr       *github.PullRequest
	cmp      github.Comparison
	comments []string
}

func (g *fakeGH) FindPR(_ context.Context, _, branch string) (github.PullRequest, error) {
	if g.pr == nil || g.pr.Head.Ref != branch {
		return github.PullRequest{}, github.ErrNoPR
	}
	return *g.pr, nil
}

func (g *fakeGH) Compare(context.Context, string, string, string) (github.Comparison, error) {
	return g.cmp, nil
}

func (g *fakeGH) EnsureComment(_ context.Context, _ string, _ int, marker, body, _ string) (bool, error) {
	for _, c := range g.comments {
		if strings.Contains(c, marker) {
			return false, nil
		}
	}
	g.comments = append(g.comments, body+marker)
	return true, nil
}

func claudeResult(status, summary string) string {
	so, _ := json.Marshal(map[string]string{"status": status, "summary": summary})
	line, _ := json.Marshal(map[string]any{"type": "result", "subtype": "success", "is_error": false,
		"terminal_reason": "completed", "result": string(so), "structured_output": json.RawMessage(so),
		"usage": map[string]int{"input_tokens": 100, "output_tokens": 50}})
	return `{"type":"system","subtype":"init"}` + "\n" + string(line) + "\n"
}

type env struct {
	origin string
	dev    *Agents
	st     *store.Store
	agent  *fakeAgent
	gh     *fakeGH
	task   store.Task
}

func newEnv(t *testing.T, cfgExtra string) *env {
	t.Helper()
	dir := t.TempDir()
	secrets := filepath.Join(dir, "secrets")
	if err := os.MkdirAll(secrets, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, v := range map[string]string{"github_developer_token": "dev-tok", "github_reviewer_token": "rev-tok", "claude_oauth_token": "claude-tok"} {
		if err := os.WriteFile(filepath.Join(secrets, name), []byte(v+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cfg, err := config.Parse([]byte(fmt.Sprintf(`
data_dir: %s
secrets_dir: %s
github: { repos: [o/r], trusted_actor: denizekinci }
runner: { git_name: deniz-agent, git_email: agent@example.com }
providers:
  claude: { cli: claude, auth: oauth_token }
  gemini: { cli: agy, auth: keyring }
  codex:  { cli: codex, auth: auth_json }
models:
  gem:    { provider: gemini, model: g }
  sonnet: { provider: claude, model: sonnet }
  sol:    { provider: codex, model: gpt-6.1-sol }
roles:
  developer: { pool: [gem, sonnet], timeout: 5m }
  reviewer:  { pool: [sonnet, sol], not_same_as: developer }
%s`, filepath.Join(dir, "data"), secrets, cfgExtra)))
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(context.Background(), filepath.Join(dir, "orch.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	origin := originRepo(t)
	agent := &fakeAgent{t: t}
	gh := &fakeGH{}
	d := &Agents{Store: st, GH: gh, Cfg: cfg, Containers: agent,
		WS:      Workspaces{Root: filepath.Join(dir, "data", "ws"), CloneURL: func(string) string { return origin }},
		RunsDir: filepath.Join(dir, "data", "runs"), User: "1000:1000",
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	task, err := st.CreateTask(context.Background(), "o/r", 3, "Add a /health endpoint!", "Return 200 OK.")
	if err != nil {
		t.Fatal(err)
	}
	return &env{origin: origin, dev: d, st: st, agent: agent, gh: gh, task: task}
}

func (e *env) state(t *testing.T) store.Task {
	t.Helper()
	got, err := e.st.GetTask(context.Background(), e.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func (e *env) lastDetail(t *testing.T) string {
	t.Helper()
	trs, err := e.st.Transitions(context.Background(), e.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	return trs[len(trs)-1].Detail
}

const branch = "agent/3-add-a-health-endpoint"

func goodPR() *github.PullRequest {
	return &github.PullRequest{Number: 12, State: "open", Body: "Adds /health.\n\nCloses #3",
		Head: github.Ref{Ref: branch, SHA: "abc123"}, Base: github.Ref{Ref: "main"}}
}

func TestDeveloperOpensPR(t *testing.T) {
	e := newEnv(t, "")
	e.gh.pr = goodPR()
	e.gh.cmp = github.Comparison{Files: []github.ChangedFile{{Filename: "health.go", Additions: 20}}}
	e.agent.stdout = claudeResult("done", "Added /health; go test passes.")
	e.agent.act = func(ws string) {
		if b := git(t, ws, "branch", "--show-current"); b != branch {
			t.Errorf("workspace on %q, want %q", b, branch)
		}
	}
	ctx := context.Background()
	if err := e.dev.Recover(ctx); err != nil || e.agent.stale != 1 {
		t.Fatalf("recover: %v, stale calls %d", err, e.agent.stale)
	}
	if err := e.dev.Once(ctx); err != nil {
		t.Fatal(err)
	}

	got := e.state(t)
	if got.State != engine.AwaitingCI || got.PRNumber != 12 || got.HeadSHA != "abc123" || got.Branch != branch || got.DevModel != "sonnet" {
		t.Fatalf("task %+v", got.Task)
	}
	if len(e.agent.specs) != 1 {
		t.Fatalf("runs %d", len(e.agent.specs))
	}
	s := e.agent.specs[0]
	if s.Secrets["GH_TOKEN"] != "dev-tok" || s.Secrets["CLAUDE_CODE_OAUTH_TOKEN"] != "claude-tok" {
		t.Errorf("secrets %v", s.Secrets)
	}
	if args := strings.Join(s.Args(), " "); strings.Contains(args, "dev-tok") || strings.Contains(args, "claude-tok") {
		t.Errorf("token in docker args")
	}
	if s.Argv[0] != "claude" || !strings.Contains(strings.Join(s.Argv, " "), `--json-schema {"type":"object"`) {
		t.Errorf("argv %v", s.Argv)
	}
	for _, want := range []string{"issue #3", "Add a /health endpoint!", "Return 200 OK.", "git push -u origin " + branch,
		"gh pr create --base main --head " + branch, "Closes #3", "--reviewer denizekinci", ".github/**"} {
		if !strings.Contains(s.Stdin, want) {
			t.Errorf("prompt missing %q", want)
		}
	}
	if s.Env["GIT_AUTHOR_NAME"] != "deniz-agent" || s.User != "1000:1000" || s.Image != "orch-agent:latest" {
		t.Errorf("spec %+v", s)
	}
	runs, err := e.st.RunsForOutbox(ctx, 2) // outbox 1 is the orch:dev label
	if err != nil || len(runs) != 1 || runs[0].Status != store.RunSucceeded || runs[0].Model != "sonnet" {
		t.Fatalf("runs %+v err %v", runs, err)
	}
	if _, err := os.Stat(filepath.Join(runs[0].LogDir, "stdout.jsonl")); err != nil {
		t.Errorf("stdout not kept: %v", err)
	}

	// Nothing queued any more: another round starts no run.
	if err := e.dev.Once(ctx); err != nil || len(e.agent.specs) != 1 {
		t.Fatalf("second round: err %v, runs %d", err, len(e.agent.specs))
	}
}

func TestDeveloperOutcomes(t *testing.T) {
	cases := []struct {
		name       string
		stdout     string
		exit       int
		killed     bool
		pr         *github.PullRequest
		files      []github.ChangedFile
		wantReason engine.Reason
		wantDetail string
	}{
		{name: "blocked", stdout: claudeResult("blocked", "Which port should /health use?"),
			wantReason: engine.ReasonRequirementsUnclear, wantDetail: "Which port"},
		{name: "no PR", stdout: claudeResult("done", "ok"),
			wantReason: engine.ReasonBadOutput, wantDetail: "no open PR"},
		{name: "forbidden path", stdout: claudeResult("done", "ok"), pr: goodPR(),
			files:      []github.ChangedFile{{Filename: ".github/workflows/ci.yml", Additions: 1}},
			wantReason: engine.ReasonScopeViolation, wantDetail: "forbidden_path: .github/workflows/ci.yml"},
		{name: "diff too big", stdout: claudeResult("done", "ok"), pr: goodPR(),
			files:      []github.ChangedFile{{Filename: "big.go", Additions: 900}},
			wantReason: engine.ReasonScopeViolation, wantDetail: "diff_size"},
		{name: "PR not linked", stdout: claudeResult("done", "ok"),
			pr:         &github.PullRequest{Number: 12, Body: "no link", Head: github.Ref{Ref: branch, SHA: "a"}, Base: github.Ref{Ref: "main"}},
			wantReason: engine.ReasonScopeViolation, wantDetail: "issue_link"},
		{name: "auth failed", exit: 1,
			stdout:     `{"type":"result","subtype":"success","is_error":true,"terminal_reason":"api_error","result":"Not logged in · Please run /login"}` + "\n",
			wantReason: engine.ReasonAuthFailed},
		{name: "timeout", killed: true, exit: 137, stdout: `{"type":"system","subtype":"init"}` + "\n",
			wantReason: engine.ReasonTimeout},
		{name: "no structured result", stdout: `{"type":"result","subtype":"success","is_error":false,"terminal_reason":"completed","result":"I did it"}` + "\n",
			wantReason: engine.ReasonBadOutput},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t, "")
			e.agent.stdout, e.agent.exit, e.agent.killed = tc.stdout, tc.exit, tc.killed
			e.gh.pr = tc.pr
			e.gh.cmp = github.Comparison{Files: tc.files}
			if err := e.dev.Once(context.Background()); err != nil {
				t.Fatal(err)
			}
			got := e.state(t)
			if got.State != engine.NeedsHuman || got.Reason != tc.wantReason {
				t.Fatalf("state %s reason %s, want needs_human %s (detail %q)", got.State, got.Reason, tc.wantReason, e.lastDetail(t))
			}
			if !strings.Contains(e.lastDetail(t), tc.wantDetail) {
				t.Errorf("detail %q, want %q", e.lastDetail(t), tc.wantDetail)
			}
			if tc.name == "blocked" && (len(e.gh.comments) != 1 || !strings.Contains(e.gh.comments[0], "Which port")) {
				t.Errorf("question not posted: %v", e.gh.comments)
			}
		})
	}
}

func TestInterruptedRunRetriesOnce(t *testing.T) {
	e := newEnv(t, "")
	ctx := context.Background()
	if _, err := e.st.ApplyEvent(ctx, e.task.ID, engine.Event{Kind: engine.EvDevStarted, Model: "sonnet"}, e.dev.Cfg.Limits); err != nil {
		t.Fatal(err)
	}
	items, _ := e.st.PendingEffectsOf(ctx, 1, engine.EffStartDevRun)
	start := func() {
		if _, err := e.st.StartRun(ctx, store.Run{TaskID: e.task.ID, OutboxID: items[0].ID, Role: "developer", Provider: "claude", Model: "sonnet"}); err != nil {
			t.Fatal(err)
		}
		if err := e.dev.Recover(ctx); err != nil { // the "restart"
			t.Fatal(err)
		}
	}
	start()
	e.agent.stdout = claudeResult("blocked", "?")
	if err := e.dev.Once(ctx); err != nil {
		t.Fatal(err)
	}
	if len(e.agent.specs) != 1 || e.state(t).Reason != engine.ReasonRequirementsUnclear {
		t.Fatalf("one interruption should re-run: runs %d, task %+v", len(e.agent.specs), e.state(t).Task)
	}

	// Second time round: two interrupted runs for the same effect hold the task.
	e2 := newEnv(t, "")
	e = e2
	if _, err := e.st.ApplyEvent(ctx, e.task.ID, engine.Event{Kind: engine.EvDevStarted, Model: "sonnet"}, e.dev.Cfg.Limits); err != nil {
		t.Fatal(err)
	}
	items, _ = e.st.PendingEffectsOf(ctx, 1, engine.EffStartDevRun)
	start()
	start()
	if err := e.dev.Once(ctx); err != nil {
		t.Fatal(err)
	}
	if len(e.agent.specs) != 0 || e.state(t).Reason != engine.ReasonCLIError {
		t.Fatalf("runs %d, task %+v", len(e.agent.specs), e.state(t).Task)
	}
}

func TestCancelledTaskSkipsRunAndCleansUp(t *testing.T) {
	e := newEnv(t, "")
	ctx := context.Background()
	if _, err := e.st.ApplyEvent(ctx, e.task.ID, engine.Event{Kind: engine.EvDevStarted, Model: "sonnet"}, e.dev.Cfg.Limits); err != nil {
		t.Fatal(err)
	}
	ws := e.dev.WS.Dir(e.task.ID, "dev")
	if err := os.MkdirAll(ws, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := e.st.ApplyEvent(ctx, e.task.ID, engine.Event{Kind: engine.EvCancel}, e.dev.Cfg.Limits); err != nil {
		t.Fatal(err)
	}
	if err := e.dev.Once(ctx); err != nil {
		t.Fatal(err)
	}
	if len(e.agent.specs) != 0 {
		t.Fatal("cancelled task was run")
	}
	if _, err := os.Stat(ws); !os.IsNotExist(err) {
		t.Fatal("workspace not cleaned up")
	}
	if left, _ := e.st.PendingEffectsOf(ctx, 10, engine.EffStartDevRun, engine.EffCleanup); len(left) != 0 {
		t.Fatalf("effects left: %+v", left)
	}
}

func TestFixRunReusesBranch(t *testing.T) {
	e := newEnv(t, "")
	ctx := context.Background()
	l := e.dev.Cfg.Limits
	existing := "agent/3-existing"
	for _, ev := range []engine.Event{
		{Kind: engine.EvDevStarted, Model: "sonnet"},
		{Kind: engine.EvDevDone, Branch: existing, PRNumber: 12, HeadSHA: "a1"},
		{Kind: engine.EvCIFailed, HeadSHA: "a1"},
	} {
		if _, err := e.st.ApplyEvent(ctx, e.task.ID, ev, l); err != nil {
			t.Fatal(err)
		}
	}
	// Drop the first run's effect: this test is about the ci_fix run.
	items, _ := e.st.PendingEffectsOf(ctx, 1, engine.EffStartDevRun)
	if err := e.st.CompleteEffect(ctx, items[0].ID); err != nil {
		t.Fatal(err)
	}
	pr := goodPR()
	pr.Head = github.Ref{Ref: existing, SHA: "b2"}
	e.gh.pr = pr
	e.agent.stdout = claudeResult("done", "fixed")
	e.agent.act = func(ws string) {
		if _, err := os.Stat(filepath.Join(ws, "fix.go")); err != nil {
			t.Error("fix run did not start from the existing branch")
		}
	}
	if err := e.dev.Once(ctx); err != nil {
		t.Fatal(err)
	}
	s := e.agent.specs[0].Stdin
	if !strings.Contains(s, "CI failed") || strings.Contains(s, "gh pr create") || !strings.Contains(s, "PR #12 already exists") {
		t.Errorf("ci_fix prompt:\n%s", s)
	}
	if got := e.state(t); got.State != engine.AwaitingCI || got.HeadSHA != "b2" {
		t.Fatalf("task %+v", got.Task)
	}
}

func TestNoContainerReadyModel(t *testing.T) {
	e := newEnv(t, "")
	e.dev.Cfg.Roles["developer"] = config.Role{Pool: []string{"gem"}} // agy needs the keyring: not yet
	if err := e.dev.Once(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := e.state(t); got.State != engine.Queued || len(e.agent.specs) != 0 {
		t.Fatalf("task %+v, runs %d", got.Task, len(e.agent.specs))
	}
}

func TestSlug(t *testing.T) {
	for in, want := range map[string]string{
		"Add a /health endpoint!":  "add-a-health-endpoint",
		"  ---  ":                  "task",
		"Ünïcode títle":            "n-code-t-tle",
		strings.Repeat("abc ", 20): "abc-abc-abc-abc-abc-abc-abc-abc-abc-abc",
	} {
		if got := Slug(in); got != want {
			t.Errorf("Slug(%q) = %q, want %q", in, got, want)
		}
	}
}
