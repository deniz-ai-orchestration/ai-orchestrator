package panel

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/deniz-ai-orchestration/ai-orchestrator/internal/store"
)

func trustTestProject(t *testing.T, e *chatEnv, git bool) string {
	t.Helper()
	path, err := e.chats.Projects.Resolve("shop")
	if err != nil {
		t.Fatal(err)
	}
	if err := e.st.UpsertProject(context.Background(), store.Project{Path: path, Trusted: true, HasGit: git}); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestProjectAndWorkflowPages(t *testing.T) {
	e := newChatEnv(t)
	shop := trustTestProject(t, e, true)

	code, h, body := e.postForm(t, "/workflows", true, map[string]string{
		"project": shop, "model": "claude-sonnet", "message": "Add a README"}, nil)
	if code != 200 || h.Get("HX-Redirect") != "/workflows/1" {
		t.Fatalf("new workflow: %d %q %s", code, h.Get("HX-Redirect"), body)
	}
	e.waitIdle(t, 1)

	if _, page := e.get(t, "/projects/1"); !strings.Contains(page, "Workflow 1") ||
		!strings.Contains(page, "orch/1-add-a-readme") || !strings.Contains(page, "Start workflow") {
		t.Errorf("project page:\n%s", page)
	}
	if _, cards := e.get(t, "/parts/projects"); !strings.Contains(cards, "/workflows/1") && !strings.Contains(cards, "1 workflows") {
		// The sidebar links the project; counts appear once open.
		if !strings.Contains(cards, "/projects/1") && !strings.Contains(cards, shop) {
			t.Errorf("sidebar lacks the project:\n%s", cards)
		}
	}

	_, page := e.get(t, "/workflows/1")
	for _, want := range []string{"workflow 1", "orch/1-add-a-readme", `href="/agents/1"`, "Send to testers", "New agent on this workflow"} {
		if !strings.Contains(page, want) {
			t.Errorf("workflow page lacks %q", want)
		}
	}
	if strings.Contains(page, "Open PR") {
		t.Error("Open PR offered without a publisher")
	}
	if _, agent := e.get(t, "/agents/1"); !strings.Contains(agent, `href="/workflows/1"`) {
		t.Error("agent page lacks the workflow breadcrumb")
	}

	// A replacement developer continues the same branch.
	code, h, body = e.postForm(t, "/workflows/1/agent", true, map[string]string{
		"role": "developer", "model": "claude-sonnet", "message": "Keep going"}, nil)
	if code != 200 || h.Get("HX-Redirect") != "/agents/2" {
		t.Fatalf("new agent: %d %q %s", code, h.Get("HX-Redirect"), body)
	}
	e.waitIdle(t, 2)
	if _, page := e.get(t, "/workflows/1"); !strings.Contains(page, `href="/agents/2"`) {
		t.Error("workflow page lacks the replacement agent")
	}

	_, h, body = e.postForm(t, "/workflows/1/close", true, nil, nil)
	if h.Get("HX-Redirect") != "/projects/1" || !strings.Contains(body, "Branch orch/1-add-a-readme stays") {
		t.Fatalf("close: %v %s", h, body)
	}
	if _, page := e.get(t, "/projects/1"); strings.Contains(page, "Workflow 1") {
		t.Error("closed workflow still listed")
	}
}

func TestAutonomousSwitch(t *testing.T) {
	e := newChatEnv(t)
	shop := trustTestProject(t, e, true)
	chatOnly := filepath.Join(filepath.Dir(shop), "notes")
	if err := os.MkdirAll(chatOnly, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := e.st.UpsertProject(context.Background(), store.Project{Path: chatOnly, Trusted: true}); err != nil {
		t.Fatal(err)
	}

	if _, _, body := e.postForm(t, "/projects/autonomous", true, map[string]string{"path": shop, "on": "1"}, nil); !strings.Contains(body, "Autonomous loop on") {
		t.Fatalf("switch on: %s", body)
	}
	if _, page := e.get(t, "/projects/1"); !strings.Contains(page, "autonomous on") {
		t.Error("project page does not show the autonomous loop")
	}
	if _, _, body := e.postForm(t, "/projects/autonomous", true, map[string]string{"path": chatOnly, "on": "1"}, nil); !strings.Contains(body, "stay manual") {
		t.Fatalf("chat-only switch on: %s", body)
	}
	p, err := e.st.GetProject(context.Background(), chatOnly)
	if err != nil || p.Autonomous {
		t.Fatalf("chat-only project %+v %v", p, err)
	}
	if _, _, body := e.postForm(t, "/projects/autonomous", true, map[string]string{"path": shop, "on": "0"}, nil); !strings.Contains(body, "manual") {
		t.Fatalf("switch off: %s", body)
	}
}

func TestNewProjectFlow(t *testing.T) {
	e := newChatEnv(t)
	shop, err := e.chats.Projects.Resolve("shop")
	if err != nil {
		t.Fatal(err)
	}
	form := map[string]string{"name": "Shop", "description": "The test store.", "path": shop}
	_, _, body := e.postForm(t, "/projects/inspect", true, form, nil)
	for _, want := range []string{"Trust this folder for <strong>Shop</strong>", "The test store.",
		`name="description" value="The test store."`, "Create project"} {
		if !strings.Contains(body, want) {
			t.Fatalf("confirm lacks %q:\n%s", want, body)
		}
	}
	if _, _, body := e.postForm(t, "/projects/trust", true, form, nil); !strings.Contains(body, "Trusted") {
		t.Fatalf("trust: %s", body)
	}
	p, err := e.st.GetProject(context.Background(), shop)
	if err != nil || p.Name != "Shop" || p.Description != "The test store." || !p.Trusted {
		t.Fatalf("project %+v %v", p, err)
	}
	if _, page := e.get(t, "/projects/1"); !strings.Contains(page, "Shop") ||
		!strings.Contains(page, "The test store.") || !strings.Contains(page, "Roles") {
		t.Errorf("project page:\n%s", page)
	}
	if _, cards := e.get(t, "/"); !strings.Contains(cards, "Shop") || !strings.Contains(cards, "The test store.") {
		t.Error("dashboard lacks the project card")
	}
	// A name is required; the description is capped.
	if _, _, body := e.postForm(t, "/projects/inspect", true, map[string]string{"path": shop}, nil); !strings.Contains(body, "a name") {
		t.Fatalf("nameless: %s", body)
	}
	if _, _, body := e.postForm(t, "/projects/inspect", true,
		map[string]string{"name": "x", "description": strings.Repeat("d", 501), "path": shop}, nil); !strings.Contains(body, "500") {
		t.Fatalf("long description: %s", body)
	}
}

func TestWorkflowRunsAreScoped(t *testing.T) {
	e := newChatEnv(t)
	shop := trustTestProject(t, e, true)
	for i, prompt := range []string{"First job", "Second job"} {
		code, h, body := e.postForm(t, "/workflows", true, map[string]string{
			"project": shop, "model": "claude-sonnet", "message": prompt}, nil)
		if want := "/workflows/" + strconv.Itoa(i+1); code != 200 || h.Get("HX-Redirect") != want {
			t.Fatalf("new workflow: %d %q %s", code, h.Get("HX-Redirect"), body)
		}
		e.waitIdle(t, int64(i+1))
	}
	if rs, _ := e.st.WorkflowRuns(context.Background(), 1, 10); len(rs) != 1 || rs[0].AgentID != 1 {
		t.Fatalf("workflow 1 runs: %+v", rs)
	}
	_, first := e.get(t, "/workflows/1")
	_, second := e.get(t, "/workflows/2")
	for _, tc := range []struct {
		page, want, leak string
	}{{first, "Run 1", "Run 2"}, {second, "Run 2", "Run 1"}} {
		if !strings.Contains(tc.page, tc.want) || strings.Contains(tc.page, tc.leak) {
			t.Errorf("runs leak across workflows: want %q without %q", tc.want, tc.leak)
		}
	}
}

func TestBrowseProjects(t *testing.T) {
	e := newChatEnv(t)
	_, body := e.get(t, "/projects/browse")
	if !strings.Contains(body, "Trusted roots") {
		t.Fatalf("roots:\n%s", body)
	}
	shop, err := e.chats.Projects.Resolve("shop")
	if err != nil {
		t.Fatal(err)
	}
	_, body = e.get(t, "/projects/browse?path="+shop)
	for _, want := range []string{"Use this directory", `data-pick-dir="` + shop + `"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("picker lacks %q:\n%s", want, body)
		}
	}
	if _, body := e.get(t, "/projects/browse?path=/tmp"); !strings.Contains(body, "outside the trusted project roots") {
		t.Fatalf("outside roots: %s", body)
	}
}

func TestProjectByID(t *testing.T) {
	e := newChatEnv(t)
	shop := trustTestProject(t, e, true)
	if code, _ := e.get(t, "/projects/99"); code != 404 {
		t.Fatalf("unknown project: %d", code)
	}
	if code, _ := e.get(t, "/projects/nope"); code != 404 {
		t.Fatalf("bad id: %d", code)
	}
	p, err := e.st.GetProject(context.Background(), shop)
	if err != nil {
		t.Fatal(err)
	}
	if p.ID == 0 {
		t.Fatal("project has no id")
	}
	if _, page := e.get(t, "/projects/"+strconv.Itoa(int(p.ID))); !strings.Contains(page, "New workflow") {
		t.Error("project page missing")
	}
	if _, cards := e.get(t, "/"); !strings.Contains(cards, "/projects/"+strconv.Itoa(int(p.ID))) {
		t.Error("sidebar lacks the /projects/<id> link")
	}
	if _, err := e.st.GetProjectByID(context.Background(), 999); err != store.ErrNotFound {
		t.Fatalf("missing project: %v", err)
	}
}
