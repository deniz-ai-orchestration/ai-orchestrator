// Package store is orch's SQLite store (WAL mode, pure-Go driver, goose
// migrations). It is the single source of truth for task state.
//
// ApplyEvent is the only way a task changes state: it loads the task, runs
// the engine, and writes the new task row, the transition and the outbox
// effects in one transaction, before any effect is executed.
package store

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"time"

	"github.com/pressly/goose/v3"
	_ "modernc.org/sqlite" // registers the "sqlite" driver

	"github.com/deniz-ai-orchestration/ai-orchestrator/internal/config"
	"github.com/deniz-ai-orchestration/ai-orchestrator/internal/engine"
)

//go:embed migrations/*.sql
var migrations embed.FS

// ErrNotFound is returned when a task does not exist.
var ErrNotFound = errors.New("not found")

// ErrActiveTaskExists is returned when the issue already has an unfinished task.
var ErrActiveTaskExists = errors.New("issue already has an active task")

// Store wraps the database handle.
type Store struct {
	db  *sql.DB
	now func() time.Time
}

// Task is a task row: the engine state plus the issue snapshot.
type Task struct {
	engine.Task
	Title     string
	Body      string
	CreatedAt time.Time
}

// OutboxItem is one pending side effect.
type OutboxItem struct {
	ID           int64
	TaskID       int64
	TransitionID int64
	Effect       engine.Effect
	Attempts     int
}

// Open opens (creating if needed) the database at path and migrates it.
func Open(ctx context.Context, path string) (*Store, error) {
	dsn := "file:" + path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)" +
		"&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)&_txlock=immediate"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	// One connection: orch has a single writer, and this keeps SQLite free of
	// SQLITE_BUSY between goroutines.
	db.SetMaxOpenConns(1)
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("open db: %w", err)
	}
	if err := migrate(ctx, db); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db, now: func() time.Time { return time.Now().UTC() }}, nil
}

func migrate(ctx context.Context, db *sql.DB) error {
	sub, err := fs.Sub(migrations, "migrations")
	if err != nil {
		return err
	}
	p, err := goose.NewProvider(goose.DialectSQLite3, db, sub)
	if err != nil {
		return fmt.Errorf("migrations: %w", err)
	}
	if _, err := p.Up(ctx); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	return nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

const timeFmt = time.RFC3339Nano

func fmtTime(t time.Time) string { return t.UTC().Format(timeFmt) }

func parseTime(s string) (time.Time, error) { return time.Parse(timeFmt, s) }

// CreateTask snapshots an issue as a new task queued for its first run.
func (s *Store) CreateTask(ctx context.Context, repo string, issue int, title, body string) (Task, error) {
	now := s.now()
	t := Task{Task: engine.NewTask(repo, issue, now), Title: title, Body: body, CreatedAt: now}
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO tasks (repo, issue_number, title, body, state, work, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		repo, issue, title, body, t.State, t.Work, fmtTime(now), fmtTime(now))
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return Task{}, ErrActiveTaskExists
		}
		return Task{}, fmt.Errorf("create task: %w", err)
	}
	t.ID, err = res.LastInsertId()
	return t, err
}

const taskCols = `id, repo, issue_number, title, body, state, work, reason, resume_state, resume_work,
	ci_attempts, review_cycles, dev_runs, branch, pr_number, head_sha, dev_model, created_at, updated_at`

type scanner interface{ Scan(dest ...any) error }

func scanTask(row scanner) (Task, error) {
	var t Task
	var created, updated string
	err := row.Scan(&t.ID, &t.Repo, &t.IssueNumber, &t.Title, &t.Body, &t.State, &t.Work, &t.Reason,
		&t.ResumeState, &t.ResumeWork, &t.CIAttempts, &t.ReviewCycles, &t.DevRuns, &t.Branch,
		&t.PRNumber, &t.HeadSHA, &t.DevModel, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return Task{}, ErrNotFound
	}
	if err != nil {
		return Task{}, err
	}
	if t.CreatedAt, err = parseTime(created); err != nil {
		return Task{}, err
	}
	t.UpdatedAt, err = parseTime(updated)
	return t, err
}

type querier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func getTask(ctx context.Context, q querier, id int64) (Task, error) {
	return scanTask(q.QueryRowContext(ctx, `SELECT `+taskCols+` FROM tasks WHERE id = ?`, id))
}

// GetTask returns one task.
func (s *Store) GetTask(ctx context.Context, id int64) (Task, error) {
	return getTask(ctx, s.db, id)
}

