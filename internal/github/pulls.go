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
	Number  int    `json:"number"`
	HTMLURL string `json:"html_url"`
	State   string `json:"state"`
	Title   string `json:"title"`
	Body    string `json:"body"`
	User    User   `json:"user"`
	Head    Ref    `json:"head"`
	Base    Ref    `json:"base"`
	Merged  bool   `json:"merged"`
	// Mergeable is nil while GitHub is still computing it.
	Mergeable      *bool  `json:"mergeable"`
	MergeableState string `json:"mergeable_state"`
}

// GetPR returns one pull request, including its merge state.
func (c *Client) GetPR(ctx context.Context, repo string, number int) (PullRequest, error) {
	p, err := repoPath(repo)
	if err != nil {
		return PullRequest{}, err
	}
	var pr PullRequest
	_, err = c.do(ctx, "GET", fmt.Sprintf("%s/pulls/%d", p, number), "", nil, &pr)
	return pr, err
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

// NewPR is a pull request to open.
type NewPR struct {
	Title string `json:"title"`
	Head  string `json:"head"`
	Base  string `json:"base"`
	Body  string `json:"body"`
}

// DefaultBranch returns a repository's default branch.
func (c *Client) DefaultBranch(ctx context.Context, repo string) (string, error) {
	p, err := repoPath(repo)
	if err != nil {
		return "", err
	}
	var r struct {
		DefaultBranch string `json:"default_branch"`
	}
	if _, err := c.do(ctx, "GET", p, "", nil, &r); err != nil {
		return "", err
	}
	if r.DefaultBranch == "" {
		return "", fmt.Errorf("%s has no default branch", repo)
	}
	return r.DefaultBranch, nil
}

// CreatePR opens a pull request.
func (c *Client) CreatePR(ctx context.Context, repo string, pr NewPR) (PullRequest, error) {
	p, err := repoPath(repo)
	if err != nil {
		return PullRequest{}, err
	}
	var out PullRequest
	_, err = c.do(ctx, "POST", p+"/pulls", "", pr, &out)
	return out, err
}

// RequestReview asks people to review a pull request.
func (c *Client) RequestReview(ctx context.Context, repo string, number int, logins []string) error {
	p, err := repoPath(repo)
	if err != nil {
		return err
	}
	_, err = c.do(ctx, "POST", fmt.Sprintf("%s/pulls/%d/requested_reviewers", p, number), "",
		map[string][]string{"reviewers": logins}, nil)
	return err
}

// AddAssignees assigns people to an issue or pull request.
func (c *Client) AddAssignees(ctx context.Context, repo string, number int, logins []string) error {
	p, err := repoPath(repo)
	if err != nil {
		return err
	}
	_, err = c.do(ctx, "POST", fmt.Sprintf("%s/issues/%d/assignees", p, number), "",
		map[string][]string{"assignees": logins}, nil)
	return err
}
