package panel

import (
	"bufio"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/deniz-ai-orchestration/ai-orchestrator/internal/config"
	"github.com/deniz-ai-orchestration/ai-orchestrator/internal/engine"
	"github.com/deniz-ai-orchestration/ai-orchestrator/internal/quota"
	"github.com/deniz-ai-orchestration/ai-orchestrator/internal/store"
)

type fakeCommands struct {
	mu   sync.Mutex
	got  []string
	stop []int64
}

func (f *fakeCommands) Command(_ context.Context, text string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.got = append(f.got, text)
	return "ok: " + text
}

func (f *fakeCommands) Stop(id int64) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stop = append(f.stop, id)
	return id == 1
}

type env struct {
	st  *store.Store
	srv *httptest.Server
	cmd *fakeCommands
	dir string
}

func newEnv(t *testing.T) *env {
	t.Helper()
	cfg, err := config.Load("../../config.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	cfg.DataDir = dir
	st, err := store.Open(context.Background(), filepath.Join(dir, "orch.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	cmd := &fakeCommands{}
	s := &Server{Store: st, Cfg: cfg, Quota: &quota.Tracker{Store: st, Cfg: cfg}, Commands: cmd, Agents: cmd,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Follow: 10 * time.Millisecond}
	h, err := s.Handler()
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return &env{st: st, srv: srv, cmd: cmd, dir: dir}
}

func (e *env) get(t *testing.T, path string) (int, http.Header, string) {
	t.Helper()
	res, err := http.Get(e.srv.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, res.Header, string(b)
}

func (e *env) post(t *testing.T, origin string, form url.Values) (int, http.Header, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, e.srv.URL+"/act", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, res.Header, string(b)
}

// startRun records a developer run for a new task with the given stdout.
func (e *env) startRun(t *testing.T, stdout string) (store.Task, int64, string) {
	t.Helper()
	ctx := context.Background()
	task, err := e.st.CreateTask(ctx, "o/r", 7, "Add sub", "body")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.st.ApplyEvent(ctx, task.ID, engine.Event{Kind: engine.EvDevStarted, Model: "claude-sonnet"}, config.Limits{DevRuns: 8}); err != nil {
		t.Fatal(err)
	}
	id, err := e.st.StartRun(ctx, store.Run{TaskID: task.ID, Role: "developer", Work: "implement", Provider: "claude", Model: "claude-sonnet"})
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(e.dir, "runs", "1")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "stdout.jsonl"), []byte(stdout), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := e.st.SetRunLogDir(ctx, id, dir); err != nil {
		t.Fatal(err)
	}
	return task, id, dir
}

func fixture(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("../../testdata/fixtures", path))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestIndex(t *testing.T) {
	e := newEnv(t)
	e.startRun(t, fixture(t, "claude/success/stdout"))
	code, h, body := e.get(t, "/")
	if code != http.StatusOK {
		t.Fatalf("status %d: %s", code, body)
	}
	for _, want := range []string{
		`id="agents"`, `id="tasks"`, `id="roles"`, `id="quota"`, "Pause all",
		"Run 1", "▸ Edit /tmp/orch-scratch/main.go", "■ success", // card preview
		`title="o/r">#7</a>`, "developing", // task row
		"codex-sol", "<td>ollama-local</td>", // next models
		`value="claude-opus"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("index lacks %q", want)
		}
	}
	if !strings.Contains(h.Get("Content-Security-Policy"), "script-src 'self'") || h.Get("X-Frame-Options") != "DENY" {
		t.Errorf("security headers %v", h)
	}
	if strings.Contains(body, "session started") {
		t.Error("preview should keep only the last lines")
	}
}

func TestParts(t *testing.T) {
	e := newEnv(t)
	for _, p := range []string{"agents", "tasks", "roles", "quota", "pause"} {
		if code, _, body := e.get(t, "/parts/"+p); code != http.StatusOK || strings.Contains(body, "<html") {
			t.Errorf("%s: %d %q", p, code, body)
		}
	}
	if code, _, _ := e.get(t, "/parts/nope"); code != http.StatusNotFound {
		t.Errorf("unknown part: %d", code)
	}
	if code, h, _ := e.get(t, "/static/vendor/htmx.min.js"); code != http.StatusOK || !strings.Contains(h.Get("Content-Type"), "javascript") {
		t.Errorf("static: %d %v", code, h.Get("Content-Type"))
	}
	if code, _, _ := e.get(t, "/runs/99"); code != http.StatusNotFound {
		t.Errorf("missing run: %d", code)
	}
}

func TestGuard(t *testing.T) {
	e := newEnv(t)
	req, _ := http.NewRequest(http.MethodGet, e.srv.URL+"/", nil)
	req.Host = "pc2.attacker.example:8787"
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusMisdirectedRequest {
		t.Errorf("DNS-named host: %d", res.StatusCode)
	}
	form := url.Values{"verb": {"pause"}}
	if code, _, _ := e.post(t, "", form); code != http.StatusForbidden {
		t.Errorf("post without Origin: %d", code)
	}
	if code, _, _ := e.post(t, "http://evil.example", form); code != http.StatusForbidden {
		t.Errorf("cross-origin post: %d", code)
	}
	if len(e.cmd.got) != 0 {
		t.Fatalf("refused posts ran commands: %v", e.cmd.got)
	}
	for _, h := range []string{"192.168.1.20:8787", "localhost:8787", "[::1]:8787", "127.0.0.1"} {
		if !directHost(h) {
			t.Errorf("%s refused", h)
		}
	}
}

func TestActions(t *testing.T) {
	e := newEnv(t)
	origin := e.srv.URL
	cases := []struct {
		form url.Values
		want string // command run, or "" for none
		body string
	}{
		{url.Values{"verb": {"use"}, "a": {"reviewer"}, "b": {"claude-opus"}}, "/use reviewer claude-opus", "ok: /use reviewer claude-opus"},
		{url.Values{"verb": {"retry"}, "a": {"3"}}, "/retry 3", ""},
		{url.Values{"verb": {"pause"}}, "/pause", ""},
		{url.Values{"verb": {"disable"}, "a": {"kimi"}}, "/disable kimi", ""},
		{url.Values{"verb": {"use"}, "a": {"reviewer"}, "b": {"x /pause"}}, "", "not allowed"},
		{url.Values{"verb": {"why"}, "a": {"3"}}, "", "Unknown action"},
		{url.Values{"verb": {"retry"}}, "", "Unknown action"},
		{url.Values{"verb": {"stop"}, "a": {"1"}}, "", "Stopping task 1"},
		{url.Values{"verb": {"stop"}, "a": {"2"}}, "", "no running agent"},
	}
	for _, c := range cases {
		before := len(e.cmd.got)
		code, h, body := e.post(t, origin, c.form)
		if code != http.StatusOK || h.Get("HX-Trigger") != "refresh" {
			t.Errorf("%v: %d %v", c.form, code, h)
		}
		ran := e.cmd.got[before:]
		if c.want == "" && len(ran) != 0 || c.want != "" && (len(ran) != 1 || ran[0] != c.want) {
			t.Errorf("%v: ran %v, want %q", c.form, ran, c.want)
		}
		if c.body != "" && !strings.Contains(body, c.body) {
			t.Errorf("%v: body %q", c.form, body)
		}
	}
	if len(e.cmd.stop) != 2 {
		t.Errorf("stops %v", e.cmd.stop)
	}
}

// readEvents reads server-sent events until the "end" event.
func readEvents(t *testing.T, body io.Reader) (lines []string, end string) {
	t.Helper()
	sc := bufio.NewScanner(body)
	event := ""
	for sc.Scan() {
		l := sc.Text()
		switch {
		case strings.HasPrefix(l, "event: "):
			event = strings.TrimPrefix(l, "event: ")
		case strings.HasPrefix(l, "data: "):
			d := strings.TrimPrefix(l, "data: ")
			if event == "end" {
				return lines, d
			}
			lines = append(lines, d)
		case l == "":
			event = ""
		}
	}
	t.Fatalf("stream closed without an end event; got %q", lines)
	return
}

func TestStreamFollowsRunToTheEnd(t *testing.T) {
	e := newEnv(t)
	all := strings.SplitAfter(fixture(t, "claude/success/stdout"), "\n")
	// The run has written its first line and half of the second so far.
	half := len(all[2]) / 2
	_, id, dir := e.startRun(t, all[0]+all[1]+all[2][:half])

	res, err := http.Get(e.srv.URL + "/runs/1/stream")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if ct := res.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content type %q", ct)
	}
	go func() {
		time.Sleep(50 * time.Millisecond)
		f, _ := os.OpenFile(filepath.Join(dir, "stdout.jsonl"), os.O_APPEND|os.O_WRONLY, 0)
		f.WriteString(all[2][half:] + strings.Join(all[3:], ""))
		f.Close()
		os.WriteFile(filepath.Join(dir, "stderr.log"), []byte("warning: one\n\x1b[31mtwo\x1b[0m\n"), 0o600)
		e.st.FinishRun(context.Background(), id, store.RunSucceeded, "ok", 0, 1, 2)
	}()
	lines, end := readEvents(t, res.Body)
	want := []string{"● session started (claude-sonnet-5-5)", "▸ Read /tmp/orch-scratch/main.go", "  ↳ 1 package main (+7 lines)",
		"▸ Edit /tmp/orch-scratch/main.go"}
	for i, w := range want {
		if i >= len(lines) || lines[i] != w {
			t.Fatalf("line %d: got %q, want %q\nall: %q", i, lines, w, lines)
		}
	}
	got := strings.Join(lines, "\n")
	for _, w := range []string{"■ success", "stderr: warning: one", "stderr: [31mtwo[0m"} {
		if !strings.Contains(got, w) {
			t.Errorf("missing %q in %q", w, lines)
		}
	}
	if strings.ContainsRune(got, 0x1b) {
		t.Error("escape character reached the terminal")
	}
	if end != "succeeded (ok)" {
		t.Errorf("end %q", end)
	}
}

func TestRunPage(t *testing.T) {
	e := newEnv(t)
	e.startRun(t, "")
	code, _, body := e.get(t, "/runs/1")
	if code != http.StatusOK || !strings.Contains(body, `data-stream="/runs/1/stream"`) || !strings.Contains(body, "task 1: Add sub") {
		t.Fatalf("%d %s", code, body)
	}
}
