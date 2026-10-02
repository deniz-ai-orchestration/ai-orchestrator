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
	"syscall"

	"github.com/deniz-ai-orchestration/ai-orchestrator/internal/config"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

const usage = `usage: orch [-config path] <command>

commands:
  run           start the orchestrator (blocks until SIGINT/SIGTERM)
  check-config  load and validate the config, then exit
  version       print the version
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
	if fs.NArg() != 1 {
		fs.Usage()
		return errors.New("expected exactly one command")
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
	default:
		fs.Usage()
		return fmt.Errorf("unknown command %q", cmd)
	}
}

// serve will wire the store, poller, engine, runner, Telegram and panel.
// For now it only starts logging and waits for a shutdown signal.
func serve(ctx context.Context, cfg *config.Config, logOut io.Writer) error {
	log := slog.New(slog.NewJSONHandler(logOut, nil))
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	log.Info("orch starting", "version", version, "data_dir", cfg.DataDir, "repos", cfg.GitHub.Repos)
	<-ctx.Done()
	log.Info("orch stopped")
	return nil
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
