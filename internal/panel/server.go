package panel

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"time"

	"github.com/deniz-ai-orchestration/ai-orchestrator/internal/config"
	"github.com/deniz-ai-orchestration/ai-orchestrator/internal/quota"
	"github.com/deniz-ai-orchestration/ai-orchestrator/internal/runner"
	"github.com/deniz-ai-orchestration/ai-orchestrator/internal/store"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed static
var staticFS embed.FS

// Commander runs the same commands as Telegram (/retry 4, /use reviewer
// codex-sol, /pause) and returns the reply.
type Commander interface {
	Command(ctx context.Context, text string) string
}

// Stopper stops a task's running agent.
type Stopper interface {
	Stop(taskID int64) bool
}

// Server is the control panel. It has no login: it listens on PC2's LAN
// address only, refuses requests addressed to a DNS name (DNS rebinding),
// and accepts form posts only from its own pages.
type Server struct {
	Store    *store.Store
	Cfg      *config.Config
	Quota    *quota.Tracker
	Commands Commander
	Agents   Stopper
	// Chats runs summoned agents; nil hides the summon form.
	Chats *runner.Chats
	Log   *slog.Logger
	// Follow is how often a live run view checks for new output (default
	// 500ms).
	Follow time.Duration
	Now    func() time.Time

	tmpl *template.Template
}

func (s *Server) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// Run serves the panel on the configured address until ctx is done.
func (s *Server) Run(ctx context.Context) error {
	h, err := s.Handler()
	if err != nil {
		return err
	}
	srv := &http.Server{Addr: s.Cfg.Panel.Listen, Handler: h, ReadHeaderTimeout: 10 * time.Second}
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	s.Log.Info("control panel listening", "addr", "http://"+s.Cfg.Panel.Listen)
	select {
	case err := <-errc:
		s.Log.Error("control panel stopped", "err", err)
		return err
	case <-ctx.Done():
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
		return nil
	}
}

// Handler returns the panel's routes behind its request guard.
func (s *Server) Handler() (http.Handler, error) {
	t, err := template.New("").Funcs(funcs).ParseFS(templateFS, "templates/*.html")
	if err != nil {
		return nil, err
	}
	s.tmpl = t
	static, err := fs.Sub(staticFS, "static")
	if err != nil {
		return nil, err
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.index)
	mux.HandleFunc("GET /parts/{name}", s.part)
	mux.HandleFunc("POST /act", s.act)
	mux.HandleFunc("GET /runs/{id}", s.runPage)
	mux.HandleFunc("GET /runs/{id}/stream", s.stream)
	mux.HandleFunc("POST /agents", s.summon)
	mux.HandleFunc("POST /projects/inspect", s.inspectProject)
	mux.HandleFunc("POST /projects/trust", s.trustProject)
	mux.HandleFunc("GET /agents/{id}", s.agentPage)
	mux.HandleFunc("GET /agents/{id}/parts/{name}", s.agentPart)
	mux.HandleFunc("POST /agents/{id}/send", s.send)
	mux.HandleFunc("POST /agents/{id}/stop", s.stopAgent)
	mux.HandleFunc("POST /agents/{id}/test", s.testAgent)
	mux.HandleFunc("POST /agents/{id}/pr", s.openPR)
	mux.HandleFunc("POST /agents/{id}/close", s.closeAgent)
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(static)))
	return guard(mux), nil
}

// guard applies the panel's request rules and security headers.
func guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !directHost(r.Host) {
			http.Error(w, "open the panel by IP address", http.StatusMisdirectedRequest)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead && !sameOrigin(r) {
			http.Error(w, "cross-origin request refused", http.StatusForbidden)
			return
		}
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		h.Set("X-Frame-Options", "DENY")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}

