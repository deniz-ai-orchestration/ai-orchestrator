package panel

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strconv"
	"strings"

	"github.com/deniz-ai-orchestration/ai-orchestrator/internal/runner"
	"github.com/deniz-ai-orchestration/ai-orchestrator/internal/store"
)

// AgentCard is one summoned agent on the dashboard and its page.
type AgentCard struct {
	store.Agent
	Since   string // time since the last change
	Last    string // the latest message, shortened
	RunID   int64  // the running turn, if any
	Working bool
}

// AgentPage is an agent's conversation page.
type AgentPage struct {
	AgentCard
	Messages []MessageView
}

// MessageView is one chat message.
type MessageView struct {
	store.Message
	Time string
}

// SummonForm fills the summon form.
type SummonForm struct {
	Enabled   bool
	Why       string // why agents cannot be summoned
	Roles     []string
	Projects  []string
	Providers []string
	Choices   []ModelChoice
	// Default is the model preselected for each role: its chosen model, or
	// the first in its fallback order that can chat.
	Default map[string]string
}

// maxUpload bounds a whole summon or message post: the files plus the text.
const maxUpload = runner.MaxFiles*runner.MaxFileBytes + 1<<20

// lastLen is how much of the latest message a card shows.
const lastLen = 160

func (s *Server) chatCards(ctx context.Context, v *View) error {
	if s.Chats == nil {
		return nil
	}
	as, err := s.Store.OpenAgents(ctx)
	if err != nil {
		return err
	}
	for _, a := range as {
		c, err := s.agentCard(ctx, a)
		if err != nil {
			return err
		}
		v.Chats = append(v.Chats, c)
	}
	return nil
}

func (s *Server) agentCard(ctx context.Context, a store.Agent) (AgentCard, error) {
	c := AgentCard{Agent: a, Working: a.State == store.AgentWorking}
	if !a.UpdatedAt.IsZero() {
		c.Since = duration(s.now().Sub(a.UpdatedAt))
	}
	ms, err := s.Store.Messages(ctx, a.ID)
	if err != nil {
		return c, err
	}
	if len(ms) > 0 {
		c.Last = shorten(ms[len(ms)-1].Body, lastLen)
	}
	runs, err := s.Store.AgentRuns(ctx, a.ID, 1)
	if err != nil {
		return c, err
	}
	if len(runs) > 0 && runs[0].Status == store.RunRunning {
		c.RunID = runs[0].ID
	}
	return c, nil
}

func (s *Server) summonForm(ctx context.Context, v *View) error {
	f := SummonForm{Default: map[string]string{}}
	v.Summon = &f
	if s.Chats == nil {
		f.Why = "Agents are not running in this orch."
		return nil
	}
	projects, err := s.Chats.Projects.List()
	if err != nil {
		f.Why = "No project folders: " + err.Error()
		return nil
	}
	if len(projects) == 0 {
		f.Why = "No git repositories in " + s.Cfg.Projects.Dir + "."
		return nil
	}
	f.Projects, f.Roles = projects, s.Chats.Roles()
	seen := map[string]bool{}
	for name, m := range s.Cfg.Models {
		if s.Chats.CanChat(name) != nil {
			continue
		}
		f.Choices = append(f.Choices, ModelChoice{Name: name, Provider: m.Provider, ModelID: m.Model})
		if !seen[m.Provider] {
			seen[m.Provider] = true
			f.Providers = append(f.Providers, m.Provider)
		}
	}
	if len(f.Choices) == 0 {
		f.Why = "No model can chat yet: summoned agents need a Claude model that is switched on."
		return nil
	}
	sortChoices(f.Providers, f.Choices)
	pins, err := s.Store.RolePins(ctx)
	if err != nil {
		return err
	}
	for _, role := range f.Roles {
		cands := append([]string{pins[role]}, s.Cfg.Roles[role].Pool...)
		for _, m := range cands {
			if m != "" && s.Chats.CanChat(m) == nil {
				f.Default[role] = m
				break
			}
		}
	}
	f.Enabled = true
	return nil
}

// summon handles the summon form and sends the browser to the new agent.
func (s *Server) summon(w http.ResponseWriter, r *http.Request) {
	if s.Chats == nil {
		s.flash(w, "Agents are not running in this orch.")
		return
	}
	files, err := s.upload(w, r)
	if err != nil {
		s.flash(w, err.Error())
		return
	}
	a, err := s.Chats.Summon(r.Context(), runner.Summon{
		Role: r.PostFormValue("role"), Model: r.PostFormValue("model"), Project: r.PostFormValue("project"),
		Branch: strings.TrimSpace(r.PostFormValue("branch")), Message: r.PostFormValue("message"), Files: files})
	if err != nil && a.ID == 0 {
		s.flash(w, "Could not summon the agent: "+err.Error())
		return
	}
	s.Log.Info("panel action", "command", "summon", "agent", a.ID, "role", a.Role, "model", a.Model, "project", a.Project)
	w.Header().Set("HX-Redirect", fmt.Sprintf("/agents/%d", a.ID))
	s.flash(w, "Summoned.")
}

