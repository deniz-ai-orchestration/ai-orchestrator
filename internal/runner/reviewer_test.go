package runner

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/deniz-ai-orchestration/ai-orchestrator/internal/engine"
)

func codexAnswer(v any) string {
	text, _ := json.Marshal(v)
	item, _ := json.Marshal(map[string]any{"type": "item.completed",
		"item": map[string]any{"id": "item_1", "type": "agent_message", "text": string(text)}})
	return `{"type":"thread.started"}` + "\n" + string(item) + "\n" +
		`{"type":"turn.completed","usage":{"input_tokens":900,"output_tokens":80}}` + "\n"
}

// toReview brings the env's task to review_queued on the origin's existing
// agent branch, developed by sonnet.
func toReview(t *testing.T, e *env) string {
	t.Helper()
	sha := git(t, e.origin, "rev-parse", "agent/3-existing")
	ctx := context.Background()
	for _, ev := range []engine.Event{
		{Kind: engine.EvDevStarted, Model: "sonnet"},
		{Kind: engine.EvDevDone, Branch: "agent/3-existing", PRNumber: 12, HeadSHA: sha},
		{Kind: engine.EvCIPassed, HeadSHA: sha},
	} {
		if _, err := e.st.ApplyEvent(ctx, e.task.ID, ev, e.dev.Cfg.Limits); err != nil {
			t.Fatal(err)
		}
	}
	items, _ := e.st.PendingEffectsOf(ctx, 5, engine.EffStartDevRun)
	for _, it := range items {
		if err := e.st.CompleteEffect(ctx, it.ID); err != nil {
			t.Fatal(err)
		}
	}
	return sha
}

