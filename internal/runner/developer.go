package runner

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"text/template"

	"github.com/deniz-ai-orchestration/ai-orchestrator/internal/config"
	"github.com/deniz-ai-orchestration/ai-orchestrator/internal/engine"
	"github.com/deniz-ai-orchestration/ai-orchestrator/internal/github"
	"github.com/deniz-ai-orchestration/ai-orchestrator/internal/provider"
	"github.com/deniz-ai-orchestration/ai-orchestrator/internal/store"
)

//go:embed prompts/developer.md
var developerPrompt string

var devTmpl = template.Must(template.New("developer").Parse(developerPrompt))

// ResultSchema is the structured answer every developer run must end with.
// "blocked" carries the agent's question in summary.
const ResultSchema = `{"type":"object","properties":{"status":{"type":"string","enum":["done","blocked"]},"summary":{"type":"string"}},"required":["status","summary"],"additionalProperties":false}`

// develop performs one developer run and returns the event it ends in.
func (a *Agents) develop(ctx context.Context, t store.Task, outboxID int64) (engine.Event, error) {
	branch := t.Branch
	if branch == "" {
		branch = github.BranchName(t.IssueNumber, Slug(t.Title))
	}
	out, err := a.runAgent(ctx, job{task: t, outboxID: outboxID, role: "developer", model: t.DevModel,
		schema: ResultSchema,
		prepare: func(ctx context.Context, token string) (string, string, error) {
			ws, base, err := a.WS.Prepare(ctx, t.ID, "dev", t.Repo, branch, token)
			if err != nil {
				return "", "", err
			}
			var extra string
			switch t.Work {
			case engine.WorkCIFix:
				extra, err = a.Store.LastDetail(ctx, t.ID, engine.EvCIFailed)
			case engine.WorkReviewFix:
				extra, err = a.Store.LastDetail(ctx, t.ID, engine.EvChangesRequested)
			}
			if err != nil {
				return "", "", err
			}
			prompt, err := devPrompt(a.Cfg, t, branch, base, extra)
			return ws, prompt, err
		}})
	if err != nil {
		return engine.Event{}, err
	}
	switch out.Kind {
	case provider.OK:
		return a.verify(ctx, t, branch)
	case provider.Blocked:
		a.askOnIssue(ctx, t, out.Detail)
		return engine.Event{Kind: engine.EvDevBlocked, Detail: out.Detail}, nil
	default:
		return outcomeEvent(t.DevModel, out), nil
	}
}

// verify checks what the agent pushed, using GitHub as the truth rather
// than the agent's own report.
func (a *Agents) verify(ctx context.Context, t store.Task, branch string) (engine.Event, error) {
	pr, err := a.GH.FindPR(ctx, t.Repo, branch)
	if errors.Is(err, github.ErrNoPR) {
		return failed(engine.ReasonBadOutput, "run reported done but no open PR exists for "+branch), nil
	}
	if err != nil {
		return engine.Event{}, fmt.Errorf("find PR: %w", err)
	}
	cmp, err := a.GH.Compare(ctx, t.Repo, pr.Base.Ref, pr.Head.SHA)
	if err != nil {
		return engine.Event{}, fmt.Errorf("compare: %w", err)
	}
	vs := github.ValidatePush(t.IssueNumber, branch, cmp, github.PushRules{
		ForbiddenPaths: a.Cfg.GitHub.ForbiddenPaths, MaxDiffLines: a.Cfg.Limits.MaxDiffLines})
	if !github.LinksIssue(pr.Body, t.IssueNumber) {
		vs = append(vs, github.Violation{Rule: "issue_link", Detail: fmt.Sprintf("PR #%d does not say Closes #%d", pr.Number, t.IssueNumber)})
	}
	if len(vs) > 0 {
		var b strings.Builder
		for i, v := range vs {
			if i > 0 {
				b.WriteString("; ")
			}
			b.WriteString(v.Rule + ": " + v.Detail)
		}
		return engine.Event{Kind: engine.EvPushRejected, Detail: b.String()}, nil
	}
	return engine.Event{Kind: engine.EvDevDone, Branch: branch, PRNumber: pr.Number, HeadSHA: pr.Head.SHA}, nil
}

// askOnIssue posts the agent's question on the issue once. Failing to post
// does not change the outcome: the question is also on the transition.
func (a *Agents) askOnIssue(ctx context.Context, t store.Task, question string) {
	body := "The developer agent needs an answer before it can continue:\n\n> " +
		strings.ReplaceAll(strings.TrimSpace(question), "\n", "\n> ") +
		"\n\nEdit the issue to answer, then send /retry."
	marker := github.Marker(t.ID, fmt.Sprintf("question-%d", t.DevRuns), "")
	if _, err := a.GH.EnsureComment(ctx, t.Repo, t.IssueNumber, marker, body, ""); err != nil {
		a.Log.Warn("could not post the question on the issue", "task", t.ID, "err", err)
	}
}

type promptData struct {
	Repo, Title, Body, Branch, Base, Human, Forbidden, Work, SHA string
	// Context is the CI failure (ci_fix), the review findings (review_fix),
	// or the previous review (reviewer).
	Context             string
	Issue, PR, MaxLines int
	Cycle               int
}

func devPrompt(cfg *config.Config, t store.Task, branch, base, extra string) (string, error) {
	var b bytes.Buffer
	err := devTmpl.Execute(&b, promptData{
		Repo: t.Repo, Issue: t.IssueNumber, Title: t.Title, Body: t.Body,
		Branch: branch, Base: base, PR: t.PRNumber, Work: string(t.Work), Context: extra,
		Human: cfg.GitHub.TrustedActor, MaxLines: cfg.Limits.MaxDiffLines,
		Forbidden: strings.Join(cfg.GitHub.ForbiddenPaths, ", "),
	})
	return b.String(), err
}

var nonSlug = regexp.MustCompile(`[^a-z0-9]+`)

// Slug turns an issue title into a branch suffix: lowercase words joined by
// dashes, at most 40 characters.
func Slug(title string) string {
	s := strings.Trim(nonSlug.ReplaceAllString(strings.ToLower(title), "-"), "-")
	if len(s) > 40 {
		s = strings.TrimRight(s[:40], "-")
	}
	if s == "" {
		return "task"
	}
	return s
}
