# Project / Workflow / Agent implementation plan

Date: 2026-10-04. Source: panel-chat discussion (PC1). Target machine: PC2 (Ubuntu 24.04).

## 1. Goal

Move from flat `Runs + Agents + Tasks` panel to hierarchical:

```
Dashboard (projects) -> Project (workflows) -> Workflow (agents + PR/CI) -> Agent (chat + terminal)
```

Definitions agreed in discussion:

- `Project` = one trusted absolute directory on PC2 (usually a git root, e.g. monorepo root).
  Autonomous workflow is **per project**, allowed only when `<dir>/.git` exists.
- `Workflow` = one job inside a project (e.g. "add model to backend" and "add feature
  to frontend" run in parallel on the same repo). First-class entity: owns branch,
  task, PR/CI state, tester rounds. Survives agent replacement.
- `Agent` = one ephemeral worker session inside a workflow (developer, tester,
  reviewer). Stopping/closing an agent never kills the workflow; user summons the
  next agent on the same branch/worktree to continue (context management).
- Deferred (not this job): summon memory options
  ("empty memory" / "project memory" / "from checkpoint"), planner role, auto
  compact/clear.

## 2. Current state (what exists today)

- Summon = `role + model + project + branch + message + files`
  (`internal/panel/chat.go:166`, `internal/runner/chat.go:169`).
  `project` is a dropdown from `projects.dir` (`~/projects`), filtered to folders
  with `.git/` (`internal/runner/worktree.go:30`). No free directory picker, no
  non-git support, no trust prompt.
- Each agent gets `data_dir/agents/<id>/work` worktree on its own branch, commits
  there, never pushes itself. Parallel turns already work (one goroutine per agent
  in `Chats.launch`); issue-based `Agents` loop is serial.
- `done` -> auto `startTests` (functional_tester + reviewer on detached checkout of
  `HEAD`), blocking findings loop back to dev, clean -> `ready` -> manual
  `Open PR / Push to PR` (`internal/runner/testers.go`, `internal/runner/pr.go`).
  No CI gate on chat path; `ci.Follower` only watches issue tasks.
- Testers hang off root dev via `parent_id + round`
  (`internal/store/migrations/00006_testers.sql`). Agents table in `00005_agents.sql`.
- Panel is one page: `summon + chats + agents + tasks + roles + quota`
  (`internal/panel/templates/index.html:18`, `internal/panel/view.go:23`,
  routes in `internal/panel/server.go:101`).

## 3. Target behavior

### 3.1 Directory + trust (PC2 paths)

- Free path input on PC2 (not limited to `projects.dir` dropdown).
- On select: resolve abs + clean, `stat <dir>/.git` -> `has_git`, read origin URL,
  count files -> show Claude-Code-style trust prompt
  ("trust `/abs/path`, N files, git yes/no?").
- Store in `projects` table: `{path PK, trusted, has_git, autonomous, github_repo}`.
  Untrusted path = no spawn, no worktree.
- Non-git dir: allowed as chat-only project, `autonomous` forced OFF, no `Open PR`.

### 3.2 Autonomous rules (per project, inherited by workflows)

- Never start with tester, always start with planner/developer
  (planner later; developer for now). Exception: tester-first allowed only when a PR
  is already open for that workflow/branch.
- Autonomous ON (git only): `dev done` -> auto testers (pre-PR) -> approve ->
  auto `OpenPR` (if origin is GitHub) -> poll CI -> green -> auto reviewer round ->
  blocking/major -> auto new dev agent in same workflow (round+1).
  Red CI -> helper (Ollama) summarize -> auto new dev agent.
  All loops bounded by existing `limits{ci_attempts, review_cycles, dev_runs}`.
- Autonomous OFF or non-git: manual `Send to testers` / `Open PR` buttons only.

### 3.3 Parallel workflows on one repo (monorepo)

- Rule: `1 workflow = 1 branch = N worktrees` (dev worktree + detached tester
  checkouts). `AddWorktree` already refuses a branch checked out twice — keep as guard.
- Example: monorepo root added once as project; workflow A (backend model) on
  `orch/<a>-...`, workflow B (frontend feature) on `orch/<b>-...`, different files,
  same repo, parallel turns.
- Quotas/roles stay global (shared OpenCode-Go-style pool across workflows).

### 3.4 Panel hierarchy

- `GET /`: sidebar `projects (name, git dot, auto dot, active workflow/agent badges)`
  + global `Roles / Models+quota / Pause`. No terminals here. Counters from
  `OpenAgents` grouped by project.
- `GET /projects/<id>`: workflow list (`state, branch, PR, round, last msg`),
  `New workflow` form (prompt, role/model preselect, branch auto), `autonomous`
  switch (disabled when `!has_git`).
- `GET /workflows/<id>`: header + agents (dev chain + tester rounds by `round`),
  PR/CI card, actions (`Send to testers, Open PR/Push, New agent, Stop, Close`).
- `GET /agents/<id>`: unchanged chat + terminal, breadcrumb back to workflow.

## 4. Technical changes

### M1 — store + migrations + tests

- New migration `00008_projects_workflows.sql`:
  `projects`, `workflows`, `agents.workflow_id FK + index`, `runs.workflow_id`.
- Backfill: one workflow per distinct `(project,branch)` root agent; testers get
  same `workflow_id` as their dev.
- CRUD + grouping queries: `OpenWorkflows(project)`, `WorkflowAgents(id)`,
  `ProjectCounts()`.

### M2 — trust + abs-path worktree

- `Projects{Dir}` subfolder-only (`internal/runner/worktree.go:18,30,52`) ->
  trusted abs roots. New `Resolve/Stat/List`.
- `Protect/AddWorktree/Dirty/RepoOf` take `project_path`; reuse `githubRepo` regex
  (`internal/runner/pr.go:32`). Config `projects.dir` -> `trusted_roots`.
- `POST /projects/trust` + confirm screen.

### M3 — workflow API (runner/Chats)

- `SummonWorkflow{project_path, prompt, model, files}` creates
  `workflow{branch: orch/<wf-id>-<slug>}` + first dev agent + worktree.
- `SummonAgent{workflow_id, role, model}` for replacement/manual tester.
- `Send/Test/OpenPR/Close` rescoped workflow-level; `canPublish`
  (`internal/runner/pr.go:53`) becomes workflow-level. Push helper unchanged.

### M4 — panel hierarchy

- `View` gains `Projects, Workflows` loaders (`internal/panel/view.go:23`).
- Split `index.html` sections into per-level partials; new routes
  (`internal/panel/server.go:101`); sidebar badges with 3s/10s htmx refresh.

### M5 — autonomous CI loop for workflows + parallel test

- Extend CI polling to workflow heads (reuse `ci.Follower` logic).
- E2E on PC2: 2 parallel workflows on one monorepo root, git vs non-git toggle,
  stop-dev + summon-new-dev continues same branch, autonomous ON runs
  dev -> test -> PR -> CI -> review -> fix without clicks.

## 5. Acceptance

- Two parallel workflows on one monorepo root, different branches, no cross-edit.
- Non-git project: autonomous locked OFF, chat + manual testers only.
- Stop dev + summon new dev continues same workflow/branch.
- Autonomous ON: full loop without manual clicks, bounded by limits.
- `gofmt -l`, `go vet ./...`, `go test -race ./...`, `go build ./cmd/orch`,
  `./orch -config config.example.yaml check-config` green.
