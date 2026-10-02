// Package engine is the task state machine.
//
// Apply is a pure function: it takes a task and an event and returns the
// updated task, the transition to record and the side effects to queue. The
// store persists all three in one transaction before any effect runs, so a
// crash never loses a transition or repeats one.
//
// Every loop is bounded: a developer run increments dev_runs, a CI failure
// increments ci_attempts, a change request increments review_cycles. When a
// limit is reached the task moves to needs_human instead of looping.
package engine

import (
	"fmt"
	"time"

	"github.com/deniz-ai-orchestration/ai-orchestrator/internal/config"
)

// State is a task's position in the workflow.
type State string

const (
	Queued        State = "queued"          // waiting for a developer run (Work says which kind)
	Developing    State = "developing"      // developer run in progress
	AwaitingCI    State = "awaiting_ci"     // pushed; waiting for CI on the head SHA
	ReviewQueued  State = "review_queued"   // CI green; waiting for a reviewer run
	Reviewing     State = "reviewing"       // reviewer run in progress
	ReadyForHuman State = "ready_for_human" // reviewer approved; Deniz reviews and merges
	NeedsHuman    State = "needs_human"     // on hold; Reason says why, ResumeState where /retry goes
	Done          State = "done"            // PR merged
	Rejected      State = "rejected"        // PR closed without merge
	Cancelled     State = "cancelled"       // /cancel
)

// Terminal reports whether no further transitions are possible.
func (s State) Terminal() bool { return s == Done || s == Rejected || s == Cancelled }

// Work says what the next developer run is for.
type Work string

const (
	WorkImplement Work = "implement"  // first run from the issue
	WorkCIFix     Work = "ci_fix"     // fix a red CI run
	WorkReviewFix Work = "review_fix" // address reviewer findings
)

// Reason is why a task is in needs_human. The list is fixed.
type Reason string

const (
	ReasonNone                  Reason = ""
	ReasonCIRetriesExhausted    Reason = "ci_retries_exhausted"
	ReasonReviewCyclesExhausted Reason = "review_cycles_exhausted"
	ReasonDevRunsExhausted      Reason = "dev_runs_exhausted"
	ReasonReviewersDisagree     Reason = "reviewers_disagree"
	ReasonMergeConflict         Reason = "merge_conflict"
	ReasonProviderUnavailable   Reason = "provider_unavailable"
	ReasonAuthFailed            Reason = "auth_failed"
	ReasonCLIError              Reason = "cli_error"
	ReasonTimeout               Reason = "timeout"
	ReasonBadOutput             Reason = "bad_output"
	ReasonScopeViolation        Reason = "scope_violation"
	ReasonSecuritySensitive     Reason = "security_sensitive"
	ReasonRequirementsUnclear   Reason = "requirements_unclear"
	ReasonHeadChanged           Reason = "head_changed_externally"
)

var validReasons = map[Reason]bool{
	ReasonCIRetriesExhausted: true, ReasonReviewCyclesExhausted: true, ReasonDevRunsExhausted: true,
	ReasonReviewersDisagree: true, ReasonMergeConflict: true, ReasonProviderUnavailable: true,
	ReasonAuthFailed: true, ReasonCLIError: true, ReasonTimeout: true, ReasonBadOutput: true,
	ReasonScopeViolation: true, ReasonSecuritySensitive: true, ReasonRequirementsUnclear: true,
	ReasonHeadChanged: true,
}

// Valid reports whether r is one of the fixed reasons.
func (r Reason) Valid() bool { return validReasons[r] }

// EventKind names what happened.
type EventKind string

const (
	EvDevStarted       EventKind = "dev_started"       // runner started a developer run
	EvDevDone          EventKind = "dev_done"          // developer pushed; carries Branch, PRNumber, HeadSHA
	EvDevBlocked       EventKind = "dev_blocked"       // developer returned blocked with a question
	EvRunFailed        EventKind = "run_failed"        // a run failed; carries Reason
	EvRunDeferred      EventKind = "run_deferred"      // the model hit a quota or capacity limit; try another model
	EvPushRejected     EventKind = "push_rejected"     // push validation failed (scope_violation)
	EvCIPassed         EventKind = "ci_passed"         // CI green on HeadSHA
	EvCIFailed         EventKind = "ci_failed"         // CI red on HeadSHA
	EvReviewStarted    EventKind = "review_started"    // runner started a reviewer run
	EvReviewApproved   EventKind = "review_approved"   // reviewer verdict: approve
	EvChangesRequested EventKind = "changes_requested" // reviewer verdict: blocker or major findings
	EvReviewEscalated  EventKind = "review_escalated"  // reviewer verdict: escalate; carries Reason
	EvHeadChanged      EventKind = "head_changed"      // someone else pushed to the branch
	EvMergeConflict    EventKind = "merge_conflict"    // PR no longer mergeable
	EvPRMerged         EventKind = "pr_merged"
	EvPRClosed         EventKind = "pr_closed"
	EvRetry            EventKind = "retry"  // /retry
	EvCancel           EventKind = "cancel" // /cancel
)

