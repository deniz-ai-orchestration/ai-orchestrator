package runner

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// originRepo makes a bare repository with one commit on main and an
// existing agent branch, and returns its path.
func originRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	git(t, dir, "init", "-q", "-b", "main", src)
	if err := os.WriteFile(filepath.Join(src, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, src, "add", ".")
	git(t, src, "commit", "-q", "-m", "init")
	git(t, src, "checkout", "-q", "-b", "agent/3-existing")
	if err := os.WriteFile(filepath.Join(src, "fix.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, src, "add", ".")
	git(t, src, "commit", "-q", "-m", "wip")
	git(t, src, "checkout", "-q", "main")
	bare := filepath.Join(dir, "origin.git")
	git(t, dir, "clone", "-q", "--bare", src, bare)
	return bare
}

func TestPrepare(t *testing.T) {
	origin := originRepo(t)
	w := Workspaces{Root: t.TempDir(), CloneURL: func(string) string { return origin }}
	ctx := context.Background()

	dir, base, err := w.Prepare(ctx, 7, "dev", "o/r", "agent/3-new", "tok-123")
	if err != nil {
		t.Fatal(err)
	}
	if dir != w.Dir(7, "dev") || base != "main" {
		t.Fatalf("dir %q base %q", dir, base)
	}
	if b := git(t, dir, "branch", "--show-current"); b != "agent/3-new" {
		t.Fatalf("branch %q", b)
	}
	cfg, _ := os.ReadFile(filepath.Join(dir, ".git", "config"))
	if strings.Contains(string(cfg), "tok-123") || strings.Contains(string(cfg), "extraheader") {
		t.Fatalf("token stored in .git/config:\n%s", cfg)
	}
	if err := os.WriteFile(filepath.Join(dir, "junk"), nil, 0o644); err != nil {
		t.Fatal(err)
	}

	dir, _, err = w.Prepare(ctx, 7, "dev", "o/r", "agent/3-existing", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "fix.go")); err != nil {
		t.Fatal("existing branch not checked out")
	}
	if _, err := os.Stat(filepath.Join(dir, "junk")); !os.IsNotExist(err) {
		t.Fatal("workspace was not fresh")
	}

	if _, _, err := w.Prepare(ctx, 7, "dev", "o/r", "", ""); err == nil {
		t.Fatal("an empty branch name should fail")
	}
	if err := w.Remove(7); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(w.Root, "task-7")); !os.IsNotExist(err) {
		t.Fatal("workspace not removed")
	}
}
