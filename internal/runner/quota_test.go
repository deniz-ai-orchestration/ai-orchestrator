package runner

import (
	"context"
	"strings"
	"testing"

	"github.com/deniz-ai-orchestration/ai-orchestrator/internal/engine"
	"github.com/deniz-ai-orchestration/ai-orchestrator/internal/quota"
)

const claudeRateLimited = `{"type":"rate_limit_event","rate_limit_info":{"status":"rejected","resetsAt":4102444800}}` + "\n" +
	`{"type":"result","subtype":"success","is_error":true,"terminal_reason":"api_error","result":"You've hit your limit"}` + "\n"

func TestRateLimitFallsBackToNextModel(t *testing.T) {
	e := newEnv(t, "")
	ctx := context.Background()
	e.dev.Quota = &quota.Tracker{Store: e.st, Cfg: e.dev.Cfg}
	r := e.dev.Cfg.Roles["developer"]
	r.Pool = []string{"sonnet", "sol"}
	e.dev.Cfg.Roles["developer"] = r

	e.agent.stdout = claudeRateLimited
	if err := e.dev.Once(ctx); err != nil {
		t.Fatal(err)
	}
	got := e.state(t)
	if got.State != engine.Queued || got.DevRuns != 0 || got.Reason != "" {
		t.Fatalf("rate-limited run must requeue without using a dev run: %+v", got.Task)
	}
	if !strings.Contains(e.lastDetail(t), "sonnet rate_limited") {
		t.Errorf("detail %q", e.lastDetail(t))
	}
	rep, _ := e.dev.Quota.Report(ctx)
	for _, s := range rep {
		if s.Name == "sonnet" && (s.Last == nil || s.Last.Kind != "rate_limit" || s.CoolingUntil.Year() != 2100) {
			t.Fatalf("sonnet status %+v", s)
		}
	}

	// Next round: sonnet is cooling down, so codex develops.
	e.agent.stdout = codexAnswer(map[string]string{"status": "blocked", "summary": "Which port?"})
	if err := e.dev.Once(ctx); err != nil {
		t.Fatal(err)
	}
	if got := e.state(t); got.DevModel != "sol" || got.Reason != engine.ReasonRequirementsUnclear {
		t.Fatalf("task %+v", got.Task)
	}
	if e.agent.specs[1].Argv[0] != "codex" {
		t.Fatalf("second run argv %v", e.agent.specs[1].Argv)
	}
}

func TestPinnedModelWins(t *testing.T) {
	e := newEnv(t, "")
	ctx := context.Background()
	e.dev.Quota = &quota.Tracker{Store: e.st, Cfg: e.dev.Cfg}
	if err := e.dev.Quota.Pin(ctx, "developer", "sol"); err != nil {
		t.Fatal(err)
	}
	e.agent.stdout = codexAnswer(map[string]string{"status": "blocked", "summary": "?"})
	if err := e.dev.Once(ctx); err != nil {
		t.Fatal(err)
	}
	if got := e.state(t); got.DevModel != "sol" {
		t.Fatalf("pin ignored: %+v", got.Task)
	}
}

func TestAllModelsCoolingLeavesTaskQueued(t *testing.T) {
	e := newEnv(t, "")
	ctx := context.Background()
	e.dev.Quota = &quota.Tracker{Store: e.st, Cfg: e.dev.Cfg}
	for _, m := range []string{"sonnet", "sol", "gem"} {
		if err := e.dev.Quota.SetEnabled(ctx, m, false); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.dev.Once(ctx); err != nil {
		t.Fatal(err)
	}
	if got := e.state(t); got.State != engine.Queued || len(e.agent.specs) != 0 {
		t.Fatalf("task %+v runs %d", got.Task, len(e.agent.specs))
	}
}
