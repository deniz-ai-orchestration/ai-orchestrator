package store

import (
	"context"
	"database/sql"
	"time"
)

// Run statuses.
const (
	RunRunning     = "running"
	RunSucceeded   = "succeeded"
	RunFailed      = "failed"
	RunInterrupted = "interrupted"
)

// Run is one agent CLI execution.
type Run struct {
	ID         int64
	TaskID     int64
	AgentID    int64 // a summoned agent's turn; TaskID is then 0
	WorkflowID int64 // the workflow the run's agent works in; 0 for issue runs
	OutboxID   int64
	Role       string
	Work       string
	Provider   string
	Model      string
	Status     string
	Outcome    string
	LogDir     string
	// StartedAt and EndedAt are zero while unknown (EndedAt while running).
	StartedAt, EndedAt time.Time
}

// StartRun records a run as running and returns its id.
func (s *Store) StartRun(ctx context.Context, r Run) (int64, error) {
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO runs (task_id, agent_id, workflow_id, outbox_id, role, work, provider, model, status, started_at, log_dir)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		nullID(r.TaskID), nullID(r.AgentID), nullID(r.WorkflowID), nullID(r.OutboxID), r.Role, r.Work, r.Provider, r.Model, RunRunning, fmtTime(s.now()), r.LogDir)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// SetRunContainer stores the container name once it is known.
func (s *Store) SetRunContainer(ctx context.Context, id int64, container string) error {
	return s.exec1(ctx, `UPDATE runs SET container_id = ? WHERE id = ?`, container, id)
}

// SetRunLogDir stores where the run's prompt and output live.
func (s *Store) SetRunLogDir(ctx context.Context, id int64, dir string) error {
	return s.exec1(ctx, `UPDATE runs SET log_dir = ? WHERE id = ?`, dir, id)
}

// FinishRun records how a running run ended. outcome is "ok" or an engine
// reason.
func (s *Store) FinishRun(ctx context.Context, id int64, status, outcome string, exitCode, inTok, outTok int) error {
	return s.exec1(ctx, `
		UPDATE runs SET status = ?, outcome = ?, exit_code = ?, input_tokens = ?, output_tokens = ?, ended_at = ?
		WHERE id = ? AND status = ?`,
		status, outcome, exitCode, inTok, outTok, fmtTime(s.now()), id, RunRunning)
}

// AddArtifact records a file kept for a run.
func (s *Store) AddArtifact(ctx context.Context, runID int64, kind, path string, size int64) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO run_artifacts (run_id, kind, path, bytes) VALUES (?, ?, ?, ?)`,
		runID, kind, path, size)
	return err
}

// InterruptRunning marks every run still running as interrupted. orch calls
// it at start-up, before any new run, so these are runs a crash or restart
// cut off. It returns how many it marked.
func (s *Store) InterruptRunning(ctx context.Context) (int64, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE runs SET status = ?, ended_at = ? WHERE status = ?`,
		RunInterrupted, fmtTime(s.now()), RunRunning)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

const runCols = `id, task_id, agent_id, workflow_id, outbox_id, role, work, provider, model, status, outcome, log_dir, started_at, ended_at`

// RunsForOutbox returns the runs started for one outbox item, oldest first.
func (s *Store) RunsForOutbox(ctx context.Context, outboxID int64) ([]Run, error) {
	return s.queryRuns(ctx, `SELECT `+runCols+` FROM runs WHERE outbox_id = ? ORDER BY id`, outboxID)
}

// RecentRuns returns the newest runs, newest first.
func (s *Store) RecentRuns(ctx context.Context, limit int) ([]Run, error) {
	return s.queryRuns(ctx, `SELECT `+runCols+` FROM runs ORDER BY id DESC LIMIT ?`, limit)
}

// GetRun returns one run, or ErrNotFound.
func (s *Store) GetRun(ctx context.Context, id int64) (Run, error) {
	runs, err := s.queryRuns(ctx, `SELECT `+runCols+` FROM runs WHERE id = ?`, id)
	if err != nil {
		return Run{}, err
	}
	if len(runs) == 0 {
		return Run{}, ErrNotFound
	}
	return runs[0], nil
}

func (s *Store) queryRuns(ctx context.Context, q string, args ...any) ([]Run, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Run
	for rows.Next() {
		var r Run
		var task, agent, workflow, ob sql.NullInt64
		var started, ended sql.NullString
		if err := rows.Scan(&r.ID, &task, &agent, &workflow, &ob, &r.Role, &r.Work, &r.Provider, &r.Model, &r.Status, &r.Outcome, &r.LogDir,
			&started, &ended); err != nil {
			return nil, err
		}
		r.TaskID, r.AgentID, r.WorkflowID, r.OutboxID = task.Int64, agent.Int64, workflow.Int64, ob.Int64
		if started.Valid {
			r.StartedAt, _ = parseTime(started.String)
		}
		if ended.Valid {
			r.EndedAt, _ = parseTime(ended.String)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func nullID(id int64) any {
	if id == 0 {
		return nil
	}
	return id
}
