package github

import (
	"context"
	"fmt"
	"strings"
)

// Marker is a hidden HTML comment that makes a posted comment or review
// idempotent: before posting, orch looks for its marker and skips the post
// if it is already there, so a crash and restart never posts twice.
func Marker(taskID int64, kind, headSHA string) string {
	m := fmt.Sprintf("<!-- orch:task=%d kind=%s", taskID, kind)
	if headSHA != "" {
		m += " sha=" + headSHA
	}
	return m + " -->"
}

// EnsureComment posts body with marker on the issue or PR unless a comment
// by the same account already carries the marker. It reports whether it
// posted.
func (c *Client) EnsureComment(ctx context.Context, repo string, number int, marker, body, self string) (bool, error) {
	cs, err := c.Comments(ctx, repo, number)
	if err != nil {
		return false, err
	}
	for _, cm := range cs {
		if strings.Contains(cm.Body, marker) && (self == "" || strings.EqualFold(cm.User.Login, self)) {
			return false, nil
		}
	}
	if _, err := c.CreateComment(ctx, repo, number, body+"\n\n"+marker); err != nil {
		return false, err
	}
	return true, nil
}