// Event is one input to the state machine.
type Event struct {
	Kind     EventKind
	Reason   Reason // EvRunFailed, EvReviewEscalated
	Detail   string // free text kept on the transition (question, summary)
	Branch   string // EvDevDone
	PRNumber int    // EvDevDone
	HeadSHA  string // EvDevDone, EvCIPassed, EvCIFailed, EvHeadChanged
	Model    string // EvDevStarted, EvReviewStarted
	At       time.Time
}

// Task is the workflow state of one issue.
type Task struct {
	ID           int64
	Repo         string
	IssueNumber  int
	State        State
	Work         Work
	Reason       Reason
	ResumeState  State
	ResumeWork   Work
	CIAttempts   int
	ReviewCycles int
	DevRuns      int
	Branch       string
	PRNumber     int
	HeadSHA      string
	DevModel     string // model of the last developer run; the reviewer must differ
	ReviewModel  string // model of the current or last reviewer run
	UpdatedAt    time.Time
}

// EffectKind names a side effect the engine asks for.
type EffectKind string

const (
	EffStartDevRun    EffectKind = "start_dev_run"
	EffStartReviewRun EffectKind = "start_review_run"
	EffNotify         EffectKind = "notify"
	EffSetLabel       EffectKind = "set_label"
	EffCleanup        EffectKind = "cleanup_workspaces"
)

// Effect is queued in the outbox and executed after the transition commits.
type Effect struct {
	Kind EffectKind
	Arg  string // label name, notification kind or run kind
}

// Transition is the audit record of one applied event.
type Transition struct {
	TaskID int64
	From   State
	To     State
	Event  EventKind
	Reason Reason
	Detail string
	At     time.Time
}

// Result is everything Apply produced.
type Result struct {
	Task       Task
	Transition Transition
	Effects    []Effect
}

// ErrInvalid is returned when an event does not apply in the task's state.
type ErrInvalid struct {
	State State
	Event EventKind
	Why   string
}

func (e *ErrInvalid) Error() string {
	if e.Why != "" {
		return fmt.Sprintf("event %s not allowed in state %s: %s", e.Event, e.State, e.Why)
	}
	return fmt.Sprintf("event %s not allowed in state %s", e.Event, e.State)
}

// NewTask returns a fresh task queued for its first developer run.
func NewTask(repo string, issue int, at time.Time) Task {
	return Task{Repo: repo, IssueNumber: issue, State: Queued, Work: WorkImplement, UpdatedAt: at}
}

type handler func(t *Task, ev Event, l config.Limits) ([]Effect, error)

// table lists every allowed (state, event) pair. Pairs not listed are invalid.
// Merge, close, cancel and conflict apply to all non-terminal states and are
// handled in Apply.
var table = map[State]map[EventKind]handler{
	Queued: {
		EvDevStarted: startDev,
	},
	Developing: {
		EvDevDone:      devDone,
		EvDevBlocked:   hold(ReasonRequirementsUnclear, Queued),
		EvRunFailed:    runFailed(Queued),
		EvRunDeferred:  devDeferred,
		EvPushRejected: hold(ReasonScopeViolation, Queued),
	},
	AwaitingCI: {
		EvCIPassed:     ciPassed,
		EvCIFailed:     ciFailed,
		EvPushRejected: hold(ReasonScopeViolation, AwaitingCI),
		EvHeadChanged:  headChanged,
	},
	ReviewQueued: {
		EvReviewStarted: startReview,
		EvHeadChanged:   headChanged,
	},
	Reviewing: {
		EvReviewApproved:   reviewApproved,
		EvChangesRequested: changesRequested,
		EvReviewEscalated:  reviewEscalated,
		EvRunFailed:        runFailed(ReviewQueued),
		EvRunDeferred:      reviewDeferred,
		EvHeadChanged:      headChanged,
	},
	ReadyForHuman: {
		EvHeadChanged: headChanged,
	},
	NeedsHuman: {
		EvRetry: retry,
	},
}

