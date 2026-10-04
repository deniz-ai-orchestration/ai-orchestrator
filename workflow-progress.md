# Workflow plan — progress tracker

Plan: `workflow-plan.md`. Machine: PC2 (Ubuntu 24.04). Updated: 2026-10-04 (PC2).

How to use after model/quota switch: open this file + `workflow-plan.md`, continue
from the first unchecked box. Keep boxes checked as you finish; note PC2-only
findings under "Notes".

## Handoff state

- [x] PC2 repo status checked (`git status`, uncommitted/unpushed handled)
- [x] `workflow-plan.md` read on PC2
- [x] Baseline green on PC2 (`gofmt`, `vet`, `test -race`, `build`, `check-config`)

## M1 — store + migrations + tests

- [x] `00008_projects_workflows.sql` (`projects`, `workflows`, `agents.workflow_id`, `runs.workflow_id`)
- [x] Backfill existing agents into workflows
- [x] Store CRUD + grouping queries (`OpenWorkflows`, `WorkflowAgents`, `ProjectCounts`)
- [x] `go test -race ./internal/store/...` green

## M2 — trust + abs-path worktree

- [x] `Projects` supports trusted abs roots (replace subfolder-only logic)
- [x] `Resolve/Stat/List` + `.git` detect + origin parse
- [x] `POST /projects/trust` confirm screen
- [x] `Protect/AddWorktree/Dirty/RepoOf` on `project_path`
- [x] Config `projects.dir` -> `trusted_roots` (+ example config updated)
- [x] Non-git project forces `autonomous=0`, hides `Open PR`

## M3 — workflow API

- [x] `SummonWorkflow` (creates workflow + branch + first dev + worktree)
- [x] `SummonAgent` (replacement / manual tester in same workflow)
- [x] Workflow-scoped `Send/Test/OpenPR/Close`
- [x] Tester-first only when PR already open
- [x] `go test -race ./internal/runner/...` green

## M4 — panel hierarchy

- [x] `GET /` dashboard: project sidebar + badges, no terminals
- [x] `GET /projects/<id>`: workflow list + new-workflow form + autonomous switch
- [x] `GET /workflows/<id>`: agents by round + PR/CI card + actions
- [x] `GET /agents/<id>`: breadcrumb back to workflow
- [x] `View` loaders + routes wired

## M5 — autonomous CI loop + parallel test

- [ ] CI polling for workflow heads
- [ ] Autonomous ON e2e: dev -> test -> PR -> CI -> review -> fix (bounded by limits)
- [ ] Monorepo parallel test: 2 workflows, same repo, different branches
- [ ] Stop-dev + summon-new-dev continues same branch
- [ ] Full CI green (`gofmt`, `vet`, `test -race`, `build`, `check-config`)

## Deferred (do NOT do in this job)

- [ ] Summon memory options (empty / project-memory / from-checkpoint)
- [ ] Planner role, auto compact/clear

## Notes (PC2 findings, append here)

- 2026-10-04 (PC2): repo clean on `main` @ 6ac8140, in sync with origin. The PR #19
  CI failure was a flake, unrelated to the PR's content (it only added these two
  markdown files; the post-merge CI run on main passed). `TestTestersIncompleteNeedsYou`
  (internal/runner) failed because each finished tester calls `settleTests`: when both
  testers finish near-simultaneously, the second call (serialized on `testMu`) re-settled
  the already-retired round and re-posted the testers' findings to the developer, so the
  test's "last message" assertion saw the duplicate report. Fix: `settleTests` now
  returns early when every tester of the current round is already closed (settled once).
  Verified with `go test -race -count=40` on the tester tests (flake reproduced ~15%
  before the fix) and a full green baseline (`gofmt`, `vet`, `test -race ./...`,
  `build`, `check-config`).
