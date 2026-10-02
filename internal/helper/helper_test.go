package helper

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/deniz-ai-orchestration/ai-orchestrator/internal/config"
)

func TestOllamaSummarize(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/generate" {
			w.WriteHeader(404)
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		_, _ = w.Write([]byte(`{"response":"TestX fails: nil map"}`))
	}))
	defer srv.Close()
	o := &Ollama{Endpoint: srv.URL + "/", Model: "q"}
	out := o.Summarize(context.Background(), "Summarize:", "a\nb\nFAIL TestX")
	if !strings.HasPrefix(out, "TestX fails: nil map") || !strings.Contains(out, "FAIL TestX") {
		t.Fatalf("out %q", out)
	}
	if got["model"] != "q" || got["stream"] != false || !strings.HasPrefix(got["prompt"].(string), "Summarize:") {
		t.Fatalf("request %v", got)
	}
}

func TestOllamaFallsBack(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
	}))
	defer srv.Close()
	log := strings.Repeat("noise\n", 100) + "FAIL TestX\n"
	for _, o := range []*Ollama{{Endpoint: srv.URL, Model: "q", Fallback: 50}, {Endpoint: "http://127.0.0.1:1", Model: "q", Fallback: 50}} {
		out := o.Summarize(context.Background(), "x", log)
		if len(out) > 60 || !strings.Contains(out, "FAIL TestX") {
			t.Fatalf("fallback %q", out)
		}
	}
}

func TestTail(t *testing.T) {
	if Tail("abc", 10) != "abc" {
		t.Fatal("short input changed")
	}
	if got := Tail("line1\nline2\nline3\n", 9); got != "…\nline3\n" {
		t.Fatalf("got %q", got)
	}
}

func TestFromConfig(t *testing.T) {
	cfg, err := config.Parse([]byte(`
github: { trusted_actor: d }
runner: { git_name: a, git_email: a@b }
providers: { claude: { cli: claude }, ollama: { endpoint: http://localhost:11434 } }
models: { s: { provider: claude, model: s }, l: { provider: ollama, model: qwen } }
roles: { developer: { pool: [s] }, helper: { pool: [l] } }
`))
	if err != nil {
		t.Fatal(err)
	}
	if o, ok := FromConfig(cfg).(*Ollama); !ok || o.Model != "qwen" {
		t.Fatalf("got %#v", FromConfig(cfg))
	}
	delete(cfg.Roles, "helper")
	if _, ok := FromConfig(cfg).(Trim); !ok {
		t.Fatal("no helper role should fall back to Trim")
	}
}
