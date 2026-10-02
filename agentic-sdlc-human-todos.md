# Agentic SDLC — Human TODOs

This checklist covers the things I personally need to configure, decide, verify, or provide while building the autonomous SDLC system.

> **Goal:** Keep human-controlled setup separate from the agentic implementation. Do not give autonomous agents unnecessary access to personal accounts, production systems, or credentials.

---

## Phase 0 — Project & Safety

### Pilot project

- [x] Choose a low-risk pilot repository
  - [x] Has automated tests
  - [x] Is not production-critical
  - [x] Has reasonably clear tasks/issues
  - [x] Does not expose production credentials
- [x] Decide whether to use an existing Laravel project or create a dedicated pilot repository
- [x] Create a dedicated test repository if needed
- [ ] Identify 3–5 simple issues suitable for autonomous implementation
- [ ] Prepare at least one intentionally failure-prone task for testing retry/review behavior

### Security

- [x] Ensure no production credentials are available to agents
- [x] Review `.env` / secret handling in the pilot repository
- [ ] Decide what filesystem locations agents may access
- [ ] Decide whether agents should run inside Docker
- [x] Ensure GitHub tokens/credentials are not unnecessarily exposed to coding agents
- [ ] Keep autonomous merge disabled initially
- [ ] Define what situations must always escalate to me

---

## Phase 1 — GitHub

### Repository

- [x] Choose the GitHub organization/account where the pilot will live
- [x] Create or prepare the pilot repository
- [x] Configure `main`/default branch protection
- [x] Require pull requests
- [ ] Require CI to pass before merge
- [x] Require at least one approval
- [x] Prevent direct pushes to the protected branch where appropriate

### Agent identity

- [ ] Decide between a dedicated GitHub bot account and GitHub App
- [ ] Prefer a GitHub App with least-privilege permissions if practical
- [ ] Create/configure the bot identity
- [ ] Verify the bot can:
  - [ ] Read repository contents
  - [ ] Create branches
  - [ ] Push branches
  - [ ] Create pull requests
  - [ ] Comment on PRs
  - [ ] Review PRs where required
- [ ] Do NOT give the developer agent merge permission initially
- [ ] Keep production repositories separate from the pilot

---

## Phase 2 — CLI Verification

Verify the current installed versions and headless behavior before building provider adapters.

### Claude Code

- [ ] Verify installation
- [ ] Record version
- [ ] Verify authentication
- [ ] Test non-interactive/headless execution
- [ ] Test reading a repository
- [ ] Test modifying a disposable repository
- [ ] Record exit code behavior
- [ ] Record timeout/failure behavior
- [ ] Observe rate-limit/quota errors

### Codex CLI

- [ ] Verify installation
- [ ] Record version
- [ ] Verify authentication
- [ ] Test non-interactive execution
- [ ] Test repository inspection
- [ ] Test review-only workflow
- [ ] Record exit code behavior
- [ ] Record timeout/failure behavior
- [ ] Observe rate-limit/quota errors

### Gemini CLI

- [ ] Verify installation
- [ ] Record version
- [ ] Verify authentication
- [ ] Test non-interactive execution
- [ ] Test repository inspection
- [ ] Record exit code behavior
- [ ] Observe quota/rate-limit behavior
- [ ] Confirm whether this account is approved for automated use

### OpenCode

- [ ] Verify installation
- [ ] Record version
- [ ] Verify authentication/provider configuration
- [ ] Test non-interactive execution
- [ ] Test repository inspection
- [ ] Record exit code behavior
- [ ] Observe quota/rate-limit behavior
- [ ] Confirm whether this account is approved for automated use

### Provider-account policy

- [ ] Confirm with family members before using their subscriptions for automation
- [ ] Check each provider's current terms/usage restrictions
- [ ] Do not assume subscription quota can be safely consumed by autonomous agents
- [ ] Make every provider independently disableable in the future system
- [ ] Keep Claude/Codex usable even if Gemini/OpenCode are disabled

---

## Phase 3 — Local Ollama

### PC1

- [ ] Verify Ollama installation
- [ ] Run `ollama list`
- [ ] Run `ollama ps`
- [ ] Record installed models
- [ ] Verify whether GTX 1080 Ti GPU acceleration is working
- [ ] Record approximate VRAM usage
- [ ] Test one lightweight model
- [ ] Test a simple non-coding task

### PC2

- [ ] Verify Ollama installation
- [ ] Run `ollama list`
- [ ] Run `ollama ps`
- [ ] Record installed models
- [ ] Verify whether GTX 1080 GPU acceleration is working
- [ ] Test one lightweight model
- [ ] Test a simple non-coding task

### Local-agent policy

- [ ] Do not use local models for important code review until quality is evaluated
- [ ] Candidate local tasks:
  - [ ] CI log summarization
  - [ ] PR summarization
  - [ ] Review-comment classification
  - [ ] Telegram command parsing
  - [ ] Simple documentation
  - [ ] Task routing
- [ ] Decide which machine should host the primary Ollama endpoint
- [ ] Decide whether PC2 should act as a secondary worker

---

## Phase 4 — Telegram

- [ ] Create a dedicated Telegram bot
- [ ] Store the bot token securely
- [ ] Determine the Telegram chat/user ID that should control the system
- [ ] Test receiving messages
- [ ] Test sending notifications
- [ ] Test inline buttons
- [ ] Initially support:
  - [ ] Merge
  - [ ] Reject
  - [ ] Retry
  - [ ] Pause
  - [ ] Resume
  - [ ] Status
