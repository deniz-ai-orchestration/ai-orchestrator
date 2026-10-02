package runner

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Workspaces are per-task clones under Root: <root>/task-<id>/<kind>.
type Workspaces struct {
	Root string
	// CloneURL maps owner/name to a clone URL. Defaults to GitHub HTTPS;
	// tests point it at a local repository.
	CloneURL func(repo string) string
}

// Dir is a task's workspace of one kind ("dev", "review").
func (w Workspaces) Dir(taskID int64, kind string) string {
	return filepath.Join(w.Root, fmt.Sprintf("task-%d", taskID), kind)
}

// Prepare makes a fresh clone of repo checked out on branch: the remote
// branch when it exists (fix runs, or a retry after a rejected push),
// otherwise a new branch from the default branch. The token is sent as an HTTP header through the
// environment, so it is neither on a command line nor stored in .git/config.
// It returns the workspace path and the default branch.
func (w Workspaces) Prepare(ctx context.Context, taskID int64, kind, repo, branch, token string) (dir, base string, err error) {
	dir = w.Dir(taskID, kind)
	if err := os.RemoveAll(dir); err != nil {
		return "", "", err
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		return "", "", err
	}
	url := "https://github.com/" + repo + ".git"
	if w.CloneURL != nil {
		url = w.CloneURL(repo)
	}
	env := append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	if token != "" {
		basic := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + token))
		env = append(env, "GIT_CONFIG_COUNT=1",
			"GIT_CONFIG_KEY_0=http.https://github.com/.extraheader",
			"GIT_CONFIG_VALUE_0=AUTHORIZATION: basic "+basic)
	}
	git := func(args ...string) (string, error) {
		cmd := exec.CommandContext(ctx, "git", args...)
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		if err != nil {
			return "", fmt.Errorf("git %s: %v: %s", args[0], err, strings.TrimSpace(string(out)))
		}
		return strings.TrimSpace(string(out)), nil
	}
	if _, err := git("clone", "--quiet", "--no-tags", url, dir); err != nil {
		return "", "", err
	}
	head, err := git("-C", dir, "symbolic-ref", "--short", "refs/remotes/origin/HEAD")
	if err != nil {
		return "", "", err
	}
	base = strings.TrimPrefix(head, "origin/")
	if _, verr := git("-C", dir, "rev-parse", "--verify", "--quiet", "refs/remotes/origin/"+branch); verr == nil {
		_, err = git("-C", dir, "checkout", "--quiet", branch)
	} else {
		_, err = git("-C", dir, "checkout", "--quiet", "-b", branch)
	}
	if err != nil {
		return "", "", err
	}
	return dir, base, nil
}

// Remove deletes all of a task's workspaces.
func (w Workspaces) Remove(taskID int64) error {
	return os.RemoveAll(filepath.Join(w.Root, fmt.Sprintf("task-%d", taskID)))
}

// Detach checks out commit sha in a prepared workspace, so a reviewer sees
// exactly the commit CI passed even if the branch moved since.
func (w Workspaces) Detach(ctx context.Context, dir, sha string) error {
	out, err := exec.CommandContext(ctx, "git", "-C", dir, "checkout", "--quiet", "--detach", sha).CombinedOutput()
	if err != nil {
		return fmt.Errorf("git checkout %s: %v: %s", sha, err, strings.TrimSpace(string(out)))
	}
	return nil
}
