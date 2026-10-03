package runner

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/deniz-ai-orchestration/ai-orchestrator/internal/github"
	"github.com/deniz-ai-orchestration/ai-orchestrator/internal/store"
)

// PRClient is the GitHub API that Open PR uses.
type PRClient interface {
	DefaultBranch(ctx context.Context, repo string) (string, error)
	FindPR(ctx context.Context, repo, branch string) (github.PullRequest, error)
	CreatePR(ctx context.Context, repo string, pr github.NewPR) (github.PullRequest, error)
	RequestReview(ctx context.Context, repo string, number int, logins []string) error
	AddAssignees(ctx context.Context, repo string, number int, logins []string) error
}

// PRDraft is the title and description Open PR starts from.
type PRDraft struct {
	Title string
	Body  string
}

// githubRepo matches a GitHub remote URL: https, ssh or scp-like.
var githubRepo = regexp.MustCompile(`^(?:https://(?:[^@/]+@)?github\.com/|ssh://git@github\.com/|git@github\.com:)([A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+?)(?:\.git)?/?$`)

// RepoOf returns the owner/name of a project's GitHub origin.
func (c *Chats) RepoOf(ctx context.Context, project string) (string, error) {
	repo, err := c.Projects.Path(project)
	if err != nil {
		return "", err
	}
	url, err := repoGit(ctx, repo, "remote", "get-url", "origin")
	if err != nil {
		return "", errors.New("the project has no origin remote")
	}
	m := githubRepo.FindStringSubmatch(url)
	if m == nil {
		return "", fmt.Errorf("origin %q is not a GitHub repository", url)
	}
	return m[1], nil
}

// canPublish reports why an agent's branch cannot be pushed now, or nil.
func canPublish(a store.Agent) error {
	if a.Role != "developer" || a.ParentID != 0 {
		return errors.New("only a developer agent's branch becomes a pull request")
	}
	switch a.State {
	case store.AgentWorking:
		return ErrBusy
	case store.AgentTesting:
		return errors.New("the testers are still running")
	case store.AgentClosed:
		return errors.New("the agent is closed")
	}
	return nil
}

// Draft proposes a PR title and description from the agent's conversation:
// your first message, its last report and the last test round.
func (c *Chats) Draft(ctx context.Context, id int64) (PRDraft, error) {
	a, err := c.Store.GetAgent(ctx, id)
	if err != nil {
		return PRDraft{}, err
	}
	msgs, err := c.Store.Messages(ctx, id)
	if err != nil {
		return PRDraft{}, err
	}
	var task, report string
	for _, m := range msgs {
		switch {
		case m.Author == store.FromYou && task == "":
			task = strings.TrimSpace(m.Body)
		case m.Author == store.FromAgent:
			report = strings.TrimSpace(m.Body)
		}
	}
	d := PRDraft{Title: prTitle(task)}
	var b strings.Builder
	if task != "" {
		fmt.Fprintf(&b, "## Task\n\n%s\n\n", quote(task))
	}
	if report != "" {
		fmt.Fprintf(&b, "## What the agent did\n\n%s\n\n", report)
	}
	if a.Round > 0 {
		ts, err := c.Store.Testers(ctx, id)
		if err != nil {
			return d, err
		}
		var lines []string
		for _, t := range ts {
			if t.Round != a.Round {
				continue
			}
			if v, ok, err := c.lastVerdict(ctx, t.ID); err != nil {
				return d, err
			} else if ok {
				lines = append(lines, fmt.Sprintf("- %s (%s): %s. %s", strings.ReplaceAll(t.Role, "_", " "), t.Model,
					verdictWords[v.Verdict], strings.TrimSpace(v.Summary)))
			}
		}
		if len(lines) > 0 {
			fmt.Fprintf(&b, "## Testers (round %d)\n\n%s\n\n", a.Round, strings.Join(lines, "\n"))
		}
	}
	fmt.Fprintf(&b, "---\nOpened by orch from agent %d (%s, %s) on branch `%s`.\n", a.ID, strings.ReplaceAll(a.Role, "_", " "), a.Model, a.Branch)
	d.Body = b.String()
	return d, nil
}

