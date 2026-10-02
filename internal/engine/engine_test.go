package engine

import (
	"errors"
	"testing"
	"time"

	"github.com/deniz-ai-orchestration/ai-orchestrator/internal/config"
)

var (
	limits = config.Limits{CIAttempts: 3, ReviewCycles: 3, DevRuns: 8, MaxDiffLines: 800}
	t0     = time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC)
)

// step applies ev and fails the test on error.
func step(t *testing.T, task Task, ev Event) Result {
	t.Helper()
	if ev.At.IsZero() {
		ev.At = t0
	}
	r, err := Apply(task, ev, limits)
	if err != nil {
		t.Fatalf("%s in %s: %v", ev.Kind, task.State, err)
	}
	if r.Transition.From != task.State || r.Transition.To != r.Task.State || r.Transition.Event != ev.Kind {
		t.Fatalf("transition record %+v does not match %s -> %s", r.Transition, task.State, r.Task.State)
	}
	return r
}

func mustFail(t *testing.T, task Task, ev Event) {
	t.Helper()
	_, err := Apply(task, ev, limits)
	var inv *ErrInvalid
	if !errors.As(err, &inv) {
		t.Fatalf("%s in %s: want ErrInvalid, got %v", ev.Kind, task.State, err)
	}
}

func hasEffect(r Result, k EffectKind, arg string) bool {
	for _, e := range r.Effects {
		if e.Kind == k && e.Arg == arg {
			return true
		}
	}
	return false
}

func devDoneEv(sha string) Event {
	return Event{Kind: EvDevDone, Branch: "agent/17-x", PRNumber: 5, HeadSHA: sha}
}

// at drives a new task to the given state along the happy path.
func at(t *testing.T, s State) Task {
	t.Helper()
	task := NewTask("o/r", 17, t0)
	path := []Event{
		{Kind: EvDevStarted, Model: "claude-sonnet"},
		devDoneEv("aaa"),
		{Kind: EvCIPassed, HeadSHA: "aaa"},
		{Kind: EvReviewStarted, Model: "codex-sol"},
		{Kind: EvReviewApproved},
	}
	for _, ev := range path {
		if task.State == s {
			return task
		}
		task = step(t, task, ev).Task
	}
	if task.State != s {
		t.Fatalf("cannot reach %s on the happy path", s)
	}
	return task
}

func TestHappyPath(t *testing.T) {
	task := NewTask("o/r", 17, t0)
	r := step(t, task, Event{Kind: EvDevStarted, Model: "claude-sonnet"})
	if r.Task.State != Developing || r.Task.DevRuns != 1 || !hasEffect(r, EffStartDevRun, "implement") {
		t.Fatalf("dev start: %+v %+v", r.Task, r.Effects)
	}
	r = step(t, r.Task, devDoneEv("aaa"))
	if r.Task.State != AwaitingCI || r.Task.PRNumber != 5 || !hasEffect(r, EffNotify, "pr_opened") {
		t.Fatalf("dev done: %+v %+v", r.Task, r.Effects)
	}
	r = step(t, r.Task, Event{Kind: EvCIPassed, HeadSHA: "aaa"})
	if r.Task.State != ReviewQueued {
		t.Fatal(r.Task.State)
	}
	r = step(t, r.Task, Event{Kind: EvReviewStarted, Model: "codex-sol"})
	if r.Task.State != Reviewing || !hasEffect(r, EffStartReviewRun, "") {
		t.Fatal(r.Task.State)
	}
	r = step(t, r.Task, Event{Kind: EvReviewApproved})
	if r.Task.State != ReadyForHuman || !hasEffect(r, EffNotify, "ready_for_review") {
		t.Fatal(r.Task.State)
	}
	r = step(t, r.Task, Event{Kind: EvPRMerged})
	if r.Task.State != Done || !hasEffect(r, EffCleanup, "") {
		t.Fatal(r.Task.State)
	}
	mustFail(t, r.Task, Event{Kind: EvRetry})
}

func TestApplyDoesNotMutateInput(t *testing.T) {
	task := NewTask("o/r", 1, t0)
	_ = step(t, task, Event{Kind: EvDevStarted})
	if task.State != Queued || task.DevRuns != 0 {
		t.Fatalf("input mutated: %+v", task)
	}
}

