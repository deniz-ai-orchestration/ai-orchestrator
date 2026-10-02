package github

import (
	"context"
	"fmt"
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"
)

// ChangedFile is one file in a comparison.
type ChangedFile struct {
	Filename         string `json:"filename"`
	PreviousFilename string `json:"previous_filename"`
	Status           string `json:"status"`
	Additions        int    `json:"additions"`
	Deletions        int    `json:"deletions"`
}

// Comparison is a base...head diff summary.
type Comparison struct {
	AheadBy int           `json:"ahead_by"`
	Files   []ChangedFile `json:"files"`
}

// Compare returns the files changed between base and head.
func (c *Client) Compare(ctx context.Context, repo, base, head string) (Comparison, error) {
	p, err := repoPath(repo)
	if err != nil {
		return Comparison{}, err
	}
	var cmp Comparison
	_, err = c.do(ctx, "GET", fmt.Sprintf("%s/compare/%s...%s", p, url.PathEscape(base), url.PathEscape(head)), "", nil, &cmp)
	return cmp, err
}

// PushRules bound what a developer run may push.
type PushRules struct {
	ForbiddenPaths []string // "dir/**" matches the whole tree; other patterns use path.Match
	MaxDiffLines   int
}

// Violation is one broken rule.
type Violation struct {
	Rule   string // branch, forbidden_path, diff_size
	Detail string
}

var branchRe = regexp.MustCompile(`^agent/(\d+)-[a-z0-9][a-z0-9-]*$`)

// BranchName is the branch a task's developer must push.
func BranchName(issue int, slug string) string {
	return fmt.Sprintf("agent/%d-%s", issue, slug)
}

// ValidatePush checks a developer push against the rules. It is pure; the
// caller fetches cmp with Compare(base, branch).
func ValidatePush(issue int, branch string, cmp Comparison, r PushRules) []Violation {
	var vs []Violation
	if m := branchRe.FindStringSubmatch(branch); m == nil || m[1] != strconv.Itoa(issue) {
		vs = append(vs, Violation{"branch", fmt.Sprintf("%q is not agent/%d-<slug>", branch, issue)})
	}
	lines := 0
	for _, f := range cmp.Files {
		lines += f.Additions + f.Deletions
		for _, name := range []string{f.Filename, f.PreviousFilename} {
			if name != "" && forbidden(name, r.ForbiddenPaths) {
				vs = append(vs, Violation{"forbidden_path", name})
			}
		}
	}
	if r.MaxDiffLines > 0 && lines > r.MaxDiffLines {
		vs = append(vs, Violation{"diff_size", fmt.Sprintf("%d changed lines > %d", lines, r.MaxDiffLines)})
	}
	return vs
}

func forbidden(name string, patterns []string) bool {
	for _, p := range patterns {
		if dir, ok := strings.CutSuffix(p, "/**"); ok {
			if name == dir || strings.HasPrefix(name, dir+"/") {
				return true
			}
			continue
		}
		if ok, _ := path.Match(p, name); ok {
			return true
		}
	}
	return false
}
