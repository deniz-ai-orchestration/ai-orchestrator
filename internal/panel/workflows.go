package panel

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/deniz-ai-orchestration/ai-orchestrator/internal/runner"
	"github.com/deniz-ai-orchestration/ai-orchestrator/internal/store"
)

// WorkflowRow is one workflow on its project's page.
type WorkflowRow struct {
	store.Workflow
	Dev   string // latest developer's state, "" when none yet
	Last  string // latest message across its agents, shortened
	Tests string // "round N" when it has testers
}

// ProjectPage is a project's summary, workflow list, roles and its
// new-workflow form.
type ProjectPage struct {
	Project   ProjectRow
	Workflows []WorkflowRow
	Summon    *SummonForm
	Roles     []RoleRow
	Providers []string
	Choices   []ModelChoice
}

// TesterGroup is one test round on the workflow page.
type TesterGroup struct {
	Round int
	Cards []AgentCard
}

// WorkflowPage is a workflow's agents, runs, PR/CI state and actions.
type WorkflowPage struct {
	store.Workflow
	Project    ProjectRow
	Devs       []AgentCard
	Testers    []TesterGroup
	Runs       []Card
	CanTest    bool
	CanPR      bool
	HasGit     bool
	Draft      runner.PRDraft
	Autonomous bool
}

// workflowRunLimit is how many of a workflow's runs its page shows.
const workflowRunLimit = 12

func (s *Server) projectFromQuery(w http.ResponseWriter, r *http.Request) (store.Project, bool) {
	path := strings.TrimSpace(r.URL.Query().Get("path"))
	if path == "" {
		http.NotFound(w, r)
		return store.Project{}, false
	}
	p, err := s.Store.GetProject(r.Context(), path)
	if err != nil {
		s.notFound(w, r, err)
		return p, false
	}
	return p, true
}

func (s *Server) projectPage(w http.ResponseWriter, r *http.Request) {
	p, ok := s.projectFromQuery(w, r)
	if !ok {
		return
	}
	page, err := s.project(r.Context(), p)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.render(w, "project.html", page)
}

func (s *Server) project(ctx context.Context, p store.Project) (ProjectPage, error) {
	page := ProjectPage{Project: ProjectRow{Project: p}}
	if counts, err := s.Store.ProjectCounts(ctx); err != nil {
		return page, err
	} else if c, ok := counts[p.Path]; ok {
		page.Project.Workflows, page.Project.Agents, page.Project.Working = c.Workflows, c.Agents, c.Working
	}
	ws, err := s.Store.OpenWorkflows(ctx, p.Path)
	if err != nil {
		return page, err
	}
	for _, wf := range ws {
		row := WorkflowRow{Workflow: wf}
		as, err := s.Store.WorkflowAgents(ctx, wf.ID)
		if err != nil {
			return page, err
		}
		for i := len(as) - 1; i >= 0; i-- {
			if as[i].Role == "developer" {
				row.Dev = as[i].State
				break
			}
		}
		if m, ok, err := s.Store.WorkflowLastMessage(ctx, wf.ID); err != nil {
			return page, err
		} else if ok {
			row.Last = shorten(m.Body, lastLen)
		}
		page.Workflows = append(page.Workflows, row)
	}
	if s.Chats != nil {
		var v View
		if err := s.summonForm(ctx, &v); err != nil {
			return page, err
		}
		page.Summon = v.Summon
	}
	var rv View
	if err := s.roles(ctx, &rv); err != nil {
		return page, err
	}
	page.Roles, page.Providers, page.Choices = rv.Roles, rv.Providers, rv.Choices
	return page, nil
}

// newWorkflow handles the new-workflow form and sends the browser to the
// workflow page.
func (s *Server) newWorkflow(w http.ResponseWriter, r *http.Request) {
	if s.Chats == nil {
		s.flash(w, "Agents are not running in this orch.")
		return
	}
	files, err := s.upload(w, r)
	if err != nil {
		s.flash(w, err.Error())
		return
	}
	wf, err := s.Chats.SummonWorkflow(r.Context(), runner.WorkflowRequest{
		ProjectPath: r.PostFormValue("project"), Prompt: r.PostFormValue("message"),
		Model: r.PostFormValue("model"), Branch: strings.TrimSpace(r.PostFormValue("branch")), Files: files})
	if err != nil {
		s.flash(w, "Could not start the workflow: "+err.Error())
		return
	}
	s.Log.Info("panel action", "command", "new_workflow", "workflow", wf.ID, "project", wf.ProjectPath)
	w.Header().Set("HX-Redirect", fmt.Sprintf("/workflows/%d", wf.ID))
	s.flash(w, "Workflow started.")
}