- [ ] Do not use Telegram as the system's source of truth

---

## Phase 5 — CI

For the pilot repository:

- [ ] Create GitHub Actions workflow
- [ ] Configure linting
- [ ] Configure type checking/static analysis where applicable
- [ ] Configure unit tests
- [ ] Configure feature/integration tests where applicable
- [ ] Configure build
- [ ] Ensure CI runs automatically on PRs
- [ ] Ensure agents can read CI results
- [ ] Test a deliberately failing CI run
- [ ] Test a successful CI run
- [ ] Decide whether self-hosted runners are necessary for the pilot
- [ ] If using self-hosted runners, configure them on PC1/PC2 with appropriate isolation

---

## Phase 6 — Workspace / Execution Environment

- [ ] Decide whether agent execution will use Docker
- [ ] Decide where temporary workspaces live on PC1
- [ ] Decide where temporary workspaces live on PC2
- [ ] Verify Git worktree support
- [ ] Test creating an isolated worktree
- [ ] Test two simultaneous worktrees
- [ ] Verify that agents cannot accidentally modify another task's workspace
- [ ] Decide workspace cleanup policy
- [ ] Test cleanup after successful task
- [ ] Test cleanup after failed task
- [ ] Test cleanup after interrupted task

---

## Phase 7 — Initial Workflow Decisions

Confirm these decisions before autonomous operation:

- [ ] Developer agent initially uses Claude Code
- [ ] Tester Lead initially uses Codex
- [ ] Human performs merge initially
- [ ] CI must pass before AI review
- [ ] Developer gets a maximum number of CI retries
- [ ] Developer gets a maximum number of review/fix cycles
- [ ] Tester agents cannot push code
- [ ] Developer cannot merge protected branches
- [ ] Uncertain/contradictory reviews escalate to human
- [ ] Rate-limit errors pause the affected provider
- [ ] Infinite autonomous loops are impossible

---

## Phase 8 — Human Issue Template

Use a consistent issue format.

- [ ] Create GitHub issue template containing:
  - [ ] Goal
  - [ ] Acceptance criteria
  - [ ] Constraints
  - [ ] Out of scope
  - [ ] Relevant files/areas
- [ ] Make acceptance criteria explicit and testable
- [ ] Avoid vague issues during the initial pilot
- [ ] Prepare 3–5 pilot issues using the template

Example:

```markdown
## Goal

Describe exactly what should be implemented.

## Acceptance Criteria

- [ ] Criterion 1
- [ ] Criterion 2
- [ ] Criterion 3

## Constraints

- ...

## Out of Scope

- ...
```

---

## Phase 9 — First MVP Acceptance

The first MVP is successful when:

- [ ] I create a suitable GitHub issue
- [ ] Orchestrator detects the task
- [ ] Developer agent receives the issue
- [ ] Developer gets an isolated workspace
- [ ] Developer implements the change
- [ ] Developer commits and pushes
- [ ] Developer creates a PR
- [ ] GitHub Actions runs
- [ ] CI results are available to the orchestrator
- [ ] Codex reviews the PR
- [ ] Review result is recorded
- [ ] Telegram notifies me
- [ ] I can approve/reject from Telegram
- [ ] Human performs the merge
- [ ] The entire run is recoverable/auditable

---

## Phase 10 — Measurement

For every pilot task, record:

- [ ] Development agent used
- [ ] Reviewer used
- [ ] Number of developer runs
- [ ] Number of CI retries
- [ ] Number of review/fix cycles
- [ ] Approximate model usage
- [ ] Rate-limit events
- [ ] Total execution time
- [ ] Human intervention points
- [ ] Final result
- [ ] Problems encountered

After approximately 5–10 successful tasks:

- [ ] Review which provider is actually the bottleneck
- [ ] Review which tasks consume the most quota
- [ ] Review whether Tester 2 provides useful additional signal
- [ ] Review whether local Ollama saves meaningful cloud usage
- [ ] Decide whether parallel developers are justified
- [ ] Decide whether automatic merge is appropriate for any low-risk task category

---

## Phase 11 — Expansion

Only after the MVP is reliable:

- [ ] Add Tester 1
- [ ] Add Tester 2
- [ ] Add Tester Lead
- [ ] Add parallel developer worktrees
- [ ] Add quota-aware provider routing
- [ ] Add daily Telegram reports
- [ ] Add analyst agent
- [ ] Add PM agent
- [ ] Add automatic issue decomposition
- [ ] Add low-risk automatic merge
- [ ] Add multi-project support

---

# Current Priority

Do not attempt to complete this entire checklist before starting.

The immediate priorities are:

1. [ ] Choose pilot repository
2. [ ] Prepare GitHub permissions/branch protection
3. [ ] Verify Claude Code headless execution
4. [ ] Verify Codex CLI headless execution
5. [ ] Verify Gemini CLI headless execution
6. [ ] Verify OpenCode headless execution
7. [ ] Verify Ollama on PC1
8. [ ] Verify Ollama on PC2
9. [ ] Create Telegram bot
10. [ ] Give the results to Codex for architecture design

After architecture approval, begin Phase 0/Phase 1 implementation.
