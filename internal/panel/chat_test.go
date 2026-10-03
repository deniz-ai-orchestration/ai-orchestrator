package panel

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/deniz-ai-orchestration/ai-orchestrator/internal/config"
	"github.com/deniz-ai-orchestration/ai-orchestrator/internal/quota"
	"github.com/deniz-ai-orchestration/ai-orchestrator/internal/runner"
	"github.com/deniz-ai-orchestration/ai-orchestrator/internal/store"
)

// replyContainer answers every turn with a done reply.
type replyContainer struct{}

func (replyContainer) Run(_ context.Context, _ runner.Spec, stdout, _ io.Writer) (runner.Result, error) {
	so, _ := json.Marshal(runner.TurnResult{Status: "done", Reply: "All done <b>here</b>."})
	line, _ := json.Marshal(map[string]any{"type": "result", "subtype": "success", "is_error": false,
		"session_id": "s1", "result": string(so), "structured_output": json.RawMessage(so)})
	_, _ = stdout.Write(append(line, '\n'))
	return runner.Result{}, nil
}

func (replyContainer) RemoveStale(context.Context) error { return nil }

type chatEnv struct {
	st  *store.Store
	srv *httptest.Server
}

func newChatEnv(t *testing.T) *chatEnv {
	t.Helper()
	cfg, err := config.Load("../../config.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	cfg.DataDir, cfg.SecretsDir = dir, filepath.Join(dir, "secrets")
	cfg.Projects.Dir = filepath.Join(dir, "projects")
	if err := os.MkdirAll(cfg.SecretsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg.SecretsDir, "claude_oauth_token"), []byte("tok"), 0o600); err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(cfg.Projects.Dir, "shop")
	for _, args := range [][]string{{"init", "-q", "-b", "main", repo}, {"-C", repo, "-c", "user.name=t", "-c", "user.email=t@t",
		"commit", "-q", "--allow-empty", "-m", "init"}} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	st, err := store.Open(context.Background(), filepath.Join(dir, "orch.db"))
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	chats := &runner.Chats{Store: st, Cfg: cfg, Containers: replyContainer{}, Projects: runner.Projects{Dir: cfg.Projects.Dir},
		Dir: filepath.Join(dir, "agents"), RunsDir: filepath.Join(dir, "runs"), User: "1000:1000", Log: log}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = chats.Run(ctx); close(done) }()
	cmd := &fakeCommands{}
	s := &Server{Store: st, Cfg: cfg, Quota: &quota.Tracker{Store: st, Cfg: cfg}, Commands: cmd, Agents: cmd, Chats: chats, Log: log}
	h, err := s.Handler()
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(func() {
		srv.Close()
		cancel()
		<-done
		st.Close()
	})
	for !chats.Running() {
		time.Sleep(time.Millisecond)
	}
	return &chatEnv{st: st, srv: srv}
}