func prTitle(task string) string {
	line, _, _ := strings.Cut(task, "\n")
	line = strings.TrimSpace(strings.TrimRight(strings.TrimSpace(line), "."))
	if line == "" {
		return "Changes from orch"
	}
	r := []rune(line)
	if len(r) > 72 {
		line = strings.TrimSpace(string(r[:71])) + "…"
	}
	return line
}

func quote(s string) string { return "> " + strings.ReplaceAll(s, "\n", "\n> ") }

// OpenPR pushes a developer agent's branch to GitHub as deniz-agent and
// opens a pull request for it, with you as reviewer and assignee. When the
// branch already has an open PR, it only pushes. It never force-pushes.
// It returns the PR's number and URL, and whether it was created now.
func (c *Chats) OpenPR(ctx context.Context, id int64, d PRDraft) (int, string, bool, error) {
	c.pubMu.Lock()
	defer c.pubMu.Unlock()
	a, err := c.Store.GetAgent(ctx, id)
	if err != nil {
		return 0, "", false, err
	}
	if err := canPublish(a); err != nil {
		return 0, "", false, err
	}
	if c.Publisher == nil {
		return 0, "", false, errors.New("Open PR is not set up in this orch")
	}
	repo, err := c.RepoOf(ctx, a.Project)
	if err != nil {
		return 0, "", false, err
	}
	head, err := c.Projects.BranchHead(ctx, a.Project, a.Branch)
	if err != nil {
		return 0, "", false, err
	}
	if head == a.Base {
		return 0, "", false, errors.New("no commits on the branch yet")
	}
	cmp, err := c.localDiff(ctx, a, head)
	if err != nil {
		return 0, "", false, err
	}
	if vs := github.CheckChanges(cmp, github.PushRules{ForbiddenPaths: c.Cfg.GitHub.ForbiddenPaths,
		MaxDiffLines: c.Cfg.Limits.MaxDiffLines}); len(vs) > 0 {
		var why []string
		for _, v := range vs {
			why = append(why, v.Rule+": "+v.Detail)
		}
		return 0, "", false, fmt.Errorf("orch does not push this branch (%s); push it yourself if the change is intended", strings.Join(why, "; "))
	}
	gh, token, err := c.Publisher()
	if err != nil {
		return 0, "", false, err
	}
	if err := c.push(ctx, a, repo, head, token); err != nil {
		return 0, "", false, err
	}

	pr, err := gh.FindPR(ctx, repo, a.Branch)
	created := false
	switch {
	case errors.Is(err, github.ErrNoPR):
		if strings.TrimSpace(d.Title) == "" || strings.TrimSpace(d.Body) == "" {
			def, derr := c.Draft(ctx, id)
			if derr != nil {
				return 0, "", false, derr
			}
			if strings.TrimSpace(d.Title) == "" {
				d.Title = def.Title
			}
			if strings.TrimSpace(d.Body) == "" {
				d.Body = def.Body
			}
		}
		base, berr := gh.DefaultBranch(ctx, repo)
		if berr != nil {
			return 0, "", false, berr
		}
		if pr, err = gh.CreatePR(ctx, repo, github.NewPR{Title: strings.TrimSpace(d.Title), Head: a.Branch, Base: base, Body: d.Body}); err != nil {
			return 0, "", false, fmt.Errorf("pushed %s, but could not open the PR: %w", a.Branch, err)
		}
		created = true
	case err != nil:
		return 0, "", false, err
	}
	if pr.HTMLURL == "" {
		pr.HTMLURL = "https://github.com/" + repo + "/pull/" + strconv.Itoa(pr.Number)
	}
	note := fmt.Sprintf("Pushed commit %s to PR #%d: %s", short(head), pr.Number, pr.HTMLURL)
	if created {
		note = fmt.Sprintf("Opened PR #%d for commit %s: %s", pr.Number, short(head), pr.HTMLURL)
		if who := c.Cfg.GitHub.TrustedActor; who != "" {
			if err := gh.RequestReview(ctx, repo, pr.Number, []string{who}); err != nil {
				note += "\nCould not request your review: " + err.Error()
			}
			if err := gh.AddAssignees(ctx, repo, pr.Number, []string{who}); err != nil {
				note += "\nCould not assign you: " + err.Error()
			}
		}
	}
	if err := c.Store.SetAgentPR(ctx, id, pr.Number, pr.HTMLURL); err != nil {
		return 0, "", false, err
	}
	if _, err := c.Store.AddMessage(ctx, store.Message{AgentID: id, Author: store.FromOrch, Body: note}); err != nil {
		return 0, "", false, err
	}
	c.log().Info("agent branch pushed", "agent", id, "repo", repo, "branch", a.Branch, "pr", pr.Number, "created", created)
	return pr.Number, pr.HTMLURL, created, nil
}

