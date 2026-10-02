# Agentic SDLC / Autonomous Coding Team

I want to build a personal, self-hosted agentic software-development workflow that behaves like a small software engineering team.

The goal is NOT to build another generic "AI agent framework". The goal is to build a reliable orchestration system that coordinates existing coding agents/CLIs and turns a GitHub issue into an implementation → CI → QA → review → human approval → merge workflow.

I want you to act as the **technical architect and implementation partner** for this project.

Do NOT start implementing the full system immediately. First inspect the requirements below, research the current ecosystem where necessary, identify risks/unknowns, and propose the architecture and implementation plan. We will approve the architecture before substantial coding begins.

---

## 1. Current resources

I have these subscriptions available:

* Claude Code — $20/month
* OpenAI/Codex — $20/month
* Gemini — $20/month
* OpenCode — $10/month

The subscriptions are not all personally used by me; some belong to family members and currently have very low usage. We must NOT assume that we can freely automate against other people's accounts. Before using them operationally, identify authentication/terms/usage concerns and design the system so providers can be enabled/disabled independently.

I personally use Claude Code and Codex.

I also have two local machines running Ollama.

### PC1

* Ryzen 7950X3D
* 32 GB DDR5 6000 MHz
* GTX 1080 Ti 11 GB
* Windows
* Ollama installed

### PC2

* Intel i7-7800K
* 16 GB DDR4 3000 MHz
* GTX 1080 8 GB
* Windows
* Ollama installed

PC1 should initially be considered the main worker/orchestrator machine.

PC2 can be used for lightweight Ollama/background jobs and potentially a secondary worker/CI runner.

Local LLMs should primarily handle inexpensive tasks such as:

* CI log summarization
* PR summarization
* issue classification
* Telegram command parsing
* review-comment categorization
* simple documentation
* routing/helper tasks

Do not assume local models are suitable for important code review without testing their quality first.

---

# 2. Desired SDLC

The desired long-term workflow is approximately:

Human
↓
Issue / task
↓
Developer agent
↓
Git branch / worktree
↓
Implementation
↓
Commit + push
↓
Pull Request
↓
Deterministic CI
↓
Tester 1
↓
Tester 2
↓
Tester Lead
↓
Human approval
↓
Merge

If testing/review fails:

Tester
↓
PR comments / review
↓
Developer agent
↓
Fix
↓
CI
↓
Tester again

There must be bounded retry loops.

For example:

* CI fix attempts: maximum 2–3
* review/fix cycles: maximum 3
* after the limit, transition to `needs-human`

Never allow an autonomous infinite loop.

---

# 3. Important architectural principles

## GitHub is the collaboration layer

Agents should NOT directly communicate through arbitrary LLM-to-LLM conversations.

GitHub should contain the important development state:

* Issues
* branches
* commits
* pull requests
* PR comments
* review results
* CI results
* labels

The orchestrator reads/writes GitHub state and invokes agents.

However, GitHub labels should NOT necessarily be the canonical internal state. The orchestrator should maintain its own persistent workflow state so it can recover after crashes/restarts.

---

## Deterministic CI comes before AI QA

Never spend LLM quota asking an agent whether:

* PHPUnit passed
* TypeScript compiles
* ESLint passed
* build succeeded
* static analysis passed

GitHub Actions should perform deterministic checks first.

AI testers should concentrate on:

* acceptance criteria
* business logic
* missing tests
* regression risks
* edge cases
* security issues
* specification mismatches

---

## Human merge initially

The first versions must NOT automatically merge normal PRs.

The desired initial flow is:

Tester Lead approves
↓
Telegram notification
↓
Human clicks Merge
↓
Orchestrator merges

Automatic merging can be introduced later for explicitly low-risk categories such as:

* documentation
* tests
* trivial chores

and only after sufficient confidence/data exists.

---

## Human escalation is a first-class state

The system must support:

`AUTONOMOUS → NEEDS_HUMAN → WAITING_FOR_HUMAN → RESUMED`

Examples:

* contradictory tester results
* repeated failures
* merge conflicts
* rate limits
* unexpected CLI failures
* unclear requirements
* security-sensitive changes
* budget/quota exhaustion

The system should stop safely rather than guessing.

---

# 4. Initial roles

Do NOT implement all roles immediately.

The eventual roles are:

### Human

Defines tasks and handles escalation.

### PM / Analyst

Turns requirements into structured tasks.

This should initially remain HUMAN.

### Developer

Implements the issue, commits, pushes, and opens the PR.

Initial candidate: Claude Code.

### Tester 1

Functional QA.

Checks:

* acceptance criteria
* obvious functional bugs
* missing tests
* specification mismatch

Initial candidate: OpenCode.

### Tester 2

Adversarial / regression QA.

Attempts to break the implementation.