func (e *chatEnv) postForm(t *testing.T, path string, origin bool, fields map[string]string, files map[string][]byte) (int, http.Header, string) {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	for k, v := range fields {
		_ = mw.WriteField(k, v)
	}
	for name, data := range files {
		fw, _ := mw.CreateFormFile("files", name)
		_, _ = fw.Write(data)
	}
	mw.Close()
	req, _ := http.NewRequest(http.MethodPost, e.srv.URL+path, &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	if origin {
		req.Header.Set("Origin", e.srv.URL)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, res.Header, string(b)
}

func (e *chatEnv) get(t *testing.T, path string) (int, string) {
	t.Helper()
	res, err := http.Get(e.srv.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(b)
}

func (e *chatEnv) waitIdle(t *testing.T, id int64) {
	t.Helper()
	for i := 0; i < 500; i++ {
		a, err := e.st.GetAgent(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if a.State != store.AgentWorking {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("agent still working")
}

func TestSummonForm(t *testing.T) {
	e := newChatEnv(t)
	_, body := e.get(t, "/")
	for _, want := range []string{`hx-post="/agents"`, `<option value="shop">shop</option>`, `value="claude-sonnet"`,
		`<option value="developer" data-model="claude-sonnet">developer</option>`, `id="chats"`} {
		if !strings.Contains(body, want) {
			t.Errorf("index lacks %q", want)
		}
	}
	form := body[strings.Index(body, `id="summon"`):strings.Index(body, `id="chats"`)]
	if strings.Contains(form, "codex-sol") || !strings.Contains(form, `value="claude-opus"`) {
		t.Error("the summon form offers a model that cannot chat yet")
	}
}

func TestSummonChatAndClose(t *testing.T) {
	e := newChatEnv(t)
	fields := map[string]string{"role": "developer", "model": "claude-sonnet", "project": "shop", "message": "Add a README"}
	if code, _, _ := e.postForm(t, "/agents", false, fields, nil); code != http.StatusForbidden {
		t.Fatalf("summon without Origin: %d", code)
	}
	code, h, body := e.postForm(t, "/agents", true, fields, map[string][]byte{"notes.md": []byte("# notes\n")})
	if code != http.StatusOK || h.Get("HX-Redirect") != "/agents/1" {
		t.Fatalf("summon: %d %q %s", code, h.Get("HX-Redirect"), body)
	}
	e.waitIdle(t, 1)

	_, page := e.get(t, "/agents/1")
	for _, want := range []string{"Add a README", "Files: notes.md", "All done &lt;b&gt;here&lt;/b&gt;.", `class="badge done"`, "Close agent"} {
		if !strings.Contains(page, want) {
			t.Errorf("agent page lacks %q", want)
		}
	}
	if _, cards := e.get(t, "/parts/chats"); !strings.Contains(cards, `href="/agents/1"`) || !strings.Contains(cards, "orch/1-add-a-readme") {
		t.Errorf("dashboard card missing:\n%s", cards)
	}

	_, _, body = e.postForm(t, "/agents/1/send", true, map[string]string{"message": "Now a LICENSE"}, map[string][]byte{"x.bin": {0, 1, 2}})
	if !strings.Contains(body, "not a text file") {
		t.Fatalf("binary upload: %s", body)
	}
	code, h, body = e.postForm(t, "/agents/1/send", true, map[string]string{"message": "Now a LICENSE"}, nil)
	if code != http.StatusOK || !strings.Contains(body, "Sent.") || !strings.Contains(h.Get("HX-Trigger"), "sent") {
		t.Fatalf("send: %d %s %v", code, body, h)
	}
	e.waitIdle(t, 1)
	if _, msgs := e.get(t, "/agents/1/parts/messages"); strings.Count(msgs, "All done") != 2 {
		t.Fatalf("second reply missing:\n%s", msgs)
	}

	_, h, body = e.postForm(t, "/agents/1/close", true, nil, nil)
	if h.Get("HX-Redirect") != "/" || !strings.Contains(body, "Branch orch/1-add-a-readme stays in shop") {
		t.Fatalf("close: %v %s", h, body)
	}
	if _, cards := e.get(t, "/parts/chats"); strings.Contains(cards, `href="/agents/1"`) {
		t.Error("closed agent still on the dashboard")
	}
	if code, _ := e.get(t, "/agents/99"); code != http.StatusNotFound {
		t.Errorf("unknown agent: %d", code)
	}
	if code, _ := e.get(t, "/agents/1/parts/nope"); code != http.StatusNotFound {
		t.Errorf("unknown part: %d", code)
	}
}

func TestSummonErrorsFlash(t *testing.T) {
	e := newChatEnv(t)
	_, h, body := e.postForm(t, "/agents", true, map[string]string{"role": "developer", "model": "codex-sol", "project": "shop", "message": "x"}, nil)
	if h.Get("HX-Redirect") != "" || !strings.Contains(body, "Could not summon the agent") {
		t.Fatalf("codex summon: %v %s", h, body)
	}
}

func TestNoChats(t *testing.T) {
	e := newEnv(t)
	_, _, body := e.get(t, "/")
	if !strings.Contains(body, "Agents are not running in this orch.") {
		t.Error("summon form should explain why it is off")
	}
}

func TestSendToTesters(t *testing.T) {
	e := newChatEnv(t)
	ctx := context.Background()
	fields := map[string]string{"role": "developer", "model": "claude-sonnet", "project": "shop", "message": "Add a README"}
	if code, _, body := e.postForm(t, "/agents", true, fields, nil); code != http.StatusOK {
		t.Fatalf("summon: %d %s", code, body)
	}
	e.waitIdle(t, 1)
	if _, head := e.get(t, "/agents/1/parts/agent-head"); !strings.Contains(head, `hx-post="/agents/1/test"`) {
		t.Fatalf("no Send to testers button:\n%s", head)
	}
	if _, _, body := e.postForm(t, "/agents/1/test", true, nil, nil); !strings.Contains(body, "no commits") {
		t.Fatalf("send to testers with no commits: %s", body)
	}

	if err := e.st.SetAgentState(ctx, 1, store.AgentTesting); err != nil {
		t.Fatal(err)
	}
	if _, err := e.st.CreateAgent(ctx, store.Agent{Role: "reviewer", Provider: "claude", Model: "claude-opus", Project: "shop",
		Branch: "b", ParentID: 1, Round: 1}); err != nil {
		t.Fatal(err)
	}
	_, head := e.get(t, "/agents/1/parts/agent-head")
	for _, want := range []string{`href="/agents/2"`, "round 1", "Stop testers"} {
		if !strings.Contains(head, want) {
			t.Errorf("developer head lacks %q:\n%s", want, head)
		}
	}
	if strings.Contains(head, `/agents/1/test"`) {
		t.Error("Send to testers offered while testing")
	}
	if _, page := e.get(t, "/agents/2"); !strings.Contains(page, `testing <a href="/agents/1">agent 1</a>`) {
		t.Error("tester page does not link its developer")
	}
	if _, cards := e.get(t, "/parts/chats"); !strings.Contains(cards, "Testing agent 1") {
		t.Error("tester card does not name its developer")
	}
}
