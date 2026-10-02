package telegram

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/deniz-ai-orchestration/ai-orchestrator/internal/config"
	"github.com/deniz-ai-orchestration/ai-orchestrator/internal/engine"
	"github.com/deniz-ai-orchestration/ai-orchestrator/internal/quota"
	"github.com/deniz-ai-orchestration/ai-orchestrator/internal/store"
)

const me = 4242

type sent struct {
	chat    int64
	text    string
	buttons [][]Button
	silent  bool
}

type fakeAPI struct {
	updates  [][]Update
	offsets  []int64
	sent     []sent
	answered []string
	sendErr  error
}

func (f *fakeAPI) GetUpdates(_ context.Context, offset int64, _ time.Duration) ([]Update, error) {
	f.offsets = append(f.offsets, offset)
	if len(f.updates) == 0 {
		return nil, nil
	}
	u := f.updates[0]
	f.updates = f.updates[1:]
	return u, nil
}

func (f *fakeAPI) Send(_ context.Context, chat int64, text string, buttons [][]Button, silent bool) error {
	if f.sendErr != nil {
		return f.sendErr
	}
	f.sent = append(f.sent, sent{chat, text, buttons, silent})
	return nil
}

func (f *fakeAPI) AnswerCallback(_ context.Context, _ string, text string) error {
	f.answered = append(f.answered, text)
	return nil
}

type fakeStopper struct{ stopped []int64 }

func (f *fakeStopper) Stop(id int64) bool { f.stopped = append(f.stopped, id); return true }

func setup(t *testing.T) (*Bot, *fakeAPI, *store.Store, store.Task) {
	t.Helper()
	cfg, err := config.Parse([]byte(`
github: { trusted_actor: d }
runner: { git_name: a, git_email: a@b }
telegram: { enabled: true, user_id: 4242 }
providers: { claude: { cli: claude }, codex: { cli: codex } }
models: { sonnet: { provider: claude, model: sonnet }, sol: { provider: codex, model: sol } }
roles: { developer: { pool: [sonnet, sol] }, reviewer: { pool: [sol, sonnet], not_same_as: developer } }
`))
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "orch.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	task, err := st.CreateTask(context.Background(), "o/r", 17, "Add /health", "b")
	if err != nil {
		t.Fatal(err)
	}
	api := &fakeAPI{}
	return &Bot{API: api, Store: st, Cfg: cfg, Quota: &quota.Tracker{Store: st, Cfg: cfg}, Agents: &fakeStopper{},
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))}, api, st, task
}