// upload reads a multipart post's text files.
func (s *Server) upload(w http.ResponseWriter, r *http.Request) ([]runner.File, error) {
	r.Body = http.MaxBytesReader(w, r.Body, maxUpload)
	if err := r.ParseMultipartForm(maxUpload); err != nil {
		return nil, errors.New("the upload is too large or not a form")
	}
	var files []runner.File
	for _, fh := range r.MultipartForm.File["files"] {
		if fh.Size > runner.MaxFileBytes {
			return nil, fmt.Errorf("%s is larger than 1 MB", fh.Filename)
		}
		data, err := readPart(fh)
		if err != nil {
			return nil, err
		}
		files = append(files, runner.File{Name: fh.Filename, Data: data})
	}
	return files, nil
}

func readPart(fh *multipart.FileHeader) ([]byte, error) {
	f, err := fh.Open()
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, runner.MaxFileBytes+1))
}

func (s *Server) agentFromPath(w http.ResponseWriter, r *http.Request) (store.Agent, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || s.Chats == nil {
		http.NotFound(w, r)
		return store.Agent{}, false
	}
	a, err := s.Store.GetAgent(r.Context(), id)
	if err != nil {
		s.notFound(w, r, err)
		return a, false
	}
	return a, true
}

func (s *Server) agentPage(w http.ResponseWriter, r *http.Request) {
	a, ok := s.agentFromPath(w, r)
	if !ok {
		return
	}
	p, err := s.page(r.Context(), a)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.render(w, "agent.html", p)
}

// agentPart renders the parts of an agent's page that refresh.
func (s *Server) agentPart(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name != "agent-head" && name != "messages" {
		http.NotFound(w, r)
		return
	}
	a, ok := s.agentFromPath(w, r)
	if !ok {
		return
	}
	p, err := s.page(r.Context(), a)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.render(w, name, p)
}

func (s *Server) page(ctx context.Context, a store.Agent) (AgentPage, error) {
	card, err := s.agentCard(ctx, a)
	if err != nil {
		return AgentPage{}, err
	}
	p := AgentPage{AgentCard: card}
	ms, err := s.Store.Messages(ctx, a.ID)
	if err != nil {
		return p, err
	}
	for _, m := range ms {
		p.Messages = append(p.Messages, MessageView{Message: m, Time: m.CreatedAt.Local().Format("Jan 2 15:04")})
	}
	return p, nil
}

func (s *Server) send(w http.ResponseWriter, r *http.Request) {
	a, ok := s.agentFromPath(w, r)
	if !ok {
		return
	}
	files, err := s.upload(w, r)
	if err != nil {
		s.flash(w, err.Error())
		return
	}
	if err := s.Chats.Send(r.Context(), a.ID, r.PostFormValue("message"), files); err != nil {
		s.flash(w, "Not sent: "+err.Error())
		return
	}
	s.Log.Info("panel action", "command", "send", "agent", a.ID, "files", len(files))
	w.Header().Set("HX-Trigger", `{"refresh":"","sent":""}`)
	s.render(w, "flash", "Sent.")
}

func (s *Server) stopAgent(w http.ResponseWriter, r *http.Request) {
	a, ok := s.agentFromPath(w, r)
	if !ok {
		return
	}
	s.Log.Info("panel action", "command", "stop", "agent", a.ID)
	if s.Chats.Stop(a.ID) {
		s.flash(w, "Stopping the agent. Its session stays; send a message to continue.")
		return
	}
	s.flash(w, "The agent is not working.")
}

func (s *Server) closeAgent(w http.ResponseWriter, r *http.Request) {
	a, ok := s.agentFromPath(w, r)
	if !ok {
		return
	}
	if err := s.Chats.Close(r.Context(), a.ID); err != nil {
		s.flash(w, "Not closed: "+err.Error())
		return
	}
	s.Log.Info("panel action", "command", "close", "agent", a.ID)
	w.Header().Set("HX-Redirect", "/")
	s.flash(w, fmt.Sprintf("Closed. Branch %s stays in %s.", a.Branch, a.Project))
}

// shorten cuts s to n runes on one line.
func shorten(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}