func TestCILimit(t *testing.T) {
	task := at(t, AwaitingCI)
	sha := "aaa"
	for i := 1; i <= limits.CIAttempts; i++ {
		r := step(t, task, Event{Kind: EvCIFailed, HeadSHA: sha})
		if r.Task.State != Queued || r.Task.Work != WorkCIFix || r.Task.CIAttempts != i {
			t.Fatalf("attempt %d: %+v", i, r.Task)
		}
		task = step(t, r.Task, Event{Kind: EvDevStarted}).Task
		sha = sha + "x"
		r = step(t, task, devDoneEv(sha))
		if !hasEffect(r, EffNotify, "pr_updated") {
			t.Fatal("fix push should notify pr_updated")
		}
		task = r.Task
	}
	r := step(t, task, Event{Kind: EvCIFailed, HeadSHA: sha})
	if r.Task.State != NeedsHuman || r.Task.Reason != ReasonCIRetriesExhausted {
		t.Fatalf("after limit: %+v", r.Task)
	}

	// /retry grants exactly one more fix run.
	r = step(t, r.Task, Event{Kind: EvRetry})
	if r.Task.State != Queued || r.Task.Work != WorkCIFix || r.Task.Reason != ReasonNone {
		t.Fatalf("retry: %+v", r.Task)
	}
	task = step(t, r.Task, Event{Kind: EvDevStarted}).Task
	task = step(t, task, devDoneEv("zzz")).Task
	r = step(t, task, Event{Kind: EvCIFailed, HeadSHA: "zzz"})
	if r.Task.State != NeedsHuman || r.Task.Reason != ReasonCIRetriesExhausted {
		t.Fatalf("second failure after retry should hold: %+v", r.Task)
	}
}

func TestReviewLimit(t *testing.T) {
	task := at(t, Reviewing)
	for i := 1; i <= limits.ReviewCycles; i++ {
		r := step(t, task, Event{Kind: EvChangesRequested})
		if r.Task.State != Queued || r.Task.Work != WorkReviewFix || r.Task.ReviewCycles != i {
			t.Fatalf("cycle %d: %+v", i, r.Task)
		}
		task = step(t, r.Task, Event{Kind: EvDevStarted, Model: "claude-sonnet"}).Task
		sha := string(rune('b' + i))
		task = step(t, task, devDoneEv(sha)).Task
		task = step(t, task, Event{Kind: EvCIPassed, HeadSHA: sha}).Task
		task = step(t, task, Event{Kind: EvReviewStarted, Model: "codex-sol"}).Task
	}
	r := step(t, task, Event{Kind: EvChangesRequested})
	if r.Task.State != NeedsHuman || r.Task.Reason != ReasonReviewCyclesExhausted || r.Task.ResumeWork != WorkReviewFix {
		t.Fatalf("after limit: %+v", r.Task)
	}
}

func TestDevRunsLimit(t *testing.T) {
	task := NewTask("o/r", 1, t0)
	task.DevRuns = limits.DevRuns
	r := step(t, task, Event{Kind: EvDevStarted})
	if r.Task.State != NeedsHuman || r.Task.Reason != ReasonDevRunsExhausted || r.Task.DevRuns != limits.DevRuns {
		t.Fatalf("%+v", r.Task)
	}
	r = step(t, r.Task, Event{Kind: EvRetry})
	if r.Task.State != Queued || r.Task.DevRuns != limits.DevRuns-1 {
		t.Fatalf("retry: %+v", r.Task)
	}
	r = step(t, r.Task, Event{Kind: EvDevStarted})
	if r.Task.State != Developing || r.Task.DevRuns != limits.DevRuns {
		t.Fatalf("granted run: %+v", r.Task)
	}
}

// Every loop must go through a dev run, so dev_runs bounds the total even if
// CI and review alternate forever.
func TestNoUnboundedLoop(t *testing.T) {
	task := at(t, AwaitingCI)
	for n := 0; n < 100 && task.State != NeedsHuman; n++ {
		var ev Event
		switch task.State {
		case AwaitingCI:
			if n%2 == 0 {
				ev = Event{Kind: EvCIFailed, HeadSHA: task.HeadSHA}
			} else {
				ev = Event{Kind: EvCIPassed, HeadSHA: task.HeadSHA}
			}
		case ReviewQueued:
			ev = Event{Kind: EvReviewStarted, Model: "codex-sol"}
		case Reviewing:
			ev = Event{Kind: EvChangesRequested}
		case Queued:
			ev = Event{Kind: EvDevStarted, Model: "claude-sonnet"}
		case Developing:
			ev = devDoneEv(task.HeadSHA + "1")
		}
		task = step(t, task, ev).Task
	}
	if task.State != NeedsHuman {
		t.Fatalf("loop never stopped: %+v", task)
	}
	if task.DevRuns > limits.DevRuns || task.CIAttempts > limits.CIAttempts || task.ReviewCycles > limits.ReviewCycles {
		t.Fatalf("counter over limit: %+v", task)
	}
}