Initial candidate: Gemini.

This should initially be optional and/or used only for larger/riskier changes.

### Tester Lead

Independent final reviewer.

Reviews:

* original requirements
* diff
* CI results
* tester reports
* tests
* potential regressions

Initial candidate: Codex.

### Release/Merge

Initially HUMAN.

---

# 5. Initial model mapping

Use this only as an initial configuration, NOT hardcoded architecture:

```yaml
roles:
  developer:
    provider: claude

  functional_tester:
    provider: opencode

  adversarial_tester:
    provider: gemini

  tester_lead:
    provider: openai
```

The architecture must allow changing providers/models without changing application code.

For example:

```yaml
roles:
  developer:
    provider: claude
    cli: claude

  tester_lead:
    provider: openai
    cli: codex
```

Later I should be able to change this configuration to use another model.

---

# 6. Important: subscription quota

Do NOT model the subscriptions as:

"$20 = X dollars of API credits."

Instead treat them as provider-specific capacity/rate-limit resources.

Track at least:

* provider
* model
* role
* run count
* duration
* approximate input/output usage when available
* success/failure
* rate-limit events
* timestamp
* task
* attempt number

If a provider returns a rate-limit/quota error:

1. stop using that provider
2. do NOT retry-spam
3. record the event
4. notify me through Telegram
5. optionally route future work to another configured provider

Design provider capacity management as a first-class component.

---

# 7. Agent execution

Agents should run in isolated workspaces.

Prefer one git worktree/workspace per task/agent.

Example:

```text
/workspaces/
    task-101/
        repo/

    task-102/
        repo/
```

Parallel agents must never accidentally share a working directory.

We should investigate whether Docker should be used for the agent runtime, especially for reproducibility and permission isolation.

The developer should be able to:

1. receive task
2. inspect repository
3. modify files
4. run tests
5. commit
6. push
7. create PR

The tester should receive a clean checkout of the PR and should not modify the developer workspace.

---

# 8. Agent permissions

Principle of least privilege.

Developer:

* read repository
* create branch
* push branch
* create PR
* comment

Developer should NOT initially be able to merge `main`.

Tester:

* read repository
* inspect PR
* run tests
* comment/review

Tester should NOT push changes.

Tester Lead:

* review
* approve/request changes

Merge should initially require the human.

Use a separate GitHub bot account or GitHub App if practical so automated actions are clearly distinguishable from my own actions.

Research the current GitHub permission model and recommend the safest practical setup.

---

# 9. Suggested workflow state

Something similar to:

```text
created
↓
ready
↓
dev:working
↓
pr:opened
↓
ci:running
↓
ci:failed → dev:fixing
↓
review:t1
↓
review:t2
↓
review:lead
↓
awaiting-human
↓
merged
```

Failure paths should lead back to the appropriate state.

Example:

```text
review:t1
↓
changes_requested
↓
dev:fixing
↓
ci:running
↓
review:t1
```

All transitions should be persisted.

Do not make this exact state machine a hard requirement; improve it if you see a better design.

---

# 10. Orchestrator

I currently prefer a small custom Go service rather than adopting a huge agent framework.

Candidate stack:

* Go
* SQLite initially
* GitHub API / GitHub CLI where appropriate
* Telegram Bot API
* Docker
* Git
* Ollama
* existing coding-agent CLIs

Do NOT introduce PostgreSQL, Redis, Kafka, Kubernetes, LangChain, CrewAI, etc. unless there is a concrete reason.

The first implementation should remain small and understandable.

Potential structure:

```text
orchestrator/
├── cmd/
│   └── orch/
├── internal/
│   ├── github/
│   ├── runner/
│   ├── roles/
│   ├── workflow/
│   ├── quota/
│   ├── state/
│   ├── telegram/
│   └── config/
├── migrations/
├── config.yaml
└── README.md
```

This is only a starting point; propose a better structure if justified.

---

# 11. Provider abstraction

We need a provider/CLI abstraction.

Something conceptually like:

```text
AgentRunner
    ↓
ProviderAdapter
    ├── Claude
    ├── Codex
    ├── Gemini
    ├── OpenCode
    └── Ollama
```

The orchestrator should not care about provider-specific command syntax.

Each adapter should normalize:

* start
* stop
* timeout
* stdout
* stderr
* exit code
* rate-limit detection
* authentication failure
* successful completion
* tool failure

We need to test the actual current CLI behavior before designing the final interface.

---

# 12. Telegram

Telegram is the human control plane.

It should initially support:

* task started
* PR created
* CI failed
* tester failed
* lead approved
* human approval required
* merge button
* reject button
* retry
* pause
* resume
* status
* quota/rate-limit alerts

Potential commands:

```text
/status
/tasks
/pause
/resume
/retry
/cancel
/cost
/quota
```

