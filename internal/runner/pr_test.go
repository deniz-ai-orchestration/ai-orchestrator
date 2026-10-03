package runner

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/deniz-ai-orchestration/ai-orchestrator/internal/github"
	"github.com/deniz-ai-orchestration/ai-orchestrator/internal/store"
)

type fakePRs struct {
	mu      sync.Mutex
	open    map[string]github.PullRequest // by branch
	created []github.NewPR
	calls   []string
}

func (f *fakePRs) DefaultBranch(context.Context, string) (string, error) { return "main", nil }

func (f *fakePRs) FindPR(_ context.Context, _ string, branch string) (github.PullRequest, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if pr, ok := f.open[branch]; ok {
		return pr, nil
	}
	return github.PullRequest{}, github.ErrNoPR
}

func (f *fakePRs) CreatePR(_ context.Context, repo string, n github.NewPR) (github.PullRequest, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.created = append(f.created, n)
	pr := github.PullRequest{Number: 40 + len(f.created), HTMLURL: "https://github.com/" + repo + "/pull/41"}
	f.open[n.Head] = pr
	return pr, nil
}

func (f *fakePRs) RequestReview(_ context.Context, _ string, n int, who []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "review "+strings.Join(who, ","))
	return nil
}

func (f *fakePRs) AddAssignees(_ context.Context, _ string, n int, who []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "assign "+strings.Join(who, ","))
	return nil
}

// withRemote gives the project a GitHub origin and points pushes at a
// local bare repository.
func (e *chatEnv) withRemote(t *testing.T) (*fakePRs, string) {
	t.Helper()
	git(t, e.project, "remote", "add", "origin", "git@github.com:deniz/shop.git")
	bare := filepath.Join(t.TempDir(), "remote.git")
	git(t, filepath.Dir(bare), "init", "-q", "--bare", bare)
	f := &fakePRs{open: map[string]github.PullRequest{}}
	e.chats.Publisher = func() (PRClient, string, error) { return f, "push-tok", nil }
	e.chats.PushURL = func(repo string) string {
		if repo != "deniz/shop" {
			t.Errorf("pushing %q", repo)
		}
		return bare
	}
	return f, bare
}

