# orch

`orch` is a small Go service that runs on PC2 and drives existing coding-agent
CLIs (Claude Code, Codex, Antigravity `agy`, OpenCode) from a labelled GitHub
issue to a CI-checked, AI-reviewed pull request that a human approves and merges.

The components live under `internal/`, one package each, filled in milestone
by milestone. Today `orch run` watches GitHub for the trigger label, mirrors
task state as `orch:*` labels, runs the developer agent in a container until
it has opened a PR, and follows the PR: on red CI it fetches the failed job
log, has Ollama summarize it, and starts a fix run. On green CI a reviewer
agent (never the developer's model) reviews the head commit read-only; orch
posts its verdict on the PR and sends blocking findings back to the developer
(milestones M1 to M5).

Models are picked per role from the ranked pool in `config.yaml`: a pin
(`orch use`) wins while its model can run. A model that hits its quota cools
down until the reset time the CLI reports (else 1h, 2h, then 4h) and the task
goes back to the queue for the next model, without using up a developer run.
An auth failure switches off every model of that provider until
`orch enable`. `max_runs_per_5h` caps a model's runs per 5-hour window (M6).

## Telegram

With `telegram.enabled: true`, your numeric `user_id` and the bot token in
`secrets_dir/telegram_token`, orch messages you about each task: PR opened,
CI failed, changes requested (silently), and ready for review or needs you
(with sound, and Retry / Cancel buttons). It answers only your user id in a
private chat: `/status`, `/why`, `/retry`, `/cancel` (also stops a running
agent), `/quota`, `/models`, `/use`, `/enable`, `/disable`, `/pause`,
`/resume`. With Telegram off, notifications go to the log (M7).

## Commands

```sh
go build -o orch ./cmd/orch
./orch -config config.example.yaml check-config   # validate a config
./orch -config ~/.config/orch/config.yaml run     # start (Ctrl-C to stop)
./orch status                      # active tasks
./orch why 4                       # task 4's history
./orch retry 4 / cancel 4          # resume a held task / end a task
./orch pause / resume              # stop or restart starting new runs
./orch quota                       # each model: ready / cooling / off, runs in 5h, last limit
./orch use reviewer claude-opus    # pin a role to a model ("auto" unpins)
./orch disable codex-sol           # switch a model off; enable switches it back on
./orch version
```

The config path defaults to `$ORCH_CONFIG`, else `~/.config/orch/config.yaml`.
Start from [`config.example.yaml`](config.example.yaml). Secrets never go in the
config: each credential is a 0600 file in `secrets_dir`.

## Running agents on PC2

Each agent run is one throwaway container from the `orch-agent` image:

```sh
docker build -t orch-agent:latest docker/agent
```

- Run `orch` as your normal user (not root) in the `docker` group. Containers
  run with your uid, so they can write the workspace, and Claude Code refuses
  `bypassPermissions` as root.
- The developer run needs `github_developer_token`, the reviewer run
  `github_reviewer_token`, each with its provider's credential (for Claude,
  `claude_oauth_token`; Codex reads `auth.json` from the `orch-codex-home`
  Docker volume) in `secrets_dir`, and orch itself needs `github_orch_token`. Tokens reach the container as environment
  variables passed by name, never on a command line.
- Workspaces are in `<data_dir>/ws/task-<id>/dev`; each run keeps its prompt,
  stdout and stderr in `<data_dir>/runs/<run id>/`.
- Models whose CLI cannot run in a container yet (`agy`, whose login is in the
  keyring) are skipped when picking a model.

## Development

```sh
gofmt -l .
go vet ./...
go test -race ./...
```

CI (`.github/workflows/ci.yml`) runs the same checks plus a build and a
`check-config` on the example config. Its job is named `go`.