Do not make Telegram the source of truth.

GitHub + orchestrator state remain authoritative.

---

# 13. First MVP

The first real MVP should NOT contain all agents.

Build this:

```text
GitHub Issue
    ↓
Claude Developer
    ↓
Pull Request
    ↓
GitHub Actions
    ↓
Codex Reviewer
    ↓
Telegram
    ↓
Human Merge
```

Success criteria:

> I can create a suitable GitHub issue and the system can autonomously implement it, create a PR, run CI, have Codex review it, and ask me through Telegram whether to merge.

I should only need to intervene at the merge/exception point.

---

# 14. Phase 2

Add:

```text
Tester 1
```

Workflow:

```text
Developer
↓
CI
↓
Tester 1
↓
Tester Lead
↓
Human
```

---

# 15. Phase 3

Add:

```text
Tester 2
```

Only use Tester 2 for:

* risky changes
* large PRs
* security-sensitive changes
* changes touching important architecture

Do not automatically spend quota on every trivial PR.

---

# 16. Phase 4

Add:

* parallel developers
* multiple worktrees
* concurrency limits
* task scheduling
* quota-aware routing
* daily Telegram reports

---

# 17. Phase 5

Optional:

* Analyst
* PM
* automatic issue decomposition
* automatic task generation
* automatic merge for low-risk labels
* multi-project support

---

# 18. Open-source research

Before implementing the architecture, investigate the current state of projects that may overlap with this system.

At minimum investigate:

* OpenHands
* Aider
* OpenCode
* Cline
* SWE-agent
* LangGraph
* CrewAI
* AutoGen
* OpenAI Agents SDK
* Claw Orchestrator
* oh-my-claudecode
* Raven
* OpenACP
* telegram-ai-bridge

Do NOT assume these are suitable just because they exist.

For each relevant project evaluate:

* GitHub activity
* maintenance
* license
* architecture
* security concerns
* supported coding CLIs
* headless operation
* Telegram support
* credential handling
* sandboxing
* whether it solves a real problem for this project

Prefer composing small reliable components over adopting a giant framework.

---

# 19. Very important: research current CLI behavior

Before implementing provider adapters, verify the CURRENT versions/documentation of:

* Claude Code
* Codex CLI
* Gemini CLI
* OpenCode
* Ollama

Specifically investigate:

* headless/non-interactive usage
* authentication
* command syntax
* exit codes
* structured output
* timeout behavior
* permissions
* sandboxing
* rate-limit behavior
* current terms/usage restrictions
* Windows support

Do not rely on memory for these details.

---

# 20. Pilot project

I have several Laravel projects and other software projects.

Recommend a suitable low-risk pilot project based on what you can inspect or what I provide.

The pilot should preferably:

* have automated tests
* not be production-critical
* have reasonably clear tasks
* allow us to intentionally test failure/retry behavior

Do not choose a project merely because it is technically interesting.

---

# 21. Security requirements

Treat all agent execution as potentially unsafe.

Investigate:

* shell command execution
* credentials
* `.env` files
* SSH keys
* GitHub tokens
* cloud credentials
* production access
* Docker socket access
* repository secrets

Agents must not automatically receive production credentials.

The initial environment should be disposable and isolated.

Never design the system assuming an LLM will always behave correctly.

---

# 22. Observability

Every agent run should eventually be auditable.

Track:

```text
task
run
agent
provider
model
start time
end time
exit code
stdout
stderr
files changed
commit
PR
attempt
state transition
failure reason
quota/rate-limit event
```

The system should be able to answer:

> "Why is task #123 currently stuck?"

without reading raw logs manually.

---

# 23. First deliverable from you

Before writing significant implementation code, produce:

## A. Architecture proposal

Include:

* components
* responsibilities
* data flow
* workflow/state machine
* provider abstraction
* GitHub integration
* Telegram integration
* local Ollama integration
* security boundaries
* persistence

## B. Technology decision

Explain:

* Go vs alternatives
* SQLite vs PostgreSQL
* Docker vs bare-metal execution
* GitHub CLI vs GitHub API
* Telegram implementation
* whether we need an agent framework

## C. Open-source evaluation

Give me a concise table:

| Project | What it provides | Useful to us? | Reuse / Reference / Ignore | Risks |
| ------- | ---------------- | ------------- | -------------------------- | ----- |

## D. Provider strategy

Explain how Claude/Codex/Gemini/OpenCode/Ollama should initially be assigned.

## E. MVP specification

Define exactly what we need to build first.

## F. Human TODO list

Separate everything I personally need to configure/create/provide.

## G. Implementation roadmap

Break the work into small milestones that can each be tested.

Do NOT start by generating hundreds of lines of code.

First help me make the architecture correct.

After I approve the architecture, we will implement Phase 0 and Phase 1 incrementally.
