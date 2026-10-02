package runner

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/deniz-ai-orchestration/ai-orchestrator/internal/config"
	"github.com/deniz-ai-orchestration/ai-orchestrator/internal/engine"
	"github.com/deniz-ai-orchestration/ai-orchestrator/internal/github"
	"github.com/deniz-ai-orchestration/ai-orchestrator/internal/provider"
	"github.com/deniz-ai-orchestration/ai-orchestrator/internal/quota"
	"github.com/deniz-ai-orchestration/ai-orchestrator/internal/router"
	"github.com/deniz-ai-orchestration/ai-orchestrator/internal/store"
)

// Role timeouts when the config sets none.
var defaultTimeouts = map[string]time.Duration{
	"developer": 45 * time.Minute,
	"reviewer":  20 * time.Minute,
}

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

// GitHub is what the runner reads and writes with orch's own token.
type GitHub interface {
	FindPR(ctx context.Context, repo, branch string) (github.PullRequest, error)
	Compare(ctx context.Context, repo, base, head string) (github.Comparison, error)
	Commenter
}

// Commenter posts idempotent comments.
type Commenter interface {
	EnsureComment(ctx context.Context, repo string, number int, marker, body, self string) (bool, error)
}

// Agents schedules developer and reviewer runs and turns their results into
// engine events. Runs are serial: one agent container at a time (parallel
// tasks are Phase 4).
type Agents struct {
	Store      *store.Store
	GH         GitHub
	Cfg        *config.Config
	Containers Containers
	WS         Workspaces
	RunsDir    string // <data_dir>/runs; one directory per run id
	User       string // container uid:gid
	Interval   time.Duration
	Log        *slog.Logger
	// ReviewGH posts the reviewer's verdict with the reviewer role's token.
	ReviewGH func() (Commenter, error)
	// Quota supplies pins, cooldowns and budgets, and learns from every run.
	// Nil means every configured model is always available.
	Quota *quota.Tracker
}

// Recover marks runs cut off by a restart as interrupted and removes their
// containers. Call it once before Run.
func (a *Agents) Recover(ctx context.Context) error {
	n, err := a.Store.InterruptRunning(ctx)
	if err != nil {
		return err
	}
	if n > 0 {
		a.Log.Warn("marked interrupted runs", "count", n)
	}
	return a.Containers.RemoveStale(ctx)
}

