package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// Workflow states. Closing a workflow ends the job; its branch, commits
// and conversation stay in the project.
const (
	WorkflowOpen   = "open"
	WorkflowClosed = "closed"
)

// CIState values of a workflow whose branch was pushed. "" means orch
// watches no CI for it.
const (
	CIPending = "pending" // pushed; waiting for CI on HeadSHA
	CIGreen   = "green"
	CIRed     = "red"
)

// Workflow is one job inside a project (1 workflow = 1 branch = N
// worktrees). It owns the branch, the test rounds and the PR/CI state,
// and survives agent replacement: stopping or closing an agent never
// closes the workflow, and the next agent continues on the same branch.
// Round, DevRuns and CIAttempts bound the autonomous loops with the
// config's review_cycles, dev_runs and ci_attempts limits.
type Workflow struct {
	ID           int64
	ProjectPath  string
	Title        string
	Branch       string
	Base         string // commit the branch started from
	State        string
	Round        int
	DevRuns      int
	CIAttempts   int
	PRNumber     int
	PRURL        string
	HeadSHA      string // the pushed head CI is watched on
	CIState      string
	Workspace    string
	Prompt       string
	Model        string
	Phase        string
	ReviewCycles int
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// CreateWorkflow records a new open workflow and returns it. The project
// row must exist: workflows only live in trusted projects.
func (s *Store) CreateWorkflow(ctx context.Context, w Workflow) (Workflow, error) {
	now := s.now()
	if w.State == "" {
		w.State = WorkflowOpen
	}
	w.CreatedAt, w.UpdatedAt = now, now
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO workflows (project_path, title, branch, base, state, round, dev_runs, ci_attempts,
			pr_number, pr_url, head_sha, ci_state, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		w.ProjectPath, w.Title, w.Branch, w.Base, w.State, w.Round, w.DevRuns, w.CIAttempts,
		w.PRNumber, w.PRURL, w.HeadSHA, w.CIState, fmtTime(now), fmtTime(now))
	if err != nil {
		return w, err
	}
	w.ID, err = res.LastInsertId()
	if err == nil {
		err = s.SetWorkflowDetails(ctx, w.ID, w.Prompt, w.Model)
	}
	return w, err
}

const workflowCols = `id, project_path, title, branch, base, state, round, dev_runs, ci_attempts,
	pr_number, pr_url, head_sha, ci_state, created_at, updated_at, workspace, prompt, model, phase, review_cycles`

func scanWorkflow(row scanner) (Workflow, error) {
	var w Workflow
	var created, updated string
	err := row.Scan(&w.ID, &w.ProjectPath, &w.Title, &w.Branch, &w.Base, &w.State, &w.Round,
		&w.DevRuns, &w.CIAttempts, &w.PRNumber, &w.PRURL, &w.HeadSHA, &w.CIState, &created, &updated, &w.Workspace, &w.Prompt, &w.Model, &w.Phase, &w.ReviewCycles)
	if errors.Is(err, sql.ErrNoRows) {
		return Workflow{}, ErrNotFound
	}
	if err != nil {
		return Workflow{}, err
	}
	w.CreatedAt, _ = parseTime(created)
	w.UpdatedAt, _ = parseTime(updated)
	return w, nil
}

// GetWorkflow returns one workflow, or ErrNotFound.
func (s *Store) GetWorkflow(ctx context.Context, id int64) (Workflow, error) {
	return scanWorkflow(s.db.QueryRowContext(ctx, `SELECT `+workflowCols+` FROM workflows WHERE id = ?`, id))
}

// OpenWorkflows returns the open workflows of one project ("" for every
// project), newest first.
func (s *Store) OpenWorkflows(ctx context.Context, projectPath string) ([]Workflow, error) {
	q := `SELECT ` + workflowCols + ` FROM workflows WHERE state = ?`
	args := []any{WorkflowOpen}
	if projectPath != "" {
		q += ` AND project_path = ?`
		args = append(args, projectPath)
	}
	rows, err := s.db.QueryContext(ctx, q+` ORDER BY id DESC`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Workflow
	for rows.Next() {
		w, err := scanWorkflow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// WorkflowByBranch returns the open workflow of a project's branch, or
// ErrNotFound. Branches are unique per project while their workflow is
// open: 1 workflow = 1 branch.
func (s *Store) WorkflowByBranch(ctx context.Context, projectPath, branch string) (Workflow, error) {
	return scanWorkflow(s.db.QueryRowContext(ctx,
		`SELECT `+workflowCols+` FROM workflows WHERE project_path = ? AND branch = ? AND state = ? ORDER BY id DESC LIMIT 1`,
		projectPath, branch, WorkflowOpen))
}

// WorkflowAgents returns every agent that worked in a workflow, oldest
// first: the developer chain and the testers of all rounds.
func (s *Store) WorkflowAgents(ctx context.Context, id int64) ([]Agent, error) {
	return s.queryAgents(ctx, `SELECT `+agentCols+` FROM agents WHERE workflow_id = ? ORDER BY id`, id)
}

// WorkflowLastMessage returns the newest message across a workflow's
// agents, for the workflow list. It reports false when nobody wrote yet.
func (s *Store) WorkflowLastMessage(ctx context.Context, id int64) (Message, bool, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT m.id, m.agent_id, m.author, m.body, m.files, m.run_id, m.data, m.created_at
		FROM messages m JOIN agents a ON a.id = m.agent_id
		WHERE a.workflow_id = ? ORDER BY m.id DESC LIMIT 1`, id)
	if err != nil {
		return Message{}, false, err
	}
	defer rows.Close()
	if !rows.Next() {
		return Message{}, false, nil
	}
	var m Message
	var files, created string
	var run sql.NullInt64
	if err := rows.Scan(&m.ID, &m.AgentID, &m.Author, &m.Body, &files, &run, &m.Data, &created); err != nil {
		return Message{}, false, err
	}
	if files != "" {
		m.Files = strings.Split(files, "\n")
	}
	m.RunID = run.Int64
	m.CreatedAt, _ = parseTime(created)
	return m, true, rows.Err()
}

// SetWorkflowState opens or closes a workflow.
func (s *Store) SetWorkflowState(ctx context.Context, id int64, state string) error {
	return s.exec1(ctx, `UPDATE workflows SET state = ?, updated_at = ? WHERE id = ?`, state, fmtTime(s.now()), id)
}

// SetWorkflowRound records the test round a workflow is in.
func (s *Store) SetWorkflowRound(ctx context.Context, id int64, round int) error {
	return s.exec1(ctx, `UPDATE workflows SET round = ?, updated_at = ? WHERE id = ?`, round, fmtTime(s.now()), id)
}

// SetWorkflowPR records the pull request a workflow's branch has and the
// head that was pushed for it.
func (s *Store) SetWorkflowPR(ctx context.Context, id int64, number int, url, headSHA string) error {
	return s.exec1(ctx, `UPDATE workflows SET pr_number = ?, pr_url = ?, head_sha = ?, updated_at = ? WHERE id = ?`,
		number, url, headSHA, fmtTime(s.now()), id)
}

// SetWorkflowCI records a CI result for a workflow's head.
func (s *Store) SetWorkflowCI(ctx context.Context, id int64, state, headSHA string) error {
	return s.exec1(ctx, `UPDATE workflows SET ci_state = ?, head_sha = ?, updated_at = ? WHERE id = ?`,
		state, headSHA, fmtTime(s.now()), id)
}

// BumpWorkflowDevRun counts a summoned developer agent and returns the
// new count.
func (s *Store) BumpWorkflowDevRun(ctx context.Context, id int64) (int, error) {
	return s.bumpWorkflow(ctx, id, "dev_runs")
}

// BumpWorkflowCIAttempt counts a CI fix attempt and returns the new count.
func (s *Store) BumpWorkflowCIAttempt(ctx context.Context, id int64) (int, error) {
	return s.bumpWorkflow(ctx, id, "ci_attempts")
}

func (s *Store) bumpWorkflow(ctx context.Context, id int64, col string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`UPDATE workflows SET `+col+` = `+col+` + 1, updated_at = ? WHERE id = ? RETURNING `+col,
		fmtTime(s.now()), id).Scan(&n)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	return n, err
}

// ProjectCount is what the dashboard shows on one project's badge.
type ProjectCount struct {
	Workflows int // open workflows
	Agents    int // agents that are not closed
	Working   int // agents in a turn or testing
}

// ProjectCounts groups the open workflows and the open agents by project.
func (s *Store) ProjectCounts(ctx context.Context) (map[string]ProjectCount, error) {
	out := map[string]ProjectCount{}
	wrows, err := s.db.QueryContext(ctx,
		`SELECT project_path, COUNT(*) FROM workflows WHERE state = ? GROUP BY project_path`, WorkflowOpen)
	if err != nil {
		return nil, err
	}
	defer wrows.Close()
	for wrows.Next() {
		var path string
		var n int
		if err := wrows.Scan(&path, &n); err != nil {
			return nil, err
		}
		c := out[path]
		c.Workflows = n
		out[path] = c
	}
	if err := wrows.Err(); err != nil {
		return nil, err
	}
	arows, err := s.db.QueryContext(ctx,
		`SELECT project, COUNT(*), SUM(state IN (?, ?)) FROM agents WHERE state != ? GROUP BY project`,
		AgentWorking, AgentTesting, AgentClosed)
	if err != nil {
		return nil, err
	}
	defer arows.Close()
	for arows.Next() {
		var path string
		var n, working int
		if err := arows.Scan(&path, &n, &working); err != nil {
			return nil, err
		}
		c := out[path]
		c.Agents, c.Working = n, working
		out[path] = c
	}
	return out, arows.Err()
}
