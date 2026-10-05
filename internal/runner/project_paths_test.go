package runner

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/deniz-ai-orchestration/ai-orchestrator/internal/store"
)

func TestProjectTrustAndSymlinkBoundary(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	git(t, root, "init", "-q", repo)
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	p := Projects{TrustedRoots: []string{root}, Store: st}
	if _, err := p.Path(repo); err == nil {
		t.Fatal("untrusted repo allowed")
	}
	info, err := p.Stat(context.Background(), repo)
	if err != nil || !info.HasGit {
		t.Fatalf("stat %+v %v", info, err)
	}
	if err := st.UpsertProject(context.Background(), store.Project{Path: repo, Trusted: true, HasGit: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Path(repo); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Resolve(filepath.Join(root, "escape")); err == nil {
		t.Fatal("symlink escaped root")
	}
	if _, err := p.Stat(context.Background(), outside); err == nil {
		t.Fatal("outside root allowed")
	}
}

func TestBareNameProbesTrustedRoots(t *testing.T) {
	root := t.TempDir()
	other := t.TempDir()
	git(t, root, "init", "-q", filepath.Join(root, "shop"))
	p := Projects{TrustedRoots: []string{root, other}}
	got, err := p.Resolve("shop")
	if err != nil {
		t.Fatal(err)
	}
	if got != filepath.Join(root, "shop") {
		t.Fatalf("resolved %q", got)
	}
	if _, err := p.Resolve("missing"); err == nil {
		t.Fatal("missing folder resolved")
	}
	// A legacy projects.dir still wins over the probe.
	dir := t.TempDir()
	q := Projects{Dir: dir, TrustedRoots: []string{root}}
	if _, err := q.Resolve("shop"); err == nil {
		t.Fatal("resolved against an empty projects.dir")
	}
}

func TestBrowseStaysInsideRoots(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "a", "b"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, ".hidden"), 0o755); err != nil {
		t.Fatal(err)
	}
	p := Projects{TrustedRoots: []string{root}}
	current, parent, dirs, err := p.Browse("")
	if err != nil || current != "" || parent != "" || len(dirs) != 1 || dirs[0].Path != root {
		t.Fatalf("roots %+v %+v %+v %v", current, parent, dirs, err)
	}
	current, parent, dirs, err = p.Browse(root)
	if err != nil || current != root || parent != "" || len(dirs) != 1 || dirs[0].Display != "a" {
		t.Fatalf("root %+v %+v %+v %v", current, parent, dirs, err)
	}
	current, parent, dirs, err = p.Browse(filepath.Join(root, "a"))
	if err != nil || parent != root || len(dirs) != 1 || dirs[0].Display != "b" {
		t.Fatalf("sub %+v %+v %+v %v", current, parent, dirs, err)
	}
	if _, _, _, err := p.Browse("/tmp"); err == nil {
		t.Fatal("browsed outside the roots")
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	if _, _, dirs, err := p.Browse(root); err != nil {
		t.Fatal(err)
	} else {
		for _, d := range dirs {
			if d.Display == "escape" {
				t.Fatal("escaping symlink listed")
			}
		}
	}
}

func TestRecoverMigratesLegacyProject(t *testing.T) {
	e := newChatEnv(t)
	ctx := context.Background()
	// A 00008-backfilled row: folder name from projects.dir days.
	if err := e.st.UpsertProject(ctx, store.Project{Path: "shop", Trusted: true, HasGit: true}); err != nil {
		t.Fatal(err)
	}
	w, err := e.st.CreateWorkflow(ctx, store.Workflow{ProjectPath: "shop", Branch: "orch/1-x"})
	if err != nil {
		t.Fatal(err)
	}
	a, err := e.st.CreateAgent(ctx, store.Agent{Role: "developer", Provider: "claude", Model: "sonnet",
		Project: "shop", Branch: "orch/1-x", WorkflowID: w.ID})
	if err != nil {
		t.Fatal(err)
	}
	// projects.dir retired: only the parent root remains.
	e.chats.Projects = Projects{TrustedRoots: []string{filepath.Dir(e.project)}}
	if err := e.chats.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	abs := e.project
	if _, err := e.st.GetProject(ctx, abs); err != nil {
		t.Fatalf("migrated project: %v", err)
	}
	if _, err := e.st.GetProject(ctx, "shop"); err != store.ErrNotFound {
		t.Fatalf("legacy row: %v", err)
	}
	if got, _ := e.st.GetWorkflow(ctx, w.ID); got.ProjectPath != abs {
		t.Fatalf("workflow %+v", got)
	}
	if got, _ := e.st.GetAgent(ctx, a.ID); got.Project != abs {
		t.Fatalf("agent %+v", got)
	}
}
