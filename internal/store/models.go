package store

import (
	"context"

	"time"
)

// ModelState is the live switch row for one model.
type ModelState struct {
	Model         string
	Disabled      bool
	CooldownUntil time.Time // zero when not cooling down
	CooldownStep  int
}

// ModelStates returns every model that has a state row.
func (s *Store) ModelStates(ctx context.Context) (map[string]ModelState, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT model, disabled, COALESCE(cooldown_until, ''), cooldown_step FROM provider_state`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]ModelState{}
	for rows.Next() {
		var m ModelState
		var until string
		if err := rows.Scan(&m.Model, &m.Disabled, &until, &m.CooldownStep); err != nil {
			return nil, err
		}
		if until != "" {
			if m.CooldownUntil, err = parseTime(until); err != nil {
				return nil, err
			}
		}
		out[m.Model] = m
	}
	return out, rows.Err()
}

// SetCooldown cools a model down until until; step is the backoff step to
// remember. A zero until clears the cooldown.
func (s *Store) SetCooldown(ctx context.Context, model string, until time.Time, step int) error {
	var u any
	if !until.IsZero() {
		u = fmtTime(until)
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO provider_state (model, cooldown_until, cooldown_step, updated_at) VALUES (?, ?, ?, ?)
		ON CONFLICT (model) DO UPDATE SET cooldown_until = excluded.cooldown_until,
			cooldown_step = excluded.cooldown_step, updated_at = excluded.updated_at`,
		model, u, step, fmtTime(s.now()))
	return err
}

// SetDisabled switches a model off or back on.
func (s *Store) SetDisabled(ctx context.Context, model string, disabled bool) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO provider_state (model, disabled, updated_at) VALUES (?, ?, ?)
		ON CONFLICT (model) DO UPDATE SET disabled = excluded.disabled, updated_at = excluded.updated_at`,
		model, disabled, fmtTime(s.now()))
	return err
}

// RolePins returns role -> pinned model.
func (s *Store) RolePins(ctx context.Context) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT role, model FROM role_pins`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var r, m string
		if err := rows.Scan(&r, &m); err != nil {
			return nil, err
		}
		out[r] = m
	}
	return out, rows.Err()
}

// SetPin pins a role to a model; an empty model removes the pin.
func (s *Store) SetPin(ctx context.Context, role, model string) error {
	if model == "" {
		_, err := s.db.ExecContext(ctx, `DELETE FROM role_pins WHERE role = ?`, role)
		return err
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO role_pins (role, model, pinned_at) VALUES (?, ?, ?)
		ON CONFLICT (role) DO UPDATE SET model = excluded.model, pinned_at = excluded.pinned_at`,
		role, model, fmtTime(s.now()))
	return err
}

// CapacityEvent is a rate-limit, quota, capacity or auth signal.
type CapacityEvent struct {
	Model   string
	RunID   int64
	Kind    string // rate_limit, capacity, auth_failed
	ResetAt time.Time
	Detail  string
	At      time.Time
}

// AddCapacityEvent records a signal.
func (s *Store) AddCapacityEvent(ctx context.Context, e CapacityEvent) error {
	var reset any
	if !e.ResetAt.IsZero() {
		reset = fmtTime(e.ResetAt)
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO capacity_events (model, run_id, kind, reset_at, detail, at) VALUES (?, ?, ?, ?, ?, ?)`,
		e.Model, nullID(e.RunID), e.Kind, reset, e.Detail, fmtTime(s.now()))
	return err
}

// LastCapacityEvents returns each model's newest signal.
func (s *Store) LastCapacityEvents(ctx context.Context) (map[string]CapacityEvent, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT model, COALESCE(run_id, 0), kind, COALESCE(reset_at, ''), detail, at FROM capacity_events
		WHERE id IN (SELECT MAX(id) FROM capacity_events GROUP BY model)`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]CapacityEvent{}
	for rows.Next() {
		var e CapacityEvent
		var reset, at string
		if err := rows.Scan(&e.Model, &e.RunID, &e.Kind, &reset, &e.Detail, &at); err != nil {
			return nil, err
		}
		if reset != "" {
			if e.ResetAt, err = parseTime(reset); err != nil {
				return nil, err
			}
		}
		if e.At, err = parseTime(at); err != nil {
			return nil, err
		}
		out[e.Model] = e
	}
	return out, rows.Err()
}

// RunsSince counts each model's runs started at or after since.
func (s *Store) RunsSince(ctx context.Context, since time.Time) (map[string]int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT model, COUNT(*) FROM runs WHERE julianday(started_at) >= julianday(?) GROUP BY model`, fmtTime(since))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var m string
		var n int
		if err := rows.Scan(&m, &n); err != nil {
			return nil, err
		}
		out[m] = n
	}
	return out, rows.Err()
}
