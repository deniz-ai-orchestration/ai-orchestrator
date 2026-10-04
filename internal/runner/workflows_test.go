package runner

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/deniz-ai-orchestration/ai-orchestrator/internal/store"
)

// trustShop records the test project as a trusted git project and returns
// its resolved path.
func trustShop(t *testing.T, e *chatEnv) string {
	t.Helper()
	path, err := e.chats.Projects.Resolve("shop")
	if err != nil {
		t.Fatal(err)
	}
	if err := e.st.UpsertProject(context.Background(), store.Project{Path: path, Trusted: true, HasGit: true}); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestSummonWorkflow(t *testing.T) {
	e := newChatEnv(t)
	ctx := context.Background()
	shop := trustShop(t, e)
	e.fake.outputs = []string{turnOutput("s1", "done", "Made the change.")}
	w, err := e.chats.SummonWorkflow(ctx, WorkflowRequest{ProjectPath: "shop", Prompt: "Add change.txt", Model: "sonnet"})
	if err != nil {
		t.Fatal(err)
	}
	if w.ID == 0 || w.ProjectPath != shop || w.State != store.WorkflowOpen || !strings.HasPrefix(w.Branch, "orch/") {
		t.Fatalf("workflow %+v", w)
	}
	if w.Workspace != filepath.Join(e.chats.Dir, "workflows", "1", "work") {
		t.Fatalf("workspace %q", w.Workspace)
	}
	as, err := e.st.WorkflowAgents(ctx, w.ID)
	if err != nil || len(as) != 1 {
		t.Fatalf("agents %+v %v", as, err)
	}
	dev := e.settle(t, as[0].ID)
	if dev.Role != "developer" || dev.State != store.AgentDone || dev.Workspace != w.Workspace || dev.Branch != w.Branch {
		t.Fatalf("developer %+v", dev)
	}
	if got, _ := e.st.GetWorkflow(ctx, w.ID); got.DevRuns != 1 || got.Model != "sonnet" {
		t.Fatalf("workflow %+v", got)
	}
	// The branch lives in the project; the checkout is the workflow's.
	if out := gitOut(t, e.project, "branch", "--list", w.Branch); !strings.Contains(out, w.Branch) {
		t.Fatalf("branch %s missing", w.Branch)
	}

	// Replacing the developer keeps the branch and the checkout: the next
	// agent continues where the old one stopped.
	e.fake.outputs = []string{turnOutput("s2", "done", "Continued.")}
	next, err := e.chats.SummonAgent(ctx, w.ID, "developer", "sonnet", "Keep going")
	if err != nil {
		t.Fatal(err)
	}
	if next.Workspace != w.Workspace || next.Branch != w.Branch {
		t.Fatalf("replacement %+v", next)
	}
	if got, _ := e.st.GetAgent(ctx, dev.ID); got.State != store.AgentClosed {
		t.Fatalf("old developer %s", got.State)
	}
	next = e.settle(t, next.ID)
	if next.State != store.AgentDone {
		t.Fatalf("replacement %s", next.State)
	}
	if got, _ := e.st.GetWorkflow(ctx, w.ID); got.DevRuns != 2 {
		t.Fatalf("dev runs %d", got.DevRuns)
	}

	// Tester-first is refused until the workflow has a developer or a PR.
	empty, err := e.st.CreateWorkflow(ctx, store.Workflow{ProjectPath: shop, Branch: "orch/empty", Workspace: shop})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.chats.SummonAgent(ctx, empty.ID, "reviewer", "sonnet", "Review"); err == nil ||
		!strings.Contains(err.Error(), "tester-first") {
		t.Fatalf("tester first: %v", err)
	}

	// Closing the workflow removes the checkout but keeps the branch.
	if err := e.chats.CloseWorkflow(ctx, w.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(w.Workspace); !os.IsNotExist(err) {
		t.Fatalf("workflow worktree still there")
	}
	if got, _ := e.st.GetWorkflow(ctx, w.ID); got.State != store.WorkflowClosed {
		t.Fatalf("workflow %s", got.State)
	}
	if out := gitOut(t, e.project, "branch", "--list", w.Branch); !strings.Contains(out, w.Branch) {
		t.Fatalf("branch %s was deleted", w.Branch)
	}
}

func TestSummonWorkflowRefusals(t *testing.T) {
	e := newChatEnv(t)
	ctx := context.Background()
	if _, err := e.chats.SummonWorkflow(ctx, WorkflowRequest{ProjectPath: "shop", Prompt: "x", Model: "sonnet"}); err == nil {
		t.Fatal("summoned in an untrusted project")
	}
	shop := trustShop(t, e)
	if _, err := e.chats.SummonWorkflow(ctx, WorkflowRequest{ProjectPath: shop, Prompt: "", Model: "sonnet"}); err == nil {
		t.Fatal("summoned without a task")
	}
	if _, err := e.chats.SummonWorkflow(ctx, WorkflowRequest{ProjectPath: shop, Prompt: "x", Model: "sol"}); err == nil {
		t.Fatal("summoned a model that cannot chat")
	}
}
