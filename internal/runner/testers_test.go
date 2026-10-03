package runner

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/deniz-ai-orchestration/ai-orchestrator/internal/store"
)

func verdictOutput(v Verdict) string {
	vo, _ := json.Marshal(v)
	line, _ := json.Marshal(map[string]any{"type": "result", "subtype": "success", "is_error": false,
		"session_id": "t", "result": string(vo), "structured_output": json.RawMessage(vo)})
	return string(line) + "\n"
}

// testerScript answers developer turns with a commit and "done", and each
// tester role with the next verdict queued for it.
type testerScript struct {
	t        *testing.T
	mu       sync.Mutex
	verdicts map[string][]Verdict
	devTurns int
	prompts  []string // developer prompts
}

func (s *testerScript) respond(spec Spec) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, role := range []string{"functional tester", "reviewer"} {
		if strings.Contains(spec.Stdin, "You are a "+role+" agent") {
			vs := s.verdicts[role]
			if len(vs) == 0 {
				return verdictOutput(Verdict{Verdict: "approve", Escalation: "none", Summary: "Looks good."})
			}
			s.verdicts[role] = vs[1:]
			return verdictOutput(vs[0])
		}
	}
	s.devTurns++
	s.prompts = append(s.prompts, spec.Stdin)
	name := filepath.Join(spec.Work, "change.txt")
	if err := os.WriteFile(name, []byte(strings.Repeat("x", s.devTurns)), 0o644); err != nil {
		s.t.Error(err)
	}
	git(s.t, spec.Work, "add", ".")
	git(s.t, spec.Work, "-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-q", "-m", "change")
	return turnOutput("dev", "done", "Made the change.")
}

