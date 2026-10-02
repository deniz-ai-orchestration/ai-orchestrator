// Package github is orch's own small GitHub REST client: issue events with
// ETags, issues, labels, marker comments and push comparison. It uses orch's
// deniz-agent token, never the agents' tokens.
//
// It is plain net/http rather than go-github because the poller needs direct
// control over conditional requests, and orch only calls a handful of
// endpoints.
package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"
)

// DefaultBaseURL is the public GitHub API.
const DefaultBaseURL = "https://api.github.com"

// ErrNotModified is returned by conditional requests that hit the ETag.
var ErrNotModified = errors.New("not modified")

// RateLimitError means GitHub refused the call until Reset.
type RateLimitError struct{ Reset time.Time }

func (e *RateLimitError) Error() string {
	return "github rate limit until " + e.Reset.Format(time.RFC3339)
}

// APIError is any other non-2xx response.
type APIError struct {
	Status int
	Body   string
}

func (e *APIError) Error() string { return fmt.Sprintf("github %d: %s", e.Status, e.Body) }

// Client calls the GitHub REST API with one token.
type Client struct {
	BaseURL string
	token   string
	http    *http.Client
}

// New returns a client for the given token.
func New(token string) *Client {
	return &Client{BaseURL: DefaultBaseURL, token: token, http: &http.Client{Timeout: 30 * time.Second}}
}

// do sends a request. With etag set it is conditional and returns
// ErrNotModified on 304. It returns the response ETag.
func (c *Client) do(ctx context.Context, method, path, etag string, in, out any) (string, error) {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return "", err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, body)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "orch")
	req.Header.Set("Authorization", "Bearer "+c.token)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return "", err
	}

	switch {
	case resp.StatusCode == http.StatusNotModified:
		return etag, ErrNotModified
	case isRateLimited(resp):
		return "", &RateLimitError{Reset: resetTime(resp)}
	case resp.StatusCode >= 300:
		return "", &APIError{Status: resp.StatusCode, Body: truncate(string(raw), 500)}
	}
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return "", fmt.Errorf("decode %s %s: %w", method, path, err)
		}
	}
	return resp.Header.Get("ETag"), nil
}

func isRateLimited(resp *http.Response) bool {
	if resp.StatusCode == http.StatusTooManyRequests {
		return true
	}
	return resp.StatusCode == http.StatusForbidden && resp.Header.Get("X-RateLimit-Remaining") == "0"
}

func resetTime(resp *http.Response) time.Time {
	if s := resp.Header.Get("Retry-After"); s != "" {
		if n, err := strconv.Atoi(s); err == nil {
			return time.Now().Add(time.Duration(n) * time.Second)
		}
	}
	if n, err := strconv.ParseInt(resp.Header.Get("X-RateLimit-Reset"), 10, 64); err == nil {
		return time.Unix(n, 0)
	}
	return time.Now().Add(time.Minute)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
