package store

import (
	"context"
	"database/sql"
	"strings"
	"time"
)

// Agent states.
const (
	AgentWorking  = "working"   // a turn is running
	AgentNeedsYou = "needs_you" // the last turn asked you something
	AgentDone     = "done"      // the last turn finished the task
	AgentStopped  = "stopped"   // you stopped the turn, or orch restarted during it
	AgentFailed   = "failed"    // the last turn failed
	AgentClosed   = "closed"    // container and worktree removed
)

// Message authors.
const (
	FromYou   = "you"
	FromAgent = "agent"
	FromOrch  = "orch"
)

// Agent is one summoned agent.
type Agent struct {
	ID        int64
	Role      string
	Provider  string
	Model     string
	Project   string
	Branch    string
	Base      string
	Workspace string
	Session   string
	State     string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Message is one entry in an agent's conversation.
type Message struct {
	ID        int64
	AgentID   int64
	Author    string
	Body      string
	Files     []string
	RunID     int64
	CreatedAt time.Time
}

// CreateAgent records a new agent in state working and returns it.
func (s *Store) CreateAgent(ctx context.Context, a Agent) (Agent, error) {
	now := s.now()
	a.State, a.CreatedAt, a.UpdatedAt = AgentWorking, now, now
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO agents (role, provider, model, project, branch, base, workspace, session, state, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		a.Role, a.Provider, a.Model, a.Project, a.Branch, a.Base, a.Workspace, a.Session, a.State, fmtTime(now), fmtTime(now))
	if err != nil {
		return a, err
	}
	a.ID, err = res.LastInsertId()
	return a, err
}

// SetAgentWorkspace stores the agent's branch, base commit and worktree once
// the worktree exists.
func (s *Store) SetAgentWorkspace(ctx context.Context, id int64, branch, base, dir string) error {
	return s.exec1(ctx, `UPDATE agents SET branch = ?, base = ?, workspace = ?, updated_at = ? WHERE id = ?`,
		branch, base, dir, fmtTime(s.now()), id)
}

// SetAgentSession stores the CLI session id that later turns resume.
func (s *Store) SetAgentSession(ctx context.Context, id int64, session string) error {
	return s.exec1(ctx, `UPDATE agents SET session = ?, updated_at = ? WHERE id = ?`, session, fmtTime(s.now()), id)
}

// SetAgentState moves an agent to state.
func (s *Store) SetAgentState(ctx context.Context, id int64, state string) error {
	return s.exec1(ctx, `UPDATE agents SET state = ?, updated_at = ? WHERE id = ?`, state, fmtTime(s.now()), id)
}

// StartTurn moves an idle agent to working. It reports false when the agent
// is already working or closed, so two messages never start two turns.
func (s *Store) StartTurn(ctx context.Context, id int64) (bool, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE agents SET state = ?, updated_at = ? WHERE id = ? AND state NOT IN (?, ?)`,
		AgentWorking, fmtTime(s.now()), id, AgentWorking, AgentClosed)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// InterruptAgents marks agents left working by a restart as stopped and
// returns their ids.
func (s *Store) InterruptAgents(ctx context.Context) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx, `UPDATE agents SET state = ?, updated_at = ? WHERE state = ? RETURNING id`,
		AgentStopped, fmtTime(s.now()), AgentWorking)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

const agentCols = `id, role, provider, model, project, branch, base, workspace, session, state, created_at, updated_at`

// GetAgent returns one agent, or ErrNotFound.
func (s *Store) GetAgent(ctx context.Context, id int64) (Agent, error) {
	as, err := s.queryAgents(ctx, `SELECT `+agentCols+` FROM agents WHERE id = ?`, id)
	if err != nil {
		return Agent{}, err
	}
	if len(as) == 0 {
		return Agent{}, ErrNotFound
	}
	return as[0], nil
}

// OpenAgents returns every agent that is not closed, newest first.
func (s *Store) OpenAgents(ctx context.Context) ([]Agent, error) {
	return s.queryAgents(ctx, `SELECT `+agentCols+` FROM agents WHERE state != ? ORDER BY id DESC`, AgentClosed)
}

func (s *Store) queryAgents(ctx context.Context, q string, args ...any) ([]Agent, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Agent
	for rows.Next() {
		var a Agent
		var created, updated string
		if err := rows.Scan(&a.ID, &a.Role, &a.Provider, &a.Model, &a.Project, &a.Branch, &a.Base, &a.Workspace,
			&a.Session, &a.State, &created, &updated); err != nil {
			return nil, err
		}
		a.CreatedAt, _ = parseTime(created)
		a.UpdatedAt, _ = parseTime(updated)
		out = append(out, a)
	}
	return out, rows.Err()
}

// AddMessage appends a message to an agent's conversation and returns its id.
func (s *Store) AddMessage(ctx context.Context, m Message) (int64, error) {
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO messages (agent_id, author, body, files, run_id, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		m.AgentID, m.Author, m.Body, strings.Join(m.Files, "\n"), nullID(m.RunID), fmtTime(s.now()))
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// Messages returns an agent's conversation, oldest first.
func (s *Store) Messages(ctx context.Context, agentID int64) ([]Message, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, agent_id, author, body, files, run_id, created_at FROM messages WHERE agent_id = ? ORDER BY id`, agentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Message
	for rows.Next() {
		var m Message
		var files, created string
		var run sql.NullInt64
		if err := rows.Scan(&m.ID, &m.AgentID, &m.Author, &m.Body, &files, &run, &created); err != nil {
			return nil, err
		}
		if files != "" {
			m.Files = strings.Split(files, "\n")
		}
		m.RunID = run.Int64
		m.CreatedAt, _ = parseTime(created)
		out = append(out, m)
	}
	return out, rows.Err()
}

// AgentRuns returns an agent's turns, newest first.
func (s *Store) AgentRuns(ctx context.Context, agentID int64, limit int) ([]Run, error) {
	return s.queryRuns(ctx, `SELECT `+runCols+` FROM runs WHERE agent_id = ? ORDER BY id DESC LIMIT ?`, agentID, limit)
}