func (e *chatEnv) waitFor(t *testing.T, id int64, state string) store.Agent {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		a := e.settle(t, id)
		if a.State == state {
			return a
		}
		if time.Now().After(deadline) {
			t.Fatalf("agent %d is %s, want %s; messages %+v", id, a.State, state, e.messages(t, id))
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (e *chatEnv) script(t *testing.T) *testerScript {
	s := &testerScript{t: t, verdicts: map[string][]Verdict{}}
	e.fake.mu.Lock()
	e.fake.respond = s.respond
	e.fake.mu.Unlock()
	return s
}

func TestTestersApprove(t *testing.T) {
	e := newChatEnv(t)
	ctx := context.Background()
	e.script(t)
	dev, err := e.chats.Summon(ctx, Summon{Role: "developer", Model: "sonnet", Project: "shop", Message: "Add change.txt"})
	if err != nil {
		t.Fatal(err)
	}
	dev = e.waitFor(t, dev.ID, store.AgentReady)
	if dev.Round != 1 {
		t.Fatalf("round %d", dev.Round)
	}
	head := gitOut(t, dev.Workspace, "rev-parse", "HEAD")
	ts, err := e.st.Testers(ctx, dev.ID)
	if err != nil || len(ts) != 2 {
		t.Fatalf("testers %+v %v", ts, err)
	}
	for i, role := range []string{"functional_tester", "reviewer"} {
		tr := ts[i]
		if tr.Role != role || tr.State != store.AgentClosed || tr.Base != head || tr.Round != 1 || tr.Model != "sonnet" {
			t.Errorf("tester %d: %+v", i, tr)
		}
		if _, err := os.Stat(tr.Workspace); !os.IsNotExist(err) {
			t.Errorf("tester %d worktree still there: %v", i, err)
		}
		ms := e.messages(t, tr.ID)
		if len(ms) != 2 || ms[0].Author != store.FromBrief || !strings.Contains(ms[0].Body, "Add change.txt") ||
			ms[1].Author != store.FromAgent || ms[1].Data == "" || !strings.HasPrefix(ms[1].Body, "Approved") {
			t.Errorf("tester %d messages %+v", i, ms)
		}
	}
	// Testers get the verdict schema, a read-only container and no token.
	for i := 1; i <= 2; i++ {
		s := e.fake.spec(t, i)
		if !strings.Contains(s.Stdin, "check another agent's work") || !strings.Contains(s.Stdin, "read-only") ||
			!strings.Contains(strings.Join(s.Argv, " "), "--disallowedTools Edit,Write") {
			t.Errorf("tester turn %d:\n%v\n%s", i, s.Argv, s.Stdin)
		}
		if _, ok := s.Secrets["GH_TOKEN"]; ok {
			t.Error("a tester got a GitHub token")
		}
	}
	ms := e.messages(t, dev.ID)
	last := ms[len(ms)-1]
	if last.Author != store.FromOrch || !strings.Contains(last.Body, "ready for a pull request") {
		t.Fatalf("developer messages %+v", ms)
	}
}

func TestTestersSendFindingsBack(t *testing.T) {
	e := newChatEnv(t)
	ctx := context.Background()
	sc := e.script(t)
	sc.verdicts["reviewer"] = []Verdict{{Verdict: "request_changes", Escalation: "none", Summary: "One bug.",
		Findings: []Finding{{Severity: "major", File: "change.txt", Line: 1, Comment: "Off by one."}}}}
	dev, err := e.chats.Summon(ctx, Summon{Role: "developer", Model: "sonnet", Project: "shop", Message: "Add change.txt"})
	if err != nil {
		t.Fatal(err)
	}
	dev = e.waitFor(t, dev.ID, store.AgentReady)
	if dev.Round != 2 {
		t.Fatalf("round %d", dev.Round)
	}
	sc.mu.Lock()
	defer sc.mu.Unlock()
	if sc.devTurns != 2 || !strings.Contains(sc.prompts[1], "## Findings from the testers") || !strings.Contains(sc.prompts[1], "Off by one.") {
		t.Fatalf("developer turns %d, second prompt:\n%s", sc.devTurns, sc.prompts[len(sc.prompts)-1])
	}
	ts, _ := e.st.Testers(ctx, dev.ID)
	if len(ts) != 4 {
		t.Fatalf("%d testers, want 4", len(ts))
	}
}

func TestTestersIncompleteNeedsYou(t *testing.T) {
	e := newChatEnv(t)
	ctx := context.Background()
	sc := e.script(t)
	sc.verdicts["functional tester"] = []Verdict{{Verdict: "incomplete", Escalation: "none", Summary: "The build needs a database."}}
	dev, err := e.chats.Summon(ctx, Summon{Role: "developer", Model: "sonnet", Project: "shop", Message: "Add change.txt"})
	if err != nil {
		t.Fatal(err)
	}
	dev = e.waitFor(t, dev.ID, store.AgentNeedsYou)
	ms := e.messages(t, dev.ID)
	if last := ms[len(ms)-1]; !strings.Contains(last.Body, "The build needs a database.") || !strings.Contains(last.Body, "Send to testers") {
		t.Fatalf("developer messages %+v", ms)
	}
	// Send to testers runs a new round.
	if err := e.chats.Test(ctx, dev.ID); err != nil {
		t.Fatal(err)
	}
	if dev = e.waitFor(t, dev.ID, store.AgentReady); dev.Round != 2 {
		t.Fatalf("round %d", dev.Round)
	}
}

func TestTestersRoundLimit(t *testing.T) {
	e := newChatEnv(t)
	ctx := context.Background()
	sc := e.script(t)
	bad := Verdict{Verdict: "request_changes", Escalation: "none", Summary: "Still wrong.",
		Findings: []Finding{{Severity: "blocker", Comment: "Wrong."}}}
	sc.verdicts["reviewer"] = []Verdict{bad, bad, bad, bad}
	dev, err := e.chats.Summon(ctx, Summon{Role: "developer", Model: "sonnet", Project: "shop", Message: "Add change.txt"})
	if err != nil {
		t.Fatal(err)
	}
	dev = e.waitFor(t, dev.ID, store.AgentNeedsYou)
	if dev.Round != e.chats.Cfg.Limits.ReviewCycles {
		t.Fatalf("round %d", dev.Round)
	}
	ms := e.messages(t, dev.ID)
	if last := ms[len(ms)-1]; !strings.Contains(last.Body, "review-cycle limit") {
		t.Fatalf("developer messages %+v", ms)
	}
	// You can still send it to the testers: one more round.
	if err := e.chats.Test(ctx, dev.ID); err != nil {
		t.Fatal(err)
	}
	if dev = e.waitFor(t, dev.ID, store.AgentNeedsYou); dev.Round != e.chats.Cfg.Limits.ReviewCycles+1 {
		t.Fatalf("round %d after a manual round", dev.Round)
	}
}

func TestTestRefusals(t *testing.T) {
	e := newChatEnv(t)
	ctx := context.Background()
	e.fake.outputs = []string{turnOutput("s", "done", "Nothing to do.")}
	dev, err := e.chats.Summon(ctx, Summon{Role: "developer", Model: "sonnet", Project: "shop", Message: "Look around"})
	if err != nil {
		t.Fatal(err)
	}
	dev = e.settle(t, dev.ID)
	if dev.State != store.AgentDone {
		t.Fatalf("state %s", dev.State)
	}
	if err := e.chats.Test(ctx, dev.ID); err == nil || !strings.Contains(err.Error(), "no commits") {
		t.Fatalf("test with no commits: %v", err)
	}
	if ts, _ := e.st.Testers(ctx, dev.ID); len(ts) != 0 {
		t.Fatalf("testers for a branch with no commits: %+v", ts)
	}
	rev, err := e.st.CreateAgent(ctx, store.Agent{Role: "reviewer", Provider: "claude", Model: "sonnet", Project: "shop"})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.st.SetAgentState(ctx, rev.ID, store.AgentDone); err != nil {
		t.Fatal(err)
	}
	if err := e.chats.Test(ctx, rev.ID); err == nil {
		t.Fatal("sent a reviewer to testers")
	}
}

func TestRecoverSettlesTesting(t *testing.T) {
	e := newChatEnv(t)
	ctx := context.Background()
	dev, err := e.st.CreateAgent(ctx, store.Agent{Role: "developer", Provider: "claude", Model: "sonnet", Project: "shop", Branch: "b", Round: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.st.SetAgentState(ctx, dev.ID, store.AgentTesting); err != nil {
		t.Fatal(err)
	}
	tr, err := e.st.CreateAgent(ctx, store.Agent{Role: "reviewer", Provider: "claude", Model: "sonnet", Project: "shop",
		Branch: "b", ParentID: dev.ID, Round: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.chats.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	got, _ := e.st.GetAgent(ctx, dev.ID)
	if got.State != store.AgentNeedsYou {
		t.Fatalf("developer %s", got.State)
	}
	if got, _ := e.st.GetAgent(ctx, tr.ID); got.State != store.AgentClosed {
		t.Fatalf("tester %s", got.State)
	}
}
