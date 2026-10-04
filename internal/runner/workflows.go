package runner

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/deniz-ai-orchestration/ai-orchestrator/internal/store"
)

type WorkflowRequest struct {
	ProjectPath, Prompt, Model, Branch string
	Files                              []File
}

// SummonWorkflow creates a job and its persistent developer checkout.
func (c *Chats) SummonWorkflow(ctx context.Context, req WorkflowRequest) (store.Workflow, error) {
	c.workflowMu.Lock()
	defer c.workflowMu.Unlock()
	var zero store.Workflow
	if !c.Running() {
		return zero, errors.New("orch is not running agents")
	}
	if err := c.CanChat(req.Model); err != nil {
		return zero, err
	}
	if strings.TrimSpace(req.Prompt) == "" {
		return zero, errors.New("give the workflow a task")
	}
	if err := checkFiles(req.Files); err != nil {
		return zero, err
	}
	path, err := c.Projects.Resolve(req.ProjectPath)
	if err != nil {
		return zero, err
	}
	p, err := c.Store.GetProject(ctx, path)
	if err != nil || !p.Trusted {
		return zero, errors.New("trust this project first")
	}
	info, err := c.Projects.Stat(ctx, path)
	if err != nil {
		return zero, err
	}
	if info.HasGit != p.HasGit {
		return zero, errors.New("project git status changed; inspect and trust it again")
	}
	if req.Branch != "" {
		if !p.HasGit {
			return zero, errors.New("chat-only projects have no branch")
		}
		if err := checkBranch(ctx, req.Branch); err != nil {
			return zero, err
		}
	}
	w, err := c.Store.CreateWorkflow(ctx, store.Workflow{ProjectPath: path, Title: prTitle(req.Prompt), Prompt: req.Prompt, Model: req.Model, Branch: req.Branch})
	if err != nil {
		return w, err
	}
	workspace := path
	branch, base := req.Branch, ""
	if p.HasGit {
		create := branch == ""
		if create {
			branch = fmt.Sprintf("orch/%d-%s", w.ID, Slug(req.Prompt))
		}
		workspace = filepath.Join(c.Dir, "workflows", fmt.Sprint(w.ID), "work")
		wt, e := c.Projects.AddWorktree(ctx, path, workspace, branch, create)
		if e != nil {
			_ = c.Store.SetWorkflowState(ctx, w.ID, store.WorkflowClosed)
			return w, e
		}
		base = wt.Base
	}
	if err := c.Store.SetWorkflowWorkspace(ctx, w.ID, branch, base, workspace); err != nil {
		return w, err
	}
	w, err = c.Store.GetWorkflow(ctx, w.ID)
	if err != nil {
		return w, err
	}
	_, err = c.summonWorkflowAgent(ctx, w, "developer", req.Model, req.Prompt, req.Files)
	return w, err
}

// SummonAgent replaces a worker without removing the workflow checkout.
func (c *Chats) SummonAgent(ctx context.Context, id int64, role, model, message string) (store.Agent, error) {
	c.workflowMu.Lock()
	defer c.workflowMu.Unlock()
	w, err := c.Store.GetWorkflow(ctx, id)
	if err != nil {
		return store.Agent{}, err
	}
	return c.summonWorkflowAgent(ctx, w, role, model, message, nil)
}

func (c *Chats) workflowIdle(ctx context.Context, w store.Workflow) error {
	if w.State == store.WorkflowClosed {
		return errors.New("workflow is closed")
	}
	as, err := c.Store.WorkflowAgents(ctx, w.ID)
	if err != nil {
		return err
	}
	for _, a := range as {
		c.mu.Lock()
		_, active := c.turns[a.ID]
		c.mu.Unlock()
		if active || a.State == store.AgentWorking || a.State == store.AgentTesting {
			return ErrBusy
		}
	}
	return nil
}

