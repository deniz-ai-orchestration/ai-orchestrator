package effects

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/deniz-ai-orchestration/ai-orchestrator/internal/config"
	"github.com/deniz-ai-orchestration/ai-orchestrator/internal/engine"
	"github.com/deniz-ai-orchestration/ai-orchestrator/internal/store"
)

type fakeLabels struct {
	set  []string
	fail error
}

func (f *fakeLabels) SetStateLabel(_ context.Context, repo string, n int, prefix, label string) error {
	if f.fail != nil {
		return f.fail
	}
	f.set = append(f.set, label)
	return nil
}

func TestLabelsMirrorStateInOrder(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "orch.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	limits := config.Limits{CIAttempts: 3, ReviewCycles: 3, DevRuns: 8, MaxDiffLines: 800}
	task, _ := st.CreateTask(ctx, "o/r", 1, "t", "b")
	for _, ev := range []engine.Event{
		{Kind: engine.EvDevStarted, Model: "claude-sonnet"},
		{Kind: engine.EvDevDone, Branch: "agent/1-t", PRNumber: 2, HeadSHA: "a"},
		{Kind: engine.EvCIPassed, HeadSHA: "a"},
	} {
		if _, err := st.ApplyEvent(ctx, task.ID, ev, limits); err != nil {
			t.Fatal(err)
		}
	}

	gh := &fakeLabels{fail: errors.New("502")}
	l := &Labels{Store: st, GH: gh, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if err := l.Once(ctx); err != nil {
		t.Fatal(err)
	}
	if left, _ := st.PendingEffectsOf(ctx, 10, engine.EffSetLabel); len(left) != 2 {
		t.Fatalf("failed label should stay pending with the next one behind it, %d pending", len(left))
	}

	gh.fail = nil
	if err := l.Once(ctx); err != nil {
		t.Fatal(err)
	}
	if len(gh.set) != 2 || gh.set[0] != "orch:dev" || gh.set[1] != "orch:review" {
		t.Fatalf("labels %v", gh.set)
	}
	if left, _ := st.PendingEffectsOf(ctx, 10, engine.EffSetLabel); len(left) != 0 {
		t.Fatalf("%d label effects left", len(left))
	}
	// Other effect kinds stay for their own executors.
	if left, _ := st.PendingEffects(ctx, 10); len(left) == 0 {
		t.Fatal("non-label effects should be untouched")
	}
}
