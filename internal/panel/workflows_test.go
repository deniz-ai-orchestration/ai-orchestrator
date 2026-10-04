package panel

import (
	"context"
	"net/url"
	"os"
	"path/filepath"
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

	if _, page := e.get(t, "/project?path="+url.QueryEscape(shop)); !strings.Contains(page, "Workflow 1") ||
		!strings.Contains(page, "orch/1-add-a-readme") || !strings.Contains(page, "Start workflow") {
		t.Errorf("project page:\n%s", page)
	}
	if _, cards := e.get(t, "/parts/projects"); !strings.Contains(cards, "/workflows/1") && !strings.Contains(cards, "1 workflows") {
		// The sidebar links the project; counts appear once open.
		if !strings.Contains(cards, url.QueryEscape(shop)) && !strings.Contains(cards, shop) {
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

	if _, _, body := e.postForm(t, "/workflows/1/close", true, nil, nil); !strings.Contains(body, "Branch orch/1-add-a-readme stays") {
		t.Fatalf("close: %s", body)
	}
	if _, page := e.get(t, "/project?path="+url.QueryEscape(shop)); strings.Contains(page, "Workflow 1") {
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
	if _, page := e.get(t, "/project?path="+url.QueryEscape(shop)); !strings.Contains(page, "autonomous on") {
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
