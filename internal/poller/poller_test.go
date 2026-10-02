package poller

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/deniz-ai-orchestration/ai-orchestrator/internal/config"
	"github.com/deniz-ai-orchestration/ai-orchestrator/internal/engine"
	"github.com/deniz-ai-orchestration/ai-orchestrator/internal/github"
	"github.com/deniz-ai-orchestration/ai-orchestrator/internal/store"
)

type fakeGH struct {
	events  []github.IssueEvent
	etag    string
	issues  map[int]github.Issue
	polls   int
	getErr  error
	gotETag []string
}

func (f *fakeGH) IssueEvents(_ context.Context, _, etag string) ([]github.IssueEvent, string, error) {
	f.polls++
	f.gotETag = append(f.gotETag, etag)
	if etag != "" && etag == f.etag {
		return nil, etag, github.ErrNotModified
	}
	return f.events, f.etag, nil
}

func (f *fakeGH) GetIssue(_ context.Context, _ string, n int) (github.Issue, error) {
	if f.getErr != nil {
		return github.Issue{}, f.getErr
	}
	return f.issues[n], nil
}

var cfg = config.GitHub{Repos: []string{"o/r"}, Trigger: "agent:ready", TrustedActor: "denizekinci", PollInterval: time.Minute}

func labeled(id int64, issue int, actor string) github.IssueEvent {
	return github.IssueEvent{ID: id, Event: "labeled", Actor: github.User{Login: actor},
		Label: &github.Label{Name: "agent:ready"}, Issue: github.Issue{Number: issue}}
}

func openIssue(n int, author string) github.Issue {
	return github.Issue{Number: n, Title: "Issue", Body: "Goal", State: "open",
		User: github.User{Login: author}, Labels: []github.Label{{Name: "agent:ready"}}}
}

func newPoller(t *testing.T, gh *fakeGH) (*Poller, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "orch.db")
	st, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return &Poller{Store: st, GH: gh, Cfg: cfg, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}, path
}

func TestLabelCreatesTaskOnce(t *testing.T) {
	gh := &fakeGH{
		events: []github.IssueEvent{labeled(1, 17, "denizekinci")},
		etag:   `"e1"`,
		issues: map[int]github.Issue{17: openIssue(17, "denizekinci")},
	}
	p, path := newPoller(t, gh)
	ctx := context.Background()
	if err := p.Round(ctx); err != nil {
		t.Fatal(err)
	}
	tasks, _ := p.Store.ListTasks(ctx)
	if len(tasks) != 1 || tasks[0].IssueNumber != 17 || tasks[0].Title != "Issue" || tasks[0].State != engine.Queued {
		t.Fatalf("tasks %+v", tasks)
	}

	// Second round: the ETag matches, nothing new.
	if err := p.Round(ctx); err != nil {
		t.Fatal(err)
	}
	if gh.gotETag[1] != `"e1"` {
		t.Fatalf("second poll should send the stored ETag, sent %q", gh.gotETag[1])
	}

	// Restart: a new store handle on the same file and a feed that changed
	// its ETag but still lists the same event. No duplicate task.
	p.Store.Close()
	st, err := store.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	gh.etag = `"e2"`
	p.Store = st
	if err := p.Round(ctx); err != nil {
		t.Fatal(err)
	}
	if tasks, _ := st.ListTasks(ctx); len(tasks) != 1 {
		t.Fatalf("restart created duplicates: %d tasks", len(tasks))
	}

	// Relabeling the same open issue (a new event id) does not add a task.
	gh.events = append(gh.events, labeled(2, 17, "denizekinci"))
	gh.etag = `"e3"`
	if err := p.Round(ctx); err != nil {
		t.Fatal(err)
	}
	if tasks, _ := st.ListTasks(ctx); len(tasks) != 1 {
		t.Fatalf("relabel created a second task: %d", len(tasks))
	}
}

func TestIgnoredEvents(t *testing.T) {
	pr := labeled(5, 21, "denizekinci")
	pr.Issue.PullRequest = &struct{}{}
	other := labeled(6, 22, "denizekinci")
	other.Label = &github.Label{Name: "bug"}
	gh := &fakeGH{
		events: []github.IssueEvent{
			labeled(3, 18, "mallory"),     // someone else added the label
			labeled(4, 19, "denizekinci"), // issue written by someone else
			pr,                            // a pull request, not an issue
			other,                         // another label
			{ID: 7, Event: "unlabeled", Actor: github.User{Login: "denizekinci"}, Label: &github.Label{Name: "agent:ready"}, Issue: github.Issue{Number: 23}},
			labeled(8, 24, "DenizEkinci"), // logins are case-insensitive; issue closed since
		},
		etag: `"x"`,
		issues: map[int]github.Issue{
			19: openIssue(19, "mallory"),
			24: func() github.Issue { is := openIssue(24, "denizekinci"); is.State = "closed"; return is }(),
		},
	}
	p, _ := newPoller(t, gh)
	ctx := context.Background()
	if err := p.Round(ctx); err != nil {
		t.Fatal(err)
	}
	if tasks, _ := p.Store.ListTasks(ctx); len(tasks) != 0 {
		t.Fatalf("no task expected, got %+v", tasks)
	}
	if pending, _ := p.Store.PendingInbox(ctx, 10); len(pending) != 0 {
		t.Fatalf("rejected events must be marked processed, %d pending", len(pending))
	}
}

func TestTransientErrorRetries(t *testing.T) {
	gh := &fakeGH{
		events: []github.IssueEvent{labeled(1, 17, "denizekinci")},
		etag:   `"e1"`,
		issues: map[int]github.Issue{17: openIssue(17, "denizekinci")},
		getErr: &github.RateLimitError{Reset: time.Now().Add(time.Minute)},
	}
	p, _ := newPoller(t, gh)
	ctx := context.Background()
	var rl *github.RateLimitError
	if err := p.Round(ctx); !errors.As(err, &rl) {
		t.Fatalf("want rate limit error, got %v", err)
	}
	if pending, _ := p.Store.PendingInbox(ctx, 10); len(pending) != 1 {
		t.Fatalf("event should stay pending, %d", len(pending))
	}
	gh.getErr = nil
	if err := p.Round(ctx); err != nil {
		t.Fatal(err)
	}
	if tasks, _ := p.Store.ListTasks(ctx); len(tasks) != 1 {
		t.Fatalf("retry should create the task, got %d", len(tasks))
	}
}

func TestRunStopsOnCancel(t *testing.T) {
	p, _ := newPoller(t, &fakeGH{etag: `"e"`})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error)
	go func() { done <- p.Run(ctx) }()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop")
	}
}
