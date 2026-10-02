// Package effects executes the side effects the engine queued in the
// outbox. Each executor handles some effect kinds and leaves the rest for
// the executors later milestones add (runs, notifications, cleanup).
package effects

import (
	"context"
	"log/slog"
	"time"

	"github.com/deniz-ai-orchestration/ai-orchestrator/internal/engine"
	"github.com/deniz-ai-orchestration/ai-orchestrator/internal/store"
)

// LabelSetter mirrors task state onto the issue for humans. Labels are
// never read back as truth.
type LabelSetter interface {
	SetStateLabel(ctx context.Context, repo string, number int, prefix, label string) error
}

// Labels executes set_label effects.
type Labels struct {
	Store    *store.Store
	GH       LabelSetter
	Log      *slog.Logger
	Interval time.Duration
}

// Run executes pending label effects until ctx is done.
func (l *Labels) Run(ctx context.Context) error {
	for {
		if err := l.Once(ctx); err != nil && ctx.Err() == nil {
			l.Log.Error("label effects failed", "err", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(l.Interval):
		}
	}
}

// Once executes every pending label effect once, in order.
func (l *Labels) Once(ctx context.Context) error {
	items, err := l.Store.PendingEffectsOf(ctx, 50, engine.EffSetLabel)
	if err != nil {
		return err
	}
	for _, it := range items {
		t, err := l.Store.GetTask(ctx, it.TaskID)
		if err != nil {
			return err
		}
		if err := l.GH.SetStateLabel(ctx, t.Repo, t.IssueNumber, "orch:", it.Effect.Arg); err != nil {
			l.Log.Warn("set label failed", "task", t.ID, "label", it.Effect.Arg, "attempt", it.Attempts+1, "err", err)
			if ferr := l.Store.FailEffect(ctx, it.ID, err); ferr != nil {
				return ferr
			}
			return nil // keep order: later labels wait for this one
		}
		if err := l.Store.CompleteEffect(ctx, it.ID); err != nil {
			return err
		}
	}
	return nil
}
