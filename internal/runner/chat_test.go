package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/deniz-ai-orchestration/ai-orchestrator/internal/config"
	"github.com/deniz-ai-orchestration/ai-orchestrator/internal/store"
)

// chatFake is a container for chat turns: it records each spec and its
// prompt, and prints the next canned stream.
type chatFake struct {
	mu      sync.Mutex
	specs   []Spec
	outputs []string
	// block makes the next turn wait for its context, like a long turn.
	block chan struct{}
}

func (f *chatFake) Run(ctx context.Context, s Spec, stdout, _ io.Writer) (Result, error) {
	f.mu.Lock()
	f.specs = append(f.specs, s)
	out := ""
	if len(f.outputs) > 0 {
		out, f.outputs = f.outputs[0], f.outputs[1:]
	}
	block := f.block
	f.block = nil
	f.mu.Unlock()
	if block != nil {
		close(block)
		<-ctx.Done()
		return Result{ExitCode: 137, Killed: true}, nil
	}
	_, _ = io.WriteString(stdout, out)
	return Result{}, nil
}

func (f *chatFake) RemoveStale(context.Context) error { return nil }

func (f *chatFake) spec(t *testing.T, i int) Spec {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if i >= len(f.specs) {
		t.Fatalf("only %d turns ran", len(f.specs))
	}
	return f.specs[i]
}

func turnOutput(session, status, reply string) string {
	so, _ := json.Marshal(TurnResult{Status: status, Reply: reply})
	line, _ := json.Marshal(map[string]any{"type": "result", "subtype": "success", "is_error": false,
		"session_id": session, "result": string(so), "structured_output": json.RawMessage(so),
		"usage": map[string]int{"input_tokens": 10, "output_tokens": 5}})
	return string(line) + "\n"
}

type chatEnv struct {
	chats   *Chats
	st      *store.Store
	fake    *chatFake
	project string
	cancel  context.CancelFunc
}

