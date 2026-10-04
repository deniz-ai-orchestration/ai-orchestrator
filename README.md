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

Models are picked per role from the ranked pool in `config.yaml`, or you
choose one (`orch use`, or the provider and model selectors in the panel).
A chosen model wins while it can run, even if it is the developer's model
for a reviewer; otherwise the pool's fallback order applies. A model that hits its quota cools
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

## Control panel

With `panel.enabled: true`, orch serves a control panel on `panel.listen`
(PC2's LAN address; open it by IP, not by a DNS name). The dashboard shows
your projects as cards with workflow and agent badges, the new-project form
(name, optional description, project directory), the pause switch and each
model's quota state. A project page shows its summary, workflows, roles with
each role's provider and model selectors, and the new-workflow form. A
workflow page shows its developer chain and tester rounds, its own runs,
the PR/CI card and its actions; an agent page is the chat with a live,
view-only terminal of what the agent says and which tools it calls. The panel
has no login: keep it on the LAN. Taking control of a terminal and
needs-you pings come later.

### Summoned agents

Set `projects.dir` (for example `~/projects`) to a folder holding one git
repository per project. The panel's Summon form takes a role, a provider and
model (preselected from the role's choice), a project folder, a branch
(empty makes a new `orch/<id>-<task words>` from the project's current commit)
and the task, with optional text files up to 1 MB each.

Each agent gets its own git worktree of the project in
`<data_dir>/agents/<id>/work`, so parallel agents never touch each other's
files or your own checkout. Its page is a chat: every message is one turn,
run in a throwaway container that resumes the agent's Claude session, so it
remembers earlier turns. The running turn streams in below the chat, and each
reply links to its full transcript. A turn ends as done or needs you. Stop
ends a turn and keeps the session; Close removes the worktree and session,
and the branch with the agent's commits stays in the project. Agents commit
but never push, and they get no GitHub token. Only Claude models can chat for
now; Codex and OpenCode follow.

When a developer agent ends a turn as done with new commits, orch summons
its testers: a functional tester and a reviewer (each if its role is
enabled), on the role's chosen model or the first in its fallback order that
can chat, else the developer's model. Each gets a read-only checkout of the
developer's latest commit, a brief with the task and the developer's report,
and one turn that ends in a verdict. Blocking findings (blocker or major) go
back into the developer's chat as a new turn, and the next done starts the
next round; a clean round marks the developer ready for a PR. A tester that
cannot finish or asks for a human decision leaves the developer waiting for
you. After `limits.review_cycles` rounds the developer waits for you too;
Send to testers on its page runs one more round, or a first one at any time.
Stop testers ends a round early with what the testers found.

Open PR on a developer agent's page pushes its branch to the project's
GitHub origin as deniz-agent (`github_developer_token`) and opens a pull
request with you (`github.trusted_actor`) as reviewer and assignee. The form
starts from your task, the agent's last report and the last test round.
`github.forbidden_paths` and `limits.max_diff_lines` apply; a branch that
breaks them is not pushed. Once the PR exists, Push to PR pushes new
commits. orch never force-pushes and never merges.

Agents commit into the project's `.git` folder, so orch treats it as
writable by them: its config, hooks and `commondir` are mounted read-only
into containers, orch's own git commands run on the project's `.git` (never
inside an agent's worktree) with hooks and fsmonitor off, and the push with
the token goes from a fresh repository orch creates for it. orch creates
`.git/commondir` (containing `.`) in each project the first time an agent
runs there.

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
./orch use reviewer claude-opus    # choose a role's model ("auto" = fallback order)
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
