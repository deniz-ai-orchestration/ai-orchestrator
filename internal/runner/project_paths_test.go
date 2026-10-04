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
