package store

import (
	"context"
	"testing"

	"github.com/deniz-ai-orchestration/ai-orchestrator/internal/engine"
)

func TestRunLifecycle(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	task, err := s.CreateTask(ctx, "o/r", 1, "t", "b")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ApplyEvent(ctx, task.ID, engine.Event{Kind: engine.EvDevStarted, Model: "m"}, limits); err != nil {
		t.Fatal(err)
	}
	items, err := s.PendingEffectsOf(ctx, 10, engine.EffStartDevRun)
	if err != nil || len(items) != 1 {
		t.Fatalf("items %v, err %v", items, err)
	}
	ob := items[0].ID

	a, err := s.StartRun(ctx, Run{TaskID: task.ID, OutboxID: ob, Role: "developer", Work: "implement", Provider: "claude", Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetRunContainer(ctx, a, "orch-run-1"); err != nil {
		t.Fatal(err)
	}
	if n, err := s.InterruptRunning(ctx); err != nil || n != 1 {
		t.Fatalf("interrupted %d, err %v", n, err)
	}
	b, err := s.StartRun(ctx, Run{TaskID: task.ID, OutboxID: ob, Role: "developer", Provider: "claude", Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.FinishRun(ctx, b, RunSucceeded, "ok", 0, 10, 20); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishRun(ctx, b, RunFailed, "x", 1, 0, 0); err != ErrNotFound {
		t.Fatalf("finishing twice: %v", err)
	}
	if err := s.AddArtifact(ctx, b, "stdout", "/x", 3); err != nil {
		t.Fatal(err)
	}
	runs, err := s.RunsForOutbox(ctx, ob)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 2 || runs[0].Status != RunInterrupted || runs[1].Status != RunSucceeded || runs[1].Outcome != "ok" {
		t.Fatalf("runs %+v", runs)
	}
}

func TestRecentAndGetRun(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	task, err := s.CreateTask(ctx, "o/r", 1, "t", "b")
	if err != nil {
		t.Fatal(err)
	}
	var ids []int64
	for _, role := range []string{"developer", "reviewer", "developer"} {
		id, err := s.StartRun(ctx, Run{TaskID: task.ID, Role: role, Provider: "claude", Model: "m"})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	if err := s.FinishRun(ctx, ids[0], RunSucceeded, "ok", 0, 0, 0); err != nil {
		t.Fatal(err)
	}
	runs, err := s.RecentRuns(ctx, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 2 || runs[0].ID != ids[2] || runs[1].Role != "reviewer" || runs[0].StartedAt.IsZero() || !runs[0].EndedAt.IsZero() {
		t.Fatalf("recent %+v", runs)
	}
	r, err := s.GetRun(ctx, ids[0])
	if err != nil || r.Status != RunSucceeded || r.EndedAt.IsZero() || r.TaskID != task.ID {
		t.Fatalf("get %+v, err %v", r, err)
	}
	if _, err := s.GetRun(ctx, 99); err != ErrNotFound {
		t.Fatalf("missing run: %v", err)
	}
}
