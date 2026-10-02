package telegram

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/deniz-ai-orchestration/ai-orchestrator/internal/engine"
	"github.com/deniz-ai-orchestration/ai-orchestrator/internal/store"
)

// reasonText explains each hold in a few words.
var reasonText = map[engine.Reason]string{
	engine.ReasonCIRetriesExhausted:    "CI is still red after every fix attempt",
	engine.ReasonReviewCyclesExhausted: "the reviewer still wants changes after every round",
	engine.ReasonDevRunsExhausted:      "the task used all its developer runs",
	engine.ReasonReviewersDisagree:     "the reviewers disagree",
	engine.ReasonMergeConflict:         "the PR has a merge conflict",
	engine.ReasonProviderUnavailable:   "no model could run",
	engine.ReasonAuthFailed:            "a provider login failed",
	engine.ReasonCLIError:              "the agent CLI failed",
	engine.ReasonTimeout:               "the agent ran past its timeout",
	engine.ReasonBadOutput:             "the agent's answer was unusable",
	engine.ReasonScopeViolation:        "the push broke a rule",
	engine.ReasonSecuritySensitive:     "the reviewer flagged it as security-sensitive",
	engine.ReasonRequirementsUnclear:   "the agent has a question",
	engine.ReasonHeadChanged:           "someone else pushed to the PR",
}

func ref(t store.Task) string {
	return fmt.Sprintf("task %d (%s#%d)", t.ID, t.Repo, t.IssueNumber)
}

func prURL(t store.Task) string {
	return fmt.Sprintf("https://github.com/%s/pull/%d", t.Repo, t.PRNumber)
}

func issueURL(t store.Task) string {
	return fmt.Sprintf("https://github.com/%s/issues/%d", t.Repo, t.IssueNumber)
}

