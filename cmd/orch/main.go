// Command orch drives coding-agent CLIs from GitHub issues to reviewed PRs.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/deniz-ai-orchestration/ai-orchestrator/internal/ci"
	"github.com/deniz-ai-orchestration/ai-orchestrator/internal/config"
	"github.com/deniz-ai-orchestration/ai-orchestrator/internal/effects"
	"github.com/deniz-ai-orchestration/ai-orchestrator/internal/github"
	"github.com/deniz-ai-orchestration/ai-orchestrator/internal/helper"
	"github.com/deniz-ai-orchestration/ai-orchestrator/internal/poller"
	"github.com/deniz-ai-orchestration/ai-orchestrator/internal/quota"
	"github.com/deniz-ai-orchestration/ai-orchestrator/internal/runner"
	"github.com/deniz-ai-orchestration/ai-orchestrator/internal/store"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

const usage = `usage: orch [-config path] <command> [args]

commands:
  run                    start the orchestrator (blocks until SIGINT/SIGTERM)
  check-config           load and validate the config, then exit
  quota                  show every model's state, runs in 5h and last limit
  use <role> <model>     pin a role to a model ("auto" removes the pin)
  enable <model>         switch a model back on and clear its cooldown
  disable <model>        switch a model off
  version                print the version
`

func main() {
	if err := run(context.Background(), os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "orch:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("orch", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprint(stderr, usage); fs.PrintDefaults() }
	cfgPath := fs.String("config", defaultConfigPath(), "path to config.yaml (env ORCH_CONFIG)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() < 1 {
		fs.Usage()
		return errors.New("expected a command")
	}
	args = fs.Args()[1:]
	nargs := map[string]int{"run": 0, "check-config": 0, "version": 0, "quota": 0, "use": 2, "enable": 1, "disable": 1}
	if n, ok := nargs[fs.Arg(0)]; ok && len(args) != n {
		fs.Usage()
		return fmt.Errorf("%s takes %d argument(s)", fs.Arg(0), n)
	}

	switch cmd := fs.Arg(0); cmd {
	case "version":
		fmt.Fprintln(stdout, version)
		return nil
	case "check-config":
		cfg, err := config.Load(*cfgPath)
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "config ok: %d providers, %d models, %d roles\n",
			len(cfg.Providers), len(cfg.Models), len(cfg.Roles))
		return nil
	case "run":
		cfg, err := config.Load(*cfgPath)
		if err != nil {
			return err
		}
		return serve(ctx, cfg, stderr)
	case "quota", "use", "enable", "disable":
		cfg, err := config.Load(*cfgPath)
		if err != nil {
			return err
		}
		return models(ctx, cfg, cmd, args, stdout)
	default:
		fs.Usage()
		return fmt.Errorf("unknown command %q", cmd)
	}
}

// serve opens the store and runs the GitHub poller, the label mirror, the
// agent runner (developer and reviewer) and the PR/CI follower until a
// shutdown signal. Later milestones add Telegram and the control panel.
func serve(ctx context.Context, cfg *config.Config, logOut io.Writer) error {
	log := slog.New(slog.NewJSONHandler(logOut, nil))
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	token, err := readSecret(cfg.SecretsDir, "github_orch_token")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return fmt.Errorf("data dir: %w", err)
	}
	// Opening and migrating must finish even if a signal is already pending.
	st, err := store.Open(context.WithoutCancel(ctx), filepath.Join(cfg.DataDir, "orch.db"))
	if err != nil {
		return err
	}
	defer st.Close()

	gh := github.New(token)
	p := &poller.Poller{Store: st, GH: gh, Cfg: cfg.GitHub, Log: log.With("component", "poller")}
	labels := &effects.Labels{Store: st, GH: gh, Log: log.With("component", "labels"), Interval: 5 * time.Second}
	agents := &runner.Agents{Store: st, GH: gh, Cfg: cfg, Containers: runner.Docker{},
		WS:       runner.Workspaces{Root: filepath.Join(cfg.DataDir, "ws")},
		RunsDir:  filepath.Join(cfg.DataDir, "runs"),
		User:     fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()),
		Interval: 10 * time.Second, Log: log.With("component", "agents"),
		Quota: &quota.Tracker{Store: st, Cfg: cfg},
		ReviewGH: func() (runner.Commenter, error) {
			t, err := readSecret(cfg.SecretsDir, "github_reviewer_token")
			if err != nil {
				return nil, err
			}
			return github.New(t), nil
		}}
	follower := &ci.Follower{Store: st, GH: gh, Helper: helper.FromConfig(cfg), Cfg: cfg,
		Interval: cfg.GitHub.PollInterval, Log: log.With("component", "ci")}
	if err := agents.Recover(context.WithoutCancel(ctx)); err != nil {
		log.Error("runner recovery failed; is Docker running?", "err", err)
	}

	log.Info("orch starting", "version", version, "data_dir", cfg.DataDir, "repos", cfg.GitHub.Repos)
	var wg sync.WaitGroup
	for _, run := range []func(context.Context) error{p.Run, labels.Run, agents.Run, follower.Run} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = run(ctx)
		}()
	}
	<-ctx.Done()
	wg.Wait()
	log.Info("orch stopped")
	return nil
}

// models runs the model-management commands against orch's database. They
// work while orch runs: SQLite serializes the writes.
func models(ctx context.Context, cfg *config.Config, cmd string, args []string, out io.Writer) error {
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return fmt.Errorf("data dir: %w", err)
	}
	st, err := store.Open(ctx, filepath.Join(cfg.DataDir, "orch.db"))
	if err != nil {
		return err
	}
	defer st.Close()
	tr := &quota.Tracker{Store: st, Cfg: cfg}
	switch cmd {
	case "use":
		if err := tr.Pin(ctx, args[0], args[1]); err != nil {
			return err
		}
		fmt.Fprintf(out, "%s: %s\n", args[0], args[1])
		return nil
	case "enable", "disable":
		if err := tr.SetEnabled(ctx, args[0], cmd == "enable"); err != nil {
			return err
		}
		fmt.Fprintf(out, "%s %sd\n", args[0], cmd)
		return nil
	}
	rep, err := tr.Report(ctx)
	if err != nil {
		return err
	}
	now := time.Now()
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "MODEL\tPROVIDER\tSTATE\tRUNS/5H\tPINNED\tLAST LIMIT")
	for _, s := range rep {
		state := s.State(now)
		if state == "cooling" {
			state += " until " + s.CoolingUntil.Local().Format("Jan 2 15:04")
		}
		runs := fmt.Sprint(s.RunsIn5h)
		if s.Budget > 0 {
			runs += fmt.Sprintf("/%d", s.Budget)
		}
		last := "-"
		if s.Last != nil {
			last = s.Last.Kind + " " + s.Last.At.Local().Format("Jan 2 15:04")
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", s.Name, s.Provider, state, runs, dash(strings.Join(s.PinnedFor, ",")), last)
	}
	return w.Flush()
}

func dash(s string) string {
	if s == "" {
		return "-"
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

func defaultConfigPath() string {
	if p := os.Getenv("ORCH_CONFIG"); p != "" {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "config.yaml"
	}
	return filepath.Join(home, ".config", "orch", "config.yaml")
}
