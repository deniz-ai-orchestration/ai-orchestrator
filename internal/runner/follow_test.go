package runner

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/deniz-ai-orchestration/ai-orchestrator/internal/github"
	"github.com/deniz-ai-orchestration/ai-orchestrator/internal/store"
)

// fakeGH is a GitHub double for workflow heads: one open PR and scripted
// check runs.
type wfGH struct {
	mu     sync.Mutex
	pr     github.PullRequest
	checks []github.CheckRun
	log    string
}

func (f *wfGH) GetPR(context.Context, string, int) (github.PullRequest, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.pr, nil
}

func (f *wfGH) CheckRuns(context.Context, string, string) ([]github.CheckRun, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.checks, nil
}

func (f *wfGH) JobLog(context.Context, string, int64) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return []byte(f.log), nil
}

func (f *wfGH) set(pr github.PullRequest, checks []github.CheckRun) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pr, f.checks = pr, checks
}

// ciHelper stands in for Ollama: it tags the summary so the test can
// follow it into the fix agent's brief.
type ciHelper struct{}

func (ciHelper) Summarize(_ context.Context, _, text string) string {
	return "helper summary of " + text
}

func autonomousEnv(t *testing.T) (*chatEnv, *fakePRs, *wfGH, *Workflows) {
	t.Helper()
	e := newChatEnv(t)
	ctx := context.Background()
	f, _ := e.withRemote(t)
	path, err := e.chats.Projects.Resolve("shop")
	if err != nil {
		t.Fatal(err)
	}
	if err := e.st.UpsertProject(ctx, store.Project{Path: path, Trusted: true, HasGit: true, GitHubRepo: "deniz/shop"}); err != nil {
		t.Fatal(err)
	}
	if err := e.st.SetProjectAutonomous(ctx, path, true); err != nil {
		t.Fatal(err)
	}
	gh := &wfGH{}
	fw := &Workflows{Store: e.st, GH: gh, Helper: ciHelper{}, Cfg: e.chats.Cfg, Chats: e.chats,
		Interval: time.Millisecond, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	return e, f, gh, fw
}

// waitWorkflowPhase polls a workflow until want holds: "pr" (a PR was
// opened), "repush:"+sha (a new head was pushed) or "ready"/"needs_you".
func waitWorkflowPhase(t *testing.T, e *chatEnv, id int64, want string) store.Workflow {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		w, err := e.st.GetWorkflow(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		var got string
		switch {
		case want == "pr" && w.PRNumber != 0:
			return w
		case strings.HasPrefix(want, "repush:") && w.HeadSHA != strings.TrimPrefix(want, "repush:") && w.CIState == store.CIPending:
			return w
		case (want == "ready" || want == "needs_you") && w.Phase == want:
			return w
		default:
			got = fmt.Sprintf("pr %d head %s ci %s phase %s", w.PRNumber, short(w.HeadSHA), w.CIState, w.Phase)
		}
		if time.Now().After(deadline) {
			t.Fatalf("workflow %d is at %s, want %s", id, got, want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// settleWorkflow waits until none of the workflow's agents is mid-turn,
// so a follower round sees it idle.
func settleWorkflow(t *testing.T, e *chatEnv, id int64) {
	t.Helper()
	as, err := e.st.WorkflowAgents(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range as {
		e.settle(t, a.ID)
	}
}

// fixDev polls for the workflow's newest developer past firstID.
func fixDev(t *testing.T, e *chatEnv, id int64, firstID int64) store.Agent {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		as, err := e.st.WorkflowAgents(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		for _, a := range as {
			if a.Role == "developer" && a.ID != firstID {
				return a
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no fix developer in workflow %d", id)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func headRef(sha string) github.Ref { return github.Ref{SHA: sha} }

func redRun() github.CheckRun {
	r := github.CheckRun{ID: 7, Name: "go", Status: "completed", Conclusion: "failure",
		HTMLURL: "https://github.com/deniz/shop/actions/runs/1"}
	r.App.Slug = "github-actions"
	return r
}

func greenRun() github.CheckRun {
	return github.CheckRun{ID: 8, Name: "go", Status: "completed", Conclusion: "success"}
}

// TestAutonomousLoop runs the whole hands-off job: dev -> testers ->
// automatic PR -> red CI -> fix agent carrying the CI summary -> testers
// -> push -> green CI -> review round -> ready, with no clicks after the
// summon.
func TestAutonomousLoop(t *testing.T) {
	e, f, gh, fw := autonomousEnv(t)
	ctx := context.Background()
	gh.log = "2026-10-04T00:00:00.000Z FAIL TestLogin (0.01s)"
	sc := e.script(t)

	w, err := e.chats.SummonWorkflow(ctx, WorkflowRequest{ProjectPath: "shop", Prompt: "Add change.txt", Model: "sonnet"})
	if err != nil {
		t.Fatal(err)
	}
	w = waitWorkflowPhase(t, e, w.ID, "pr")
	if len(f.created) != 1 {
		t.Fatalf("PRs created: %d", len(f.created))
	}
	settleWorkflow(t, e, w.ID)
	gh.set(github.PullRequest{Number: w.PRNumber, State: "open", Head: headRef(w.HeadSHA)}, []github.CheckRun{redRun()})
	if err := fw.Once(ctx); err != nil {
		t.Fatal(err)
	}

	// The fix agent carries the helper's summary of the failed job.
	fix := fixDev(t, e, w.ID, 1)
	fix = e.waitFor(t, fix.ID, store.AgentReady)
	sc.mu.Lock()
	prompts := append([]string{}, sc.prompts...)
	sc.mu.Unlock()
	if len(prompts) < 2 || !strings.Contains(prompts[1], "helper summary of") || !strings.Contains(prompts[1], "FAIL TestLogin") {
		t.Fatalf("fix brief:\n%v", prompts)
	}
	if got, _ := e.st.GetWorkflow(ctx, w.ID); got.CIAttempts != 1 || got.DevRuns != 2 {
		t.Fatalf("counters %+v", got)
	}

	// The fix passes CI: green starts the review round, which approves.
	settleWorkflow(t, e, w.ID)
	w = waitWorkflowPhase(t, e, w.ID, "repush:"+w.HeadSHA)
	gh.set(github.PullRequest{Number: w.PRNumber, State: "open", Head: headRef(w.HeadSHA)}, []github.CheckRun{greenRun()})
	if err := fw.Once(ctx); err != nil {
		t.Fatal(err)
	}
	w = waitWorkflowPhase(t, e, w.ID, "ready")
	if w.CIState != store.CIGreen || len(f.created) != 1 {
		t.Fatalf("end %+v, PRs %d", w, len(f.created))
	}
}

func TestWorkflowCILimitsHold(t *testing.T) {
	e, _, gh, fw := autonomousEnv(t)
	ctx := context.Background()
	e.script(t)
	e.chats.Cfg.Limits.CIAttempts = 1

	w, err := e.chats.SummonWorkflow(ctx, WorkflowRequest{ProjectPath: "shop", Prompt: "Add change.txt", Model: "sonnet"})
	if err != nil {
		t.Fatal(err)
	}
	w = waitWorkflowPhase(t, e, w.ID, "pr")
	settleWorkflow(t, e, w.ID)
	gh.set(github.PullRequest{Number: w.PRNumber, State: "open", Head: headRef(w.HeadSHA)}, []github.CheckRun{redRun()})
	if err := fw.Once(ctx); err != nil {
		t.Fatal(err)
	}
	// One fix round runs (attempt 1 of 1)...
	fix := fixDev(t, e, w.ID, 1)
	e.waitFor(t, fix.ID, store.AgentReady)
	settleWorkflow(t, e, w.ID)
	w = waitWorkflowPhase(t, e, w.ID, "repush:"+w.HeadSHA)
	gh.set(github.PullRequest{Number: w.PRNumber, State: "open", Head: headRef(w.HeadSHA)}, []github.CheckRun{redRun()})
	if err := fw.Once(ctx); err != nil {
		t.Fatal(err)
	}
	// ...then the CI-attempt limit parks the workflow for you.
	w = waitWorkflowPhase(t, e, w.ID, "needs_you")
	if w.CIAttempts != 1 {
		t.Fatalf("attempts %d", w.CIAttempts)
	}
	// dev, 2 testers, fix dev, 2 testers: no third developer was summoned.
	n := 0
	as, _ := e.st.WorkflowAgents(ctx, w.ID)
	for _, a := range as {
		if a.Role == "developer" {
			n++
		}
	}
	if n != 2 {
		t.Fatalf("developers after the limit: %d", n)
	}
}

func TestWorkflowsParallel(t *testing.T) {
	e := newChatEnv(t)
	ctx := context.Background()
	path, err := e.chats.Projects.Resolve("shop")
	if err != nil {
		t.Fatal(err)
	}
	if err := e.st.UpsertProject(ctx, store.Project{Path: path, Trusted: true, HasGit: true}); err != nil {
		t.Fatal(err)
	}
	e.fake.outputs = []string{turnOutput("a", "done", "Backend done."), turnOutput("b", "done", "Frontend done.")}
	type result struct {
		w   store.Workflow
		err error
	}
	ch := make(chan result, 2)
	for _, prompt := range []string{"Add the backend model", "Add the frontend feature"} {
		go func() {
			w, err := e.chats.SummonWorkflow(ctx, WorkflowRequest{ProjectPath: "shop", Prompt: prompt, Model: "sonnet"})
			ch <- result{w, err}
		}()
	}
	var ws []store.Workflow
	for range 2 {
		r := <-ch
		if r.err != nil {
			t.Fatal(r.err)
		}
		ws = append(ws, r.w)
	}
	if ws[0].Branch == ws[1].Branch || ws[0].Workspace == ws[1].Workspace {
		t.Fatalf("workflows share a branch or checkout: %+v", ws)
	}
	for _, w := range ws {
		as, err := e.st.WorkflowAgents(ctx, w.ID)
		if err != nil || len(as) != 1 {
			t.Fatalf("agents %+v %v", as, err)
		}
		if got := e.settle(t, as[0].ID); got.State != store.AgentDone || got.Workspace != w.Workspace {
			t.Fatalf("agent %+v", got)
		}
	}
	// Summoning the next developer continues the same branch and checkout.
	e.fake.outputs = []string{turnOutput("c", "done", "More backend.")}
	next, err := e.chats.SummonAgent(ctx, ws[0].ID, "developer", "sonnet", "Keep going")
	if err != nil {
		t.Fatal(err)
	}
	if next.Branch != ws[0].Branch || next.Workspace != ws[0].Workspace {
		t.Fatalf("replacement %+v", next)
	}
	if got := e.settle(t, next.ID); got.State != store.AgentDone {
		t.Fatalf("replacement %s", got.State)
	}
}