func (s *Server) workflowFromPath(w http.ResponseWriter, r *http.Request) (store.Workflow, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || s.Chats == nil {
		http.NotFound(w, r)
		return store.Workflow{}, false
	}
	wf, err := s.Store.GetWorkflow(r.Context(), id)
	if err != nil {
		s.notFound(w, r, err)
		return wf, false
	}
	return wf, true
}

func (s *Server) workflowPage(w http.ResponseWriter, r *http.Request) {
	wf, ok := s.workflowFromPath(w, r)
	if !ok {
		return
	}
	page, err := s.workflow(r.Context(), wf)
	if err != nil {
		s.fail(w, err)
		return
	}
	if page.CanPR && wf.PRNumber == 0 {
		dev := latestDev(page)
		if dev != nil {
			if page.Draft, err = s.Chats.Draft(r.Context(), dev.ID); err != nil {
				s.fail(w, err)
				return
			}
		}
	}
	s.render(w, "workflow.html", page)
}

// workflowPart refreshes the workflow header: state, PR/CI and actions.
func (s *Server) workflowPart(w http.ResponseWriter, r *http.Request) {
	if r.PathValue("name") != "workflow-head" {
		http.NotFound(w, r)
		return
	}
	wf, ok := s.workflowFromPath(w, r)
	if !ok {
		return
	}
	page, err := s.workflow(r.Context(), wf)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.render(w, "workflow-head", page)
}

func latestDev(page WorkflowPage) *AgentCard {
	if len(page.Devs) == 0 {
		return nil
	}
	return &page.Devs[len(page.Devs)-1]
}

func (s *Server) workflow(ctx context.Context, wf store.Workflow) (WorkflowPage, error) {
	page := WorkflowPage{Workflow: wf}
	p, err := s.Store.GetProject(ctx, wf.ProjectPath)
	if err != nil {
		return page, err
	}
	page.Project = ProjectRow{Project: p}
	page.HasGit, page.Autonomous = p.HasGit, p.Autonomous
	as, err := s.Store.WorkflowAgents(ctx, wf.ID)
	if err != nil {
		return page, err
	}
	rs, err := s.Store.WorkflowRuns(ctx, wf.ID, workflowRunLimit)
	if err != nil {
		return page, err
	}
	for _, r := range rs {
		c, err := s.card(ctx, r, false)
		if err != nil {
			return page, err
		}
		page.Runs = append(page.Runs, c)
	}
	byRound := map[int][]AgentCard{}
	var rounds []int
	for _, a := range as {
		card, err := s.agentCard(ctx, a)
		if err != nil {
			return page, err
		}
		if a.Role == "developer" {
			page.Devs = append(page.Devs, card)
			continue
		}
		if _, ok := byRound[a.Round]; !ok {
			rounds = append(rounds, a.Round)
		}
		byRound[a.Round] = append(byRound[a.Round], card)
	}
	for i := len(rounds) - 1; i >= 0; i-- {
		page.Testers = append(page.Testers, TesterGroup{Round: rounds[i], Cards: byRound[rounds[i]]})
	}
	if dev := latestDev(page); dev != nil {
		switch dev.State {
		case store.AgentWorking, store.AgentTesting, store.AgentClosed:
		default:
			page.CanTest = true
		}
		page.CanPR = dev.State != store.AgentClosed && s.Chats.Publisher != nil && page.HasGit
	}
	return page, nil
}

// workflowAgent summons a replacement or manual tester on the workflow.
func (s *Server) workflowAgent(w http.ResponseWriter, r *http.Request) {
	wf, ok := s.workflowFromPath(w, r)
	if !ok {
		return
	}
	a, err := s.Chats.SummonAgent(r.Context(), wf.ID, r.PostFormValue("role"),
		r.PostFormValue("model"), r.PostFormValue("message"))
	if err != nil {
		s.flash(w, "Not summoned: "+err.Error())
		return
	}
	s.Log.Info("panel action", "command", "summon_agent", "workflow", wf.ID, "agent", a.ID, "role", a.Role)
	w.Header().Set("HX-Redirect", fmt.Sprintf("/agents/%d", a.ID))
	s.flash(w, "Summoned.")
}

