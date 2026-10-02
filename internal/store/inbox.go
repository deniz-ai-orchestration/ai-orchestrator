package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"github.com/deniz-ai-orchestration/ai-orchestrator/internal/engine"
)

// InboxItem is an external event waiting to be applied.
type InboxItem struct {
	ID        int64
	Source    string
	DedupeKey string
	Payload   string
}

// AddInbox stores an event once per dedupe key. It reports whether the
// event is new, so re-polling the same GitHub page is harmless.
func (s *Store) AddInbox(ctx context.Context, source, dedupeKey, payload string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO inbox (source, dedupe_key, payload, received_at) VALUES (?, ?, ?, ?)
		ON CONFLICT (dedupe_key) DO NOTHING`, source, dedupeKey, payload, fmtTime(s.now()))
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// PendingInbox returns unprocessed events, oldest first.
func (s *Store) PendingInbox(ctx context.Context, limit int) ([]InboxItem, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, source, dedupe_key, payload FROM inbox
		WHERE processed_at IS NULL ORDER BY id LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []InboxItem
	for rows.Next() {
		var it InboxItem
		if err := rows.Scan(&it.ID, &it.Source, &it.DedupeKey, &it.Payload); err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// FinishInbox marks an event processed, linking the task it touched and any
// error text (an event that failed for good is still marked processed).
func (s *Store) FinishInbox(ctx context.Context, id, taskID int64, errText string) error {
	var task any
	if taskID != 0 {
		task = taskID
	}
	return s.exec1(ctx, `UPDATE inbox SET processed_at = ?, task_id = ?, error = ? WHERE id = ? AND processed_at IS NULL`,
		fmtTime(s.now()), task, errText, id)
}

// Cursor returns the stored ETag for a polled endpoint.
func (s *Store) Cursor(ctx context.Context, endpoint string) (string, error) {
	var etag string
	err := s.db.QueryRowContext(ctx, `SELECT etag FROM github_cursor WHERE endpoint = ?`, endpoint).Scan(&etag)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return etag, err
}

// SetCursor stores the ETag for a polled endpoint.
func (s *Store) SetCursor(ctx context.Context, endpoint, etag string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO github_cursor (endpoint, etag, updated_at) VALUES (?, ?, ?)
		ON CONFLICT (endpoint) DO UPDATE SET etag = excluded.etag, updated_at = excluded.updated_at`,
		endpoint, etag, fmtTime(s.now()))
	return err
}

// ActiveTaskFor returns the unfinished task for an issue, or ErrNotFound.
func (s *Store) ActiveTaskFor(ctx context.Context, repo string, issue int) (Task, error) {
	return scanTask(s.db.QueryRowContext(ctx, `SELECT `+taskCols+` FROM tasks
		WHERE repo = ? AND issue_number = ? AND state NOT IN ('done', 'rejected', 'cancelled')`, repo, issue))
}

// PendingEffectsOf returns unfinished outbox items of the given kinds, so
// each executor only sees the effects it handles.
func (s *Store) PendingEffectsOf(ctx context.Context, limit int, kinds ...engine.EffectKind) ([]OutboxItem, error) {
	if len(kinds) == 0 {
		return s.PendingEffects(ctx, limit)
	}
	args := make([]any, 0, len(kinds)+1)
	for _, k := range kinds {
		args = append(args, k)
	}
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, task_id, transition_id, kind, arg, attempts
		FROM outbox WHERE done_at IS NULL AND kind IN (?`+strings.Repeat(`, ?`, len(kinds)-1)+`)
		ORDER BY id LIMIT ?`, args...)
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
