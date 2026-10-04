package runner

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/deniz-ai-orchestration/ai-orchestrator/internal/ci"
	"github.com/deniz-ai-orchestration/ai-orchestrator/internal/config"
	"github.com/deniz-ai-orchestration/ai-orchestrator/internal/github"
	"github.com/deniz-ai-orchestration/ai-orchestrator/internal/helper"
	"github.com/deniz-ai-orchestration/ai-orchestrator/internal/store"
)

// WorkflowGitHub reads pull requests and check runs with orch's token.
type WorkflowGitHub interface {
	GetPR(ctx context.Context, repo string, number int) (github.PullRequest, error)
	CheckRuns(ctx context.Context, repo, sha string) ([]github.CheckRun, error)
	JobLog(ctx context.Context, repo string, jobID int64) ([]byte, error)
}

// Workflows follows autonomous workflows' pull requests: CI results on
// the pushed head, pushes by someone else, and the merge or close that
// ends the job. Everything is polled, like the issue tasks' Follower.
type Workflows struct {
	Store    *store.Store
	GH       WorkflowGitHub
	Helper   helper.Summarizer
	Cfg      *config.Config
	Chats    *Chats
	Log      *slog.Logger
	Interval time.Duration
}

// Run follows workflow PRs until ctx is done.
func (f *Workflows) Run(ctx context.Context) error {
	for {
		wait := f.Interval
		if err := f.Once(ctx); err != nil {
			var rl *github.RateLimitError
			if errors.As(err, &rl) {
				wait = max(time.Until(rl.Reset), wait)
				f.Log.Warn("github rate limited", "until", rl.Reset)
			} else if ctx.Err() == nil {
				f.Log.Error("workflow round failed", "err", err)
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(wait):
		}
	}
}

// Once checks every open workflow with a pull request once.
func (f *Workflows) Once(ctx context.Context) error {
	ws, err := f.Store.OpenWorkflows(ctx, "")
	if err != nil {
		return err
	}
	for _, w := range ws {
		if w.PRNumber == 0 {
			continue
		}
		if err := f.follow(ctx, w); err != nil {
			return err
		}
	}
	return nil
}

func (f *Workflows) follow(ctx context.Context, w store.Workflow) error {
	p, err := f.Store.GetProject(ctx, w.ProjectPath)
	if err != nil || !p.Trusted || p.GitHubRepo == "" {
		return nil // not a pollable GitHub project
	}
	pr, err := f.GH.GetPR(ctx, p.GitHubRepo, w.PRNumber)
	if err != nil {
		return fmt.Errorf("workflow %d: get PR: %w", w.ID, err)
	}
	switch {
	case pr.Merged:
		ok, err := f.Chats.AutoCloseWorkflow(ctx, w.ID)
		if err != nil {
			return err
		}
		if !ok {
			return nil // busy; the merge is still there next round
		}
		f.Log.Info("workflow PR merged", "workflow", w.ID, "pr", w.PRNumber)
		return nil
	case pr.State == "closed":
		return f.hold(ctx, w, fmt.Sprintf("PR #%d was closed without a merge.", w.PRNumber))
	case w.Phase == "needs_you":
		return nil // only merge and close move a held workflow
	case pr.Head.SHA != w.HeadSHA:
		if err := f.Store.SetWorkflowCI(ctx, w.ID, store.CIPending, pr.Head.SHA); err != nil {
			return err
		}
		return f.note(ctx, w, "PR head moved to "+short(pr.Head.SHA)+" outside orch; watching CI on it.")
	case w.CIState != store.CIPending:
		return nil
	}
	return f.checkCI(ctx, w, p)
}

func (f *Workflows) checkCI(ctx context.Context, w store.Workflow, p store.Project) error {
	runs, err := f.GH.CheckRuns(ctx, p.GitHubRepo, w.HeadSHA)
	if err != nil {
		return fmt.Errorf("workflow %d: check runs: %w", w.ID, err)
	}
	if len(runs) == 0 {
		return nil // CI has not started yet
	}
	var failed []github.CheckRun
	for _, r := range runs {
		if r.Status != "completed" {
			return nil // still running
		}
		if r.Failed() {
			failed = append(failed, r)
		}
	}
	if len(failed) == 0 {
		return f.passed(ctx, w, p)
	}
	summary := f.summarize(ctx, p.GitHubRepo, failed)
	if err := f.Store.SetWorkflowCI(ctx, w.ID, store.CIRed, w.HeadSHA); err != nil {
		return err
	}
	if !p.Autonomous {
		return f.hold(ctx, w, fmt.Sprintf("CI failed on commit %s:\n\n%s", short(w.HeadSHA), summary))
	}
	ok, err := f.Chats.AutoFixCI(ctx, w.ID, w.HeadSHA, summary)
	if err != nil {
		return err
	}
	if !ok {
		return nil // busy; the red CI is still there next round
	}
	f.Log.Info("workflow CI failed, fix agent summoned", "workflow", w.ID, "pr", w.PRNumber)
	return nil
}

func (f *Workflows) passed(ctx context.Context, w store.Workflow, p store.Project) error {
	if err := f.Store.SetWorkflowCI(ctx, w.ID, store.CIGreen, w.HeadSHA); err != nil {
		return err
	}
	if !p.Autonomous {
		return f.note(ctx, w, "CI is green on commit "+short(w.HeadSHA)+".")
	}
	dev := latestWorkflowDev(ctx, f.Chats, w.ID)
	if dev == nil {
		return nil
	}
	ok, err := f.Chats.AutoTest(ctx, dev.ID)
	if err != nil {
		return err
	}
	if !ok {
		return nil // busy; green CI keeps until the review round starts
	}
	f.Log.Info("workflow CI green, review round started", "workflow", w.ID, "pr", w.PRNumber)
	return nil
}

// hold leaves an orch note on the workflow's developer and parks the
// workflow for you.
func (f *Workflows) hold(ctx context.Context, w store.Workflow, note string) error {
	if err := f.note(ctx, w, note); err != nil {
		return err
	}
	return f.Store.SetWorkflowPhase(ctx, w.ID, "needs_you")
}

// note leaves an orch note on the workflow's latest developer, if any.
func (f *Workflows) note(ctx context.Context, w store.Workflow, note string) error {
	dev := latestWorkflowDev(ctx, f.Chats, w.ID)
	if dev == nil {
		return nil
	}
	_, err := f.Store.AddMessage(ctx, store.Message{AgentID: dev.ID, Author: store.FromOrch, Body: note})
	return err
}

const ciSummaryInstruction = `Below is the log of a failed CI job. In at most 15 lines, say which step failed, ` +
	`the exact failing test names or compiler/lint errors with file:line, and the error messages. ` +
	`Quote errors verbatim. Do not suggest fixes.`

// summarize describes the red checks for the fix agent's brief.
func (f *Workflows) summarize(ctx context.Context, repo string, failed []github.CheckRun) string {
	var b strings.Builder
	for _, r := range failed {
		fmt.Fprintf(&b, "### %s: %s\n%s\n\n", r.Name, r.Conclusion, r.HTMLURL)
		var text string
		if r.IsActions() {
			log, err := f.GH.JobLog(ctx, repo, r.ID)
			if err != nil {
				f.Log.Warn("could not fetch the job log", "check", r.Name, "err", err)
			} else {
				text = ci.CleanLog(string(log))
			}
		}
		if text == "" {
			text = strings.TrimSpace(r.Output.Title + "\n" + r.Output.Summary + "\n" + r.Output.Text)
		}
		if text != "" {
			b.WriteString(f.Helper.Summarize(ctx, ciSummaryInstruction, text))
			b.WriteString("\n\n")
		}
	}
	return strings.TrimSpace(b.String())
}