func (c *Chats) summonWorkflowAgent(ctx context.Context, w store.Workflow, role, model, message string, files []File) (store.Agent, error) {
	var zero store.Agent
	if err := c.workflowIdle(ctx, w); err != nil {
		return zero, err
	}
	if !c.Running() {
		return zero, errors.New("orch is not running agents")
	}
	if err := c.CanChat(model); err != nil {
		return zero, err
	}
	if role != "developer" && role != "reviewer" && role != "functional_tester" {
		return zero, errors.New("unsupported workflow role")
	}
	if r, ok := c.Cfg.Roles[role]; !ok || !r.IsEnabled() {
		return zero, errors.New("role is disabled")
	}
	p, err := c.Store.GetProject(ctx, w.ProjectPath)
	if err != nil || !p.Trusted {
		return zero, errors.New("project is not trusted")
	}
	if _, err := c.Projects.Resolve(w.ProjectPath); err != nil {
		return zero, err
	}
	as, err := c.Store.WorkflowAgents(ctx, w.ID)
	if err != nil {
		return zero, err
	}
	if role != "developer" && len(as) == 0 && w.PRNumber == 0 {
		return zero, errors.New("start with a developer; tester-first requires an open PR")
	}
	if role == "developer" {
		fresh, err := c.Store.GetWorkflow(ctx, w.ID)
		if err != nil {
			return zero, err
		}
		if fresh.DevRuns >= c.Cfg.Limits.DevRuns {
			return zero, fmt.Errorf("the workflow used its %d developer runs; close it or raise limits.dev_runs", fresh.DevRuns)
		}
	}
	for _, old := range as {
		if old.Role == "developer" && old.State != store.AgentClosed {
			if err := c.Store.SetAgentState(ctx, old.ID, store.AgentClosed); err != nil {
				return zero, err
			}
		}
	}
	if strings.TrimSpace(message) == "" {
		message = w.Prompt
	}
	a, err := c.Store.CreateAgent(ctx, store.Agent{WorkflowID: w.ID, Project: w.ProjectPath, Branch: w.Branch, Base: w.Base, Workspace: w.Workspace, Role: role, Model: model, Provider: c.Cfg.Models[model].Provider, Round: w.Round})
	if err != nil {
		return a, err
	}
	if role != "developer" && p.HasGit {
		head, e := c.Projects.BranchHead(ctx, w.ProjectPath, w.Branch)
		if e != nil {
			c.fail(ctx, a.ID, e.Error())
			return a, e
		}
		wt, e := c.Projects.AddDetached(ctx, w.ProjectPath, filepath.Join(c.agentDir(a.ID), "work"), head)
		if e != nil {
			c.fail(ctx, a.ID, e.Error())
			return a, e
		}
		if err = c.Store.SetAgentWorkspace(ctx, a.ID, w.Branch, head, wt.Dir); err != nil {
			return a, err
		}
	}
	if _, err = c.addMessage(ctx, a.ID, message, files); err != nil {
		c.fail(ctx, a.ID, err.Error())
		return a, err
	}
	if role == "developer" {
		if _, err = c.Store.BumpWorkflowDevRun(ctx, w.ID); err != nil {
			return a, err
		}
		if err = c.Store.SetWorkflowDetails(ctx, w.ID, w.Prompt, model); err != nil {
			return a, err
		}
	}
	if err = c.Store.SetWorkflowPhase(ctx, w.ID, "developing"); err != nil {
		return a, err
	}
	c.launch(a.ID)
	return a, nil
}

func (c *Chats) workflowDev(ctx context.Context, id int64) (store.Workflow, store.Agent, error) {
	w, err := c.Store.GetWorkflow(ctx, id)
	if err != nil {
		return w, store.Agent{}, err
	}
	as, err := c.Store.WorkflowAgents(ctx, id)
	if err != nil {
		return w, store.Agent{}, err
	}
	for i := len(as) - 1; i >= 0; i-- {
		if as[i].Role == "developer" {
			return w, as[i], nil
		}
	}
	return w, store.Agent{}, errors.New("workflow has no developer")
}

