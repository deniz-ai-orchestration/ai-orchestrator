package panel

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"

	"github.com/deniz-ai-orchestration/ai-orchestrator/internal/store"
)

// ProjectRow is one trusted project in the panel's list. Name comes from
// the embedded store.Project (yours, or empty); Title falls back to the
// directory's last folder.
type ProjectRow struct {
	store.Project
	// Workflows counts open workflows; Agents counts open agents, Working
	// the ones in a turn or testing. Only the requested views fill them.
	Workflows int
	Agents    int
	Working   int
}

func base(path string) string { return filepath.Base(path) }

func query(path string) string { return url.QueryEscape(path) }

// TrustPage is the confirm screen before a path becomes a project.
type TrustPage struct {
	Path        string
	Name        string
	Description string
	HasGit      bool
	Repo        string
	Files       string // count, or "N+" at the cap
	Why         string // what trusting allows, or why it is refused
}

// Title is the display name: yours, or the directory's last folder.
func (r ProjectRow) Title() string {
	if r.Name != "" {
		return r.Name
	}
	return filepath.Base(r.Path)
}

// maxDescription bounds the optional project description.
const maxDescription = 500

func (s *Server) projects(ctx context.Context, v *View) error {
	ps, err := s.Store.ListProjects(ctx)
	if err != nil {
		return err
	}
	counts, err := s.Store.ProjectCounts(ctx)
	if err != nil {
		return err
	}
	for _, p := range ps {
		row := ProjectRow{Project: p}
		if c, ok := counts[p.Path]; ok {
			row.Workflows, row.Agents, row.Working = c.Workflows, c.Agents, c.Working
		}
		v.Projects = append(v.Projects, row)
	}
	return nil
}

// inspectProject answers the "new project" form with the trust confirm
// screen: what is at the path, before anything is stored.
func (s *Server) inspectProject(w http.ResponseWriter, r *http.Request) {
	if s.Chats == nil {
		s.flash(w, "Agents are not running in this orch.")
		return
	}
	page, err := s.trustPage(r.Context(), r.PostFormValue("name"), r.PostFormValue("description"), r.PostFormValue("path"))
	if err != nil {
		s.flash(w, "Not a project: "+err.Error())
		return
	}
	s.render(w, "trust-confirm", page)
}

// trustProject stores the confirmed path as a trusted project.
func (s *Server) trustProject(w http.ResponseWriter, r *http.Request) {
	if s.Chats == nil {
		s.flash(w, "Agents are not running in this orch.")
		return
	}
	page, err := s.trustPage(r.Context(), r.PostFormValue("name"), r.PostFormValue("description"), r.PostFormValue("path"))
	if err != nil {
		s.flash(w, "Not a project: "+err.Error())
		return
	}
	p := store.Project{Path: page.Path, Trusted: true, HasGit: page.HasGit, GitHubRepo: page.Repo,
		Name: page.Name, Description: page.Description}
	if err := s.Store.UpsertProject(r.Context(), p); err != nil {
		s.fail(w, err)
		return
	}
	s.Log.Info("panel action", "command", "trust_project", "path", p.Path, "name", p.Name, "git", p.HasGit, "repo", p.GitHubRepo)
	w.Header().Set("HX-Trigger", "refresh")
	kind := "chat-only"
	if p.HasGit {
		kind = "git"
	}
	s.flash(w, fmt.Sprintf("Trusted %s (%s).", p.Path, kind))
}

// trustPage resolves and stats a directory for the confirm screen.
func (s *Server) trustPage(ctx context.Context, name, description, path string) (TrustPage, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return TrustPage{}, fmt.Errorf("give the project a name")
	}
	if len([]rune(description)) > maxDescription {
		return TrustPage{}, fmt.Errorf("the description is longer than %d characters", maxDescription)
	}
	info, err := s.Chats.Projects.Stat(ctx, strings.TrimSpace(path))
	if err != nil {
		return TrustPage{}, err
	}
	page := TrustPage{Path: info.Path, Name: name, Description: strings.TrimSpace(description),
		HasGit: info.HasGit, Repo: info.Repo, Files: fmt.Sprint(info.Files)}
	if info.Capped {
		page.Files += "+"
	}
	if info.HasGit {
		page.Why = "Agents get git worktrees of it; with the autonomous switch on, orch also opens PRs and follows CI."
	} else {
		page.Why = "No git: chat-only with manual testers. Agents work in this folder; PRs and autonomous mode are unavailable."
	}
	return page, nil
}
