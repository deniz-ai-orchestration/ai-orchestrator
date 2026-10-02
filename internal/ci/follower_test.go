package ci

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"github.com/deniz-ai-orchestration/ai-orchestrator/internal/config"
	"github.com/deniz-ai-orchestration/ai-orchestrator/internal/engine"
	"github.com/deniz-ai-orchestration/ai-orchestrator/internal/github"
	"github.com/deniz-ai-orchestration/ai-orchestrator/internal/helper"
	"github.com/deniz-ai-orchestration/ai-orchestrator/internal/store"
)

type fakeGH struct {
	pr    github.PullRequest
	runs  []github.CheckRun
	logs  map[int64]string
	prErr error
	calls int
}

func (g *fakeGH) GetPR(context.Context, string, int) (github.PullRequest, error) {
	g.calls++
	return g.pr, g.prErr
}
func (g *fakeGH) CheckRuns(context.Context, string, string) ([]github.CheckRun, error) {
	return g.runs, nil
}
func (g *fakeGH) JobLog(_ context.Context, _ string, id int64) ([]byte, error) {
	l, ok := g.logs[id]
	if !ok {
		return nil, errors.New("404")
	}
	return []byte(l), nil
}

var limits = config.Limits{CIAttempts: 3, ReviewCycles: 3, DevRuns: 8, MaxDiffLines: 800}

