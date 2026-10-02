package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/deniz-ai-orchestration/ai-orchestrator/internal/config"
	"github.com/deniz-ai-orchestration/ai-orchestrator/internal/engine"
)

var limits = config.Limits{CIAttempts: 3, ReviewCycles: 3, DevRuns: 8, MaxDiffLines: 800}

func open(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "orch.db")
	s, err := Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, path
}

func TestOpenIsIdempotentAndWAL(t *testing.T) {
	s, path := open(t)
	var mode string
	if err := s.db.QueryRow(`PRAGMA journal_mode`).Scan(&mode); err != nil || mode != "wal" {
		t.Fatalf("journal_mode = %q, %v", mode, err)
	}
	s.Close()
	s2, err := Open(context.Background(), path) // re-running migrations is a no-op
	if err != nil {
		t.Fatal(err)
	}
	s2.Close()
}

func TestCreateAndGetTask(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	created, err := s.CreateTask(ctx, "o/r", 17, "Add login", "body")
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.GetTask(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != engine.Queued || got.Work != engine.WorkImplement || got.Title != "Add login" || got.IssueNumber != 17 {
		t.Fatalf("got %+v", got)
	}
	if _, err := s.GetTask(ctx, 999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing task: %v", err)
	}
}

func TestOneActiveTaskPerIssue(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	first, err := s.CreateTask(ctx, "o/r", 1, "t", "b")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateTask(ctx, "o/r", 1, "t", "b"); !errors.Is(err, ErrActiveTaskExists) {
		t.Fatalf("duplicate: %v", err)
	}
	if _, err := s.CreateTask(ctx, "o/other", 1, "t", "b"); err != nil {
		t.Fatalf("other repo: %v", err)
	}
	if _, err := s.ApplyEvent(ctx, first.ID, engine.Event{Kind: engine.EvCancel}, limits); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateTask(ctx, "o/r", 1, "t", "b"); err != nil {
		t.Fatalf("after cancel the issue can be picked up again: %v", err)
	}
}

func TestApplyEventPersistsTransitionAndOutbox(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	task, _ := s.CreateTask(ctx, "o/r", 1, "t", "b")

	events := []engine.Event{
		{Kind: engine.EvDevStarted, Model: "claude-sonnet"},
		{Kind: engine.EvDevDone, Branch: "agent/1-t", PRNumber: 4, HeadSHA: "abc"},
		{Kind: engine.EvCIFailed, HeadSHA: "abc", Detail: "lint"},
	}
	for _, ev := range events {
		if _, err := s.ApplyEvent(ctx, task.ID, ev, limits); err != nil {
			t.Fatal(err)
		}
	}
	got, _ := s.GetTask(ctx, task.ID)
	if got.State != engine.Queued || got.Work != engine.WorkCIFix || got.CIAttempts != 1 ||
		got.DevRuns != 1 || got.PRNumber != 4 || got.HeadSHA != "abc" || got.DevModel != "claude-sonnet" {
		t.Fatalf("task %+v", got)
	}

	trs, err := s.Transitions(ctx, task.ID)
	if err != nil || len(trs) != 3 {
		t.Fatalf("transitions %v %v", trs, err)
	}
	if trs[2].From != engine.AwaitingCI || trs[2].To != engine.Queued || trs[2].Detail != "lint" {
		t.Fatalf("last transition %+v", trs[2])
	}

	items, err := s.PendingEffects(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	// dev_started: set_label + start_dev_run; dev_done: notify; ci_failed: notify.
	if len(items) != 4 || items[1].Effect.Kind != engine.EffStartDevRun {
		t.Fatalf("outbox %+v", items)
	}
	if err := s.FailEffect(ctx, items[0].ID, errors.New("github 502")); err != nil {
		t.Fatal(err)
	}
	if err := s.CompleteEffect(ctx, items[0].ID); err != nil {
		t.Fatal(err)
	}
	if err := s.CompleteEffect(ctx, items[0].ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("completing twice: %v", err)
	}
	items, _ = s.PendingEffects(ctx, 100)
	if len(items) != 3 {
		t.Fatalf("pending after complete: %d", len(items))
	}
}

func TestInvalidEventWritesNothing(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	task, _ := s.CreateTask(ctx, "o/r", 1, "t", "b")
	_, err := s.ApplyEvent(ctx, task.ID, engine.Event{Kind: engine.EvCIPassed}, limits)
	var inv *engine.ErrInvalid
	if !errors.As(err, &inv) {
		t.Fatalf("want ErrInvalid, got %v", err)
	}
	trs, _ := s.Transitions(ctx, task.ID)
	items, _ := s.PendingEffects(ctx, 10)
	got, _ := s.GetTask(ctx, task.ID)
	if len(trs) != 0 || len(items) != 0 || got.State != engine.Queued {
		t.Fatalf("invalid event left traces: %d transitions, %d effects, state %s", len(trs), len(items), got.State)
	}
	if _, err := s.ApplyEvent(ctx, 999, engine.Event{Kind: engine.EvCancel}, limits); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing task: %v", err)
	}
}

func TestStateSurvivesReopen(t *testing.T) {
	s, path := open(t)
	ctx := context.Background()
	task, _ := s.CreateTask(ctx, "o/r", 1, "t", "b")
	if _, err := s.ApplyEvent(ctx, task.ID, engine.Event{Kind: engine.EvDevStarted}, limits); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s2, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	got, err := s2.GetTask(ctx, task.ID)
	if err != nil || got.State != engine.Developing {
		t.Fatalf("after reopen: %+v %v", got, err)
	}
	list, err := s2.ListTasks(ctx, engine.Developing, engine.Reviewing)
	if err != nil || len(list) != 1 {
		t.Fatalf("list: %v %v", list, err)
	}
	if list, _ := s2.ListTasks(ctx, engine.Done); len(list) != 0 {
		t.Fatalf("done list: %v", list)
	}
	if all, _ := s2.ListTasks(ctx); len(all) != 1 {
		t.Fatalf("all: %v", all)
	}
}
