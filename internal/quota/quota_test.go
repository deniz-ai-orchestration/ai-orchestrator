package quota

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/deniz-ai-orchestration/ai-orchestrator/internal/config"
	"github.com/deniz-ai-orchestration/ai-orchestrator/internal/provider"
	"github.com/deniz-ai-orchestration/ai-orchestrator/internal/store"
)

func setup(t *testing.T) (*Tracker, *store.Store, *time.Time) {
	t.Helper()
	cfg, err := config.Parse([]byte(`
github: { trusted_actor: d }
runner: { git_name: a, git_email: a@b }
providers: { claude: { cli: claude }, codex: { cli: codex } }
models:
  sonnet: { provider: claude, model: sonnet, max_runs_per_5h: 2 }
  opus:   { provider: claude, model: opus }
  sol:    { provider: codex, model: sol }
roles: { developer: { pool: [sonnet, opus, sol] }, reviewer: { pool: [sol] } }
`))
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "orch.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	now := time.Now().UTC().Truncate(time.Second)
	return &Tracker{Store: st, Cfg: cfg, Now: func() time.Time { return now }}, st, &now
}

func skip(t *testing.T, tr *Tracker, name string) string {
	t.Helper()
	f, err := tr.Skip(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return f(name)
}

func TestRateLimitBackoffAndReset(t *testing.T) {
	tr, st, now := setup(t)
	ctx := context.Background()
	// No reset time: 1h, then 2h, then 4h, then 4h.
	for i, want := range []time.Duration{time.Hour, 2 * time.Hour, 4 * time.Hour, 4 * time.Hour} {
		if err := tr.Record(ctx, "opus", 0, provider.Outcome{Kind: provider.RateLimited, Detail: "429"}); err != nil {
			t.Fatal(err)
		}
		s, _ := st.ModelStates(ctx)
		if got := s["opus"].CooldownUntil.Sub(*now); got != want+time.Minute {
			t.Fatalf("round %d: cooldown %s, want %s", i, got, want+time.Minute)
		}
	}
	if r := skip(t, tr, "opus"); !strings.HasPrefix(r, "cooling down until") {
		t.Fatalf("skip %q", r)
	}
	// A success resets the backoff step; the reported reset time wins.
	if err := tr.Record(ctx, "opus", 0, provider.Outcome{Kind: provider.OK}); err != nil {
		t.Fatal(err)
	}
	reset := now.Add(37 * time.Minute)
	if err := tr.Record(ctx, "opus", 0, provider.Outcome{Kind: provider.RateLimited, ResetAt: reset}); err != nil {
		t.Fatal(err)
	}
	s, _ := st.ModelStates(ctx)
	if !s["opus"].CooldownUntil.Equal(reset.Add(time.Minute)) || s["opus"].CooldownStep != 0 {
		t.Fatalf("state %+v", s["opus"])
	}
	*now = reset.Add(2 * time.Minute)
	if r := skip(t, tr, "opus"); r != "" {
		t.Fatalf("cooldown should be over: %q", r)
	}
}

func TestOverloadAndAuth(t *testing.T) {
	tr, st, now := setup(t)
	ctx := context.Background()
	if err := tr.Record(ctx, "sol", 0, provider.Outcome{Kind: provider.Unavailable}); err != nil {
		t.Fatal(err)
	}
	s, _ := st.ModelStates(ctx)
	if s["sol"].CooldownUntil.Sub(*now) != CapacityPause {
		t.Fatalf("overload pause %s", s["sol"].CooldownUntil.Sub(*now))
	}
	if err := tr.Record(ctx, "sonnet", 0, provider.Outcome{Kind: provider.AuthFailed, Detail: "Not logged in"}); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{"sonnet": "switched off", "opus": "switched off"} {
		if r := skip(t, tr, name); r != want {
			t.Errorf("%s: %q", name, r)
		}
	}
	if err := tr.SetEnabled(ctx, "opus", true); err != nil {
		t.Fatal(err)
	}
	if r := skip(t, tr, "opus"); r != "" {
		t.Fatalf("re-enabled opus: %q", r)
	}
	if err := tr.SetEnabled(ctx, "ghost", true); err == nil {
		t.Fatal("unknown model enabled")
	}
}

func TestBudgetPinsAndReport(t *testing.T) {
	tr, st, now := setup(t)
	ctx := context.Background()
	task, err := st.CreateTask(ctx, "o/r", 1, "t", "b")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, err := st.StartRun(ctx, store.Run{TaskID: task.ID, Role: "developer", Provider: "claude", Model: "sonnet"}); err != nil {
			t.Fatal(err)
		}
	}
	if r := skip(t, tr, "sonnet"); r != "budget used (2 of 2 runs in 5h)" {
		t.Fatalf("skip %q", r)
	}
	if err := tr.Pin(ctx, "reviewer", "opus"); err != nil {
		t.Fatal(err)
	}
	for _, bad := range [][2]string{{"nope", "opus"}, {"reviewer", "ghost"}} {
		if err := tr.Pin(ctx, bad[0], bad[1]); err == nil {
			t.Errorf("pin %v accepted", bad)
		}
	}
	if err := tr.Record(ctx, "sol", 0, provider.Outcome{Kind: provider.RateLimited, Detail: "weekly limit"}); err != nil {
		t.Fatal(err)
	}
	rep, err := tr.Report(ctx)
	if err != nil {
		t.Fatal(err)
	}
	by := map[string]Status{}
	for _, s := range rep {
		by[s.Name] = s
	}
	if len(rep) != 3 || rep[0].Name != "opus" {
		t.Fatalf("report order %+v", rep)
	}
	if s := by["sonnet"]; s.State(*now) != "budget used" || s.RunsIn5h != 2 || s.Budget != 2 {
		t.Errorf("sonnet %+v", s)
	}
	if s := by["opus"]; s.State(*now) != "ready" || len(s.PinnedFor) != 1 || s.PinnedFor[0] != "reviewer" {
		t.Errorf("opus %+v", s)
	}
	if s := by["sol"]; s.State(*now) != "cooling" || s.Last == nil || s.Last.Detail != "weekly limit" {
		t.Errorf("sol %+v", s)
	}
	if err := tr.Pin(ctx, "reviewer", "auto"); err != nil {
		t.Fatal(err)
	}
	if pins, _ := st.RolePins(ctx); len(pins) != 0 {
		t.Fatalf("auto must clear the pin: %v", pins)
	}
}
