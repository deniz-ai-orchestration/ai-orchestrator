package runner

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"strings"
	"text/template"

	"github.com/deniz-ai-orchestration/ai-orchestrator/internal/engine"
	"github.com/deniz-ai-orchestration/ai-orchestrator/internal/github"
	"github.com/deniz-ai-orchestration/ai-orchestrator/internal/provider"
	"github.com/deniz-ai-orchestration/ai-orchestrator/internal/store"
)

//go:embed prompts/reviewer.md
var reviewerPrompt string

var reviewTmpl = template.Must(template.New("reviewer").Parse(reviewerPrompt))

// VerdictSchema is the reviewer's structured answer. Every property is
// required and no others are allowed, as Codex's strict output schemas need.
const VerdictSchema = `{"type":"object","properties":{` +
	`"verdict":{"type":"string","enum":["approve","request_changes","escalate","incomplete"]},` +
	`"escalation":{"type":"string","enum":["none","security_sensitive","requirements_unclear"]},` +
	`"summary":{"type":"string"},` +
	`"findings":{"type":"array","items":{"type":"object","properties":{` +
	`"severity":{"type":"string","enum":["blocker","major","minor","nit"]},` +
	`"file":{"type":"string"},"line":{"type":"integer"},"comment":{"type":"string"}},` +
	`"required":["severity","file","line","comment"],"additionalProperties":false}}},` +
	`"required":["verdict","escalation","summary","findings"],"additionalProperties":false}`

// Verdict is a parsed reviewer answer.
type Verdict struct {
	Verdict    string    `json:"verdict"`
	Escalation string    `json:"escalation"`
	Summary    string    `json:"summary"`
	Findings   []Finding `json:"findings"`
}

// Finding is one review comment.
type Finding struct {
	Severity string `json:"severity"`
	File     string `json:"file"`
	Line     int    `json:"line"`
	Comment  string `json:"comment"`
}

// Blocking reports whether the finding must be fixed before approval.
func (f Finding) Blocking() bool { return f.Severity == "blocker" || f.Severity == "major" }

// review performs one reviewer run on the task's head commit.
func (a *Agents) review(ctx context.Context, t store.Task, outboxID int64) (engine.Event, error) {
	out, err := a.runAgent(ctx, job{task: t, outboxID: outboxID, role: "reviewer", model: t.ReviewModel,
		schema: VerdictSchema, readOnly: true,
		prepare: func(ctx context.Context, token string) (string, string, error) {
			ws, base, err := a.WS.Prepare(ctx, t.ID, "review", t.Repo, t.Branch, token)
			if err != nil {
				return "", "", err
			}
			if err := a.WS.Detach(ctx, ws, t.HeadSHA); err != nil {
				return "", "", err
			}
			prev, err := a.Store.LastDetail(ctx, t.ID, engine.EvChangesRequested)
			if err != nil {
				return "", "", err
			}
			var b bytes.Buffer
			err = reviewTmpl.Execute(&b, promptData{
				Repo: t.Repo, Issue: t.IssueNumber, Title: t.Title, Body: t.Body, PR: t.PRNumber,
				SHA: t.HeadSHA, Base: base, Context: prev, Cycle: t.ReviewCycles + 1,
				Forbidden: strings.Join(a.Cfg.GitHub.ForbiddenPaths, ", "),
			})
			return ws, b.String(), err
		}})
	if err != nil {
		return engine.Event{}, err
	}
	if out.Kind != provider.OK {
		return outcomeEvent(t.ReviewModel, out), nil
	}
	var v Verdict
	if err := json.Unmarshal(out.Result, &v); err != nil {
		return failed(engine.ReasonBadOutput, "verdict is not valid JSON: "+err.Error()), nil
	}
	ev, err := verdictEvent(v)
	if err != nil {
		return failed(engine.ReasonBadOutput, err.Error()), nil
	}
	a.postReview(ctx, t, v, ev)
	return ev, nil
}