// sendWorkflow messages the workflow's latest developer.
func (s *Server) sendWorkflow(w http.ResponseWriter, r *http.Request) {
	wf, ok := s.workflowFromPath(w, r)
	if !ok {
		return
	}
	files, err := s.upload(w, r)
	if err != nil {
		s.flash(w, err.Error())
		return
	}
	if err := s.Chats.SendWorkflow(r.Context(), wf.ID, r.PostFormValue("message"), files); err != nil {
		s.flash(w, "Not sent: "+err.Error())
		return
	}
	s.Log.Info("panel action", "command", "send_workflow", "workflow", wf.ID, "files", len(files))
	w.Header().Set("HX-Trigger", `{"refresh":"","sent":""}`)
	s.render(w, "flash", "Sent.")
}

// testWorkflow sends the workflow's developer to the testers.
func (s *Server) testWorkflow(w http.ResponseWriter, r *http.Request) {
	wf, ok := s.workflowFromPath(w, r)
	if !ok {
		return
	}
	if err := s.Chats.TestWorkflow(r.Context(), wf.ID); err != nil {
		s.flash(w, "Not sent to testers: "+err.Error())
		return
	}
	s.Log.Info("panel action", "command", "test_workflow", "workflow", wf.ID)
	w.Header().Set("HX-Trigger", "refresh")
	s.flash(w, "Sent to testers.")
}

// openWorkflowPR pushes the workflow's branch and opens its pull request.
func (s *Server) openWorkflowPR(w http.ResponseWriter, r *http.Request) {
	wf, ok := s.workflowFromPath(w, r)
	if !ok {
		return
	}
	n, url, created, err := s.Chats.OpenWorkflowPR(r.Context(), wf.ID, runner.PRDraft{
		Title: r.PostFormValue("title"), Body: r.PostFormValue("body")})
	if err != nil {
		s.flash(w, "No PR: "+err.Error())
		return
	}
	s.Log.Info("panel action", "command", "open_workflow_pr", "workflow", wf.ID, "pr", n, "created", created)
	if created {
		s.flash(w, fmt.Sprintf("Opened PR #%d: %s", n, url))
		return
	}
	s.flash(w, fmt.Sprintf("Pushed to PR #%d: %s", n, url))
}

// stopWorkflow stops the workflow's running agents; the job waits for you.
func (s *Server) stopWorkflow(w http.ResponseWriter, r *http.Request) {
	wf, ok := s.workflowFromPath(w, r)
	if !ok {
		return
	}
	s.Log.Info("panel action", "command", "stop_workflow", "workflow", wf.ID)
	if err := s.Chats.StopWorkflow(r.Context(), wf.ID); err != nil {
		s.fail(w, err)
		return
	}
	s.flash(w, "Stopping the workflow's agents. Summon the next agent to continue.")
}

// closeWorkflow removes the workflow's checkout and closes its agents. Its
// branch and commits stay in the project.
func (s *Server) closeWorkflow(w http.ResponseWriter, r *http.Request) {
	wf, ok := s.workflowFromPath(w, r)
	if !ok {
		return
	}
	if err := s.Chats.CloseWorkflow(r.Context(), wf.ID); err != nil {
		s.flash(w, "Not closed: "+err.Error())
		return
	}
	s.Log.Info("panel action", "command", "close_workflow", "workflow", wf.ID)
	w.Header().Set("HX-Redirect", "/project?path="+query(wf.ProjectPath))
	s.flash(w, fmt.Sprintf("Closed. Branch %s stays in the project.", wf.Branch))
}

// switchAutonomous flips a project's autonomous loop. Chat-only projects
// stay manual: the store forces the switch off without git.
func (s *Server) switchAutonomous(w http.ResponseWriter, r *http.Request) {
	path := r.PostFormValue("path")
	on := r.PostFormValue("on") == "1"
	p, err := s.Store.GetProject(r.Context(), path)
	if err != nil {
		s.notFound(w, r, err)
		return
	}
	if on && !p.HasGit {
		s.flash(w, "Chat-only projects stay manual: no git, no PR, no autonomous loop.")
		return
	}
	if err := s.Store.SetProjectAutonomous(r.Context(), path, on); err != nil {
		s.fail(w, err)
		return
	}
	s.Log.Info("panel action", "command", "autonomous", "path", path, "on", on)
	w.Header().Set("HX-Trigger", "refresh")
	if on {
		s.flash(w, "Autonomous loop on: testers, PR and CI follow automatically.")
		return
	}
	s.flash(w, "Autonomous loop off: Send to testers and Open PR are manual.")
}
