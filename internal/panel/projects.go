package panel

import (
	"context"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/deniz-ai-orchestration/ai-orchestrator/internal/store"
)

// ProjectRow is one trusted project in the panel's list.
type ProjectRow struct {
	store.Project
	Name string // display name: the path's last folder
}

// TrustPage is the confirm screen before a path becomes a project.
type TrustPage struct {
	Path   string
	HasGit bool
	Repo   string
	Files  string // count, or "N+" at the cap
	Why    string // what trusting allows, or why it is refused
}

func (s *Server) projects(ctx context.Context, v *View) error {
	ps, err := s.Store.ListProjects(ctx)
	if err != nil {
		return err
	}
	for _, p := range ps {
		v.Projects = append(v.Projects, ProjectRow{Project: p, Name: filepath.Base(p.Path)})
	}
	return nil
}

// inspectProject answers the "add a project" form with the trust confirm
// screen: what is at the path, before anything is stored.
func (s *Server) inspectProject(w http.ResponseWriter, r *http.Request) {
	if s.Chats == nil {
		s.flash(w, "Agents are not running in this orch.")
		return
	}
	page, err := s.trustPage(r.Context(), r.PostFormValue("path"))
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
	page, err := s.trustPage(r.Context(), r.PostFormValue("path"))
	if err != nil {
		s.flash(w, "Not a project: "+err.Error())
		return
	}
	p := store.Project{Path: page.Path, Trusted: true, HasGit: page.HasGit, GitHubRepo: page.Repo}
	if err := s.Store.UpsertProject(r.Context(), p); err != nil {
		s.fail(w, err)
		return
	}
	s.Log.Info("panel action", "command", "trust_project", "path", p.Path, "git", p.HasGit, "repo", p.GitHubRepo)
	w.Header().Set("HX-Trigger", "refresh")
	kind := "chat-only"
	if p.HasGit {
		kind = "git"
	}
	s.flash(w, fmt.Sprintf("Trusted %s (%s).", p.Path, kind))
}

// trustPage resolves and stats a free path for the confirm screen.
func (s *Server) trustPage(ctx context.Context, path string) (TrustPage, error) {
	info, err := s.Chats.Projects.Stat(ctx, strings.TrimSpace(path))
	if err != nil {
		return TrustPage{}, err
	}
	page := TrustPage{Path: info.Path, HasGit: info.HasGit, Repo: info.Repo, Files: fmt.Sprint(info.Files)}
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
