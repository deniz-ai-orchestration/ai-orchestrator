package runner

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"text/template"
	"time"

	"github.com/deniz-ai-orchestration/ai-orchestrator/internal/config"
	"github.com/deniz-ai-orchestration/ai-orchestrator/internal/engine"
	"github.com/deniz-ai-orchestration/ai-orchestrator/internal/github"
	"github.com/deniz-ai-orchestration/ai-orchestrator/internal/provider"
	"github.com/deniz-ai-orchestration/ai-orchestrator/internal/router"
	"github.com/deniz-ai-orchestration/ai-orchestrator/internal/store"
)

//go:embed prompts/developer.md
var developerPrompt string

var devTmpl = template.Must(template.New("developer").Parse(developerPrompt))

// ResultSchema is the structured answer every developer run must end with.
// "blocked" carries the agent's question in summary.
const ResultSchema = `{"type":"object","properties":{"status":{"type":"string","enum":["done","blocked"]},"summary":{"type":"string"}},"required":["status","summary"],"additionalProperties":false}`

// DefaultDevTimeout applies when the developer role sets no timeout.
const DefaultDevTimeout = 45 * time.Minute

// roleTokens names the GitHub token file each role's container gets.
var roleTokens = map[string]string{
	"developer": "github_developer_token",
	"reviewer":  "github_reviewer_token",
}

// volumeTargets maps the credential volumes adapters ask for to where they
// are mounted. A provider whose adapter needs an unknown volume (agy's
// keyring) cannot run in a container yet.
var volumeTargets = map[string]string{
	"orch-codex-home": HomeDir + "/.codex",
}

// DevGitHub is what the developer flow needs from GitHub, called with
// orch's own token.
type DevGitHub interface {
	FindPR(ctx context.Context, repo, branch string) (github.PullRequest, error)
	Compare(ctx context.Context, repo, base, head string) (github.Comparison, error)
	EnsureComment(ctx context.Context, repo string, number int, marker, body, self string) (bool, error)
}

// Dev starts developer runs for queued tasks and turns their results into
// engine events. Runs are serial in M3: one developer run at a time.
type Dev struct {
	Store      *store.Store
	GH         DevGitHub
	Cfg        *config.Config
	Containers Containers
	WS         Workspaces
	RunsDir    string // <data_dir>/runs; one directory per run id
	User       string // container uid:gid
	Interval   time.Duration
	Log        *slog.Logger
}

// Recover marks runs cut off by a restart as interrupted and removes their
// containers. Call it once before Run.
func (d *Dev) Recover(ctx context.Context) error {
	n, err := d.Store.InterruptRunning(ctx)
	if err != nil {
		return err
	}
	if n > 0 {
		d.Log.Warn("marked interrupted runs", "count", n)
	}
	return d.Containers.RemoveStale(ctx)
}

