package runner

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Projects are the git repositories under one folder on PC2 that summoned
// agents work on. Each agent gets its own git worktree, so parallel agents
// never edit each other's files.
type Projects struct {
	Dir string
}

// projectName is a folder directly under Dir: no path separators, no
// leading dot.
var projectName = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]{0,99}$`)

// ErrNoProjects means projects.dir is not set.
var ErrNoProjects = errors.New("projects.dir is not set in the config")

// List returns the folders under Dir that are git repositories.
func (p Projects) List() ([]string, error) {
	if p.Dir == "" {
		return nil, ErrNoProjects
	}
	entries, err := os.ReadDir(p.Dir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() || !projectName.MatchString(e.Name()) {
			continue
		}
		if fi, err := os.Stat(filepath.Join(p.Dir, e.Name(), ".git")); err == nil && fi.IsDir() {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out, nil
}

// Path returns a project's folder after checking the name.
func (p Projects) Path(name string) (string, error) {
	if p.Dir == "" {
		return "", ErrNoProjects
	}
	if !projectName.MatchString(name) {
		return "", fmt.Errorf("%q is not a project folder name", name)
	}
	dir := filepath.Join(p.Dir, name)
	if fi, err := os.Stat(filepath.Join(dir, ".git")); err != nil || !fi.IsDir() {
		return "", fmt.Errorf("%s is not a git repository", dir)
	}
	return dir, nil
}

// Worktree is an agent's checkout of a project.
type Worktree struct {
	Dir    string // the worktree on the host
	Branch string
	Base   string // commit the worktree started from
	// GitDir is the project's .git directory. The worktree's .git file
	// points into it by absolute path, so it is mounted into the container
	// at the same path.
	GitDir string
}

// AddWorktree checks out a project into dir: on a new branch from the
// project's current commit when create is set, otherwise on an existing
// branch (which git refuses if another worktree has it checked out).
func (p Projects) AddWorktree(ctx context.Context, project, dir, branch string, create bool) (Worktree, error) {
	repo, err := p.Path(project)
	if err != nil {
		return Worktree{}, err
	}
	if err := checkBranch(ctx, branch); err != nil {
		return Worktree{}, err
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		return Worktree{}, err
	}
	args := []string{"worktree", "add", "--quiet", dir, branch}
	if create {
		args = []string{"worktree", "add", "--quiet", "-b", branch, dir, "HEAD"}
	}
	if _, err := gitIn(ctx, repo, args...); err != nil {
		return Worktree{}, err
	}
	w := Worktree{Dir: dir, Branch: branch}
	if w.Base, err = gitIn(ctx, dir, "rev-parse", "HEAD"); err != nil {
		return w, err
	}
	if w.GitDir, err = gitIn(ctx, repo, "rev-parse", "--path-format=absolute", "--git-common-dir"); err != nil {
		return w, err
	}
	return w, nil
}

// RemoveWorktree deletes an agent's worktree. Its branch and commits stay in
// the project.
func (p Projects) RemoveWorktree(ctx context.Context, project, dir string) error {
	repo, err := p.Path(project)
	if err != nil {
		return err
	}
	if _, err := os.Stat(dir); err == nil {
		if _, err := gitIn(ctx, repo, "worktree", "remove", "--force", dir); err != nil {
			return err
		}
	}
	_, err = gitIn(ctx, repo, "worktree", "prune")
	return err
}

// checkBranch rejects names git would not accept, and names that would
// read as an option.
func checkBranch(ctx context.Context, branch string) error {
	if branch == "" || strings.HasPrefix(branch, "-") {
		return fmt.Errorf("%q is not a branch name", branch)
	}
	if err := exec.CommandContext(ctx, "git", "check-ref-format", "--branch", branch).Run(); err != nil {
		return fmt.Errorf("%q is not a branch name", branch)
	}
	return nil
}

func gitIn(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %v: %s", args[0], err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}
