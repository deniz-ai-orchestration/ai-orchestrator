package github

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// User is a GitHub account.
type User struct {
	Login string `json:"login"`
}

// Label is an issue label.
type Label struct {
	Name string `json:"name"`
}

// Issue is the part of an issue orch uses.
type Issue struct {
	Number      int       `json:"number"`
	Title       string    `json:"title"`
	Body        string    `json:"body"`
	State       string    `json:"state"`
	User        User      `json:"user"`
	Labels      []Label   `json:"labels"`
	PullRequest *struct{} `json:"pull_request"`
}

// IssueEvent is one entry of the repository issue events feed.
type IssueEvent struct {
	ID        int64     `json:"id"`
	Event     string    `json:"event"`
	Actor     User      `json:"actor"`
	Label     *Label    `json:"label"`
	Issue     Issue     `json:"issue"`
	CreatedAt time.Time `json:"created_at"`
}

// Comment is an issue or PR conversation comment.
type Comment struct {
	ID   int64  `json:"id"`
	Body string `json:"body"`
	User User   `json:"user"`
}

func repoPath(repo string) (string, error) {
	owner, name, ok := strings.Cut(repo, "/")
	if !ok || owner == "" || name == "" {
		return "", fmt.Errorf("repo %q is not owner/name", repo)
	}
	return "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(name), nil
}

// IssueEvents returns the newest page of the repository's issue events. With
// a non-empty etag the request is conditional: an unchanged feed returns
// ErrNotModified and costs no rate limit.
func (c *Client) IssueEvents(ctx context.Context, repo, etag string) ([]IssueEvent, string, error) {
	p, err := repoPath(repo)
	if err != nil {
		return nil, "", err
	}
	var evs []IssueEvent
	newTag, err := c.do(ctx, "GET", p+"/issues/events?per_page=100", etag, nil, &evs)
	return evs, newTag, err
}

// GetIssue returns one issue.
func (c *Client) GetIssue(ctx context.Context, repo string, number int) (Issue, error) {
	p, err := repoPath(repo)
	if err != nil {
		return Issue{}, err
	}
	var is Issue
	_, err = c.do(ctx, "GET", fmt.Sprintf("%s/issues/%d", p, number), "", nil, &is)
	return is, err
}

// Comments returns an issue's or PR's conversation comments (first 100).
func (c *Client) Comments(ctx context.Context, repo string, number int) ([]Comment, error) {
	p, err := repoPath(repo)
	if err != nil {
		return nil, err
	}
	var cs []Comment
	_, err = c.do(ctx, "GET", fmt.Sprintf("%s/issues/%d/comments?per_page=100", p, number), "", nil, &cs)
	return cs, err
}

// CreateComment posts a comment.
func (c *Client) CreateComment(ctx context.Context, repo string, number int, body string) (Comment, error) {
	p, err := repoPath(repo)
	if err != nil {
		return Comment{}, err
	}
	var out Comment
	_, err = c.do(ctx, "POST", fmt.Sprintf("%s/issues/%d/comments", p, number), "", map[string]string{"body": body}, &out)
	return out, err
}

// SetStateLabel makes label the only label with the given prefix on the
// issue, leaving every other label alone. It is idempotent.
func (c *Client) SetStateLabel(ctx context.Context, repo string, number int, prefix, label string) error {
	p, err := repoPath(repo)
	if err != nil {
		return err
	}
	is, err := c.GetIssue(ctx, repo, number)
	if err != nil {
		return err
	}
	has := false
	for _, l := range is.Labels {
		switch {
		case l.Name == label:
			has = true
		case strings.HasPrefix(l.Name, prefix):
			path := fmt.Sprintf("%s/issues/%d/labels/%s", p, number, url.PathEscape(l.Name))
			if _, err := c.do(ctx, "DELETE", path, "", nil, nil); err != nil && !isNotFound(err) {
				return err
			}
		}
	}
	if has || label == "" {
		return nil
	}
	_, err = c.do(ctx, "POST", fmt.Sprintf("%s/issues/%d/labels", p, number), "",
		map[string][]string{"labels": {label}}, nil)
	return err
}

func isNotFound(err error) bool {
	ae, ok := err.(*APIError)
	return ok && ae.Status == 404
}