// Apply runs one event against a task. It never mutates its input.
func Apply(t Task, ev Event, l config.Limits) (Result, error) {
	if t.State.Terminal() {
		return Result{}, &ErrInvalid{State: t.State, Event: ev.Kind, Why: "task is finished"}
	}
	next := t
	next.UpdatedAt = ev.At
	var effects []Effect
	var err error

	switch ev.Kind {
	case EvPRMerged:
		next.State, next.Reason = Done, ReasonNone
		effects = []Effect{{EffNotify, "merged"}, {EffCleanup, ""}}
	case EvPRClosed:
		next.State, next.Reason = Rejected, ReasonNone
		effects = []Effect{{EffNotify, "rejected"}, {EffCleanup, ""}}
	case EvCancel:
		next.State, next.Reason = Cancelled, ReasonNone
		effects = []Effect{{EffNotify, "cancelled"}, {EffCleanup, ""}}
	case EvMergeConflict:
		if t.State == NeedsHuman {
			return Result{}, &ErrInvalid{State: t.State, Event: ev.Kind, Why: "already on hold"}
		}
		effects = toHold(&next, ReasonMergeConflict, t.State, t.Work)
	default:
		h, ok := table[t.State][ev.Kind]
		if !ok {
			return Result{}, &ErrInvalid{State: t.State, Event: ev.Kind}
		}
		effects, err = h(&next, ev, l)
		if err != nil {
			return Result{}, err
		}
	}

	return Result{
		Task:    next,
		Effects: effects,
		Transition: Transition{
			TaskID: t.ID, From: t.State, To: next.State, Event: ev.Kind,
			Reason: next.Reason, Detail: ev.Detail, At: ev.At,
		},
	}, nil
}

// toHold moves the task to needs_human, remembering where /retry resumes.
func toHold(t *Task, r Reason, resume State, work Work) []Effect {
	t.State, t.Reason, t.ResumeState, t.ResumeWork = NeedsHuman, r, resume, work
	return []Effect{{EffSetLabel, "orch:needs-human"}, {EffNotify, "needs_human"}}
}

func hold(r Reason, resume State) handler {
	return func(t *Task, _ Event, _ config.Limits) ([]Effect, error) {
		return toHold(t, r, resume, t.Work), nil
	}
}

func startDev(t *Task, ev Event, l config.Limits) ([]Effect, error) {
	if t.DevRuns >= l.DevRuns {
		return toHold(t, ReasonDevRunsExhausted, Queued, t.Work), nil
	}
	t.DevRuns++
	t.State = Developing
	t.DevModel = ev.Model
	return []Effect{{EffSetLabel, "orch:dev"}, {EffStartDevRun, string(t.Work)}}, nil
}

func devDone(t *Task, ev Event, _ config.Limits) ([]Effect, error) {
	if ev.HeadSHA == "" || ev.Branch == "" || ev.PRNumber <= 0 {
		return nil, &ErrInvalid{State: t.State, Event: ev.Kind, Why: "needs branch, PR number and head SHA"}
	}
	t.State, t.Branch, t.PRNumber, t.HeadSHA = AwaitingCI, ev.Branch, ev.PRNumber, ev.HeadSHA
	notify := "pr_updated"
	if t.Work == WorkImplement {
		notify = "pr_opened"
	}
	return []Effect{{EffNotify, notify}}, nil
}

func runFailed(resume State) handler {
	return func(t *Task, ev Event, _ config.Limits) ([]Effect, error) {
		if !ev.Reason.Valid() {
			return nil, &ErrInvalid{State: t.State, Event: ev.Kind, Why: fmt.Sprintf("unknown reason %q", ev.Reason)}
		}
		return toHold(t, ev.Reason, resume, t.Work), nil
	}
}

// devDeferred puts the task back in the queue when the model was out of
// quota. The run did no work, so it does not count against dev_runs.
func devDeferred(t *Task, _ Event, _ config.Limits) ([]Effect, error) {
	t.State = Queued
	if t.DevRuns > 0 {
		t.DevRuns--
	}
	return nil, nil
}