// ListTasks returns tasks in the given states (all tasks when none), oldest first.
func (s *Store) ListTasks(ctx context.Context, states ...engine.State) ([]Task, error) {
	q := `SELECT ` + taskCols + ` FROM tasks`
	args := make([]any, len(states))
	if len(states) > 0 {
		q += ` WHERE state IN (?` + strings.Repeat(`, ?`, len(states)-1) + `)`
		for i, st := range states {
			args[i] = st
		}
	}
	rows, err := s.db.QueryContext(ctx, q+` ORDER BY id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Task
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// ApplyEvent runs ev through the engine and commits the new state, the
// transition and the queued effects atomically. An *engine.ErrInvalid is
// returned unchanged and nothing is written.
func (s *Store) ApplyEvent(ctx context.Context, taskID int64, ev engine.Event, l config.Limits) (engine.Result, error) {
	if ev.At.IsZero() {
		ev.At = s.now()
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return engine.Result{}, err
	}
	defer tx.Rollback()

	cur, err := getTask(ctx, tx, taskID)
	if err != nil {
		return engine.Result{}, err
	}
	res, err := engine.Apply(cur.Task, ev, l)
	if err != nil {
		return engine.Result{}, err
	}
	n := res.Task
	_, err = tx.ExecContext(ctx, `
		UPDATE tasks SET state = ?, work = ?, reason = ?, resume_state = ?, resume_work = ?,
			ci_attempts = ?, review_cycles = ?, dev_runs = ?, branch = ?, pr_number = ?,
			head_sha = ?, dev_model = ?, updated_at = ?, version = version + 1
		WHERE id = ?`,
		n.State, n.Work, n.Reason, n.ResumeState, n.ResumeWork, n.CIAttempts, n.ReviewCycles,
		n.DevRuns, n.Branch, n.PRNumber, n.HeadSHA, n.DevModel, fmtTime(n.UpdatedAt), taskID)
	if err != nil {
		return engine.Result{}, fmt.Errorf("update task: %w", err)
	}
	tr := res.Transition
	r, err := tx.ExecContext(ctx, `
		INSERT INTO transitions (task_id, from_state, to_state, event, reason, detail, at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		taskID, tr.From, tr.To, tr.Event, tr.Reason, tr.Detail, fmtTime(tr.At))
	if err != nil {
		return engine.Result{}, fmt.Errorf("insert transition: %w", err)
	}
	trID, err := r.LastInsertId()
	if err != nil {
		return engine.Result{}, err
	}
	for _, e := range res.Effects {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO outbox (task_id, transition_id, kind, arg, created_at) VALUES (?, ?, ?, ?, ?)`,
			taskID, trID, e.Kind, e.Arg, fmtTime(ev.At)); err != nil {
			return engine.Result{}, fmt.Errorf("insert outbox: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return engine.Result{}, err
	}
	return res, nil
}

// Transitions returns a task's audit log, oldest first.
func (s *Store) Transitions(ctx context.Context, taskID int64) ([]engine.Transition, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT task_id, from_state, to_state, event, reason, detail, at
		FROM transitions WHERE task_id = ? ORDER BY id`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []engine.Transition
	for rows.Next() {
		var tr engine.Transition
		var at string
		if err := rows.Scan(&tr.TaskID, &tr.From, &tr.To, &tr.Event, &tr.Reason, &tr.Detail, &at); err != nil {
			return nil, err
		}
		if tr.At, err = parseTime(at); err != nil {
			return nil, err
		}
		out = append(out, tr)
	}
	return out, rows.Err()
}

// PendingEffects returns up to limit unfinished outbox items, oldest first.
func (s *Store) PendingEffects(ctx context.Context, limit int) ([]OutboxItem, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, task_id, transition_id, kind, arg, attempts
		FROM outbox WHERE done_at IS NULL ORDER BY id LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []OutboxItem
	for rows.Next() {
		var it OutboxItem
		if err := rows.Scan(&it.ID, &it.TaskID, &it.TransitionID, &it.Effect.Kind, &it.Effect.Arg, &it.Attempts); err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// CompleteEffect marks an outbox item done.
func (s *Store) CompleteEffect(ctx context.Context, id int64) error {
	return s.exec1(ctx, `UPDATE outbox SET done_at = ?, attempts = attempts + 1 WHERE id = ? AND done_at IS NULL`,
		fmtTime(s.now()), id)
}

// FailEffect records a failed attempt; the item stays pending.
func (s *Store) FailEffect(ctx context.Context, id int64, cause error) error {
	return s.exec1(ctx, `UPDATE outbox SET attempts = attempts + 1, last_error = ? WHERE id = ? AND done_at IS NULL`,
		cause.Error(), id)
}

func (s *Store) exec1(ctx context.Context, q string, args ...any) error {
	res, err := s.db.ExecContext(ctx, q, args...)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n != 1 {
		return ErrNotFound
	}
	return nil
}

// LastDetail returns the detail of the task's most recent transition caused
// by ev, or "" when there is none.
func (s *Store) LastDetail(ctx context.Context, taskID int64, ev engine.EventKind) (string, error) {
	var d string
	err := s.db.QueryRowContext(ctx, `SELECT detail FROM transitions WHERE task_id = ? AND event = ? ORDER BY id DESC LIMIT 1`,
		taskID, ev).Scan(&d)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return d, err
}
