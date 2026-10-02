package github

import (
	"context"
	"fmt"
	"net/url"
)

// CheckRun is one check on a commit.
type CheckRun struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	Status     string `json:"status"`     // queued, in_progress, completed
	Conclusion string `json:"conclusion"` // success, failure, neutral, cancelled, skipped, timed_out, action_required
	HTMLURL    string `json:"html_url"`
	App        struct {
		Slug string `json:"slug"`
	} `json:"app"`
	Output struct {
		Title   string `json:"title"`
		Summary string `json:"summary"`
		Text    string `json:"text"`
	} `json:"output"`
}

// Failed reports whether a completed check counts as red.
func (r CheckRun) Failed() bool {
	switch r.Conclusion {
	case "success", "neutral", "skipped":
		return false
	}
	return r.Status == "completed"
}

// IsActions reports whether the check is a GitHub Actions job, whose id is
// also the job id for JobLog.
func (r CheckRun) IsActions() bool { return r.App.Slug == "github-actions" }

// CheckRuns returns the check runs on a commit (first 100).
func (c *Client) CheckRuns(ctx context.Context, repo, sha string) ([]CheckRun, error) {
	p, err := repoPath(repo)
	if err != nil {
		return nil, err
	}
	var out struct {
		CheckRuns []CheckRun `json:"check_runs"`
	}
	_, err = c.do(ctx, "GET", fmt.Sprintf("%s/commits/%s/check-runs?per_page=100", p, url.PathEscape(sha)), "", nil, &out)
	return out.CheckRuns, err
}

// JobLog returns the plain-text log of a GitHub Actions job.
func (c *Client) JobLog(ctx context.Context, repo string, jobID int64) ([]byte, error) {
	p, err := repoPath(repo)
	if err != nil {
		return nil, err
	}
	return c.raw(ctx, fmt.Sprintf("%s/actions/jobs/%d/logs", p, jobID))
}