func setup(t *testing.T) (*Follower, *store.Store, *fakeGH, int64) {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "orch.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	task, err := st.CreateTask(context.Background(), "o/r", 3, "t", "b")
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range []engine.Event{
		{Kind: engine.EvDevStarted, Model: "sonnet"},
		{Kind: engine.EvDevDone, Branch: "agent/3-t", PRNumber: 12, HeadSHA: "aaa"},
	} {
		if _, err := st.ApplyEvent(context.Background(), task.ID, ev, limits); err != nil {
			t.Fatal(err)
		}
	}
	gh := &fakeGH{pr: github.PullRequest{Number: 12, State: "open", Head: github.Ref{Ref: "agent/3-t", SHA: "aaa"}}}
	f := &Follower{Store: st, GH: gh, Helper: helper.Trim{Max: 2000}, Cfg: &config.Config{Limits: limits},
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	return f, st, gh, task.ID
}

func state(t *testing.T, st *store.Store, id int64) store.Task {
	t.Helper()
	got, err := st.GetTask(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func run(name, status, conclusion string, id int64, actions bool) github.CheckRun {
	r := github.CheckRun{ID: id, Name: name, Status: status, Conclusion: conclusion, HTMLURL: "https://ci/" + name}
	if actions {
		r.App.Slug = "github-actions"
	}
	return r
}

func TestWaitsThenPasses(t *testing.T) {
	f, st, gh, id := setup(t)
	ctx := context.Background()
	for _, runs := range [][]github.CheckRun{
		nil, // CI has not started
		{run("go", "in_progress", "", 1, true), run("lint", "completed", "success", 2, true)},
	} {
		gh.runs = runs
		if err := f.Once(ctx); err != nil {
			t.Fatal(err)
		}
		if s := state(t, st, id).State; s != engine.AwaitingCI {
			t.Fatalf("moved to %s while CI was pending", s)
		}
	}
	gh.runs = []github.CheckRun{run("go", "completed", "success", 1, true), run("docs", "completed", "skipped", 2, false)}
	if err := f.Once(ctx); err != nil {
		t.Fatal(err)
	}
	if s := state(t, st, id).State; s != engine.ReviewQueued {
		t.Fatalf("state %s", s)
	}
	// Green again on the next round is a no-op, not an error.
	if err := f.Once(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestFailureQueuesFixWithLog(t *testing.T) {
	f, st, gh, id := setup(t)
	ctx := context.Background()
	gh.runs = []github.CheckRun{
		run("go", "completed", "failure", 7, true),
		run("ext", "completed", "timed_out", 8, false),
	}
	gh.runs[1].Output.Title = "external check timed out"
	gh.logs = map[int64]string{7: "2026-10-02T10:00:00.1234567Z ##[group]Run go test\n2026-10-02T10:00:01.0000000Z --- FAIL: TestHealth (0.00s)\n"}
	if err := f.Once(ctx); err != nil {
		t.Fatal(err)
	}
	got := state(t, st, id)
	if got.State != engine.Queued || got.Work != engine.WorkCIFix || got.CIAttempts != 1 {
		t.Fatalf("task %+v", got.Task)
	}
	d, err := st.LastDetail(ctx, id, engine.EvCIFailed)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"### go: failure", "--- FAIL: TestHealth", "### ext: timed_out", "external check timed out", "https://ci/go"} {
		if !strings.Contains(d, want) {
			t.Errorf("detail missing %q:\n%s", want, d)
		}
	}
	if strings.Contains(d, "2026-10-02T") {
		t.Errorf("timestamps kept:\n%s", d)
	}
}

func TestPRStateChanges(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*github.PullRequest)
		want   engine.State
		reason engine.Reason
	}{
		{"merged", func(p *github.PullRequest) { p.Merged, p.State = true, "closed" }, engine.Done, ""},
		{"closed", func(p *github.PullRequest) { p.State = "closed" }, engine.Rejected, ""},
		{"pushed by someone else", func(p *github.PullRequest) { p.Head.SHA = "bbb" }, engine.NeedsHuman, engine.ReasonHeadChanged},
		{"conflict", func(p *github.PullRequest) { no := false; p.Mergeable, p.MergeableState = &no, "dirty" }, engine.NeedsHuman, engine.ReasonMergeConflict},
		{"mergeable unknown", func(p *github.PullRequest) { p.MergeableState = "unknown" }, engine.AwaitingCI, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, st, gh, id := setup(t)
			tc.mutate(&gh.pr)
			if err := f.Once(context.Background()); err != nil {
				t.Fatal(err)
			}
			got := state(t, st, id)
			if got.State != tc.want || got.Reason != tc.reason {
				t.Fatalf("state %s reason %s", got.State, got.Reason)
			}
		})
	}
}

func TestHeldTaskOnlyWatchesMergeAndClose(t *testing.T) {
	f, st, gh, id := setup(t)
	ctx := context.Background()
	gh.pr.Head.SHA = "bbb"
	if err := f.Once(ctx); err != nil { // -> needs_human (head changed)
		t.Fatal(err)
	}
	gh.pr.Head.SHA = "ccc"
	gh.runs = []github.CheckRun{run("go", "completed", "failure", 1, true)}
	if err := f.Once(ctx); err != nil {
		t.Fatal(err)
	}
	if got := state(t, st, id); got.State != engine.NeedsHuman || got.HeadSHA != "bbb" {
		t.Fatalf("held task changed: %+v", got.Task)
	}
	gh.pr.Merged = true
	if err := f.Once(ctx); err != nil {
		t.Fatal(err)
	}
	if got := state(t, st, id); got.State != engine.Done {
		t.Fatalf("state %s", got.State)
	}
	calls := gh.calls
	if err := f.Once(ctx); err != nil || gh.calls != calls {
		t.Fatalf("finished task still polled: err %v calls %d -> %d", err, calls, gh.calls)
	}
}

func TestGitHubErrorStopsRound(t *testing.T) {
	f, _, gh, _ := setup(t)
	gh.prErr = &github.RateLimitError{}
	if err := f.Once(context.Background()); err == nil {
		t.Fatal("expected the error")
	}
}

func TestCleanLog(t *testing.T) {
	in := "2026-10-02T10:00:00.1234567Z a\n2026-10-02T10:00:01.1Z b\nno stamp\n"
	if got := CleanLog(in); got != "a\nb\nno stamp\n" {
		t.Fatalf("got %q", got)
	}
}