func TestReviewApproves(t *testing.T) {
	e := newEnv(t, "")
	sha := toReview(t, e)
	reviewGH := &fakeGH{}
	e.dev.ReviewGH = func() (Commenter, error) { return reviewGH, nil }
	e.agent.stdout = codexAnswer(Verdict{Verdict: "approve", Escalation: "none", Summary: "Looks right; tests cover it.",
		Findings: []Finding{{Severity: "nit", File: "fix.go", Line: 1, Comment: "name | could be clearer"}}})
	e.agent.act = func(ws string) {
		if head := git(t, ws, "rev-parse", "HEAD"); head != sha {
			t.Errorf("reviewer saw %s, want %s", head, sha)
		}
	}
	if err := e.dev.Once(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := e.state(t)
	if got.State != engine.ReadyForHuman || got.ReviewModel != "sol" {
		t.Fatalf("task %+v (detail %q)", got.Task, e.lastDetail(t))
	}
	s := e.agent.specs[0]
	if s.Argv[0] != "codex" || !strings.Contains(strings.Join(s.Argv, " "), "--dangerously-bypass-approvals-and-sandbox") {
		t.Errorf("argv %v", s.Argv)
	}
	if s.Secrets["GH_TOKEN"] != "rev-tok" || s.Volumes["orch-codex-home"] != HomeDir+"/.codex" {
		t.Errorf("reviewer must get the reviewer token and the codex home: %+v %+v", s.Secrets, s.Volumes)
	}
	for _, want := range []string{"pull request #12", "issue #3", sha, "git diff origin/main...HEAD"} {
		if !strings.Contains(s.Stdin, want) {
			t.Errorf("prompt missing %q", want)
		}
	}
	if len(reviewGH.comments) != 1 || !strings.Contains(reviewGH.comments[0], "orch review: Approved") ||
		!strings.Contains(reviewGH.comments[0], `name \| could be clearer`) || !strings.Contains(reviewGH.comments[0], "kind=review sha="+sha) {
		t.Fatalf("review comment %v", reviewGH.comments)
	}
}

func TestReviewerNeverUsesDeveloperModel(t *testing.T) {
	e := newEnv(t, "")
	toReview(t, e)
	r := e.dev.Cfg.Roles["reviewer"]
	r.Pool = []string{"sonnet"} // only the developer's model
	e.dev.Cfg.Roles["reviewer"] = r
	if err := e.dev.Once(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := e.state(t); got.State != engine.ReviewQueued || len(e.agent.specs) != 0 {
		t.Fatalf("reviewed with the developer's model: %+v", got.Task)
	}
}

func TestReviewRequestsChangesThenFixPromptHasFindings(t *testing.T) {
	e := newEnv(t, "")
	toReview(t, e)
	e.agent.stdout = codexAnswer(Verdict{Verdict: "approve", Escalation: "none", Summary: "Almost.",
		Findings: []Finding{{Severity: "major", File: "fix.go", Line: 3, Comment: "missing error check"}}})
	ctx := context.Background()
	if err := e.dev.Once(ctx); err != nil {
		t.Fatal(err)
	}
	got := e.state(t)
	if got.State != engine.Queued || got.Work != engine.WorkReviewFix || got.ReviewCycles != 1 {
		t.Fatalf("an approve with a major finding must request changes: %+v", got.Task)
	}
	// The fix run's prompt carries the findings.
	pr := goodPR()
	pr.Head.Ref = "agent/3-existing"
	e.gh.pr = pr
	e.agent.stdout = claudeResult("done", "fixed")
	if err := e.dev.Once(ctx); err != nil {
		t.Fatal(err)
	}
	fix := e.agent.specs[1].Stdin
	if !strings.Contains(fix, "[major] fix.go:3: missing error check") || !strings.Contains(fix, "Review findings") {
		t.Fatalf("review_fix prompt:\n%s", fix)
	}
}

func TestReviewOutcomes(t *testing.T) {
	cases := []struct {
		name   string
		stdout string
		want   engine.State
		reason engine.Reason
	}{
		{"escalate security", codexAnswer(Verdict{Verdict: "escalate", Escalation: "security_sensitive", Summary: "touches auth", Findings: []Finding{}}),
			engine.NeedsHuman, engine.ReasonSecuritySensitive},
		{"escalate unclear", codexAnswer(Verdict{Verdict: "escalate", Escalation: "none", Summary: "?", Findings: []Finding{}}),
			engine.NeedsHuman, engine.ReasonRequirementsUnclear},
		{"request_changes with only nits", codexAnswer(Verdict{Verdict: "request_changes", Escalation: "none", Summary: "nits",
			Findings: []Finding{{Severity: "nit", Comment: "typo"}}}), engine.ReadyForHuman, ""},
		{"incomplete", codexAnswer(Verdict{Verdict: "incomplete", Escalation: "none", Summary: "sandbox cannot create a namespace", Findings: []Finding{}}),
			engine.NeedsHuman, engine.ReasonCLIError},
		{"request_changes without findings", codexAnswer(Verdict{Verdict: "request_changes", Escalation: "none", Summary: "could not review", Findings: []Finding{}}),
			engine.NeedsHuman, engine.ReasonBadOutput},
		{"unknown verdict", codexAnswer(map[string]any{"verdict": "maybe", "summary": "x"}), engine.NeedsHuman, engine.ReasonBadOutput},
		{"prose", `{"type":"item.completed","item":{"type":"agent_message","text":"LGTM"}}` + "\n" + `{"type":"turn.completed","usage":{}}` + "\n",
			engine.NeedsHuman, engine.ReasonBadOutput},
		{"quota", `{"type":"turn.failed","error":{"message":"You've hit your usage limit. Try again later."}}` + "\n",
			engine.ReviewQueued, ""}, // another model reviews next round
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t, "")
			toReview(t, e)
			e.agent.stdout = tc.stdout
			if err := e.dev.Once(context.Background()); err != nil {
				t.Fatal(err)
			}
			got := e.state(t)
			if got.State != tc.want || got.Reason != tc.reason {
				t.Fatalf("state %s reason %s, want %s %s (detail %q)", got.State, got.Reason, tc.want, tc.reason, e.lastDetail(t))
			}
			if tc.reason == engine.ReasonBadOutput {
				return
			}
			if got.State == engine.NeedsHuman && got.ResumeState != engine.ReviewQueued {
				t.Errorf("resume %s", got.ResumeState)
			}
		})
	}
}

func TestFormatFindings(t *testing.T) {
	got := FormatFindings(Verdict{Summary: " ok ", Findings: []Finding{
		{Severity: "blocker", File: "a.go", Line: 2, Comment: "x"},
		{Severity: "minor", File: "b.go", Comment: "y"},
		{Severity: "nit", Comment: "z"},
	}})
	want := "ok\n- [blocker] a.go:2: x\n- [minor] b.go: y\n- [nit] general: z"
	if got != want {
		t.Fatalf("got %q", got)
	}
}
