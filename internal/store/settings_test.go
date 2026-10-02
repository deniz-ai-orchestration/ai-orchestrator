package store

import (
	"context"
	"testing"
)

func TestSettingsAndActions(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	if p, err := s.Paused(ctx); err != nil || p {
		t.Fatalf("paused %v err %v", p, err)
	}
	if err := s.SetPaused(ctx, true); err != nil {
		t.Fatal(err)
	}
	if p, _ := s.Paused(ctx); !p {
		t.Fatal("not paused")
	}
	if err := s.SetPaused(ctx, false); err != nil {
		t.Fatal(err)
	}
	if p, _ := s.Paused(ctx); p {
		t.Fatal("still paused")
	}
	if err := s.SetSetting(ctx, SettingTelegramOffset, "42"); err != nil {
		t.Fatal(err)
	}
	if v, _ := s.Setting(ctx, SettingTelegramOffset); v != "42" {
		t.Fatalf("offset %q", v)
	}

	task, _ := s.CreateTask(ctx, "o/r", 1, "t", "b")
	nonce, err := s.CreateAction(ctx, Action{TaskID: task.ID, Action: "retry"})
	if err != nil || len(nonce) != 24 {
		t.Fatalf("nonce %q err %v", nonce, err)
	}
	a, err := s.UseAction(ctx, nonce)
	if err != nil || a.TaskID != task.ID || a.Action != "retry" {
		t.Fatalf("action %+v err %v", a, err)
	}
	if _, err := s.UseAction(ctx, nonce); err != ErrActionUsed {
		t.Fatalf("second use: %v", err)
	}
	if _, err := s.UseAction(ctx, "nope"); err != ErrActionUsed {
		t.Fatalf("unknown nonce: %v", err)
	}
}
