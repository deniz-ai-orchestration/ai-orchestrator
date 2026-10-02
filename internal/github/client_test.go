package github

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fakeAPI is a tiny in-memory GitHub for one issue.
type fakeAPI struct {
	mu       sync.Mutex
	labels   []string
	comments []Comment
	calls    []string
}

func (f *fakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, r.Method+" "+r.URL.Path)
	if r.Header.Get("Authorization") != "Bearer tok" {
		w.WriteHeader(401)
		return
	}
	switch {
	case r.URL.Path == "/repos/o/r/issues/events":
		if r.Header.Get("If-None-Match") == `"v1"` {
			w.WriteHeader(304)
			return
		}
		w.Header().Set("ETag", `"v1"`)
		_ = json.NewEncoder(w).Encode([]IssueEvent{{ID: 9, Event: "labeled", Actor: User{"denizekinci"}, Label: &Label{"agent:ready"}, Issue: Issue{Number: 3}}})
	case r.URL.Path == "/repos/o/r/issues/3" && r.Method == "GET":
		ls := []Label{}
		for _, l := range f.labels {
			ls = append(ls, Label{l})
		}
		_ = json.NewEncoder(w).Encode(Issue{Number: 3, Title: "t", State: "open", Labels: ls})
	case r.URL.Path == "/repos/o/r/issues/3/labels" && r.Method == "POST":
		var in map[string][]string
		_ = json.NewDecoder(r.Body).Decode(&in)
		f.labels = append(f.labels, in["labels"]...)
		w.WriteHeader(200)
		_, _ = w.Write([]byte("[]"))
	case strings.HasPrefix(r.URL.Path, "/repos/o/r/issues/3/labels/") && r.Method == "DELETE":
		name := strings.TrimPrefix(r.URL.Path, "/repos/o/r/issues/3/labels/")
		for i, l := range f.labels {
			if l == name {
				f.labels = append(f.labels[:i], f.labels[i+1:]...)
				break
			}
		}
		w.WriteHeader(200)
	case r.URL.Path == "/repos/o/r/issues/3/comments" && r.Method == "GET":
		_ = json.NewEncoder(w).Encode(f.comments)
	case r.URL.Path == "/repos/o/r/issues/3/comments" && r.Method == "POST":
		var in map[string]string
		_ = json.NewDecoder(r.Body).Decode(&in)
		c := Comment{ID: int64(len(f.comments) + 1), Body: in["body"], User: User{"deniz-agent"}}
		f.comments = append(f.comments, c)
		w.WriteHeader(201)
		_ = json.NewEncoder(w).Encode(c)
	case r.URL.Path == "/repos/o/r/issues/4":
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("X-RateLimit-Reset", "1790934600")
		w.WriteHeader(403)
	default:
		w.WriteHeader(404)
		_, _ = w.Write([]byte(`{"message":"Not Found"}`))
	}
}

func newTest(t *testing.T) (*Client, *fakeAPI) {
	f := &fakeAPI{}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	c := New("tok")
	c.BaseURL = srv.URL
	return c, f
}

func TestIssueEventsETag(t *testing.T) {
	c, _ := newTest(t)
	ctx := context.Background()
	evs, tag, err := c.IssueEvents(ctx, "o/r", "")
	if err != nil || len(evs) != 1 || tag != `"v1"` || evs[0].Label.Name != "agent:ready" {
		t.Fatalf("%v %q %v", evs, tag, err)
	}
	if _, _, err := c.IssueEvents(ctx, "o/r", tag); !errors.Is(err, ErrNotModified) {
		t.Fatalf("conditional request: %v", err)
	}
}

func TestErrors(t *testing.T) {
	c, _ := newTest(t)
	ctx := context.Background()
	_, err := c.GetIssue(ctx, "o/r", 4)
	var rl *RateLimitError
	if !errors.As(err, &rl) || rl.Reset.Unix() != 1790934600 {
		t.Fatalf("rate limit: %v", err)
	}
	_, err = c.GetIssue(ctx, "o/r", 99)
	var ae *APIError
	if !errors.As(err, &ae) || ae.Status != 404 {
		t.Fatalf("404: %v", err)
	}
	if _, err := c.GetIssue(ctx, "bad", 1); err == nil {
		t.Fatal("bad repo accepted")
	}
	bad := New("wrong")
	bad.BaseURL = c.BaseURL
	if _, err := bad.GetIssue(ctx, "o/r", 3); !errors.As(err, &ae) || ae.Status != 401 {
		t.Fatalf("401: %v", err)
	}
}

func TestSetStateLabel(t *testing.T) {
	c, f := newTest(t)
	ctx := context.Background()
	f.labels = []string{"bug", "orch:dev", "agent:ready"}
	if err := c.SetStateLabel(ctx, "o/r", 3, "orch:", "orch:review"); err != nil {
		t.Fatal(err)
	}
	want := "bug,agent:ready,orch:review"
	if got := strings.Join(f.labels, ","); got != want {
		t.Fatalf("labels = %s, want %s", got, want)
	}
	n := len(f.calls)
	if err := c.SetStateLabel(ctx, "o/r", 3, "orch:", "orch:review"); err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != n+1 { // only the GET
		t.Fatalf("second call should be a no-op, made %v", f.calls[n:])
	}
}

func TestEnsureCommentIsIdempotent(t *testing.T) {
	c, f := newTest(t)
	ctx := context.Background()
	m := Marker(17, "review", "abc")
	if m != "<!-- orch:task=17 kind=review sha=abc -->" {
		t.Fatalf("marker %q", m)
	}
	for i, want := range []bool{true, false} {
		posted, err := c.EnsureComment(ctx, "o/r", 3, m, "LGTM", "deniz-agent")
		if err != nil || posted != want {
			t.Fatalf("call %d: posted=%v err=%v", i, posted, err)
		}
	}
	if len(f.comments) != 1 || !strings.HasSuffix(f.comments[0].Body, m) {
		t.Fatalf("comments %+v", f.comments)
	}
	// Same marker from someone else does not count.
	f.comments[0].User.Login = "mallory"
	if posted, _ := c.EnsureComment(ctx, "o/r", 3, m, "LGTM", "deniz-agent"); !posted {
		t.Fatal("a copied marker from another account must not suppress the post")
	}
}