func newChatEnv(t *testing.T) *chatEnv {
	t.Helper()
	dir := t.TempDir()
	secrets := filepath.Join(dir, "secrets")
	if err := os.MkdirAll(secrets, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(secrets, "claude_oauth_token"), []byte("claude-tok\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	projects := filepath.Join(dir, "projects")
	project := filepath.Join(projects, "shop")
	if err := os.MkdirAll(project, 0o700); err != nil {
		t.Fatal(err)
	}
	git(t, project, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(project, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, project, "add", ".")
	git(t, project, "-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-q", "-m", "init")
	git(t, project, "branch", "feature/old")
	if err := os.MkdirAll(filepath.Join(projects, "notes"), 0o700); err != nil { // not a repository
		t.Fatal(err)
	}

	cfg, err := config.Parse([]byte(fmt.Sprintf(`
data_dir: %s
secrets_dir: %s
projects: { dir: %s }
github: { trusted_actor: denizekinci }
runner: { git_name: deniz-agent, git_email: agent@example.com }
providers:
  claude: { cli: claude, auth: oauth_token }
  codex:  { cli: codex, auth: auth_json }
models:
  sonnet: { provider: claude, model: sonnet }
  sol:    { provider: codex, model: gpt-6.1-sol }
roles:
  developer: { pool: [sonnet] }
  reviewer:  { pool: [sonnet, sol] }
  helper:    { pool: [sonnet] }
`, filepath.Join(dir, "data"), secrets, projects)))
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(context.Background(), filepath.Join(dir, "orch.db"))
	if err != nil {
		t.Fatal(err)
	}
	fake := &chatFake{}
	c := &Chats{Store: st, Cfg: cfg, Containers: fake, Projects: Projects{Dir: projects},
		Dir: filepath.Join(dir, "data", "agents"), RunsDir: filepath.Join(dir, "data", "runs"), User: "1000:1000",
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = c.Run(ctx); close(done) }()
	t.Cleanup(func() {
		cancel()
		<-done
		st.Close()
	})
	for !c.Running() {
		time.Sleep(time.Millisecond)
	}
	return &chatEnv{chats: c, st: st, fake: fake, project: project, cancel: cancel}
}

// settle waits until the agent's turn has ended.
func (e *chatEnv) settle(t *testing.T, id int64) store.Agent {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		a, err := e.st.GetAgent(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		e.chats.mu.Lock()
		_, busy := e.chats.turns[id]
		e.chats.mu.Unlock()
		if a.State != store.AgentWorking && !busy {
			return a
		}
		if time.Now().After(deadline) {
			t.Fatalf("agent %d still %s", id, a.State)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (e *chatEnv) messages(t *testing.T, id int64) []store.Message {
	t.Helper()
	ms, err := e.st.Messages(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return ms
}

func TestSummonAndTwoTurns(t *testing.T) {
	e := newChatEnv(t)
	ctx := context.Background()
	e.fake.outputs = []string{
		turnOutput("sess-1", "needs_you", "Which port should it use?"),
		turnOutput("sess-1", "done", "Added /health on 8080 and a test."),
	}
	a, err := e.chats.Summon(ctx, Summon{Role: "developer", Model: "sonnet", Project: "shop",
		Message: "Add a /health endpoint", Files: []File{{Name: "../spec notes.md", Data: []byte("# spec\n")}}})
	if err != nil {
		t.Fatal(err)
	}
	if a.Branch != fmt.Sprintf("orch/%d-add-a-health-endpoint", a.ID) {
		t.Fatalf("branch %q", a.Branch)
	}
	a = e.settle(t, a.ID)
	if a.State != store.AgentNeedsYou || a.Session != "sess-1" {
		t.Fatalf("after turn 1: %+v", a)
	}

	s := e.fake.spec(t, 0)
	if s.Work != a.Workspace || !strings.HasPrefix(a.Workspace, e.chats.Dir) {
		t.Fatalf("workspace %q, mounted %q", a.Workspace, s.Work)
	}
	if got := strings.TrimSpace(gitOut(t, a.Workspace, "branch", "--show-current")); got != a.Branch {
		t.Fatalf("worktree on %q", got)
	}
	gitDir := filepath.Join(e.project, ".git")
	inbox := filepath.Join(e.chats.Dir, fmt.Sprint(a.ID), "inbox")
	for _, want := range []string{inbox + ":/inbox:ro", gitDir + ":" + gitDir} {
		if !slices.Contains(s.Binds, want) {
			t.Errorf("bind %q missing from %v", want, s.Binds)
		}
	}
	if _, ok := s.Secrets["GH_TOKEN"]; ok {
		t.Error("a chat agent got a GitHub token")
	}
	if s.Secrets["CLAUDE_CODE_OAUTH_TOKEN"] != "claude-tok" || slices.Contains(s.Argv, "--resume") {
		t.Fatalf("first turn argv %v", s.Argv)
	}
	if !strings.Contains(s.Stdin, "a git worktree on branch "+a.Branch) || !strings.Contains(s.Stdin, "Add a /health endpoint") ||
		!strings.Contains(s.Stdin, "/inbox/1-spec_notes.md") {
		t.Fatalf("first prompt:\n%s", s.Stdin)
	}
	if b, err := os.ReadFile(filepath.Join(inbox, "1-spec_notes.md")); err != nil || string(b) != "# spec\n" {
		t.Fatalf("inbox file: %q %v", b, err)
	}

	if err := e.chats.Send(ctx, a.ID, "Use 8080.", nil); err != nil {
		t.Fatal(err)
	}
	a = e.settle(t, a.ID)
	if a.State != store.AgentDone {
		t.Fatalf("after turn 2: %s", a.State)
	}
	s = e.fake.spec(t, 1)
	if i := slices.Index(s.Argv, "--resume"); i < 0 || s.Argv[i+1] != "sess-1" {
		t.Fatalf("second turn does not resume: %v", s.Argv)
	}
	if strings.Contains(s.Stdin, "summoned from orch") || !strings.Contains(s.Stdin, "Use 8080.") || strings.Contains(s.Stdin, "health endpoint") {
		t.Fatalf("second prompt:\n%s", s.Stdin)
	}
	ms := e.messages(t, a.ID)
	var got []string
	for _, m := range ms {
		got = append(got, m.Author+": "+m.Body)
	}
	want := []string{"you: Add a /health endpoint", "agent: Which port should it use?", "you: Use 8080.", "agent: Added /health on 8080 and a test."}
	if !slices.Equal(got, want) {
		t.Fatalf("conversation %q", got)
	}
	runs, err := e.st.AgentRuns(ctx, a.ID, 10)
	if err != nil || len(runs) != 2 || runs[0].Status != store.RunSucceeded || runs[0].AgentID != a.ID {
		t.Fatalf("runs %+v %v", runs, err)
	}
}

func TestReadOnlyRoleAndExistingBranch(t *testing.T) {
	e := newChatEnv(t)
	e.fake.outputs = []string{turnOutput("s", "done", "Tests pass.")}
	a, err := e.chats.Summon(context.Background(), Summon{Role: "reviewer", Model: "sonnet", Project: "shop",
		Branch: "feature/old", Message: "Review this branch"})
	if err != nil {
		t.Fatal(err)
	}
	e.settle(t, a.ID)
	s := e.fake.spec(t, 0)
	if !strings.Contains(strings.Join(s.Argv, " "), "--disallowedTools Edit,Write") || !strings.Contains(s.Stdin, "read-only") {
		t.Fatalf("reviewer is not read-only: %v\n%s", s.Argv, s.Stdin)
	}
	if got := strings.TrimSpace(gitOut(t, a.Workspace, "branch", "--show-current")); got != "feature/old" {
		t.Fatalf("worktree on %q", got)
	}
}

func TestSummonRefusals(t *testing.T) {
	e := newChatEnv(t)
	ctx := context.Background()
	big := make([]byte, MaxFileBytes+1)
	for i := range big {
		big[i] = 'a'
	}
	cases := []struct {
		name string
		s    Summon
		want string
	}{
		{"helper", Summon{Role: "helper", Model: "sonnet", Project: "shop", Message: "x"}, "not a role"},
		{"codex chat", Summon{Role: "developer", Model: "sol", Project: "shop", Message: "x"}, "later step"},
		{"no task", Summon{Role: "developer", Model: "sonnet", Project: "shop", Message: " "}, "give the agent a task"},
		{"not a repo", Summon{Role: "developer", Model: "sonnet", Project: "notes", Message: "x"}, "not a git repository"},
		{"traversal", Summon{Role: "developer", Model: "sonnet", Project: "../shop", Message: "x"}, "not a project folder"},
		{"option branch", Summon{Role: "developer", Model: "sonnet", Project: "shop", Branch: "--force", Message: "x"}, "not a branch name"},
		{"binary", Summon{Role: "developer", Model: "sonnet", Project: "shop", Message: "x", Files: []File{{Name: "a.bin", Data: []byte{0, 1}}}}, "not a text file"},
		{"too big", Summon{Role: "developer", Model: "sonnet", Project: "shop", Message: "x", Files: []File{{Name: "a.md", Data: big}}}, "larger than 1 MB"},
	}
	for _, tc := range cases {
		if _, err := e.chats.Summon(ctx, tc.s); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err %v, want %q", tc.name, err, tc.want)
		}
	}
	if as, _ := e.st.OpenAgents(ctx); len(as) != 0 {
		t.Fatalf("refused summons created agents: %+v", as)
	}
	names, err := e.chats.Projects.List()
	if err != nil || !slices.Equal(names, []string{"shop"}) {
		t.Fatalf("projects %v %v", names, err)
	}
}

func TestBusyStopAndClose(t *testing.T) {
	e := newChatEnv(t)
	ctx := context.Background()
	started := make(chan struct{})
	e.fake.block = started
	a, err := e.chats.Summon(ctx, Summon{Role: "developer", Model: "sonnet", Project: "shop", Message: "Long task"})
	if err != nil {
		t.Fatal(err)
	}
	<-started
	if err := e.chats.Send(ctx, a.ID, "more", nil); !errors.Is(err, ErrBusy) {
		t.Fatalf("send while working: %v", err)
	}
	if err := e.chats.Close(ctx, a.ID); err == nil {
		t.Fatal("closed a working agent")
	}
	if !e.chats.Stop(a.ID) {
		t.Fatal("stop found no turn")
	}
	a = e.settle(t, a.ID)
	ms := e.messages(t, a.ID)
	if a.State != store.AgentStopped || ms[len(ms)-1].Author != store.FromOrch {
		t.Fatalf("after stop: %s %+v", a.State, ms)
	}

	if err := e.chats.Close(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(a.Workspace); !os.IsNotExist(err) {
		t.Fatalf("worktree still there: %v", err)
	}
	if out := gitOut(t, e.project, "branch", "--list", a.Branch); !strings.Contains(out, a.Branch) {
		t.Fatalf("branch %s was deleted", a.Branch)
	}
	if got, _ := e.st.GetAgent(ctx, a.ID); got.State != store.AgentClosed {
		t.Fatalf("state %s", got.State)
	}
	if err := e.chats.Send(ctx, a.ID, "hello?", nil); err == nil {
		t.Fatal("sent to a closed agent")
	}
}

func TestFailedTurnResendsMessage(t *testing.T) {
	e := newChatEnv(t)
	ctx := context.Background()
	e.fake.outputs = []string{
		`{"type":"result","subtype":"success","is_error":true,"result":"You've hit your usage limit","session_id":"s1"}` + "\n",
		turnOutput("s1", "done", "Done."),
	}
	a, err := e.chats.Summon(ctx, Summon{Role: "developer", Model: "sonnet", Project: "shop", Message: "First"})
	if err != nil {
		t.Fatal(err)
	}
	a = e.settle(t, a.ID)
	ms := e.messages(t, a.ID)
	if a.State != store.AgentFailed || !strings.Contains(ms[len(ms)-1].Body, "usage limit") {
		t.Fatalf("after limit: %s %+v", a.State, ms)
	}
	if err := e.chats.Send(ctx, a.ID, "Second", nil); err != nil {
		t.Fatal(err)
	}
	e.settle(t, a.ID)
	if p := e.fake.spec(t, 1).Stdin; !strings.Contains(p, "First") || !strings.Contains(p, "Second") {
		t.Fatalf("retry prompt:\n%s", p)
	}
}

func TestChatRecover(t *testing.T) {
	e := newChatEnv(t)
	ctx := context.Background()
	a, err := e.st.CreateAgent(ctx, store.Agent{Role: "developer", Provider: "claude", Model: "sonnet", Project: "shop", Branch: "b"})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.chats.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	got, _ := e.st.GetAgent(ctx, a.ID)
	ms := e.messages(t, a.ID)
	if got.State != store.AgentStopped || len(ms) != 1 || !strings.Contains(ms[0].Body, "restarted") {
		t.Fatalf("after recover: %s %+v", got.State, ms)
	}
}

func TestCleanName(t *testing.T) {
	for in, want := range map[string]string{
		"notes.md": "notes.md", "../../etc/passwd": "passwd", `C:\x\a b.js`: "a_b.js", ".env": "env", "": "file.txt",
	} {
		if got := cleanName(in); got != want {
			t.Errorf("cleanName(%q) = %q, want %q", in, got, want)
		}
	}
}

func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := gitIn(context.Background(), dir, args...)
	if err != nil {
		t.Fatal(err)
	}
	return out
}
