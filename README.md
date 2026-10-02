# orch

`orch` is a small Go service that runs on PC2 and drives existing coding-agent
CLIs (Claude Code, Codex, Antigravity `agy`, OpenCode) from a labelled GitHub
issue to a CI-checked, AI-reviewed pull request that a human approves and merges.

The components live under `internal/`, one package each, filled in milestone
by milestone. Today `orch run` watches GitHub for the trigger label, mirrors
task state as `orch:*` labels, and runs the developer agent in a container
until it has opened a PR (milestones M1 to M3).

## Commands

```sh
go build -o orch ./cmd/orch
./orch -config config.example.yaml check-config   # validate a config
./orch -config ~/.config/orch/config.yaml run     # start (Ctrl-C to stop)
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
- The developer run needs `github_developer_token` and the provider's
  credential (for Claude, `claude_oauth_token`) in `secrets_dir`, and orch
  itself needs `github_orch_token`. Tokens reach the container as environment
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