// verdictEvent maps a verdict to the engine. Blocking findings decide over
// the verdict word: approve with a blocker is a change request, and
// request_changes with only minor findings is an approval. A review that
// could not be done, or that requests changes without naming any, holds the
// task for a human.
func verdictEvent(v Verdict) (engine.Event, error) {
	blocking := 0
	for _, f := range v.Findings {
		if f.Blocking() {
			blocking++
		}
	}
	switch v.Verdict {
	case "escalate":
		r := engine.ReasonRequirementsUnclear
		if v.Escalation == "security_sensitive" {
			r = engine.ReasonSecuritySensitive
		}
		return engine.Event{Kind: engine.EvReviewEscalated, Reason: r, Detail: FormatFindings(v)}, nil
	case "incomplete":
		return failed(engine.ReasonCLIError, "the reviewer could not complete the review: "+v.Summary), nil
	case "approve", "request_changes":
		if v.Verdict == "request_changes" && len(v.Findings) == 0 {
			// Changes requested but none named: never an approval.
			return failed(engine.ReasonBadOutput, "the reviewer requested changes but named none: "+v.Summary), nil
		}
		if blocking > 0 {
			return engine.Event{Kind: engine.EvChangesRequested, Detail: FormatFindings(v)}, nil
		}
		return engine.Event{Kind: engine.EvReviewApproved, Detail: v.Summary}, nil
	}
	return engine.Event{}, fmt.Errorf("unknown verdict %q", v.Verdict)
}

// FormatFindings renders a verdict as plain text for the next prompt and
// the transition log.
func FormatFindings(v Verdict) string {
	var b strings.Builder
	b.WriteString(strings.TrimSpace(v.Summary))
	for _, f := range v.Findings {
		fmt.Fprintf(&b, "\n- [%s] %s: %s", f.Severity, where(f), strings.TrimSpace(f.Comment))
	}
	return b.String()
}

func where(f Finding) string {
	switch {
	case f.File == "":
		return "general"
	case f.Line > 0:
		return fmt.Sprintf("%s:%d", f.File, f.Line)
	}
	return f.File
}

var verdictTitle = map[engine.EventKind]string{
	engine.EvReviewApproved:   "Approved",
	engine.EvChangesRequested: "Changes requested",
	engine.EvReviewEscalated:  "Needs a human decision",
}

// postReview posts the verdict on the PR once per head commit, with the
// reviewer's token. A failure is logged: the verdict is on the transition
// and reaches the developer either way.
func (a *Agents) postReview(ctx context.Context, t store.Task, v Verdict, ev engine.Event) {
	if a.ReviewGH == nil {
		return
	}
	gh, err := a.ReviewGH()
	if err != nil {
		a.Log.Warn("cannot post the review", "task", t.ID, "err", err)
		return
	}
	var b strings.Builder
	fmt.Fprintf(&b, "**orch review: %s** (%s, commit %s, round %d)\n\n%s\n",
		verdictTitle[ev.Kind], t.ReviewModel, short(t.HeadSHA), t.ReviewCycles+1, strings.TrimSpace(v.Summary))
	if len(v.Findings) > 0 {
		b.WriteString("\n| Severity | Where | Finding |\n|---|---|---|\n")
		for _, f := range v.Findings {
			fmt.Fprintf(&b, "| %s | `%s` | %s |\n", f.Severity, where(f), cell(f.Comment))
		}
	}
	marker := github.Marker(t.ID, "review", t.HeadSHA)
	if _, err := gh.EnsureComment(ctx, t.Repo, t.PRNumber, marker, b.String(), ""); err != nil {
		a.Log.Warn("could not post the review", "task", t.ID, "err", err)
	}
}

func cell(s string) string {
	s = strings.ReplaceAll(strings.TrimSpace(s), "|", `\|`)
	return strings.ReplaceAll(s, "\n", "<br>")
}

func short(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}
