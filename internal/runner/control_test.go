package runner

import (
	"context"
	"testing"

	"github.com/deniz-ai-orchestration/ai-orchestrator/internal/engine"
	"github.com/deniz-ai-orchestration/ai-orchestrator/internal/store"
)

func TestStopKillsTheRunningContainer(t *testing.T) {
	for _, cancelFirst := range []bool{false, true} {
		e := newEnv(t, "")
		ctx := context.Background()
		e.agent.started = make(chan struct{})
		done := make(chan error, 1)
		go func() { done <- e.dev.Once(ctx) }()
		<-e.agent.started
		if e.dev.Stop(e.task.ID + 1) {
			t.Fatal("stopped another task's run")
		}
		if cancelFirst {
			if _, err := e.st.ApplyEvent(ctx, e.task.ID, engine.Event{Kind: engine.EvCancel}, e.dev.Cfg.Limits); err != nil {
				t.Fatal(err)
			}
		}
		if !e.dev.Stop(e.task.ID) {
			t.Fatal("Stop found no run")
		}
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		got := e.state(t)
		want := engine.NeedsHuman
		if cancelFirst {
			want = engine.Cancelled
		}
		if got.State != want {
			t.Fatalf("cancelFirst=%v: state %s", cancelFirst, got.State)
		}
		if !cancelFirst && got.Reason != engine.ReasonCLIError {
			t.Fatalf("reason %s", got.Reason)
		}
		runs, _ := e.st.RunsForOutbox(ctx, 2)
		if len(runs) != 1 || runs[0].Status != store.RunFailed || runs[0].Outcome != "stopped" {
			t.Fatalf("runs %+v", runs)
		}
		if left, _ := e.st.PendingEffectsOf(ctx, 5, engine.EffStartDevRun); len(left) != 0 {
			t.Fatalf("effect left pending: %+v", left)
		}
		if e.dev.Stop(e.task.ID) {
			t.Fatal("Stop after the run ended")
		}
	}
}

func TestPauseStopsNewRuns(t *testing.T) {
	e := newEnv(t, "")
	ctx := context.Background()
	if err := e.st.SetPaused(ctx, true); err != nil {
		t.Fatal(err)
	}
	e.agent.stdout = claudeResult("blocked", "?")
	if err := e.dev.Once(ctx); err != nil {
		t.Fatal(err)
	}
	if got := e.state(t); got.State != engine.Queued || len(e.agent.specs) != 0 {
		t.Fatalf("paused orch started a run: %+v", got.Task)
	}
	if err := e.st.SetPaused(ctx, false); err != nil {
		t.Fatal(err)
	}
	if err := e.dev.Once(ctx); err != nil {
		t.Fatal(err)
	}
	if len(e.agent.specs) != 1 {
		t.Fatal("resume did not start the run")
	}
}
