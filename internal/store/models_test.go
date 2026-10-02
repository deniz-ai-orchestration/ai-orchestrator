package store

import (
	"context"
	"testing"
	"time"
)

func TestModelStatePinsAndCapacity(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	now := time.Date(2026, 10, 2, 10, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }

	until := now.Add(time.Hour)
	if err := s.SetCooldown(ctx, "sonnet", until, 1); err != nil {
		t.Fatal(err)
	}
	if err := s.SetDisabled(ctx, "sonnet", true); err != nil {
		t.Fatal(err)
	}
	if err := s.SetDisabled(ctx, "sol", true); err != nil {
		t.Fatal(err)
	}
	if err := s.SetDisabled(ctx, "sol", false); err != nil {
		t.Fatal(err)
	}
	st, err := s.ModelStates(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if m := st["sonnet"]; !m.Disabled || !m.CooldownUntil.Equal(until) || m.CooldownStep != 1 {
		t.Fatalf("sonnet %+v", m)
	}
	if m := st["sol"]; m.Disabled || !m.CooldownUntil.IsZero() {
		t.Fatalf("sol %+v", m)
	}
	if err := s.SetCooldown(ctx, "sonnet", time.Time{}, 0); err != nil {
		t.Fatal(err)
	}
	if st, _ = s.ModelStates(ctx); !st["sonnet"].CooldownUntil.IsZero() || !st["sonnet"].Disabled {
		t.Fatalf("clearing the cooldown must keep the disable: %+v", st["sonnet"])
	}

	if err := s.SetPin(ctx, "reviewer", "sol"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetPin(ctx, "reviewer", "gem"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetPin(ctx, "developer", "opus"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetPin(ctx, "developer", ""); err != nil {
		t.Fatal(err)
	}
	pins, err := s.RolePins(ctx)
	if err != nil || len(pins) != 1 || pins["reviewer"] != "gem" {
		t.Fatalf("pins %v err %v", pins, err)
	}

	for _, e := range []CapacityEvent{
		{Model: "sonnet", Kind: "rate_limit", Detail: "first"},
		{Model: "sonnet", Kind: "rate_limit", ResetAt: until, Detail: "second"},
		{Model: "sol", Kind: "auth_failed"},
	} {
		if err := s.AddCapacityEvent(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	last, err := s.LastCapacityEvents(ctx)
	if err != nil || last["sonnet"].Detail != "second" || !last["sonnet"].ResetAt.Equal(until) || last["sol"].Kind != "auth_failed" {
		t.Fatalf("last %+v err %v", last, err)
	}
}

func TestRunsSince(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	task, err := s.CreateTask(ctx, "o/r", 1, "t", "b")
	if err != nil {
		t.Fatal(err)
	}
	// Second-boundary times sort wrongly as RFC3339Nano text ("…:00.5Z" <
	// "…:00Z"); the query must compare them as times.
	for _, at := range []time.Time{
		time.Date(2026, 10, 2, 9, 59, 59, 0, time.UTC),
		time.Date(2026, 10, 2, 10, 0, 0, 500_000_000, time.UTC),
		time.Date(2026, 10, 2, 11, 0, 0, 0, time.UTC),
	} {
		s.now = func() time.Time { return at }
		if _, err := s.StartRun(ctx, Run{TaskID: task.ID, Role: "developer", Provider: "claude", Model: "sonnet"}); err != nil {
			t.Fatal(err)
		}
	}
	n, err := s.RunsSince(ctx, time.Date(2026, 10, 2, 10, 0, 0, 0, time.UTC))
	if err != nil || n["sonnet"] != 2 {
		t.Fatalf("runs %v err %v", n, err)
	}
}