// Run schedules and executes agent runs until ctx is done.
func (a *Agents) Run(ctx context.Context) error {
	for {
		if err := a.Once(ctx); err != nil && ctx.Err() == nil {
			a.Log.Error("agent round failed", "err", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(a.Interval):
		}
	}
}

var runEffects = []engine.EffectKind{engine.EffStartDevRun, engine.EffStartReviewRun}

// Once removes finished tasks' workspaces, starts the next waiting task if
// no run is pending, and executes the pending run.
func (a *Agents) Once(ctx context.Context) error {
	if err := a.cleanup(ctx); err != nil {
		return err
	}
	items, err := a.Store.PendingEffectsOf(ctx, 1, runEffects...)
	if err != nil {
		return err
	}
	if len(items) == 0 {
		if err := a.schedule(ctx); err != nil {
			return err
		}
		if items, err = a.Store.PendingEffectsOf(ctx, 1, runEffects...); err != nil {
			return err
		}
	}
	for _, it := range items {
		if err := a.execute(ctx, it); err != nil {
			return err
		}
	}
	return nil
}

// ContainerReady reports whether a provider can run in an agent container.
func ContainerReady(p config.Provider) bool {
	ad, err := provider.For(p)
	if err != nil {
		return false
	}
	c, err := ad.Build(provider.Request{Model: "m", Prompt: "p"})
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

// schedule starts the oldest task waiting for a developer or a reviewer.
func (a *Agents) schedule(ctx context.Context) error {
	tasks, err := a.Store.ListTasks(ctx, engine.Queued, engine.ReviewQueued)
	if err != nil || len(tasks) == 0 {
		return err
	}
	var pins map[string]string
	var skip func(string) string
	if a.Quota != nil {
		if pins, err = a.Store.RolePins(ctx); err != nil {
			return err
		}
		if skip, err = a.Quota.Skip(ctx); err != nil {
			return err
		}
	}
	for _, t := range tasks {
		role, kind, avoid := "developer", engine.EvDevStarted, ""
		if t.State == engine.ReviewQueued {
			role, kind, avoid = "reviewer", engine.EvReviewStarted, t.DevModel
		}
		choice, err := router.Pick(a.Cfg, role, router.Options{Avoid: avoid, Usable: ContainerReady, Pin: pins[role], Skip: skip})
		if err != nil {
			a.Log.Warn("no model for role; task waits", "task", t.ID, "role", role, "err", err)
			continue
		}
		_, err = a.Store.ApplyEvent(ctx, t.ID, engine.Event{Kind: kind, Model: choice.Name}, a.Cfg.Limits)
		return err
	}
	return nil
}

func (a *Agents) cleanup(ctx context.Context) error {
	items, err := a.Store.PendingEffectsOf(ctx, 20, engine.EffCleanup)
	if err != nil {
		return err
	}
	for _, it := range items {
		if err := a.WS.Remove(it.TaskID); err != nil {
			if ferr := a.Store.FailEffect(ctx, it.ID, err); ferr != nil {
				return ferr
			}
			continue
		}
		if err := a.Store.CompleteEffect(ctx, it.ID); err != nil {
			return err
		}
	}
	return nil
}

// execute runs one run effect to its engine event. A run cut off by a
// restart is retried once; a second interruption holds the task.
func (a *Agents) execute(ctx context.Context, it store.OutboxItem) error {
	t, err := a.Store.GetTask(ctx, it.TaskID)
	if err != nil {
		return err
	}
	want, work := engine.Developing, a.develop
	if it.Effect.Kind == engine.EffStartReviewRun {
		want, work = engine.Reviewing, a.review
	}
	if t.State != want {
		a.Log.Info("task moved on before its run started", "task", t.ID, "state", t.State, "effect", it.Effect.Kind)
		return a.Store.CompleteEffect(ctx, it.ID)
	}
	prior, err := a.Store.RunsForOutbox(ctx, it.ID)
	if err != nil {
		return err
	}
	var ev engine.Event
	if len(prior) >= 2 {
		ev = failed(engine.ReasonCLIError, "run was interrupted twice by an orch restart")
	} else {
		ev, err = work(ctx, t, it.ID)
		if err != nil {
			if ctx.Err() != nil {
				return err // shutting down: the effect stays pending and the run reads as interrupted
			}
			ev = failed(engine.ReasonCLIError, err.Error())
		}
	}
	if _, err := a.Store.ApplyEvent(ctx, t.ID, ev, a.Cfg.Limits); err != nil {
		var inv *engine.ErrInvalid
		if !errors.As(err, &inv) {
			return err
		}
		a.Log.Warn("run result no longer applies", "task", t.ID, "event", ev.Kind, "err", err)
	}
	return a.Store.CompleteEffect(ctx, it.ID)
}

func failed(r engine.Reason, detail string) engine.Event {
	return engine.Event{Kind: engine.EvRunFailed, Reason: r, Detail: detail}
}

// outcomeEvent maps a failed run: quota and capacity limits send the task
// back to the queue for another model, everything else holds it.
func outcomeEvent(model string, out provider.Outcome) engine.Event {
	if out.Kind == provider.RateLimited || out.Kind == provider.Unavailable {
		return engine.Event{Kind: engine.EvRunDeferred, Detail: fmt.Sprintf("%s %s: %s", model, out.Kind, out.Detail)}
	}
	return failed(out.Reason(), string(out.Kind)+": "+out.Detail)
}

// job is one agent run.
type job struct {
	task     store.Task
	outboxID int64
	role     string
	model    string // config models.<name>
	schema   string
	readOnly bool
	// prepare makes the workspace and the prompt. It gets the role's token.
	prepare func(ctx context.Context, token string) (ws, prompt string, err error)
}

// runAgent records a run, starts its container and classifies the output.
// An error means the agent could not be run (or orch is stopping).
func (a *Agents) runAgent(ctx context.Context, j job) (provider.Outcome, error) {
	m, ok := a.Cfg.Models[j.model]
	if !ok {
		return provider.Outcome{}, fmt.Errorf("model %q is not in the config", j.model)
	}
	ad, err := provider.For(a.Cfg.Providers[m.Provider])
	if err != nil {
		return provider.Outcome{}, err
	}
	work := ""
	if j.role == "developer" {
		work = string(j.task.Work)
	}
	runID, err := a.Store.StartRun(ctx, store.Run{TaskID: j.task.ID, OutboxID: j.outboxID, Role: j.role,
		Work: work, Provider: m.Provider, Model: j.model})
	if err != nil {
		return provider.Outcome{}, err
	}
	out, res, err := a.container(ctx, j, runID, ad, m)
	if err != nil && ctx.Err() != nil {
		return out, err // left running; Recover marks it interrupted
	}
	if err == nil && a.Quota != nil {
		if qerr := a.Quota.Record(ctx, j.model, runID, out); qerr != nil {
			a.Log.Error("record quota signal", "run", runID, "err", qerr)
		}
	}
	status, outcome := store.RunFailed, string(out.Reason())
	switch {
	case err != nil:
		outcome = string(engine.ReasonCLIError)
	case out.Kind == provider.OK:
		status, outcome = store.RunSucceeded, "ok"
	case out.Kind == provider.Blocked:
		status, outcome = store.RunSucceeded, "blocked"
	}
	if ferr := a.Store.FinishRun(context.WithoutCancel(ctx), runID, status, outcome, res.ExitCode, out.InputTokens, out.OutputTokens); ferr != nil {
		a.Log.Error("finish run", "run", runID, "err", ferr)
	}
	a.Log.Info("agent run finished", "task", j.task.ID, "run", runID, "role", j.role, "model", j.model,
		"exit", res.ExitCode, "killed", res.Killed, "outcome", outcome)
	return out, err
}

func (a *Agents) container(ctx context.Context, j job, runID int64, ad provider.Adapter, m config.Model) (
	out provider.Outcome, res Result, err error) {
	runDir := filepath.Join(a.RunsDir, fmt.Sprint(runID))
	ioDir := filepath.Join(runDir, "io")
	if err = os.MkdirAll(ioDir, 0o700); err != nil {
		return
	}
	if err = a.Store.SetRunLogDir(ctx, runID, runDir); err != nil {
		return
	}
	token, err := readSecret(a.Cfg.SecretsDir, roleTokens[j.role])
	if err != nil {
		return
	}
	ws, prompt, err := j.prepare(ctx, token)
	if err != nil {
		return
	}
	if err = os.WriteFile(filepath.Join(ioDir, "schema.json"), []byte(j.schema), 0o600); err != nil {
		return
	}
	promptPath := filepath.Join(runDir, "prompt.md")
	if err = os.WriteFile(promptPath, []byte(prompt), 0o600); err != nil {
		return
	}
	req := provider.Request{Model: m.Model, Prompt: prompt, Schema: j.schema, ReadOnly: j.readOnly,
		SchemaPath: IODir + "/schema.json", ResultPath: IODir + "/result.json"}
	cmd, err := ad.Build(req)
	if err != nil {
		return
	}
	spec, err := a.spec(runID, cmd, ws, ioDir, token)
	if err != nil {
		return
	}
	if err = a.Store.SetRunContainer(ctx, runID, spec.Name); err != nil {
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

	timeout := a.Cfg.Roles[j.role].Timeout
	if timeout <= 0 {
		timeout = defaultTimeouts[j.role]
	}
	rctx, cancel := context.WithTimeout(ctx, timeout)
	res, err = a.Containers.Run(rctx, spec, stdout, stderr)
	cancel()
	if err != nil {
		return
	}
	if ctx.Err() != nil { // orch is stopping, not the role timeout
		err = ctx.Err()
		return
	}
	run := provider.RunOutput{ExitCode: res.ExitCode, Killed: res.Killed}
	if run.Stdout, err = os.ReadFile(stdoutPath); err != nil {
		return
	}
	if run.Stderr, err = os.ReadFile(stderrPath); err != nil {
		return
	}
	for kind, path := range map[string]string{"prompt": promptPath, "stdout": stdoutPath, "stderr": stderrPath} {
		if fi, serr := os.Stat(path); serr == nil {
			_ = a.Store.AddArtifact(ctx, runID, kind, path, fi.Size())
		}
	}
	out = ad.Parse(req, run)
	return
}

// spec assembles a run's container. Secret values travel only in
// Spec.Secrets.
func (a *Agents) spec(runID int64, cmd provider.Command, ws, ioDir, ghToken string) (Spec, error) {
	s := Spec{
		Name:  fmt.Sprintf("%s%d", Prefix, runID),
		Image: a.Cfg.Runner.Image, User: a.User,
		Work: ws, IO: ioDir,
		CPUs: a.Cfg.Runner.CPUs, Memory: a.Cfg.Runner.Memory, Pids: a.Cfg.Runner.Pids,
		Argv: cmd.Argv, Stdin: cmd.Stdin,
		Volumes: map[string]string{},
		Env: map[string]string{
			"HOME":                HomeDir,
			"GIT_TERMINAL_PROMPT": "0",
			"GIT_AUTHOR_NAME":     a.Cfg.Runner.GitName,
			"GIT_AUTHOR_EMAIL":    a.Cfg.Runner.GitEmail,
			"GIT_COMMITTER_NAME":  a.Cfg.Runner.GitName,
			"GIT_COMMITTER_EMAIL": a.Cfg.Runner.GitEmail,
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
		v, err := readSecret(a.Cfg.SecretsDir, file)
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
