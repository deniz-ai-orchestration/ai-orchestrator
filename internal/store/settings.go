package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
)

// Setting keys.
const (
	SettingPaused         = "paused"          // "1" stops new runs from starting
	SettingTelegramOffset = "telegram_offset" // next getUpdates offset
)

// Setting returns a setting's value, or "" when it is unset.
func (s *Store) Setting(ctx context.Context, key string) (string, error) {
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return v, err
}

// SetSetting stores a setting; an empty value deletes it.
func (s *Store) SetSetting(ctx context.Context, key, value string) error {
	if value == "" {
		_, err := s.db.ExecContext(ctx, `DELETE FROM settings WHERE key = ?`, key)
		return err
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO settings (key, value, updated_at) VALUES (?, ?, ?)
		ON CONFLICT (key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		key, value, fmtTime(s.now()))
	return err
}

// Paused reports whether new runs are paused.
func (s *Store) Paused(ctx context.Context) (bool, error) {
	v, err := s.Setting(ctx, SettingPaused)
	return v == "1", err
}

// SetPaused pauses or resumes starting new runs.
func (s *Store) SetPaused(ctx context.Context, paused bool) error {
	v := ""
	if paused {
		v = "1"
	}
	return s.SetSetting(ctx, SettingPaused, v)
}

// Action is what a Telegram button does.
type Action struct {
	TaskID int64
	Action string
	Arg    string
}

// ErrActionUsed is returned for a button nonce that was already used or
// never existed.
var ErrActionUsed = errors.New("action already used or unknown")

// CreateAction stores a single-use button action and returns its nonce.
func (s *Store) CreateAction(ctx context.Context, a Action) (string, error) {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	nonce := hex.EncodeToString(b)
	_, err := s.db.ExecContext(ctx, `INSERT INTO telegram_actions (nonce, task_id, action, arg, created_at) VALUES (?, ?, ?, ?, ?)`,
		nonce, nullID(a.TaskID), a.Action, a.Arg, fmtTime(s.now()))
	return nonce, err
}

// UseAction consumes a nonce once.
func (s *Store) UseAction(ctx context.Context, nonce string) (Action, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Action{}, err
	}
	defer tx.Rollback()
	var a Action
	var task sql.NullInt64
	err = tx.QueryRowContext(ctx, `SELECT task_id, action, arg FROM telegram_actions WHERE nonce = ? AND used_at IS NULL`, nonce).
		Scan(&task, &a.Action, &a.Arg)
	if errors.Is(err, sql.ErrNoRows) {
		return Action{}, ErrActionUsed
	}
	if err != nil {
		return Action{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE telegram_actions SET used_at = ? WHERE nonce = ?`, fmtTime(s.now()), nonce); err != nil {
		return Action{}, err
	}
	a.TaskID = task.Int64
	return a, tx.Commit()
}
