package store

import (
	"context"
	"database/sql"
	"io/fs"
	"path/filepath"
	"testing"
	"time"

	"github.com/pressly/goose/v3"
)

func TestProjectUpsertGetList(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	shop := Project{Path: "/home/u/projects/shop", Trusted: true, HasGit: true, GitHubRepo: "o/shop"}
	notes := Project{Path: "/home/u/notes", Trusted: true} // chat-only: no git
	for _, p := range []Project{shop, notes} {
		if err := s.UpsertProject(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.GetProject(ctx, "/nope"); err != ErrNotFound {
		t.Fatalf("missing project: %v", err)
	}
	got, err := s.GetProject(ctx, shop.Path)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Trusted || !got.HasGit || got.Autonomous || got.GitHubRepo != "o/shop" || got.CreatedAt.IsZero() {
		t.Fatalf("project %+v", got)
	}
	// The autonomous switch is yours: re-statting the path keeps it.
	if err := s.SetProjectAutonomous(ctx, shop.Path, true); err != nil {
		t.Fatal(err)
	}
	shop.GitHubRepo = "o/shop-moved"
	if err := s.UpsertProject(ctx, shop); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GetProject(ctx, shop.Path); !got.Autonomous || got.GitHubRepo != "o/shop-moved" {
		t.Fatalf("after upsert %+v", got)
	}
	all, err := s.ListProjects(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 || all[0].Path != notes.Path || all[1].Path != shop.Path {
		t.Fatalf("projects %+v", all)
	}
	// Switching autonomous off again works too.
	if err := s.SetProjectAutonomous(ctx, shop.Path, false); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GetProject(ctx, shop.Path); got.Autonomous {
		t.Fatalf("autonomous %+v", got)
	}
}

func TestWorkflowCRUD(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	const proj = "/home/u/projects/shop"
	if err := s.UpsertProject(ctx, Project{Path: proj, Trusted: true, HasGit: true}); err != nil {
		t.Fatal(err)
	}
	// A workflow only lives in a known project.
	if _, err := s.CreateWorkflow(ctx, Workflow{ProjectPath: "/nope", Branch: "b"}); err == nil {
		t.Fatal("created a workflow in an unknown project")
	}
	w, err := s.CreateWorkflow(ctx, Workflow{ProjectPath: proj, Title: "Add login", Branch: "orch/wf-add-login", Base: "abc123"})
	if err != nil {
		t.Fatal(err)
	}
	if w.ID == 0 || w.State != WorkflowOpen || w.CreatedAt.IsZero() {
		t.Fatalf("workflow %+v", w)
	}
	if got, err := s.GetWorkflow(ctx, w.ID); err != nil || got.Title != "Add login" || got.Branch != "orch/wf-add-login" || got.Base != "abc123" {
		t.Fatalf("get %+v %v", got, err)
	}
	if _, err := s.GetWorkflow(ctx, 999); err != ErrNotFound {
		t.Fatalf("missing workflow: %v", err)
	}
	if got, err := s.WorkflowByBranch(ctx, proj, "orch/wf-add-login"); err != nil || got.ID != w.ID {
		t.Fatalf("by branch %+v %v", got, err)
	}
	if _, err := s.WorkflowByBranch(ctx, proj, "orch/other"); err != ErrNotFound {
		t.Fatalf("unknown branch: %v", err)
	}

	// Round, PR/CI state and the bounded-loop counters.
	if err := s.SetWorkflowRound(ctx, w.ID, 2); err != nil {
		t.Fatal(err)
	}
	if err := s.SetWorkflowPR(ctx, w.ID, 7, "https://github.com/o/shop/pull/7", "deadbeef"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetWorkflowCI(ctx, w.ID, CIPending, "deadbeef"); err != nil {
		t.Fatal(err)
	}
	if n, err := s.BumpWorkflowDevRun(ctx, w.ID); err != nil || n != 1 {
		t.Fatalf("dev run %d %v", n, err)
	}
	if n, err := s.BumpWorkflowCIAttempt(ctx, w.ID); err != nil || n != 1 {
		t.Fatalf("ci attempt %d %v", n, err)
	}
	if n, err := s.BumpWorkflowCIAttempt(ctx, w.ID); err != nil || n != 2 {
		t.Fatalf("ci attempt %d %v", n, err)
	}
	got, err := s.GetWorkflow(ctx, w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Round != 2 || got.PRNumber != 7 || got.PRURL == "" || got.HeadSHA != "deadbeef" ||
		got.CIState != CIPending || got.DevRuns != 1 || got.CIAttempts != 2 {
		t.Fatalf("workflow %+v", got)
	}
	if _, err := s.BumpWorkflowDevRun(ctx, 999); err != ErrNotFound {
		t.Fatalf("missing workflow: %v", err)
	}

	// OpenWorkflows lists the open ones per project, newest first; closing
	// removes it from the list and from WorkflowByBranch.
	w2, err := s.CreateWorkflow(ctx, Workflow{ProjectPath: proj, Branch: "orch/wf-second"})
	if err != nil {
		t.Fatal(err)
	}
	open, err := s.OpenWorkflows(ctx, proj)
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 2 || open[0].ID != w2.ID || open[1].ID != w.ID {
		t.Fatalf("open %+v", open)
	}
	if err := s.SetWorkflowState(ctx, w2.ID, WorkflowClosed); err != nil {
		t.Fatal(err)
	}
	if open, _ := s.OpenWorkflows(ctx, proj); len(open) != 1 || open[0].ID != w.ID {
		t.Fatalf("after close %+v", open)
	}
	if all, _ := s.OpenWorkflows(ctx, ""); len(all) != 1 {
		t.Fatalf("every project %+v", all)
	}
}

func TestWorkflowAgentsAndCounts(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	const shop, notes = "/home/u/projects/shop", "/home/u/notes"
	for _, p := range []string{shop, notes} {
		if err := s.UpsertProject(ctx, Project{Path: p, Trusted: true, HasGit: true}); err != nil {
			t.Fatal(err)
		}
	}
	ws, err := s.CreateWorkflow(ctx, Workflow{ProjectPath: shop, Branch: "orch/wf-a"})
	if err != nil {
		t.Fatal(err)
	}
	wn, err := s.CreateWorkflow(ctx, Workflow{ProjectPath: notes, Branch: "orch/wf-n"})
	if err != nil {
		t.Fatal(err)
	}
	dev, err := s.CreateAgent(ctx, Agent{Role: "developer", Provider: "claude", Model: "sonnet",
		Project: shop, Branch: "orch/wf-a", WorkflowID: ws.ID})
	if err != nil {
		t.Fatal(err)
	}
	tester, err := s.CreateAgent(ctx, Agent{Role: "reviewer", Provider: "claude", Model: "sonnet",
		Project: shop, Branch: "orch/wf-a", ParentID: dev.ID, Round: 1, WorkflowID: ws.ID})
	if err != nil {
		t.Fatal(err)
	}
	other, err := s.CreateAgent(ctx, Agent{Role: "developer", Provider: "claude", Model: "sonnet",
		Project: notes, Branch: "orch/wf-n", WorkflowID: wn.ID})
	if err != nil {
		t.Fatal(err)
	}
	legacy, err := s.CreateAgent(ctx, Agent{Role: "developer", Provider: "claude", Model: "sonnet",
		Project: shop, Branch: "old"})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := s.GetAgent(ctx, dev.ID); err != nil || got.WorkflowID != ws.ID {
		t.Fatalf("agent workflow %+v %v", got, err)
	}
	if got, err := s.GetAgent(ctx, legacy.ID); err != nil || got.WorkflowID != 0 {
		t.Fatalf("legacy agent %+v %v", got, err)
	}
	as, err := s.WorkflowAgents(ctx, ws.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(as) != 2 || as[0].ID != dev.ID || as[1].ID != tester.ID {
		t.Fatalf("workflow agents %+v", as)
	}
	// The tester's turn records its workflow too.
	runID, err := s.StartRun(ctx, Run{AgentID: tester.ID, WorkflowID: ws.ID, Role: "reviewer", Provider: "claude", Model: "sonnet"})
	if err != nil {
		t.Fatal(err)
	}
	if r, err := s.GetRun(ctx, runID); err != nil || r.WorkflowID != ws.ID {
		t.Fatalf("run %+v %v", r, err)
	}

	// A closed tester does not count as active; a working one does.
	if err := s.SetAgentState(ctx, tester.ID, AgentClosed); err != nil {
		t.Fatal(err)
	}
	counts, err := s.ProjectCounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if c := counts[shop]; c.Workflows != 1 || c.Agents != 2 || c.Working != 2 {
		t.Fatalf("shop %+v", c)
	}
	if c := counts[notes]; c.Workflows != 1 || c.Agents != 1 || c.Working != 1 {
		t.Fatalf("notes %+v", c)
	}
	if _, ok := counts["/unknown"]; ok {
		t.Fatalf("counts %+v", counts)
	}
	_ = other
}

// TestBackfillProjectsWorkflows migrates a database that stops at 00007 to
// the current schema and checks that every legacy agent and run lands in
// the right workflow.
func TestBackfillProjectsWorkflows(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "orch.db")
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)"+
		"&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)&_txlock=immediate")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	sub, err := fs.Sub(migrations, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	p, err := goose.NewProvider(goose.DialectSQLite3, db, sub)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.UpTo(ctx, 7); err != nil {
		t.Fatal(err)
	}
	// Legacy state: developer A on shop/orch/1-task with a tester and a PR,
	// B summoned later on the same branch (they share one workflow), D whose
	// branch never got created, E in another project, and one issue run.
	const t1 = "2026-10-01T10:00:00Z"
	ins := func(id int64, role, project, branch, state string, parent any, round, pr int, at string) {
		t.Helper()
		if _, err := db.ExecContext(ctx, `
			INSERT INTO agents (id, role, provider, model, project, branch, base, state, parent_id, round, pr_number, pr_url, created_at, updated_at)
			VALUES (?, ?, 'claude', 'sonnet', ?, ?, 'base', ?, ?, ?, ?, '', ?, ?)`,
			id, role, project, branch, state, parent, round, pr, at, at); err != nil {
			t.Fatal(err)
		}
	}
	ins(1, "developer", "shop", "orch/1-task", "ready", nil, 2, 5, t1)
	ins(2, "developer", "shop", "orch/1-task", "closed", nil, 0, 0, "2026-10-02T10:00:00Z")
	ins(3, "reviewer", "shop", "orch/1-task", "closed", int64(1), 1, 0, t1)
	ins(4, "developer", "shop", "", "failed", nil, 0, 0, "2026-10-03T10:00:00Z")
	ins(5, "developer", "notes", "orch/2-x", "done", nil, 0, 0, "2026-10-04T10:00:00Z")
	for _, r := range [][2]any{{1, int64(1)}, {2, nil}} {
		if _, err := db.ExecContext(ctx, `
			INSERT INTO runs (id, agent_id, role, provider, model, status, started_at)
			VALUES (?, ?, 'developer', 'claude', 'sonnet', 'succeeded', ?)`, r[0], r[1], t1); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := p.Up(ctx); err != nil {
		t.Fatal(err)
	}
	s := &Store{db: db, now: func() time.Time { return time.Now().UTC() }}

	projects, err := s.ListProjects(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(projects) != 2 || projects[0].Path != "notes" || projects[1].Path != "shop" ||
		!projects[1].Trusted || !projects[1].HasGit || projects[1].Autonomous {
		t.Fatalf("projects %+v", projects)
	}
	// One workflow per distinct (project, branch); D's empty branch got one
	// of its own.
	if ws, _ := s.OpenWorkflows(ctx, "shop"); len(ws) != 2 {
		t.Fatalf("shop workflows %+v", ws)
	}
	wf, err := s.WorkflowByBranch(ctx, "shop", "orch/1-task")
	if err != nil {
		t.Fatal(err)
	}
	if wf.Round != 2 || wf.PRNumber != 5 || wf.Base != "base" {
		t.Fatalf("workflow %+v", wf)
	}
	a, b, c, d, e := mustAgent(t, s, 1), mustAgent(t, s, 2), mustAgent(t, s, 3), mustAgent(t, s, 4), mustAgent(t, s, 5)
	if a.WorkflowID != wf.ID || b.WorkflowID != wf.ID || c.WorkflowID != wf.ID {
		t.Fatalf("agents on the branch: %d %d %d, want %d", a.WorkflowID, b.WorkflowID, c.WorkflowID, wf.ID)
	}
	if d.WorkflowID == 0 || d.WorkflowID == wf.ID || d.WorkflowID == e.WorkflowID {
		t.Fatalf("branchless agent workflow %d", d.WorkflowID)
	}
	if wn, err := s.WorkflowByBranch(ctx, "notes", "orch/2-x"); err != nil || e.WorkflowID != wn.ID {
		t.Fatalf("notes agent %d workflow %+v %v", e.WorkflowID, wn, err)
	}
	r1, r2 := mustRun(t, s, 1), mustRun(t, s, 2)
	if r1.WorkflowID != wf.ID {
		t.Fatalf("agent run workflow %d, want %d", r1.WorkflowID, wf.ID)
	}
	if r2.WorkflowID != 0 {
		t.Fatalf("issue run workflow %d, want 0", r2.WorkflowID)
	}
	counts, err := s.ProjectCounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if counts["shop"].Workflows != 2 || counts["shop"].Agents != 2 || counts["notes"].Agents != 1 {
		t.Fatalf("counts %+v", counts)
	}
}

func mustAgent(t *testing.T, s *Store, id int64) Agent {
	t.Helper()
	a, err := s.GetAgent(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func mustRun(t *testing.T, s *Store, id int64) Run {
	t.Helper()
	r, err := s.GetRun(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestSetAgentStateUnlessClosed(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	a, err := s.CreateAgent(ctx, Agent{Role: "developer", Provider: "claude", Model: "sonnet", Project: "shop"})
	if err != nil {
		t.Fatal(err)
	}
	if closed, err := s.SetAgentStateUnlessClosed(ctx, a.ID, AgentFailed); err != nil || closed {
		t.Fatalf("working agent: closed=%v err=%v", closed, err)
	}
	if got, _ := s.GetAgent(ctx, a.ID); got.State != AgentFailed {
		t.Fatalf("state %s", got.State)
	}
	if err := s.SetAgentState(ctx, a.ID, AgentClosed); err != nil {
		t.Fatal(err)
	}
	// A turn that ends after its agent was closed leaves it closed.
	if closed, err := s.SetAgentStateUnlessClosed(ctx, a.ID, AgentFailed); err != nil || !closed {
		t.Fatalf("closed agent: closed=%v err=%v", closed, err)
	}
	if got, _ := s.GetAgent(ctx, a.ID); got.State != AgentClosed {
		t.Fatalf("state %s", got.State)
	}
	if _, err := s.SetAgentStateUnlessClosed(ctx, 999, AgentFailed); err != ErrNotFound {
		t.Fatalf("missing agent: %v", err)
	}
}

func TestNonGitProjectStaysManual(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	const notes = "/home/u/notes"
	if err := s.UpsertProject(ctx, Project{Path: notes, Trusted: true}); err != nil {
		t.Fatal(err)
	}
	// Chat-only: the autonomous switch cannot be turned on.
	if err := s.SetProjectAutonomous(ctx, notes, true); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GetProject(ctx, notes); got.Autonomous {
		t.Fatalf("chat-only project %+v", got)
	}
	// Gaining git unlocks it; losing git forces it off again.
	if err := s.UpsertProject(ctx, Project{Path: notes, Trusted: true, HasGit: true}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetProjectAutonomous(ctx, notes, true); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GetProject(ctx, notes); !got.Autonomous {
		t.Fatalf("git project %+v", got)
	}
	if err := s.UpsertProject(ctx, Project{Path: notes, Trusted: true}); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GetProject(ctx, notes); got.Autonomous {
		t.Fatalf("git removed %+v", got)
	}
}