// reviewDeferred puts the task back in the review queue for another model.
func reviewDeferred(t *Task, _ Event, _ config.Limits) ([]Effect, error) {
	t.State, t.ReviewModel = ReviewQueued, ""
	return nil, nil
}

// staleSHA rejects CI results for a commit that is no longer the head.
func staleSHA(t *Task, ev Event) error {
	if ev.HeadSHA != t.HeadSHA {
		return &ErrInvalid{State: t.State, Event: ev.Kind, Why: fmt.Sprintf("result for %s but head is %s", ev.HeadSHA, t.HeadSHA)}
	}
	return nil
}

func ciPassed(t *Task, ev Event, _ config.Limits) ([]Effect, error) {
	if err := staleSHA(t, ev); err != nil {
		return nil, err
	}
	t.State = ReviewQueued
	return []Effect{{EffSetLabel, "orch:review"}}, nil
}

func ciFailed(t *Task, ev Event, l config.Limits) ([]Effect, error) {
	if err := staleSHA(t, ev); err != nil {
		return nil, err
	}
	if t.CIAttempts >= l.CIAttempts {
		return toHold(t, ReasonCIRetriesExhausted, Queued, WorkCIFix), nil
	}
	t.CIAttempts++
	t.State, t.Work = Queued, WorkCIFix
	return []Effect{{EffNotify, "ci_failed"}}, nil
}

func startReview(t *Task, ev Event, _ config.Limits) ([]Effect, error) {
	if ev.Model != "" && ev.Model == t.DevModel {
		return nil, &ErrInvalid{State: t.State, Event: ev.Kind, Why: "reviewer model must differ from developer model " + t.DevModel}
	}
	t.State = Reviewing
	t.ReviewModel = ev.Model
	return []Effect{{EffStartReviewRun, ""}}, nil
}

func reviewApproved(t *Task, _ Event, _ config.Limits) ([]Effect, error) {
	t.State = ReadyForHuman
	return []Effect{{EffNotify, "ready_for_review"}}, nil
}

func changesRequested(t *Task, _ Event, l config.Limits) ([]Effect, error) {
	if t.ReviewCycles >= l.ReviewCycles {
		return toHold(t, ReasonReviewCyclesExhausted, Queued, WorkReviewFix), nil
	}
	t.ReviewCycles++
	t.State, t.Work = Queued, WorkReviewFix
	return []Effect{{EffNotify, "changes_requested"}}, nil
}

func reviewEscalated(t *Task, ev Event, _ config.Limits) ([]Effect, error) {
	r := ev.Reason
	if r == ReasonNone {
		r = ReasonRequirementsUnclear
	}
	if !r.Valid() {
		return nil, &ErrInvalid{State: t.State, Event: ev.Kind, Why: fmt.Sprintf("unknown reason %q", r)}
	}
	return toHold(t, r, ReviewQueued, t.Work), nil
}

func headChanged(t *Task, ev Event, _ config.Limits) ([]Effect, error) {
	if ev.HeadSHA == t.HeadSHA {
		return nil, &ErrInvalid{State: t.State, Event: ev.Kind, Why: "head did not change"}
	}
	resume := t.State
	t.HeadSHA = ev.HeadSHA
	if resume == ReviewQueued || resume == Reviewing || resume == ReadyForHuman {
		// The reviewed commit is gone: CI has to pass again on the new head.
		resume = AwaitingCI
	}
	return toHold(t, ReasonHeadChanged, resume, t.Work), nil
}

// retry resumes where the hold started. A CI or review limit hold resumes
// into one more fix run; the counter stays at its limit, so the next failure
// holds again. A dev_runs hold lowers the counter to grant exactly one run.
func retry(t *Task, _ Event, l config.Limits) ([]Effect, error) {
	if t.Reason == ReasonDevRunsExhausted {
		t.DevRuns = l.DevRuns - 1
	}
	if t.ResumeState == "" {
		t.ResumeState, t.ResumeWork = Queued, WorkImplement
	}
	t.State, t.Work = t.ResumeState, t.ResumeWork
	t.Reason, t.ResumeState, t.ResumeWork = ReasonNone, "", ""
	label := "orch:dev"
	if t.State == ReviewQueued || t.State == Reviewing || t.State == ReadyForHuman {
		label = "orch:review"
	}
	return []Effect{{EffSetLabel, label}, {EffNotify, "resumed"}}, nil
}