// directHost accepts an IP address or localhost. A DNS name that resolves to
// PC2 could belong to a rebinding attack page.
func directHost(hostport string) bool {
	host, _, err := net.SplitHostPort(hostport)
	if err != nil {
		host = hostport
	}
	return host == "localhost" || net.ParseIP(host) != nil || (len(host) > 2 && host[0] == '[' && net.ParseIP(host[1:len(host)-1]) != nil)
}

// sameOrigin requires the browser's Origin header to name this host.
func sameOrigin(r *http.Request) bool {
	o := r.Header.Get("Origin")
	if o == "" {
		return false
	}
	u, err := url.Parse(o)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host == r.Host
}

func (s *Server) render(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if err := s.tmpl.ExecuteTemplate(w, name, data); err != nil {
		s.Log.Error("render", "template", name, "err", err)
	}
}

func (s *Server) fail(w http.ResponseWriter, err error) {
	s.Log.Error("panel request failed", "err", err)
	http.Error(w, "internal error", http.StatusInternalServerError)
}

func (s *Server) index(w http.ResponseWriter, r *http.Request) {
	v, err := s.view(r.Context(), "projects", "summon", "chats", "agents", "tasks", "roles", "quota")
	if err != nil {
		s.fail(w, err)
		return
	}
	s.render(w, "index.html", v)
}

func (s *Server) part(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	switch name {
	case "projects", "chats", "agents", "tasks", "roles", "quota", "pause":
	default:
		http.NotFound(w, r)
		return
	}
	v, err := s.view(r.Context(), name)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.render(w, name, v)
}

// arg is what a form may put in a command: names and numbers, never a space
// that would add arguments.
var arg = regexp.MustCompile(`^[A-Za-z0-9._:/-]{1,64}$`)

// commands maps each panel action to its command and argument count.
var commands = map[string]int{"pause": 0, "resume": 0, "retry": 1, "cancel": 1, "use": 2, "enable": 1, "disable": 1}

func (s *Server) act(w http.ResponseWriter, r *http.Request) {
	verb := r.PostFormValue("verb")
	args := []string{}
	for _, k := range []string{"a", "b"} {
		if v := r.PostFormValue(k); v != "" {
			args = append(args, v)
		}
	}
	for _, a := range args {
		if !arg.MatchString(a) {
			s.flash(w, "That value is not allowed.")
			return
		}
	}
	if verb == "stop" {
		s.flash(w, s.stop(args))
		return
	}
	n, ok := commands[verb]
	if !ok || len(args) != n {
		s.flash(w, "Unknown action.")
		return
	}
	text := "/" + verb
	for _, a := range args {
		text += " " + a
	}
	s.Log.Info("panel action", "command", text)
	s.flash(w, s.Commands.Command(r.Context(), text))
}

func (s *Server) stop(args []string) string {
	if len(args) != 1 {
		return "Unknown action."
	}
	id, err := strconv.ParseInt(args[0], 10, 64)
	if err != nil {
		return "Unknown action."
	}
	s.Log.Info("panel action", "command", "stop", "task", id)
	if s.Agents != nil && s.Agents.Stop(id) {
		return fmt.Sprintf("Stopping task %d's agent. The task waits for you; Retry resumes it.", id)
	}
	return fmt.Sprintf("Task %d has no running agent.", id)
}

// flash answers an action; every section refreshes on the "refresh" event.
func (s *Server) flash(w http.ResponseWriter, msg string) {
	w.Header().Set("HX-Trigger", "refresh")
	s.render(w, "flash", msg)
}

func (s *Server) runPage(w http.ResponseWriter, r *http.Request) {
	run, err := s.run(r)
	if err != nil {
		s.notFound(w, r, err)
		return
	}
	card, err := s.card(r.Context(), run, false)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.render(w, "run.html", card)
}

func (s *Server) run(r *http.Request) (store.Run, error) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		return store.Run{}, store.ErrNotFound
	}
	return s.Store.GetRun(r.Context(), id)
}

func (s *Server) notFound(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	s.fail(w, err)
}