// Run schedules and executes developer runs until ctx is done.
func (d *Dev) Run(ctx context.Context) error {
	for {
		if err := d.Once(ctx); err != nil && ctx.Err() == nil {
			d.Log.Error("developer round failed", "err", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(d.Interval):
		}
	}
}

// Once removes finished tasks' workspaces, starts the next queued task if no
// developer run is pending, and executes the pending one.
func (d *Dev) Once(ctx context.Context) error {
	if err := d.cleanup(ctx); err != nil {
		return err
	}
	items, err := d.Store.PendingEffectsOf(ctx, 1, engine.EffStartDevRun)
	if err != nil {
		return err
	}
	if len(items) == 0 {
		if err := d.schedule(ctx); err != nil {
			return err
		}
		if items, err = d.Store.PendingEffectsOf(ctx, 1, engine.EffStartDevRun); err != nil {
			return err
		}
	}
	for _, it := range items {
		if err := d.execute(ctx, it); err != nil {
			return err
		}
	}
	return nil
}

// ContainerReady reports whether a provider can run in an agent container.
func ContainerReady(p config.Provider) bool {
	a, err := provider.For(p)
	if err != nil {
		return false
	}
	c, err := a.Build(provider.Request{Model: "m", Prompt: "p"})
	if err != nil {
		return false
	}
	for _, v := range c.Mounts {
		if _, ok := volumeTargets[v]; !ok {
			return false
		}
	}
	return true
}

// schedule picks a model for the oldest queued task and starts it.
func (d *Dev) schedule(ctx context.Context) error {
	tasks, err := d.Store.ListTasks(ctx, engine.Queued)
	if err != nil || len(tasks) == 0 {
		return err
	}
	t := tasks[0]
	choice, err := router.Pick(d.Cfg, "developer", "", ContainerReady)
	if err != nil {
		d.Log.Warn("no developer model; task stays queued", "task", t.ID, "err", err)
		return nil
	}
	_, err = d.Store.ApplyEvent(ctx, t.ID, engine.Event{Kind: engine.EvDevStarted, Model: choice.Name}, d.Cfg.Limits)
	return err
}

func (d *Dev) cleanup(ctx context.Context) error {
	items, err := d.Store.PendingEffectsOf(ctx, 20, engine.EffCleanup)
	if err != nil {
		return err
	}
	for _, it := range items {
		if err := d.WS.Remove(it.TaskID); err != nil {
			if ferr := d.Store.FailEffect(ctx, it.ID, err); ferr != nil {
				return ferr
			}
			continue
		}
		if err := d.Store.CompleteEffect(ctx, it.ID); err != nil {
			return err
		}
	}
	return nil
}

// execute runs one start_dev_run effect to its engine event. A run cut off
// by a restart is retried once; a second interruption holds the task.
func (d *Dev) execute(ctx context.Context, it store.OutboxItem) error {
	t, err := d.Store.GetTask(ctx, it.TaskID)
	if err != nil {
		return err
	}
	if t.State != engine.Developing {
		d.Log.Info("task left developing before its run started", "task", t.ID, "state", t.State)
		return d.Store.CompleteEffect(ctx, it.ID)
	}
	prior, err := d.Store.RunsForOutbox(ctx, it.ID)
	if err != nil {
		return err
	}
	var ev engine.Event
	if len(prior) >= 2 {
		ev = failed(engine.ReasonCLIError, "developer run was interrupted twice by an orch restart")
	} else {
		ev, err = d.develop(ctx, t, it.ID)
		if err != nil {
			if ctx.Err() != nil {
				return err // shutting down: the effect stays pending and the run reads as interrupted
			}
			ev = failed(engine.ReasonCLIError, err.Error())
		}
	}
	if _, err := d.Store.ApplyEvent(ctx, t.ID, ev, d.Cfg.Limits); err != nil {
		var inv *engine.ErrInvalid
		if !errors.As(err, &inv) {
			return err
		}
		d.Log.Warn("run result no longer applies", "task", t.ID, "event", ev.Kind, "err", err)
	}
	return d.Store.CompleteEffect(ctx, it.ID)
}

func failed(r engine.Reason, detail string) engine.Event {
	return engine.Event{Kind: engine.EvRunFailed, Reason: r, Detail: detail}
}

// develop performs one developer run and returns the event it ends in. An
// error means orch could not run the agent at all.
func (d *Dev) develop(ctx context.Context, t store.Task, outboxID int64) (engine.Event, error) {
	m, ok := d.Cfg.Models[t.DevModel]
	if !ok {
		return engine.Event{}, fmt.Errorf("model %q is not in the config", t.DevModel)
	}
	p := d.Cfg.Providers[m.Provider]
	adapter, err := provider.For(p)
	if err != nil {
		return engine.Event{}, err
	}
	branch := t.Branch
	if branch == "" {
		branch = github.BranchName(t.IssueNumber, Slug(t.Title))
	}

	runID, err := d.Store.StartRun(ctx, store.Run{TaskID: t.ID, OutboxID: outboxID, Role: "developer",
		Work: string(t.Work), Provider: m.Provider, Model: t.DevModel})
	if err != nil {
		return engine.Event{}, err
	}
	ev, status, outcome, res, out, err := d.runDev(ctx, t, runID, adapter, m, branch)
	if err != nil {
		if ctx.Err() != nil {
			return engine.Event{}, err // left running; Recover marks it interrupted
		}
		status, outcome = store.RunFailed, string(engine.ReasonCLIError)
	}
	if ferr := d.Store.FinishRun(context.WithoutCancel(ctx), runID, status, outcome, res.ExitCode, out.InputTokens, out.OutputTokens); ferr != nil {
		d.Log.Error("finish run", "run", runID, "err", ferr)
	}
	return ev, err
}

func (d *Dev) runDev(ctx context.Context, t store.Task, runID int64, adapter provider.Adapter, m config.Model, branch string) (
	ev engine.Event, status, outcome string, res Result, out provider.Outcome, err error) {
	runDir := filepath.Join(d.RunsDir, fmt.Sprint(runID))
	ioDir := filepath.Join(runDir, "io")
	if err = os.MkdirAll(ioDir, 0o700); err != nil {
		return
	}
	if err = d.Store.SetRunLogDir(ctx, runID, runDir); err != nil {
		return
	}
	token, err := readSecret(d.Cfg.SecretsDir, roleTokens["developer"])
	if err != nil {
		return
	}
	ws, base, err := d.WS.Prepare(ctx, t.ID, "dev", t.Repo, branch, token)
	if err != nil {
		return
	}
	var extra string
	switch t.Work {
	case engine.WorkCIFix:
		extra, err = d.Store.LastDetail(ctx, t.ID, engine.EvCIFailed)
	case engine.WorkReviewFix:
		extra, err = d.Store.LastDetail(ctx, t.ID, engine.EvChangesRequested)
	}
	if err != nil {
		return
	}
	prompt, err := devPrompt(d.Cfg, t, branch, base, extra)
	if err != nil {
		return
	}
	if err = os.WriteFile(filepath.Join(ioDir, "schema.json"), []byte(ResultSchema), 0o600); err != nil {
		return
	}
	if err = os.WriteFile(filepath.Join(runDir, "prompt.md"), []byte(prompt), 0o600); err != nil {
		return
	}
	req := provider.Request{Model: m.Model, Prompt: prompt, Schema: ResultSchema,
		SchemaPath: IODir + "/schema.json", ResultPath: IODir + "/result.json"}
	cmd, err := adapter.Build(req)
	if err != nil {
		return
	}
	spec, err := d.spec(runID, cmd, ws, ioDir, token)
	if err != nil {
		return
	}
	if err = d.Store.SetRunContainer(ctx, runID, spec.Name); err != nil {
		return
	}

	stdoutPath, stderrPath := filepath.Join(runDir, "stdout.jsonl"), filepath.Join(runDir, "stderr.log")
	stdout, err := os.Create(stdoutPath)
	if err != nil {
		return
	}
	defer stdout.Close()
	stderr, err := os.Create(stderrPath)
	if err != nil {
		return
	}
	defer stderr.Close()

	timeout := d.Cfg.Roles["developer"].Timeout
	if timeout <= 0 {
		timeout = DefaultDevTimeout
	}
	rctx, cancel := context.WithTimeout(ctx, timeout)
	res, err = d.Containers.Run(rctx, spec, stdout, stderr)
	cancel()
	if err != nil {
		return
	}
	if ctx.Err() != nil { // orch is stopping, not the role timeout
		err = ctx.Err()
		return
	}
	d.Log.Info("developer run finished", "task", t.ID, "run", runID, "exit", res.ExitCode, "killed", res.Killed)

	run := provider.RunOutput{ExitCode: res.ExitCode, Killed: res.Killed}
	if run.Stdout, err = os.ReadFile(stdoutPath); err != nil {
		return
	}
	if run.Stderr, err = os.ReadFile(stderrPath); err != nil {
		return
	}
	for kind, path := range map[string]string{"prompt": filepath.Join(runDir, "prompt.md"), "stdout": stdoutPath, "stderr": stderrPath} {
		if fi, serr := os.Stat(path); serr == nil {
			_ = d.Store.AddArtifact(ctx, runID, kind, path, fi.Size())
		}
	}

	out = adapter.Parse(req, run)
	switch out.Kind {
	case provider.OK:
		ev, err = d.verify(ctx, t, branch)
		status, outcome = store.RunSucceeded, "ok"
	case provider.Blocked:
		ev = engine.Event{Kind: engine.EvDevBlocked, Detail: out.Detail}
		status, outcome = store.RunSucceeded, "blocked"
		d.askOnIssue(ctx, t, runID, out.Detail)
	default:
		ev = failed(out.Reason(), string(out.Kind)+": "+out.Detail)
		status, outcome = store.RunFailed, string(out.Reason())
	}
	return
}

// spec assembles the container for a developer run. Secret values travel
// only in Spec.Secrets.
func (d *Dev) spec(runID int64, cmd provider.Command, ws, ioDir, ghToken string) (Spec, error) {
	s := Spec{
		Name:  fmt.Sprintf("%s%d", Prefix, runID),
		Image: d.Cfg.Runner.Image, User: d.User,
		Work: ws, IO: ioDir,
		CPUs: d.Cfg.Runner.CPUs, Memory: d.Cfg.Runner.Memory, Pids: d.Cfg.Runner.Pids,
		Argv: cmd.Argv, Stdin: cmd.Stdin,
		Volumes: map[string]string{},
		Env: map[string]string{
			"HOME":                HomeDir,
			"GIT_TERMINAL_PROMPT": "0",
			"GIT_AUTHOR_NAME":     d.Cfg.Runner.GitName,
			"GIT_AUTHOR_EMAIL":    d.Cfg.Runner.GitEmail,
			"GIT_COMMITTER_NAME":  d.Cfg.Runner.GitName,
			"GIT_COMMITTER_EMAIL": d.Cfg.Runner.GitEmail,
			// git push authenticates with GH_TOKEN through this helper; gh
			// reads GH_TOKEN itself.
			"GIT_CONFIG_COUNT":   "2",
			"GIT_CONFIG_KEY_0":   "credential.https://github.com.helper",
			"GIT_CONFIG_VALUE_0": `!f() { test "$1" = get && echo username=x-access-token && echo "password=$GH_TOKEN"; }; f`,
			"GIT_CONFIG_KEY_1":   "safe.directory",
			"GIT_CONFIG_VALUE_1": "*",
		},
		Secrets: map[string]string{"GH_TOKEN": ghToken},
	}
	for k, v := range cmd.Env {
		s.Env[k] = v
	}
	for env, file := range cmd.SecretEnv {
		v, err := readSecret(d.Cfg.SecretsDir, file)
		if err != nil {
			return Spec{}, err
		}
		s.Secrets[env] = v
	}
	for _, v := range cmd.Mounts {
		target, ok := volumeTargets[v]
		if !ok {
			return Spec{}, fmt.Errorf("volume %q cannot be mounted into a container yet", v)
		}
		s.Volumes[v] = target
	}
	return s, nil
}

// verify checks what the agent pushed, using GitHub as the truth rather
// than the agent's own report.
func (d *Dev) verify(ctx context.Context, t store.Task, branch string) (engine.Event, error) {
	pr, err := d.GH.FindPR(ctx, t.Repo, branch)
	if errors.Is(err, github.ErrNoPR) {
		return failed(engine.ReasonBadOutput, "run reported done but no open PR exists for "+branch), nil
	}
	if err != nil {
		return engine.Event{}, fmt.Errorf("find PR: %w", err)
	}
	cmp, err := d.GH.Compare(ctx, t.Repo, pr.Base.Ref, pr.Head.SHA)
	if err != nil {
		return engine.Event{}, fmt.Errorf("compare: %w", err)
	}
	vs := github.ValidatePush(t.IssueNumber, branch, cmp, github.PushRules{
		ForbiddenPaths: d.Cfg.GitHub.ForbiddenPaths, MaxDiffLines: d.Cfg.Limits.MaxDiffLines})
	if !github.LinksIssue(pr.Body, t.IssueNumber) {
		vs = append(vs, github.Violation{Rule: "issue_link", Detail: fmt.Sprintf("PR #%d does not say Closes #%d", pr.Number, t.IssueNumber)})
	}
	if len(vs) > 0 {
		var b strings.Builder
		for i, v := range vs {
			if i > 0 {
				b.WriteString("; ")
			}
			b.WriteString(v.Rule + ": " + v.Detail)
		}
		return engine.Event{Kind: engine.EvPushRejected, Detail: b.String()}, nil
	}
	return engine.Event{Kind: engine.EvDevDone, Branch: branch, PRNumber: pr.Number, HeadSHA: pr.Head.SHA}, nil
}

// askOnIssue posts the agent's question on the issue once. Failing to post
// does not change the outcome: the question is also on the transition.
func (d *Dev) askOnIssue(ctx context.Context, t store.Task, runID int64, question string) {
	body := "The developer agent needs an answer before it can continue:\n\n> " +
		strings.ReplaceAll(strings.TrimSpace(question), "\n", "\n> ") +
		"\n\nEdit the issue to answer, then send /retry."
	marker := github.Marker(t.ID, fmt.Sprintf("question-%d", runID), "")
	if _, err := d.GH.EnsureComment(ctx, t.Repo, t.IssueNumber, marker, body, ""); err != nil {
		d.Log.Warn("could not post the question on the issue", "task", t.ID, "err", err)
	}
}

type promptData struct {
	Repo, Title, Body, Branch, Base, Human, Forbidden, Work string
	// Context is the CI failure (ci_fix) or the review findings (review_fix).
	Context             string
	Issue, PR, MaxLines int
}

func devPrompt(cfg *config.Config, t store.Task, branch, base, extra string) (string, error) {
	var b bytes.Buffer
	err := devTmpl.Execute(&b, promptData{
		Repo: t.Repo, Issue: t.IssueNumber, Title: t.Title, Body: t.Body,
		Branch: branch, Base: base, PR: t.PRNumber, Work: string(t.Work), Context: extra,
		Human: cfg.GitHub.TrustedActor, MaxLines: cfg.Limits.MaxDiffLines,
		Forbidden: strings.Join(cfg.GitHub.ForbiddenPaths, ", "),
	})
	return b.String(), err
}

var nonSlug = regexp.MustCompile(`[^a-z0-9]+`)

// Slug turns an issue title into a branch suffix: lowercase words joined by
// dashes, at most 40 characters.
func Slug(title string) string {
	s := strings.Trim(nonSlug.ReplaceAllString(strings.ToLower(title), "-"), "-")
	if len(s) > 40 {
		s = strings.TrimRight(s[:40], "-")
	}
	if s == "" {
		return "task"
	}
	return s
}

// readSecret reads one credential file from the secrets directory.
func readSecret(dir, name string) (string, error) {
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		return "", fmt.Errorf("secret %s: %w", name, err)
	}
	v := strings.TrimSpace(string(b))
	if v == "" {
		return "", fmt.Errorf("secret %s is empty", name)
	}
	return v, nil
}