// localDiff summarizes the agent's commits the way GitHub's compare does.
func (c *Chats) localDiff(ctx context.Context, a store.Agent, head string) (github.Comparison, error) {
	out, err := c.Projects.Git(ctx, a.Project, "diff", "--no-ext-diff", "--no-textconv", "--numstat", "-z", "-M", a.Base, head)
	if err != nil {
		return github.Comparison{}, err
	}
	return parseNumstat(out), nil
}

// parseNumstat reads `git diff --numstat -z`: "add\tdel\tpath\0", or for a
// rename "add\tdel\t\0old\0new\0". Binary files count as 0 lines.
func parseNumstat(out string) github.Comparison {
	var cmp github.Comparison
	fields := strings.Split(out, "\x00")
	for i := 0; i < len(fields); i++ {
		parts := strings.SplitN(fields[i], "\t", 3)
		if len(parts) != 3 {
			continue
		}
		f := github.ChangedFile{Filename: parts[2]}
		f.Additions, _ = strconv.Atoi(parts[0])
		f.Deletions, _ = strconv.Atoi(parts[1])
		if parts[2] == "" && i+2 < len(fields) {
			f.PreviousFilename, f.Filename = fields[i+1], fields[i+2]
			i += 2
		}
		cmp.Files = append(cmp.Files, f)
	}
	cmp.AheadBy = len(cmp.Files)
	return cmp
}

// push copies the agent's commit into a fresh repository that orch made,
// then pushes that to GitHub. The token is only ever used by git in that
// clean repository, never in one an agent could have configured.
func (c *Chats) push(ctx context.Context, a store.Agent, repo, head, token string) error {
	if err := os.MkdirAll(c.Dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.MkdirTemp(c.Dir, ".push-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	if _, err := gitEnv(ctx, nil, "init", "--quiet", "--bare", tmp); err != nil {
		return err
	}
	ref := "refs/heads/" + a.Branch
	if _, err := c.Projects.Git(ctx, a.Project, "push", "--quiet", "--no-verify", tmp, head+":"+ref); err != nil {
		return err
	}
	askpass := filepath.Join(tmp, "askpass")
	if err := os.WriteFile(askpass, []byte("#!/bin/sh\nprintf '%s\\n' \"$ORCH_GIT_TOKEN\"\n"), 0o700); err != nil {
		return err
	}
	url := "https://x-access-token@github.com/" + repo + ".git"
	if c.PushURL != nil {
		url = c.PushURL(repo)
	}
	env := []string{"GIT_DIR=" + tmp, "GIT_CONFIG_GLOBAL=/dev/null", "GIT_ASKPASS=" + askpass, "ORCH_GIT_TOKEN=" + token}
	if _, err := gitEnv(ctx, env, "-c", "credential.helper=", "-c", "core.hooksPath=/dev/null", "push", "--quiet", url, ref+":"+ref); err != nil {
		return fmt.Errorf("push %s to %s: %w", a.Branch, repo, err)
	}
	return nil
}