func clip(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// render writes one notification. loud ones ring; the rest arrive silently.
func (b *Bot) render(ctx context.Context, t store.Task, kind string) (text string, loud bool, err error) {
	trs, err := b.Store.Transitions(ctx, t.ID)
	if err != nil {
		return "", false, err
	}
	var last engine.Transition
	if len(trs) > 0 {
		last = trs[len(trs)-1]
	}
	l := b.Cfg.Limits
	switch kind {
	case "pr_opened":
		return fmt.Sprintf("PR opened for %s %q by %s: %s\nWaiting for CI.", ref(t), t.Title, t.DevModel, prURL(t)), false, nil
	case "pr_updated":
		return fmt.Sprintf("%s pushed a fix to PR #%d (%s). Waiting for CI.", t.DevModel, t.PRNumber, ref(t)), false, nil
	case "ci_failed":
		return fmt.Sprintf("CI failed on PR #%d (%s); fix attempt %d of %d is queued.", t.PRNumber, ref(t), t.CIAttempts, l.CIAttempts), false, nil
	case "changes_requested":
		return fmt.Sprintf("%s asked for changes on PR #%d (%s), round %d of %d; the developer is on it.\n\n%s",
			t.ReviewModel, t.PRNumber, ref(t), t.ReviewCycles, l.ReviewCycles, clip(last.Detail, 800)), false, nil
	case "ready_for_review":
		return fmt.Sprintf("Ready for you: PR #%d for %s %q\n%s\n\nCI is green. Developer %s, reviewer %s approved:\n%s",
			t.PRNumber, ref(t), t.Title, prURL(t), t.DevModel, t.ReviewModel, clip(last.Detail, 600)), true, nil
	case "needs_human":
		why := reasonText[t.Reason]
		if why == "" {
			why = string(t.Reason)
		}
		link := issueURL(t)
		if t.PRNumber > 0 {
			link = prURL(t)
		}
		msg := fmt.Sprintf("%s needs you: %s.\n%s", ref(t), why, link)
		if d := clip(last.Detail, 800); d != "" {
			msg += "\n\n" + d
		}
		return msg + fmt.Sprintf("\n\n/retry %d resumes it, /cancel %d stops it.", t.ID, t.ID), true, nil
	case "merged":
		return fmt.Sprintf("Merged: PR #%d for %s. Workspaces removed.", t.PRNumber, ref(t)), false, nil
	case "rejected":
		return fmt.Sprintf("PR #%d for %s was closed without merging; task ended.", t.PRNumber, ref(t)), false, nil
	case "cancelled":
		return fmt.Sprintf("Cancelled %s.", ref(t)), false, nil
	case "resumed":
		return fmt.Sprintf("Resumed %s; it is %s.", ref(t), strings.ReplaceAll(string(t.State), "_", " ")), false, nil
	}
	return fmt.Sprintf("%s: %s", ref(t), kind), false, nil
}

const help = `orch commands:
/status — active tasks
/why <task> — a task's history
/retry <task> — resume a held task
/cancel <task> — end a task (stops its run)
/quota — each model's state and limits
/models — roles, pools and pins
/use <role> <model|auto> — pin a role to a model
/enable <model>, /disable <model>
/pause, /resume — stop or restart starting new runs`

// Command runs one command and returns the reply.
func (b *Bot) Command(ctx context.Context, text string) string {
	f := strings.Fields(text)
	if len(f) == 0 || !strings.HasPrefix(f[0], "/") {
		return help
	}
	cmd, _, _ := strings.Cut(strings.TrimPrefix(f[0], "/"), "@")
	args := f[1:]
	reply, err := b.command(ctx, strings.ToLower(cmd), args)
	if err != nil {
		return "Error: " + err.Error()
	}
	return reply
}

func (b *Bot) command(ctx context.Context, cmd string, args []string) (string, error) {
	need := func(n int, usage string) error {
		if len(args) != n {
			return fmt.Errorf("usage: %s", usage)
		}
		return nil
	}
	switch cmd {
	case "start", "help":
		return help, nil
	case "status":
		return b.status(ctx)
	case "why", "retry", "cancel":
		if err := need(1, "/"+cmd+" <task>"); err != nil {
			return "", err
		}
		id, err := strconv.ParseInt(strings.TrimPrefix(args[0], "#"), 10, 64)
		if err != nil {
			return "", fmt.Errorf("%q is not a task number", args[0])
		}
		t, err := b.Store.GetTask(ctx, id)
		if err != nil {
			return "", fmt.Errorf("task %d: %w", id, err)
		}
		switch cmd {
		case "why":
			return b.why(ctx, t)
		case "retry":
			if _, err := b.Store.ApplyEvent(ctx, id, engine.Event{Kind: engine.EvRetry, Detail: "/retry from Telegram"}, b.Cfg.Limits); err != nil {
				return "", err
			}
			return fmt.Sprintf("Resuming %s.", ref(t)), nil
		default:
			if _, err := b.Store.ApplyEvent(ctx, id, engine.Event{Kind: engine.EvCancel, Detail: "/cancel from Telegram"}, b.Cfg.Limits); err != nil {
				return "", err
			}
			if b.Agents != nil && b.Agents.Stop(id) {
				return fmt.Sprintf("Cancelled %s and stopped its running agent.", ref(t)), nil
			}
			return fmt.Sprintf("Cancelled %s.", ref(t)), nil
		}
	case "quota":
		return b.quota(ctx)
	case "models":
		return b.models(ctx)
	case "use":
		if err := need(2, "/use <role> <model|auto>"); err != nil {
			return "", err
		}
		if err := b.Quota.Pin(ctx, args[0], args[1]); err != nil {
			return "", err
		}
		if args[1] == "auto" {
			return fmt.Sprintf("%s picks from its pool again.", args[0]), nil
		}
		return fmt.Sprintf("%s is pinned to %s while it can run.", args[0], args[1]), nil
	case "enable", "disable":
		if err := need(1, "/"+cmd+" <model>"); err != nil {
			return "", err
		}
		if err := b.Quota.SetEnabled(ctx, args[0], cmd == "enable"); err != nil {
			return "", err
		}
		return fmt.Sprintf("%s %sd.", args[0], cmd), nil
	case "pause", "resume":
		if err := b.Store.SetPaused(ctx, cmd == "pause"); err != nil {
			return "", err
		}
		if cmd == "pause" {
			return "Paused: no new runs start. A run in progress finishes. /resume to continue.", nil
		}
		return "Resumed: queued tasks start again.", nil
	}
	return "Unknown command.\n\n" + help, nil
}

var active = []engine.State{engine.Queued, engine.Developing, engine.AwaitingCI, engine.ReviewQueued,
	engine.Reviewing, engine.ReadyForHuman, engine.NeedsHuman}

func (b *Bot) status(ctx context.Context) (string, error) {
	tasks, err := b.Store.ListTasks(ctx, active...)
	if err != nil {
		return "", err
	}
	var s strings.Builder
	if p, _ := b.Store.Paused(ctx); p {
		s.WriteString("Paused: no new runs start.\n\n")
	}
	if len(tasks) == 0 {
		s.WriteString("No active tasks.")
		return s.String(), nil
	}
	for _, t := range tasks {
		fmt.Fprintf(&s, "%d  %s#%d  %s", t.ID, t.Repo, t.IssueNumber, strings.ReplaceAll(string(t.State), "_", " "))
		switch t.State {
		case engine.Developing:
			fmt.Fprintf(&s, " (%s, %s)", t.DevModel, t.Work)
		case engine.Reviewing:
			fmt.Fprintf(&s, " (%s)", t.ReviewModel)
		case engine.NeedsHuman:
			fmt.Fprintf(&s, ": %s", reasonText[t.Reason])
		}
		if t.PRNumber > 0 {
			fmt.Fprintf(&s, "  PR #%d", t.PRNumber)
		}
		s.WriteString("\n")
	}
	return strings.TrimSpace(s.String()), nil
}

func (b *Bot) why(ctx context.Context, t store.Task) (string, error) {
	trs, err := b.Store.Transitions(ctx, t.ID)
	if err != nil {
		return "", err
	}
	var s strings.Builder
	fmt.Fprintf(&s, "%s %q: %s\n", ref(t), t.Title, strings.ReplaceAll(string(t.State), "_", " "))
	if len(trs) > 15 {
		fmt.Fprintf(&s, "(last 15 of %d steps)\n", len(trs))
		trs = trs[len(trs)-15:]
	}
	for _, tr := range trs {
		fmt.Fprintf(&s, "\n%s  %s → %s", tr.At.Local().Format("Jan 2 15:04"), tr.Event, tr.To)
		if tr.Reason != "" {
			fmt.Fprintf(&s, " (%s)", tr.Reason)
		}
		if d := clip(tr.Detail, 200); d != "" {
			s.WriteString("\n   " + strings.ReplaceAll(d, "\n", "\n   "))
		}
	}
	return s.String(), nil
}

func (b *Bot) quota(ctx context.Context) (string, error) {
	rep, err := b.Quota.Report(ctx)
	if err != nil {
		return "", err
	}
	now := time.Now()
	var s strings.Builder
	for _, m := range rep {
		state := m.State(now)
		if state == "cooling" {
			state += " until " + m.CoolingUntil.Local().Format("15:04")
		}
		fmt.Fprintf(&s, "%s: %s, %d runs in 5h", m.Name, state, m.RunsIn5h)
		if m.Budget > 0 {
			fmt.Fprintf(&s, " of %d", m.Budget)
		}
		if m.Last != nil {
			fmt.Fprintf(&s, ", last %s %s", strings.ReplaceAll(m.Last.Kind, "_", " "), m.Last.At.Local().Format("Jan 2 15:04"))
		}
		s.WriteString("\n")
	}
	return strings.TrimSpace(s.String()), nil
}

func (b *Bot) models(ctx context.Context) (string, error) {
	pins, err := b.Store.RolePins(ctx)
	if err != nil {
		return "", err
	}
	var s strings.Builder
	for _, role := range sortedRoles(b.Cfg.Roles) {
		r := b.Cfg.Roles[role]
		fmt.Fprintf(&s, "%s: %s", role, strings.Join(r.Pool, " > "))
		if p := pins[role]; p != "" {
			fmt.Fprintf(&s, " (pinned: %s)", p)
		}
		if !r.IsEnabled() {
			s.WriteString(" (off)")
		}
		s.WriteString("\n")
	}
	return strings.TrimSpace(s.String()), nil
}

func sortedRoles[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