func TestHolds(t *testing.T) {
	cases := []struct {
		name   string
		from   State
		ev     Event
		reason Reason
		resume State
	}{
		{"dev blocked", Developing, Event{Kind: EvDevBlocked, Detail: "which API?"}, ReasonRequirementsUnclear, Queued},
		{"dev run failed", Developing, Event{Kind: EvRunFailed, Reason: ReasonTimeout}, ReasonTimeout, Queued},
		{"push rejected in dev", Developing, Event{Kind: EvPushRejected}, ReasonScopeViolation, Queued},
		{"push rejected in ci", AwaitingCI, Event{Kind: EvPushRejected}, ReasonScopeViolation, AwaitingCI},
		{"review run failed", Reviewing, Event{Kind: EvRunFailed, Reason: ReasonProviderUnavailable}, ReasonProviderUnavailable, ReviewQueued},
		{"review escalated", Reviewing, Event{Kind: EvReviewEscalated, Reason: ReasonSecuritySensitive}, ReasonSecuritySensitive, ReviewQueued},
		{"review escalated default", Reviewing, Event{Kind: EvReviewEscalated}, ReasonRequirementsUnclear, ReviewQueued},
		{"head changed in ci", AwaitingCI, Event{Kind: EvHeadChanged, HeadSHA: "ext"}, ReasonHeadChanged, AwaitingCI},
		{"head changed in review queue", ReviewQueued, Event{Kind: EvHeadChanged, HeadSHA: "ext"}, ReasonHeadChanged, AwaitingCI},
		{"head changed in review", Reviewing, Event{Kind: EvHeadChanged, HeadSHA: "ext"}, ReasonHeadChanged, AwaitingCI},
		{"head changed when ready", ReadyForHuman, Event{Kind: EvHeadChanged, HeadSHA: "ext"}, ReasonHeadChanged, AwaitingCI},
		{"merge conflict queued", Queued, Event{Kind: EvMergeConflict}, ReasonMergeConflict, Queued},
		{"merge conflict when ready", ReadyForHuman, Event{Kind: EvMergeConflict}, ReasonMergeConflict, ReadyForHuman},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := step(t, at(t, tc.from), tc.ev)
			if r.Task.State != NeedsHuman || r.Task.Reason != tc.reason || r.Task.ResumeState != tc.resume {
				t.Fatalf("got state=%s reason=%s resume=%s", r.Task.State, r.Task.Reason, r.Task.ResumeState)
			}
			if r.Transition.Reason != tc.reason || r.Transition.Detail != tc.ev.Detail {
				t.Fatalf("transition %+v", r.Transition)
			}
			if !hasEffect(r, EffSetLabel, "orch:needs-human") || !hasEffect(r, EffNotify, "needs_human") {
				t.Fatalf("effects %+v", r.Effects)
			}
			back := step(t, r.Task, Event{Kind: EvRetry})
			if back.Task.State != tc.resume || back.Task.Reason != ReasonNone || back.Task.ResumeState != "" {
				t.Fatalf("retry: %+v", back.Task)
			}
		})
	}
}

func TestTerminalFromEveryState(t *testing.T) {
	for _, s := range []State{Queued, Developing, AwaitingCI, ReviewQueued, Reviewing, ReadyForHuman} {
		for kind, want := range map[EventKind]State{EvPRMerged: Done, EvPRClosed: Rejected, EvCancel: Cancelled} {
			r := step(t, at(t, s), Event{Kind: kind})
			if r.Task.State != want || !hasEffect(r, EffCleanup, "") {
				t.Errorf("%s in %s: got %s", kind, s, r.Task.State)
			}
			for _, ev := range []EventKind{EvRetry, EvCancel, EvPRMerged, EvDevStarted} {
				mustFail(t, r.Task, Event{Kind: ev})
			}
		}
	}
	held := step(t, at(t, Developing), Event{Kind: EvDevBlocked}).Task
	if got := step(t, held, Event{Kind: EvCancel}).Task.State; got != Cancelled {
		t.Errorf("cancel from needs_human: %s", got)
	}
	mustFail(t, held, Event{Kind: EvMergeConflict})
}

func TestInvalidEvents(t *testing.T) {
	cases := []struct {
		from State
		ev   Event
	}{
		{Queued, Event{Kind: EvDevDone}},
		{Queued, Event{Kind: EvCIPassed}},
		{Queued, Event{Kind: EvRetry}},
		{Developing, Event{Kind: EvDevStarted}},
		{Developing, devDoneEv("")},                                     // missing head SHA
		{Developing, Event{Kind: EvDevDone, HeadSHA: "a", PRNumber: 1}}, // missing branch
		{Developing, Event{Kind: EvRunFailed, Reason: "boom"}},          // not a fixed reason
		{Developing, Event{Kind: EvRunFailed}},                          // no reason
		{AwaitingCI, Event{Kind: EvCIPassed, HeadSHA: "old"}},           // stale SHA
		{AwaitingCI, Event{Kind: EvCIFailed, HeadSHA: "old"}},
		{AwaitingCI, Event{Kind: EvHeadChanged, HeadSHA: "aaa"}}, // same head
		{AwaitingCI, Event{Kind: EvReviewApproved}},
		{ReviewQueued, Event{Kind: EvReviewStarted, Model: "claude-sonnet"}}, // reviewer == developer model
		{ReviewQueued, Event{Kind: EvReviewApproved}},
		{Reviewing, Event{Kind: EvReviewEscalated, Reason: "nope"}},
		{Reviewing, Event{Kind: EvCIPassed, HeadSHA: "aaa"}},
		{ReadyForHuman, Event{Kind: EvReviewApproved}},
		{ReadyForHuman, Event{Kind: EvRetry}},
	}
	for _, tc := range cases {
		mustFail(t, at(t, tc.from), tc.ev)
	}
}

func TestReasonsValid(t *testing.T) {
	if !ReasonHeadChanged.Valid() || ReasonNone.Valid() || Reason("x").Valid() {
		t.Fatal("reason validation wrong")
	}
}
