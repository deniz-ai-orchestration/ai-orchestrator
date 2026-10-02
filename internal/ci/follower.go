// Package ci follows each task's pull request after the developer pushed:
// CI results on the head commit, pushes by someone else, merge conflicts,
// and the merge or close that ends the task. Everything is polled, so orch
// needs no inbound webhook.
package ci

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"

	"github.com/deniz-ai-orchestration/ai-orchestrator/internal/config"
	"github.com/deniz-ai-orchestration/ai-orchestrator/internal/engine"
	"github.com/deniz-ai-orchestration/ai-orchestrator/internal/github"
	"github.com/deniz-ai-orchestration/ai-orchestrator/internal/helper"
	"github.com/deniz-ai-orchestration/ai-orchestrator/internal/store"
)

// GitHub is what the follower reads, with orch's own token.
type GitHub interface {
	GetPR(ctx context.Context, repo string, number int) (github.PullRequest, error)
	CheckRuns(ctx context.Context, repo, sha string) ([]github.CheckRun, error)
	JobLog(ctx context.Context, repo string, jobID int64) ([]byte, error)
}

// Follower turns PR and CI state into engine events.
type Follower struct {
	Store    *store.Store
	GH       GitHub
	Helper   helper.Summarizer
	Cfg      *config.Config
	Log      *slog.Logger
	Interval time.Duration
}

// watched are the states with a PR orch follows. Queued and Developing are
// left alone: a developer run is about to push or is pushing.
var watched = []engine.State{engine.AwaitingCI, engine.ReviewQueued, engine.Reviewing,
	engine.ReadyForHuman, engine.NeedsHuman}

// Run follows PRs until ctx is done.
func (f *Follower) Run(ctx context.Context) error {
	for {
		wait := f.Interval
		if err := f.Once(ctx); err != nil {
			var rl *github.RateLimitError
			if errors.As(err, &rl) {
				wait = max(time.Until(rl.Reset), wait)
				f.Log.Warn("github rate limited", "until", rl.Reset)
			} else if ctx.Err() == nil {
				f.Log.Error("ci round failed", "err", err)
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(wait):
		}
	}
}

// Once checks every followed task once.
func (f *Follower) Once(ctx context.Context) error {
	tasks, err := f.Store.ListTasks(ctx, watched...)
	if err != nil {
		return err
	}
	for _, t := range tasks {
		if t.PRNumber == 0 {
			continue
		}
		ev, ok, err := f.check(ctx, t)
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		if _, err := f.Store.ApplyEvent(ctx, t.ID, ev, f.Cfg.Limits); err != nil {
			var inv *engine.ErrInvalid
			if !errors.As(err, &inv) {
				return err
			}
			f.Log.Debug("event does not apply", "task", t.ID, "event", ev.Kind, "err", err)
			continue
		}
		f.Log.Info("pr event", "task", t.ID, "pr", t.PRNumber, "event", ev.Kind)
	}
	return nil
}

// check returns the event the PR's current state calls for, if any.
func (f *Follower) check(ctx context.Context, t store.Task) (engine.Event, bool, error) {
	pr, err := f.GH.GetPR(ctx, t.Repo, t.PRNumber)
	if err != nil {
		return engine.Event{}, false, fmt.Errorf("task %d: get PR: %w", t.ID, err)
	}
	switch {
	case pr.Merged:
		return engine.Event{Kind: engine.EvPRMerged}, true, nil
	case pr.State == "closed":
		return engine.Event{Kind: engine.EvPRClosed}, true, nil
	case t.State == engine.NeedsHuman:
		return engine.Event{}, false, nil // only merge and close move a held task
	case pr.Head.SHA != t.HeadSHA:
		return engine.Event{Kind: engine.EvHeadChanged, HeadSHA: pr.Head.SHA,
			Detail: "the PR head moved to " + short(pr.Head.SHA) + " outside orch"}, true, nil
	case pr.Mergeable != nil && !*pr.Mergeable && pr.MergeableState == "dirty":
		return engine.Event{Kind: engine.EvMergeConflict, Detail: "PR has a merge conflict with its base"}, true, nil
	case t.State != engine.AwaitingCI:
		return engine.Event{}, false, nil
	}
	return f.checkCI(ctx, t)
}

// checkCI reads the check runs on the head commit. It waits while any run
// is pending or none has started.
func (f *Follower) checkCI(ctx context.Context, t store.Task) (engine.Event, bool, error) {
	runs, err := f.GH.CheckRuns(ctx, t.Repo, t.HeadSHA)
	if err != nil {
		return engine.Event{}, false, fmt.Errorf("task %d: check runs: %w", t.ID, err)
	}
	if len(runs) == 0 {
		if time.Since(t.UpdatedAt) > 30*time.Minute {
			f.Log.Warn("no CI on the head commit yet", "task", t.ID, "sha", short(t.HeadSHA))
		}
		return engine.Event{}, false, nil
	}
	var failed []github.CheckRun
	for _, r := range runs {
		if r.Status != "completed" {
			return engine.Event{}, false, nil
		}
		if r.Failed() {
			failed = append(failed, r)
		}
	}
	if len(failed) == 0 {
		return engine.Event{Kind: engine.EvCIPassed, HeadSHA: t.HeadSHA}, true, nil
	}
	return engine.Event{Kind: engine.EvCIFailed, HeadSHA: t.HeadSHA, Detail: f.failure(ctx, t, failed)}, true, nil
}

const summaryInstruction = `Below is the log of a failed CI job. In at most 15 lines, say which step failed, ` +
	`the exact failing test names or compiler/lint errors with file:line, and the error messages. ` +
	`Quote errors verbatim. Do not suggest fixes.`

// failure describes the red checks for the fix run's prompt.
func (f *Follower) failure(ctx context.Context, t store.Task, failed []github.CheckRun) string {
	var b strings.Builder
	for _, r := range failed {
		fmt.Fprintf(&b, "### %s: %s\n%s\n\n", r.Name, r.Conclusion, r.HTMLURL)
		var text string
		if r.IsActions() {
			log, err := f.GH.JobLog(ctx, t.Repo, r.ID)
			if err != nil {
				f.Log.Warn("could not fetch the job log", "task", t.ID, "check", r.Name, "err", err)
			} else {
				text = CleanLog(string(log))
			}
		}
		if text == "" {
			text = strings.TrimSpace(r.Output.Title + "\n" + r.Output.Summary + "\n" + r.Output.Text)
		}
		if text != "" {
			b.WriteString(f.Helper.Summarize(ctx, summaryInstruction, text))
			b.WriteString("\n\n")
		}
	}
	return strings.TrimSpace(b.String())
}

var stamp = regexp.MustCompile(`(?m)^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d\.\d+Z ?`)

// CleanLog drops the timestamp GitHub puts on every log line.
func CleanLog(s string) string { return stamp.ReplaceAllString(s, "") }

func short(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}
