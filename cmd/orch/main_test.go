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
	for _, args := range [][]string{{}, {"nope"}, {"-config", "/does/not/exist", "check-config"}} {
		if err := run(context.Background(), args, &bytes.Buffer{}, &bytes.Buffer{}); err == nil {
			t.Errorf("args %v: expected error", args)
		}
	}
}
