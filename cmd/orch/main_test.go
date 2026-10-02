package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestVersion(t *testing.T) {
	var out bytes.Buffer
	if err := run(context.Background(), []string{"version"}, &out, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(out.String()) != version {
		t.Errorf("got %q", out.String())
	}
}

func TestCheckConfig(t *testing.T) {
	var out bytes.Buffer
	err := run(context.Background(), []string{"-config", "../../config.example.yaml", "check-config"}, &out, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out.String(), "config ok:") {
		t.Errorf("got %q", out.String())
	}
}

func TestRunCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var logs bytes.Buffer
	if err := run(ctx, []string{"-config", "../../config.example.yaml", "run"}, &bytes.Buffer{}, &logs); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logs.String(), "orch stopped") {
		t.Errorf("logs = %q", logs.String())
	}
}

func TestBadCommand(t *testing.T) {
	for _, args := range [][]string{{}, {"nope"}, {"-config", "/does/not/exist", "check-config"}} {
		if err := run(context.Background(), args, &bytes.Buffer{}, &bytes.Buffer{}); err == nil {
			t.Errorf("args %v: expected error", args)
		}
	}
}
