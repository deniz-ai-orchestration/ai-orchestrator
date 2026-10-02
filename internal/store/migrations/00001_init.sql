-- +goose Up

-- One row per issue being worked. Counters bound every loop (see engine).
CREATE TABLE tasks (
    id             INTEGER PRIMARY KEY,
    repo           TEXT    NOT NULL,
    issue_number   INTEGER NOT NULL,
    title          TEXT    NOT NULL,
    body           TEXT    NOT NULL,
    state          TEXT    NOT NULL,
    work           TEXT    NOT NULL DEFAULT '',
    reason         TEXT    NOT NULL DEFAULT '',
    resume_state   TEXT    NOT NULL DEFAULT '',
    resume_work    TEXT    NOT NULL DEFAULT '',
    ci_attempts    INTEGER NOT NULL DEFAULT 0,
    review_cycles  INTEGER NOT NULL DEFAULT 0,
    dev_runs       INTEGER NOT NULL DEFAULT 0,
    branch         TEXT    NOT NULL DEFAULT '',
    pr_number      INTEGER NOT NULL DEFAULT 0,
    head_sha       TEXT    NOT NULL DEFAULT '',
    dev_model      TEXT    NOT NULL DEFAULT '',
    version        INTEGER NOT NULL DEFAULT 1,
    created_at     TEXT    NOT NULL,
    updated_at     TEXT    NOT NULL
);
-- At most one unfinished task per issue.
CREATE UNIQUE INDEX tasks_active_issue ON tasks (repo, issue_number)
    WHERE state NOT IN ('done', 'rejected', 'cancelled');
CREATE INDEX tasks_state ON tasks (state);

-- Append-only audit log: every state change with its cause.
CREATE TABLE transitions (
    id          INTEGER PRIMARY KEY,
    task_id     INTEGER NOT NULL REFERENCES tasks (id),
    from_state  TEXT    NOT NULL,
    to_state    TEXT    NOT NULL,
    event       TEXT    NOT NULL,
    reason      TEXT    NOT NULL DEFAULT '',
    detail      TEXT    NOT NULL DEFAULT '',
    at          TEXT    NOT NULL
);
CREATE INDEX transitions_task ON transitions (task_id, id);

-- One row per agent run (developer, reviewer, tester, helper).
CREATE TABLE runs (
    id             INTEGER PRIMARY KEY,
    task_id        INTEGER REFERENCES tasks (id),  -- NULL for ad-hoc sessions
    role           TEXT    NOT NULL,
    work           TEXT    NOT NULL DEFAULT '',
    provider       TEXT    NOT NULL,
    model          TEXT    NOT NULL,
    status         TEXT    NOT NULL,                -- queued, running, succeeded, failed, interrupted
    outcome        TEXT    NOT NULL DEFAULT '',     -- engine reason or 'ok'
    container_id   TEXT    NOT NULL DEFAULT '',
    exit_code      INTEGER,
    input_tokens   INTEGER,
    output_tokens  INTEGER,
    started_at     TEXT,
    ended_at       TEXT,
    log_dir        TEXT    NOT NULL DEFAULT ''
);
CREATE INDEX runs_task ON runs (task_id, id);
CREATE INDEX runs_status ON runs (status);

-- Files kept for a run: prompt, stdout, stderr, result JSON, diff.
CREATE TABLE run_artifacts (
    id      INTEGER PRIMARY KEY,
    run_id  INTEGER NOT NULL REFERENCES runs (id),
    kind    TEXT    NOT NULL,
    path    TEXT    NOT NULL,
    bytes   INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX run_artifacts_run ON run_artifacts (run_id);

-- Live switches per model: manual disable, cooldown, pin per role.
CREATE TABLE provider_state (
    model           TEXT PRIMARY KEY,
    disabled        INTEGER NOT NULL DEFAULT 0,
    cooldown_until  TEXT,
    cooldown_step   INTEGER NOT NULL DEFAULT 0,    -- 1h, 2h, 4h backoff when no reset is reported
    updated_at      TEXT    NOT NULL
);
CREATE TABLE role_pins (
    role        TEXT PRIMARY KEY,
    model       TEXT NOT NULL,
    pinned_at   TEXT NOT NULL
);

-- Rate-limit, quota and auth signals, for /quota and cooldown decisions.
CREATE TABLE capacity_events (
    id        INTEGER PRIMARY KEY,
    model     TEXT    NOT NULL,
    run_id    INTEGER REFERENCES runs (id),
    kind      TEXT    NOT NULL,                     -- rate_limit, quota, auth_failed, capacity
    reset_at  TEXT,
    detail    TEXT    NOT NULL DEFAULT '',
    at        TEXT    NOT NULL
);
CREATE INDEX capacity_events_model ON capacity_events (model, at);

-- External events waiting to be applied; dedupe_key makes polling idempotent.
CREATE TABLE inbox (
    id            INTEGER PRIMARY KEY,
    source        TEXT    NOT NULL,                 -- github, telegram, runner, panel
    dedupe_key    TEXT    NOT NULL UNIQUE,
    task_id       INTEGER REFERENCES tasks (id),
    payload       TEXT    NOT NULL,                 -- JSON
    received_at   TEXT    NOT NULL,
    processed_at  TEXT,
    error         TEXT    NOT NULL DEFAULT ''
);
CREATE INDEX inbox_pending ON inbox (id) WHERE processed_at IS NULL;

-- Side effects queued in the same transaction as the transition that caused them.
CREATE TABLE outbox (
    id             INTEGER PRIMARY KEY,
    task_id        INTEGER NOT NULL REFERENCES tasks (id),
    transition_id  INTEGER NOT NULL REFERENCES transitions (id),
    kind           TEXT    NOT NULL,
    arg            TEXT    NOT NULL DEFAULT '',
    created_at     TEXT    NOT NULL,
    attempts       INTEGER NOT NULL DEFAULT 0,
    done_at        TEXT,
    last_error     TEXT    NOT NULL DEFAULT ''
);
CREATE INDEX outbox_pending ON outbox (id) WHERE done_at IS NULL;

-- GitHub poll cursors and ETags per endpoint.
CREATE TABLE github_cursor (
    endpoint    TEXT PRIMARY KEY,
    etag        TEXT NOT NULL DEFAULT '',
    since       TEXT NOT NULL DEFAULT '',
    updated_at  TEXT NOT NULL
);

-- Single-use nonces behind Telegram inline buttons.
CREATE TABLE telegram_actions (
    nonce       TEXT PRIMARY KEY,
    task_id     INTEGER REFERENCES tasks (id),
    action      TEXT NOT NULL,
    arg         TEXT NOT NULL DEFAULT '',
    created_at  TEXT NOT NULL,
    used_at     TEXT
);

-- +goose Down
DROP TABLE telegram_actions;
DROP TABLE github_cursor;
DROP TABLE outbox;
DROP TABLE inbox;
DROP TABLE capacity_events;
DROP TABLE role_pins;
DROP TABLE provider_state;
DROP TABLE run_artifacts;
DROP TABLE runs;
DROP TABLE transitions;
DROP TABLE tasks;
