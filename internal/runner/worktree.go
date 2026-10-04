package runner

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/deniz-ai-orchestration/ai-orchestrator/internal/store"
)

// Projects are the directories on PC2 that summoned agents work on. Each
// agent gets its own git worktree, so parallel agents never edit each
// other's files.
//
// A project is named either by its folder under Dir (the legacy form, what
// agents summoned before workflows store) or by a free absolute path
// inside one of the TrustedRoots. Per-project trust lives in the store's
// projects table and is granted from the panel; the roots here are the
// outer boundary: orch never resolves a path outside them.
type Projects struct {
	Dir          string
	TrustedRoots []string
	Store        *store.Store
}

// projectName is a folder directly under Dir: no path separators, no
// leading dot.
var projectName = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]{0,99}$`)

// ErrNoProjects means no project root is set.
var ErrNoProjects = errors.New("projects.dir or projects.trusted_roots is not set in the config")

// roots are the trusted roots, cleaned and deduped, Dir first.
func (p Projects) roots() []string {
	var out []string
	seen := map[string]bool{}
	for _, r := range append([]string{p.Dir}, p.TrustedRoots...) {
		if r == "" {
			continue
		}
		r = filepath.Clean(r)
		if !filepath.IsAbs(r) || seen[r] {
			continue
		}
		seen[r] = true
		out = append(out, r)
	}
	return out
}

// Resolve turns a project (a legacy folder name under Dir, "~/..." or an
// absolute path) into a clean absolute directory inside a trusted root.
// It does not look at the filesystem.
func (p Projects) Resolve(project string) (string, error) {
	roots := p.roots()
	if len(roots) == 0 {
		return "", ErrNoProjects
	}
	target := strings.TrimSpace(project)
	if target == "" {
		return "", errors.New("no project path")
	}
	if target != "~" && !strings.ContainsAny(target, `/\`) {
		// A legacy folder name under projects.dir.
		if !projectName.MatchString(target) {
			return "", fmt.Errorf("%q is not a project folder name", target)
		}
		if p.Dir == "" {
			return "", ErrNoProjects
		}
		target = filepath.Join(p.Dir, target)
	}
	if target == "~" || strings.HasPrefix(target, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		target = filepath.Join(home, strings.TrimPrefix(target, "~"))
	}
	if !filepath.IsAbs(target) {
		return "", fmt.Errorf("%q is not a project folder name or absolute path", project)
	}
	target = filepath.Clean(target)
	resolved, err := filepath.EvalSymlinks(target)
	if err != nil {
		return "", err
	}
	for _, r := range roots {
		root, err := filepath.EvalSymlinks(r)
		if err != nil {
			continue
		}
		rel, err := filepath.Rel(root, resolved)
		if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return resolved, nil
		}
	}
	return "", fmt.Errorf("%s is outside the trusted project roots", target)
}

// maxTrustFiles caps how many files a trust prompt counts.
const maxTrustFiles = 10000

// ProjectInfo is what the trust prompt shows about a directory.
type ProjectInfo struct {
	Path   string // resolved, absolute
	HasGit bool   // .git is a directory: orch can make worktrees of it
	Repo   string // owner/name of a GitHub origin, "" when there is none
	Files  int    // files counted, up to maxTrustFiles
	Capped bool   // counting stopped at maxTrustFiles
}

// Stat inspects a project directory for the trust prompt: is it a git
// repository, where does its origin point, how big is it. A linked
// worktree (.git is a file) counts as no git: orch's own worktrees and
// its protected .git mounts need a real repository directory.
func (p Projects) Stat(ctx context.Context, project string) (ProjectInfo, error) {
	dir, err := p.Resolve(project)
	if err != nil {
		return ProjectInfo{}, err
	}
	fi, err := os.Stat(dir)
	if err != nil {
		return ProjectInfo{}, err
	}
	if !fi.IsDir() {
		return ProjectInfo{}, fmt.Errorf("%s is not a directory", dir)
	}
	info := ProjectInfo{Path: dir}
	if g, err := os.Stat(filepath.Join(dir, ".git")); err == nil && g.IsDir() {
		info.HasGit = true
		if url, err := repoGit(ctx, dir, "remote", "get-url", "origin"); err == nil {
			if m := githubRepo.FindStringSubmatch(url); m != nil {
				info.Repo = m[1]
			}
		}
	}
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // an unreadable corner simply does not count
		}
		if d.IsDir() {
			if d.Name() == ".git" && path != dir {
				return fs.SkipDir
			}
			return nil
		}
		info.Files++
		if info.Files >= maxTrustFiles {
			info.Capped = true
			return fs.SkipAll
		}
		return nil
	})
	return info, nil
}

// List returns the projects the summon form offers: the git repositories
// directly under each root. Folders under the legacy Dir are returned as
// names (what agents summoned from them store), folders under the other
// roots as absolute paths; a folder listed both ways appears once, as a
// name.
func (p Projects) List() ([]string, error) {
	roots := p.roots()
	if len(roots) == 0 {
		return nil, ErrNoProjects
	}
	var out []string
	seen := map[string]bool{}
	for _, r := range roots {
		entries, err := os.ReadDir(r)
		if err != nil {
			if os.IsNotExist(err) {
				continue // a root that is not there offers no projects
			}
			return nil, err
		}
		legacy := p.Dir != "" && r == filepath.Clean(p.Dir)
		for _, e := range entries {
			if !e.IsDir() || !projectName.MatchString(e.Name()) {
				continue
			}
			sub := filepath.Join(r, e.Name())
			if fi, err := os.Stat(filepath.Join(sub, ".git")); err != nil || !fi.IsDir() {
				continue
			}
			if seen[sub] {
				continue
			}
			seen[sub] = true
			if legacy {
				out = append(out, e.Name())
			} else {
				out = append(out, sub)
			}
		}
	}
	sort.Strings(out)
	return out, nil
}

// Path resolves a project and requires a git repository there: every
// worktree and git command orch runs for an agent needs one. A chat-only
// project (no git) is resolved with Resolve instead.
func (p Projects) Path(project string) (string, error) {
	dir, err := p.Resolve(project)
	if err != nil {
		return "", err
	}
	if p.Store != nil {
		pr, err := p.Store.GetProject(context.Background(), dir)
		if err != nil || !pr.Trusted {
			return "", fmt.Errorf("trust %s in the panel first", dir)
		}
	}
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
	if _, err := repoGit(ctx, repo, args...); err != nil {
		return Worktree{}, err
	}
	w := Worktree{Dir: dir, Branch: branch, GitDir: filepath.Join(repo, ".git")}
	if w.Base, err = p.BranchHead(ctx, project, branch); err != nil {
		return w, err
	}
	return w, nil
}

// AddDetached checks out one commit of a project into dir without a
// branch: a tester's view of exactly what the developer committed.
func (p Projects) AddDetached(ctx context.Context, project, dir, sha string) (Worktree, error) {
	repo, err := p.Path(project)
	if err != nil {
		return Worktree{}, err
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		return Worktree{}, err
	}
	if _, err := repoGit(ctx, repo, "worktree", "add", "--quiet", "--detach", dir, sha); err != nil {
		return Worktree{}, err
	}
	return Worktree{Dir: dir, Base: sha, GitDir: filepath.Join(repo, ".git")}, nil
}

// RemoveWorktree deletes an agent's worktree. Its branch and commits stay in
// the project.
func (p Projects) RemoveWorktree(ctx context.Context, project, dir string) error {
	repo, err := p.Path(project)
	if err != nil {
		return err
	}
	if _, err := os.Stat(dir); err == nil {
		// Forced twice, git skips its status check, which would run inside
		// the worktree's own (agent-writable) git folder.
		if _, err := repoGit(ctx, repo, "worktree", "remove", "--force", "--force", dir); err != nil {
			return err
		}
	}
	_, err = repoGit(ctx, repo, "worktree", "prune")
	return err
}

// Agents commit into the project's .git directory, so they can write it.
// orch's own git commands therefore never run in an agent's worktree: its
// .git file and the worktree's admin folder (with its commondir) are the
// agent's to change. They run on the project's .git, named explicitly, with
// hooks and fsmonitor off. In agent containers that folder's config,
// hooks and commondir are read-only, so an agent cannot redirect it.

// CommonDir is a project's .git directory.
func (p Projects) CommonDir(project string) (string, error) {
	repo, err := p.Path(project)
	if err != nil {
		return "", err
	}
	return filepath.Join(repo, ".git"), nil
}

// Protect prepares a project's .git for an agent container: a commondir
// file naming the folder itself (which git accepts) and a hooks folder,
// so both exist to be mounted read-only. It returns the .git path.
func (p Projects) Protect(project string) (string, error) {
	common, err := p.CommonDir(project)
	if err != nil {
		return "", err
	}
	cd := filepath.Join(common, "commondir")
	b, err := os.ReadFile(cd)
	switch {
	case os.IsNotExist(err):
		if err := os.WriteFile(cd, []byte(".\n"), 0o644); err != nil {
			return "", err
		}
	case err != nil:
		return "", err
	case strings.TrimSpace(string(b)) != ".":
		return "", fmt.Errorf("%s points elsewhere (%q); remove it", cd, strings.TrimSpace(string(b)))
	}
	if err := os.MkdirAll(filepath.Join(common, "hooks"), 0o755); err != nil {
		return "", err
	}
	return common, nil
}

// Git runs git on a project's repository.
func (p Projects) Git(ctx context.Context, project string, args ...string) (string, error) {
	repo, err := p.Path(project)
	if err != nil {
		return "", err
	}
	return repoGit(ctx, repo, args...)
}

// BranchHead is the commit a branch points to.
func (p Projects) BranchHead(ctx context.Context, project, branch string) (string, error) {
	if err := checkBranch(ctx, branch); err != nil {
		return "", err
	}
	return p.Git(ctx, project, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch+"^{commit}")
}

// Dirty reports whether an agent's worktree differs from its branch:
// uncommitted or untracked files. Git works on a temporary index and
// never reads the worktree's own git files.
func (p Projects) Dirty(ctx context.Context, project, dir, branch string) (bool, error) {
	repo, err := p.Path(project)
	if err != nil {
		return false, err
	}
	common := filepath.Join(repo, ".git")
	idx, err := os.CreateTemp("", "orch-index-")
	if err != nil {
		return false, err
	}
	idx.Close()
	defer os.Remove(idx.Name())
	env := []string{"GIT_DIR=" + common, "GIT_WORK_TREE=" + dir, "GIT_INDEX_FILE=" + idx.Name()}
	git := func(args ...string) (string, error) {
		return gitEnv(ctx, env, append([]string{"-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=false"}, args...)...)
	}
	// Read the branch into the copy and compare the files themselves.
	if _, err := git("read-tree", "refs/heads/"+branch); err != nil {
		return false, err
	}
	// read-tree leaves no file stats: refresh them (into the copy) so only
	// real changes show. It fails when files changed, which diff-files says.
	_, _ = git("update-index", "-q", "--refresh")
	changed, err := git("diff-files", "--name-only", "--no-ext-diff")
	if err != nil {
		return false, err
	}
	// The worktree's .git file is not the project's; it never counts.
	untracked, err := git("ls-files", "--others", "--exclude-standard", "--exclude=/.git")
	if err != nil {
		return false, err
	}
	return changed != "" || untracked != "", nil
}

// repoGit runs git in a project's own checkout.
func repoGit(ctx context.Context, repo string, args ...string) (string, error) {
	return gitEnv(ctx, []string{"GIT_DIR=" + filepath.Join(repo, ".git"), "GIT_WORK_TREE=" + repo},
		append([]string{"-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=false"}, args...)...)
}

func gitEnv(ctx context.Context, env []string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Env = append(os.Environ(), append([]string{"GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_NOSYSTEM=1"}, env...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %v: %s", gitVerb(args), err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

// gitVerb is the git subcommand, past any -c options.
func gitVerb(args []string) string {
	for i := 0; i < len(args); i++ {
		if args[i] == "-c" {
			i++
			continue
		}
		return args[i]
	}
	return ""
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
