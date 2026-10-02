# orch

`orch` is a small Go service that runs on PC2 and drives existing coding-agent
CLIs (Claude Code, Codex, Antigravity `agy`, OpenCode) from a labelled GitHub
issue to a CI-checked, AI-reviewed pull request that a human approves and merges.

This is the Phase 0 skeleton: config loading and the command line. The
components live under `internal/`, one package each, filled in milestone by
milestone (see each package's `doc.go`).

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

## Development

```sh
gofmt -l .
go vet ./...
go test -race ./...
```

CI (`.github/workflows/ci.yml`) runs the same checks plus a build and a
`check-config` on the example config. Its job is named `go`.
