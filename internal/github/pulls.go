package github

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// ErrNoPR is returned when a branch has no open pull request.
var ErrNoPR = errors.New("no open pull request for branch")

// Ref is one side of a pull request.
type Ref struct {
	Ref string `json:"ref"`
	SHA string `json:"sha"`
}

// PullRequest is the part of a PR orch checks after a developer run.
type PullRequest struct {
	Number int    `json:"number"`
	State  string `json:"state"`
	Title  string `json:"title"`
	Body   string `json:"body"`
	User   User   `json:"user"`
	Head   Ref    `json:"head"`
	Base   Ref    `json:"base"`
}

// FindPR returns the open pull request whose head is branch in repo itself
// (never a fork).
func (c *Client) FindPR(ctx context.Context, repo, branch string) (PullRequest, error) {
	p, err := repoPath(repo)
	if err != nil {
		return PullRequest{}, err
	}
	owner, _, _ := strings.Cut(repo, "/")
	q := url.Values{"state": {"open"}, "head": {owner + ":" + branch}}
	var prs []PullRequest
	if _, err := c.do(ctx, "GET", fmt.Sprintf("%s/pulls?%s", p, q.Encode()), "", nil, &prs); err != nil {
		return PullRequest{}, err
	}
	for _, pr := range prs {
		if pr.Head.Ref == branch {
			return pr, nil
		}
	}
	return PullRequest{}, ErrNoPR
}

var closesRe = regexp.MustCompile(`(?i)\b(?:close[sd]?|fix(?:e[sd])?|resolve[sd]?)\s+#(\d+)\b`)

// LinksIssue reports whether a PR body closes the issue with a GitHub
// closing keyword ("Closes #17").
func LinksIssue(body string, issue int) bool {
	for _, m := range closesRe.FindAllStringSubmatch(body, -1) {
		if m[1] == strconv.Itoa(issue) {
			return true
		}
	}
	return false
}