func (c *Chats) SendWorkflow(ctx context.Context, id int64, text string, files []File) error {
	c.workflowMu.Lock()
	defer c.workflowMu.Unlock()
	w, a, err := c.workflowDev(ctx, id)
	if err != nil {
		return err
	}
	if err := c.workflowIdle(ctx, w); err != nil {
		return err
	}
	if a.State == store.AgentClosed {
		_, err = c.summonWorkflowAgent(ctx, w, "developer", w.Model, text, files)
		return err
	}
	if err := c.Store.SetWorkflowPhase(ctx, id, "developing"); err != nil {
		return err
	}
	return c.Send(ctx, a.ID, text, files)
}

func (c *Chats) TestWorkflow(ctx context.Context, id int64) error {
	c.workflowMu.Lock()
	defer c.workflowMu.Unlock()
	w, a, err := c.workflowDev(ctx, id)
	if err != nil {
		return err
	}
	if err := c.workflowIdle(ctx, w); err != nil {
		return err
	}
	if a.State == store.AgentClosed {
		if err := c.Store.SetAgentState(ctx, a.ID, store.AgentDone); err != nil {
			return err
		}
	}
	return c.startTests(ctx, a.ID, true)
}

func (c *Chats) OpenWorkflowPR(ctx context.Context, id int64, d PRDraft) (int, string, bool, error) {
	c.workflowMu.Lock()
	defer c.workflowMu.Unlock()
	return c.openWorkflowPR(ctx, id, d)
}

func (c *Chats) openWorkflowPR(ctx context.Context, id int64, d PRDraft) (int, string, bool, error) {
	w, a, err := c.workflowDev(ctx, id)
	if err != nil {
		return 0, "", false, err
	}
	if err := c.workflowIdle(ctx, w); err != nil {
		return 0, "", false, err
	}
	if a.State == store.AgentClosed {
		if err := c.Store.SetAgentState(ctx, a.ID, store.AgentDone); err != nil {
			return 0, "", false, err
		}
	}
	n, url, created, err := c.OpenPR(ctx, a.ID, d)
	if err != nil {
		return n, url, created, err
	}
	head, err := c.Projects.BranchHead(ctx, w.ProjectPath, w.Branch)
	if err != nil {
		return n, url, created, err
	}
	if err = c.Store.SetWorkflowPR(ctx, id, n, url, head); err != nil {
		return n, url, created, err
	}
	if err = c.Store.SetWorkflowCI(ctx, id, store.CIPending, head); err != nil {
		return n, url, created, err
	}
	err = c.Store.SetWorkflowPhase(ctx, id, "awaiting_ci")
	return n, url, created, err
}

// AutoTest runs the testers for a workflow developer outside a turn. It
// reports false when the workflow is busy: poll again later.
func (c *Chats) AutoTest(ctx context.Context, devID int64) (bool, error) {
	dev, err := c.Store.GetAgent(ctx, devID)
	if err != nil {
		return true, err
	}
	switch dev.State {
	case store.AgentWorking, store.AgentTesting:
		return false, nil
	case store.AgentClosed:
		return true, nil
	}
	return true, c.startTests(ctx, devID, false)
}