func apply(t *testing.T, st *store.Store, id int64, evs ...engine.Event) {
	t.Helper()
	for _, ev := range evs {
		if _, err := st.ApplyEvent(context.Background(), id, ev, config.Limits{CIAttempts: 3, ReviewCycles: 3, DevRuns: 8, MaxDiffLines: 800}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestNotificationsInOrderWithButtons(t *testing.T) {
	b, api, st, task := setup(t)
	ctx := context.Background()
	apply(t, st, task.ID,
		engine.Event{Kind: engine.EvDevStarted, Model: "sonnet"},
		engine.Event{Kind: engine.EvDevDone, Branch: "agent/17-x", PRNumber: 5, HeadSHA: "a"},
		engine.Event{Kind: engine.EvCIFailed, HeadSHA: "a", Detail: "FAIL TestX"},
		engine.Event{Kind: engine.EvDevStarted, Model: "sonnet"},
		engine.Event{Kind: engine.EvRunFailed, Reason: engine.ReasonTimeout, Detail: "killed after 45m"},
	)
	if err := b.NotifyOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if len(api.sent) != 3 {
		t.Fatalf("sent %d: %+v", len(api.sent), api.sent)
	}
	for i, want := range []string{"PR opened for task 1 (o/r#17)", "CI failed on PR #5", "task 1 (o/r#17) needs you: the agent ran past its timeout"} {
		if !strings.Contains(api.sent[i].text, want) {
			t.Errorf("message %d %q lacks %q", i, api.sent[i].text, want)
		}
		if api.sent[i].chat != me {
			t.Errorf("sent to %d", api.sent[i].chat)
		}
	}
	hold := api.sent[2]
	if hold.silent || !api.sent[0].silent || !strings.Contains(hold.text, "killed after 45m") || !strings.Contains(hold.text, "https://github.com/o/r/pull/5") {
		t.Fatalf("hold message %+v", hold)
	}
	if len(hold.buttons) != 1 || len(hold.buttons[0]) != 2 || !strings.HasPrefix(hold.buttons[0][0].CallbackData, "a:") {
		t.Fatalf("buttons %+v", hold.buttons)
	}
	if err := b.NotifyOnce(ctx); err != nil || len(api.sent) != 3 {
		t.Fatalf("resent: err %v, %d", err, len(api.sent))
	}

	// The Retry button resumes the task once.
	retry := hold.buttons[0][0].CallbackData
	press := func(id int64) Update {
		return Update{UpdateID: id, CallbackQuery: &CallbackQuery{ID: "q", From: User{ID: me},
			Message: &Message{Chat: Chat{ID: me}}, Data: retry}}
	}
	api.updates = [][]Update{{press(10), press(11)}}
	if err := b.PollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	got, _ := st.GetTask(ctx, task.ID)
	if got.State != engine.Queued || got.Work != engine.WorkCIFix {
		t.Fatalf("after retry: %+v", got.Task)
	}
	if len(api.answered) != 2 || api.answered[0] != "Done" || api.answered[1] != "That button was already used" {
		t.Fatalf("answers %v", api.answered)
	}
	if err := b.PollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if api.offsets[1] != 12 {
		t.Fatalf("offset not saved: %v", api.offsets)
	}
}

func TestSendFailureKeepsOrder(t *testing.T) {
	b, api, st, task := setup(t)
	apply(t, st, task.ID, engine.Event{Kind: engine.EvDevStarted, Model: "sonnet"},
		engine.Event{Kind: engine.EvDevDone, Branch: "agent/17-x", PRNumber: 5, HeadSHA: "a"})
	api.sendErr = &RetryError{After: time.Second}
	if err := b.NotifyOnce(context.Background()); err == nil {
		t.Fatal("expected the send error")
	}
	api.sendErr = nil
	if err := b.NotifyOnce(context.Background()); err != nil || len(api.sent) != 1 {
		t.Fatalf("retry: err %v sent %d", err, len(api.sent))
	}
}

func TestStrangersAreIgnored(t *testing.T) {
	b, api, st, task := setup(t)
	apply(t, st, task.ID, engine.Event{Kind: engine.EvDevStarted, Model: "sonnet"},
		engine.Event{Kind: engine.EvRunFailed, Reason: engine.ReasonTimeout})
	api.updates = [][]Update{{
		{UpdateID: 1, Message: &Message{From: &User{ID: 99}, Chat: Chat{ID: 99}, Text: "/retry 1"}},
		{UpdateID: 2, Message: &Message{From: &User{ID: me}, Chat: Chat{ID: -100, Type: "group"}, Text: "/retry 1"}},
		{UpdateID: 3, CallbackQuery: &CallbackQuery{ID: "x", From: User{ID: 99}, Data: "a:whatever"}},
	}}
	if err := b.PollOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(api.sent) != 0 || len(api.answered) != 0 {
		t.Fatalf("answered a stranger: %+v %+v", api.sent, api.answered)
	}
	if got, _ := st.GetTask(context.Background(), task.ID); got.State != engine.NeedsHuman {
		t.Fatalf("a stranger changed the task: %s", got.State)
	}
}

func TestCommands(t *testing.T) {
	b, _, st, task := setup(t)
	ctx := context.Background()
	apply(t, st, task.ID, engine.Event{Kind: engine.EvDevStarted, Model: "sonnet"})
	cases := []struct{ in, want string }{
		{"hello", "orch commands"},
		{"/status", "1  o/r#17  developing (sonnet, implement)"},
		{"/why 1", "dev_started → developing"},
		{"/why x", "is not a task number"},
		{"/why 99", "task 99"},
		{"/models", "reviewer: sol > sonnet"},
		{"/use reviewer sonnet", "reviewer now uses sonnet"},
		{"/models", "(pinned: sonnet)"},
		{"/use reviewer", "usage: /use"},
		{"/use reviewer ghost", `unknown model "ghost"`},
		{"/disable sol", "sol disabled."},
		{"/quota", "sol: off"},
		{"/enable sol", "sol enabled."},
		{"/pause", "Paused"},
		{"/status", "Paused: no new runs start."},
		{"/resume@orch_bot", "Resumed"},
		{"/retry 1", "not allowed in state developing"},
		{"/cancel 1", "Cancelled task 1 (o/r#17) and stopped its running agent."},
		{"/status", "No active tasks."},
		{"/nope", "Unknown command."},
	}
	for _, tc := range cases {
		if got := b.Command(ctx, tc.in); !strings.Contains(got, tc.want) {
			t.Errorf("%s: got %q, want %q", tc.in, got, tc.want)
		}
	}
	if s := b.Agents.(*fakeStopper).stopped; len(s) != 1 || s[0] != task.ID {
		t.Fatalf("stopped %v", s)
	}
}