func TestOpenPR(t *testing.T) {
	e := newChatEnv(t)
	ctx := context.Background()
	f, bare := e.withRemote(t)
	e.script(t)
	dev, err := e.chats.Summon(ctx, Summon{Role: "developer", Model: "sonnet", Project: "shop", Message: "Add change.txt.\n\nKeep it short."})
	if err != nil {
		t.Fatal(err)
	}
	dev = e.waitFor(t, dev.ID, store.AgentReady)

	d, err := e.chats.Draft(ctx, dev.ID)
	if err != nil {
		t.Fatal(err)
	}
	if d.Title != "Add change.txt" || !strings.Contains(d.Body, "> Keep it short.") || !strings.Contains(d.Body, "Made the change.") ||
		!strings.Contains(d.Body, "reviewer (sonnet): Approved") {
		t.Fatalf("draft %+v", d)
	}
	n, url, created, err := e.chats.OpenPR(ctx, dev.ID, PRDraft{Title: "My title"})
	if err != nil || n != 41 || !created || url == "" {
		t.Fatalf("open: %d %q %v %v", n, url, created, err)
	}
	head := gitOut(t, dev.Workspace, "rev-parse", "HEAD")
	if got := gitOut(t, bare, "rev-parse", "refs/heads/"+dev.Branch); got != head {
		t.Fatalf("remote has %s, want %s", got, head)
	}
	if len(f.created) != 1 || f.created[0].Title != "My title" || f.created[0].Base != "main" || f.created[0].Head != dev.Branch ||
		!strings.Contains(f.created[0].Body, "Opened by orch from agent") {
		t.Fatalf("created %+v", f.created)
	}
	if strings.Join(f.calls, ";") != "review denizekinci;assign denizekinci" {
		t.Fatalf("calls %v", f.calls)
	}
	got, _ := e.st.GetAgent(ctx, dev.ID)
	if got.PRNumber != 41 || got.PRURL != url {
		t.Fatalf("agent PR %d %q", got.PRNumber, got.PRURL)
	}

	// More commits go to the same PR.
	if err := os.WriteFile(filepath.Join(dev.Workspace, "more.txt"), []byte("more\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, dev.Workspace, "add", ".")
	git(t, dev.Workspace, "-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-q", "-m", "more")
	if n, _, created, err := e.chats.OpenPR(ctx, dev.ID, PRDraft{}); err != nil || n != 41 || created {
		t.Fatalf("second push: %d %v %v", n, created, err)
	}
	if len(f.created) != 1 {
		t.Fatal("opened a second PR")
	}
	if got := gitOut(t, bare, "rev-parse", "refs/heads/"+dev.Branch); got != gitOut(t, dev.Workspace, "rev-parse", "HEAD") {
		t.Fatal("the second push did not land")
	}
	ms := e.messages(t, dev.ID)
	if last := ms[len(ms)-1]; !strings.HasPrefix(last.Body, "Pushed commit") {
		t.Fatalf("last message %+v", last)
	}
}

func TestOpenPRRefusals(t *testing.T) {
	e := newChatEnv(t)
	ctx := context.Background()
	f, _ := e.withRemote(t)
	e.fake.outputs = []string{turnOutput("s", "done", "Looked.")}
	dev, err := e.chats.Summon(ctx, Summon{Role: "developer", Model: "sonnet", Project: "shop", Message: "Look"})
	if err != nil {
		t.Fatal(err)
	}
	dev = e.settle(t, dev.ID)
	if _, _, _, err := e.chats.OpenPR(ctx, dev.ID, PRDraft{}); err == nil || !strings.Contains(err.Error(), "no commits") {
		t.Fatalf("no commits: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(dev.Workspace, ".github", "workflows"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dev.Workspace, ".github", "workflows", "x.yml"), []byte("on: push\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, dev.Workspace, "add", ".")
	git(t, dev.Workspace, "-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-q", "-m", "ci")
	if _, _, _, err := e.chats.OpenPR(ctx, dev.ID, PRDraft{}); err == nil || !strings.Contains(err.Error(), "forbidden_path") {
		t.Fatalf("forbidden path: %v", err)
	}
	if len(f.created) != 0 {
		t.Fatal("opened a PR anyway")
	}
	e.chats.Publisher = nil
	if _, _, _, err := e.chats.OpenPR(ctx, dev.ID, PRDraft{}); err == nil {
		t.Fatal("opened a PR without a publisher")
	}
}

// An agent can write the project's .git: it must not be able to point
// orch's host-side git at a config or hooks of its own.
func TestHostGitIgnoresAgentLayout(t *testing.T) {
	e := newChatEnv(t)
	ctx := context.Background()
	_, bare := e.withRemote(t)
	e.script(t)
	dev, err := e.chats.Summon(ctx, Summon{Role: "developer", Model: "sonnet", Project: "shop", Message: "Add change.txt"})
	if err != nil {
		t.Fatal(err)
	}
	dev = e.waitFor(t, dev.ID, store.AgentReady)
	head := gitOut(t, dev.Workspace, "rev-parse", "HEAD")
	common := filepath.Join(e.project, ".git")
	admin, err := adminDir(common, dev.Workspace)
	if err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "pwned")
	hook := "#!/bin/sh\ntouch " + marker + "\n"

	// A repository of the agent's own with hostile hooks and config...
	evil := filepath.Join(t.TempDir(), "evil")
	git(t, filepath.Dir(evil), "init", "-q", "--bare", evil)
	for _, h := range []string{"pre-push", "post-checkout", "reference-transaction"} {
		if err := os.WriteFile(filepath.Join(evil, "hooks", h), []byte(hook), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// ...that the worktree's .git file and its commondir now point to,
	git(t, evil, "config", "core.hooksPath", filepath.Join(evil, "hooks"))
	if err := os.WriteFile(filepath.Join(dev.Workspace, ".git"), []byte("gitdir: "+evil+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(admin, "commondir"), []byte(evil+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// plus hooks in the real hooks folder and an fsmonitor in the real
	// config (which containers only get read-only; orch ignores both too).
	for _, h := range []string{"pre-push", "reference-transaction"} {
		if err := os.WriteFile(filepath.Join(common, "hooks", h), []byte(hook), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	fsmon := filepath.Join(t.TempDir(), "fsmon")
	if err := os.WriteFile(fsmon, []byte(hook), 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := gitEnv(ctx, []string{"GIT_DIR=" + common}, "config", "core.fsmonitor", fsmon); err != nil {
		t.Fatal(out, err)
	}

	if got, err := e.chats.Projects.BranchHead(ctx, "shop", dev.Branch); err != nil || got != head {
		t.Fatalf("HEAD %q %v, want %s", got, err, head)
	}
	if dirty, err := e.chats.Projects.Dirty(ctx, "shop", dev.Workspace, dev.Branch); err != nil || dirty {
		t.Fatalf("dirty %v %v", dirty, err)
	}
	if _, _, _, err := e.chats.OpenPR(ctx, dev.ID, PRDraft{}); err != nil {
		t.Fatal(err)
	}
	if got := gitOut(t, bare, "rev-parse", "refs/heads/"+dev.Branch); got != head {
		t.Fatalf("remote has %s, want %s", got, head)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("a hook or fsmonitor ran on the host")
	}
}

func TestDirtyAndProtect(t *testing.T) {
	e := newChatEnv(t)
	ctx := context.Background()
	e.fake.outputs = []string{turnOutput("s", "done", "Looked.")}
	dev, err := e.chats.Summon(ctx, Summon{Role: "developer", Model: "sonnet", Project: "shop", Message: "Look"})
	if err != nil {
		t.Fatal(err)
	}
	dev = e.settle(t, dev.ID)
	if dirty, err := e.chats.Projects.Dirty(ctx, "shop", dev.Workspace, dev.Branch); err != nil || dirty {
		t.Fatalf("clean worktree: %v %v", dirty, err)
	}
	if err := os.WriteFile(filepath.Join(dev.Workspace, "main.go"), []byte("package main // edited\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if dirty, err := e.chats.Projects.Dirty(ctx, "shop", dev.Workspace, dev.Branch); err != nil || !dirty {
		t.Fatalf("edited worktree: %v %v", dirty, err)
	}
	common := filepath.Join(e.project, ".git")
	if b, err := os.ReadFile(filepath.Join(common, "commondir")); err != nil || strings.TrimSpace(string(b)) != "." {
		t.Fatalf("commondir %q %v", b, err)
	}
	s := e.fake.spec(t, 0)
	for _, f := range []string{"config", "hooks", "commondir"} {
		want := common + "/" + f + ":" + common + "/" + f + ":ro"
		if !slices.Contains(s.Binds, want) {
			t.Errorf("bind %q missing from %v", want, s.Binds)
		}
	}
	if err := os.WriteFile(filepath.Join(common, "commondir"), []byte("/elsewhere\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := e.chats.Projects.Protect("shop"); err == nil {
		t.Fatal("Protect accepted a commondir pointing elsewhere")
	}
}

// adminDir finds a worktree's folder under .git/worktrees.
func adminDir(common, dir string) (string, error) {
	want := realPath(filepath.Join(dir, ".git"))
	entries, err := os.ReadDir(filepath.Join(common, "worktrees"))
	if err != nil {
		return "", err
	}
	for _, e := range entries {
		admin := filepath.Join(common, "worktrees", e.Name())
		b, err := os.ReadFile(filepath.Join(admin, "gitdir"))
		if err == nil && realPath(strings.TrimSpace(string(b))) == want {
			return admin, nil
		}
	}
	return "", fmt.Errorf("%s is not a worktree of this project", dir)
}

func realPath(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return filepath.Clean(p)
}
