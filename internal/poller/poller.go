// Package poller watches GitHub without an inbound port. Each round it
// fetches every repo's issue events with an ETag (an unchanged feed costs no
// rate limit), writes the trigger labels it accepts to the inbox once per
// event id, and then turns inbox items into tasks.
package poller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/deniz-ai-orchestration/ai-orchestrator/internal/config"
	"github.com/deniz-ai-orchestration/ai-orchestrator/internal/github"
	"github.com/deniz-ai-orchestration/ai-orchestrator/internal/store"
)

// GitHub is the part of the GitHub client the poller uses.
type GitHub interface {
	IssueEvents(ctx context.Context, repo, etag string) ([]github.IssueEvent, string, error)
	GetIssue(ctx context.Context, repo string, number int) (github.Issue, error)
}

// Poller turns trigger labels into tasks.
type Poller struct {
	Store *store.Store
	GH    GitHub
	Cfg   config.GitHub
	Log   *slog.Logger
}

type labelPayload struct {
	Repo  string `json:"repo"`
	Issue int    `json:"issue"`
	Actor string `json:"actor"`
}

// Run polls until ctx is done. A rate limit pauses polling until it resets.
func (p *Poller) Run(ctx context.Context) error {
	for {
		wait := p.Cfg.PollInterval
		if err := p.Round(ctx); err != nil {
			var rl *github.RateLimitError
			if errors.As(err, &rl) {
				wait = max(time.Until(rl.Reset), wait)
				p.Log.Warn("github rate limited", "until", rl.Reset)
			} else if ctx.Err() == nil {
				p.Log.Error("poll round failed", "err", err)
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(wait):
		}
	}
}

// Round polls every repo once and then processes the inbox.
func (p *Poller) Round(ctx context.Context) error {
	var errs []error
	for _, repo := range p.Cfg.Repos {
		if err := p.pollRepo(ctx, repo); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", repo, err))
		}
	}
	if err := p.ProcessInbox(ctx); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

func (p *Poller) pollRepo(ctx context.Context, repo string) error {
	endpoint := "issue_events:" + repo
	etag, err := p.Store.Cursor(ctx, endpoint)
	if err != nil {
		return err
	}
	evs, newTag, err := p.GH.IssueEvents(ctx, repo, etag)
	if errors.Is(err, github.ErrNotModified) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, ev := range evs {
		if !p.accepts(ev) {
			continue
		}
		payload, _ := json.Marshal(labelPayload{Repo: repo, Issue: ev.Issue.Number, Actor: ev.Actor.Login})
		key := fmt.Sprintf("github:%s:issue_event:%d", repo, ev.ID)
		added, err := p.Store.AddInbox(ctx, "github", key, string(payload))
		if err != nil {
			return err
		}
		if added {
			p.Log.Info("trigger label seen", "repo", repo, "issue", ev.Issue.Number, "event", ev.ID)
		}
	}
	// The cursor moves only after the events are stored, so a crash in
	// between re-reads the page and the dedupe key drops repeats.
	return p.Store.SetCursor(ctx, endpoint, newTag)
}

// accepts keeps only the trigger label added by the trusted account on an
// issue (not a PR).
func (p *Poller) accepts(ev github.IssueEvent) bool {
	return ev.Event == "labeled" && ev.Label != nil && ev.Label.Name == p.Cfg.Trigger &&
		strings.EqualFold(ev.Actor.Login, p.Cfg.TrustedActor) && ev.Issue.PullRequest == nil
}

// ProcessInbox creates a task for each accepted label. An issue gets at most
// one active task; the snapshot of its title and body is taken here, and
// only issues the trusted account wrote are accepted.
func (p *Poller) ProcessInbox(ctx context.Context) error {
	items, err := p.Store.PendingInbox(ctx, 50)
	if err != nil {
		return err
	}
	for _, it := range items {
		if it.Source != "github" {
			continue
		}
		taskID, note, err := p.createTask(ctx, it)
		if err != nil {
			return err // transient (GitHub or DB); retried next round
		}
		if err := p.Store.FinishInbox(ctx, it.ID, taskID, note); err != nil {
			return err
		}
	}
	return nil
}

func (p *Poller) createTask(ctx context.Context, it store.InboxItem) (int64, string, error) {
	var lp labelPayload
	if err := json.Unmarshal([]byte(it.Payload), &lp); err != nil {
		return 0, "bad payload: " + err.Error(), nil
	}
	is, err := p.GH.GetIssue(ctx, lp.Repo, lp.Issue)
	if err != nil {
		return 0, "", err
	}
	switch {
	case !strings.EqualFold(is.User.Login, p.Cfg.TrustedActor):
		p.Log.Warn("ignoring issue not written by the trusted account", "repo", lp.Repo, "issue", lp.Issue, "author", is.User.Login)
		return 0, "issue author " + is.User.Login + " is not trusted", nil
	case is.State != "open":
		return 0, "issue is " + is.State, nil
	case !hasLabel(is, p.Cfg.Trigger):
		return 0, "trigger label was removed", nil
	}
	if t, err := p.Store.ActiveTaskFor(ctx, lp.Repo, lp.Issue); err == nil {
		return t.ID, "issue already has an active task", nil
	}
	t, err := p.Store.CreateTask(ctx, lp.Repo, lp.Issue, is.Title, is.Body)
	if errors.Is(err, store.ErrActiveTaskExists) {
		return 0, "issue already has an active task", nil
	}
	if err != nil {
		return 0, "", err
	}
	p.Log.Info("task created", "task", t.ID, "repo", lp.Repo, "issue", lp.Issue)
	return t.ID, "", nil
}

func hasLabel(is github.Issue, name string) bool {
	for _, l := range is.Labels {
		if l.Name == name {
			return true
		}
	}
	return false
}
