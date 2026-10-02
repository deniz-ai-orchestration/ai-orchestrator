package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
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

// testConfig writes a config whose data and secrets live in a temp dir.
func testConfig(t *testing.T, withToken bool) string {
	t.Helper()
	dir := t.TempDir()
	secrets := filepath.Join(dir, "secrets")
	if err := os.MkdirAll(secrets, 0o700); err != nil {
		t.Fatal(err)
	}
	if withToken {
		if err := os.WriteFile(filepath.Join(secrets, "github_orch_token"), []byte("test-token\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	example, err := os.ReadFile("../../config.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	cfg := strings.Replace(string(example), "data_dir: ~/.local/share/orch", "data_dir: "+filepath.Join(dir, "data"), 1)
	cfg = strings.Replace(cfg, "secrets_dir: ~/.config/orch/secrets", "secrets_dir: "+secrets, 1)
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRunCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cfg := testConfig(t, true)
	var logs bytes.Buffer
	if err := run(ctx, []string{"-config", cfg, "run"}, &bytes.Buffer{}, &logs); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logs.String(), "orch stopped") {
		t.Errorf("logs = %q", logs.String())
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(cfg), "data", "orch.db")); err != nil {
		t.Errorf("store not created: %v", err)
	}
}

func TestRunNeedsToken(t *testing.T) {
	err := run(context.Background(), []string{"-config", testConfig(t, false), "run"}, &bytes.Buffer{}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "github_orch_token") {
		t.Fatalf("err = %v", err)
	}
}

func TestBadCommand(t *testing.T) {
	for _, args := range [][]string{{}, {"nope"}, {"-config", "/does/not/exist", "check-config"}, {"version", "extra"}, {"use", "developer"}} {
		if err := run(context.Background(), args, &bytes.Buffer{}, &bytes.Buffer{}); err == nil {
			t.Errorf("args %v: expected error", args)
		}
	}
}

func TestModelCommands(t *testing.T) {
	cfg := testConfig(t, false)
	ctx := context.Background()
	for _, args := range [][]string{{"use", "reviewer", "claude-opus"}, {"disable", "codex-sol"}, {"enable", "codex-sol"}, {"disable", "kimi"}} {
		if err := run(ctx, append([]string{"-config", cfg}, args...), &bytes.Buffer{}, &bytes.Buffer{}); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
	}
	if err := run(ctx, []string{"-config", cfg, "use", "reviewer", "ghost"}, &bytes.Buffer{}, &bytes.Buffer{}); err == nil {
		t.Fatal("unknown model pinned")
	}
	var out bytes.Buffer
	if err := run(ctx, []string{"-config", cfg, "quota"}, &out, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 7 || !strings.HasPrefix(lines[0], "MODEL") {
		t.Fatalf("quota output:\n%s", out.String())
	}
	for _, want := range []string{"claude-opus", "reviewer", "kimi"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("missing %q:\n%s", want, out.String())
		}
	}
	for _, l := range lines {
		if strings.HasPrefix(l, "kimi") && !strings.Contains(l, " off ") {
			t.Errorf("kimi should be off: %q", l)
		}
		if strings.HasPrefix(l, "codex-sol") && !strings.Contains(l, "ready") {
			t.Errorf("codex-sol should be ready: %q", l)
		}
	}
}

func TestTaskCommands(t *testing.T) {
	cfg := testConfig(t, false)
	ctx := context.Background()
	out := func(args ...string) string {
		var b bytes.Buffer
		if err := run(ctx, append([]string{"-config", cfg}, args...), &b, &bytes.Buffer{}); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		return b.String()
	}
	if got := out("status"); !strings.Contains(got, "No active tasks") {
		t.Fatalf("status %q", got)
	}
	if got := out("pause"); !strings.Contains(got, "Paused") {
		t.Fatalf("pause %q", got)
	}
	if got := out("status"); !strings.Contains(got, "Paused") {
		t.Fatalf("status %q", got)
	}
	if got := out("why", "3"); !strings.Contains(got, "task 3") {
		t.Fatalf("why %q", got)
	}
	out("resume")
}

func TestRunNeedsTelegramToken(t *testing.T) {
	cfg := testConfig(t, true)
	raw, _ := os.ReadFile(cfg)
	raw = []byte(strings.Replace(string(raw), "enabled: false\n  user_id: 0", "enabled: true\n  user_id: 7", 1))
	if err := os.WriteFile(cfg, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	err := run(context.Background(), []string{"-config", cfg, "run"}, &bytes.Buffer{}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "telegram_token") {
		t.Fatalf("err = %v", err)
	}
}