// AutoFixCI summons a replacement developer carrying the CI failure
// summary. Past the CI-attempt or dev-run limits the workflow waits for
// you instead. It reports false when the workflow is busy.
func (c *Chats) AutoFixCI(ctx context.Context, id int64, head, summary string) (bool, error) {
	c.workflowMu.Lock()
	defer c.workflowMu.Unlock()
	w, err := c.Store.GetWorkflow(ctx, id)
	if err != nil {
		return true, err
	}
	if w.State == store.WorkflowClosed {
		return true, nil
	}
	if err := c.workflowIdle(ctx, w); err != nil {
		return false, nil
	}
	fresh, err := c.Store.GetWorkflow(ctx, id)
	if err != nil {
		return true, err
	}
	dev := latestWorkflowDev(ctx, c, id)
	held := func(note string) (bool, error) {
		if dev != nil {
			if err := c.devNote(ctx, dev.ID, note, store.AgentNeedsYou); err != nil {
				return true, err
			}
		}
		if err := c.Store.SetWorkflowPhase(ctx, id, "needs_you"); err != nil {
			return true, err
		}
		return true, nil
	}
	if fresh.CIAttempts >= c.Cfg.Limits.CIAttempts {
		return held(fmt.Sprintf("CI failed on commit %s %d times, the CI-attempt limit. Look at the summary, then summon the next agent from the workflow page.",
			short(head), fresh.CIAttempts))
	}
	if fresh.DevRuns >= c.Cfg.Limits.DevRuns {
		return held(fmt.Sprintf("The workflow used its %d developer runs. Look at the CI summary, then summon the next agent from the workflow page.", fresh.DevRuns))
	}
	if _, err := c.Store.BumpWorkflowCIAttempt(ctx, id); err != nil {
		return true, err
	}
	msg := fmt.Sprintf("CI failed on commit %s:\n\n%s\n\nFix it on this branch. Original task:\n%s", short(head), summary, w.Prompt)
	if _, err := c.summonWorkflowAgent(ctx, w, "developer", w.Model, msg, nil); err != nil {
		return true, err
	}
	return true, nil
}

// AutoCloseWorkflow closes a workflow whose pull request was merged. It
// reports false when the workflow is busy: poll again later.
func (c *Chats) AutoCloseWorkflow(ctx context.Context, id int64) (bool, error) {
	err := c.CloseWorkflow(ctx, id)
	if errors.Is(err, ErrBusy) {
		return false, nil
	}
	return true, err
}

// latestWorkflowDev returns a workflow's newest developer, if it has one.
func latestWorkflowDev(ctx context.Context, c *Chats, id int64) *store.Agent {
	as, err := c.Store.WorkflowAgents(ctx, id)
	if err != nil {
		return nil
	}
	for i := len(as) - 1; i >= 0; i-- {
		if as[i].Role == "developer" {
			a := as[i]
			return &a
		}
	}
	return nil
}

func (c *Chats) StopWorkflow(ctx context.Context, id int64) error {
	c.workflowMu.Lock()
	defer c.workflowMu.Unlock()
	if err := c.Store.SetWorkflowPhase(ctx, id, "needs_you"); err != nil {
		return err
	}
	as, err := c.Store.WorkflowAgents(ctx, id)
	if err != nil {
		return err
	}
	for _, a := range as {
		c.Stop(a.ID)
	}
	return nil
}

func (c *Chats) CloseWorkflow(ctx context.Context, id int64) error {
	c.workflowMu.Lock()
	defer c.workflowMu.Unlock()
	w, err := c.Store.GetWorkflow(ctx, id)
	if err != nil {
		return err
	}
	if err := c.workflowIdle(ctx, w); err != nil {
		return err
	}
	p, err := c.Store.GetProject(ctx, w.ProjectPath)
	if err != nil {
		return err
	}
	if p.HasGit && w.Workspace != "" {
		if err := c.Projects.RemoveWorktree(ctx, w.ProjectPath, w.Workspace); err != nil {
			return err
		}
	}
	as, err := c.Store.WorkflowAgents(ctx, id)
	if err != nil {
		return err
	}
	for _, a := range as {
		if err := c.Close(ctx, a.ID); err != nil {
			return err
		}
	}
	if err := os.RemoveAll(filepath.Join(c.Dir, "workflows", fmt.Sprint(id))); err != nil {
		return err
	}
	return c.Store.SetWorkflowState(ctx, id, store.WorkflowClosed)
}
