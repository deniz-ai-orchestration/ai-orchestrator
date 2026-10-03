-- +goose Up

-- Agents summoned from the panel: one CLI session in its own git worktree
-- of a project folder on PC2.
CREATE TABLE agents (
    id          INTEGER PRIMARY KEY,
    role        TEXT NOT NULL,
    provider    TEXT NOT NULL,
    model       TEXT NOT NULL,
    project     TEXT NOT NULL,              -- folder name under projects.dir
    branch      TEXT NOT NULL,
    base        TEXT NOT NULL DEFAULT '',   -- commit the branch started from
    workspace   TEXT NOT NULL DEFAULT '',   -- the worktree on the host
    session     TEXT NOT NULL DEFAULT '',   -- the CLI's session id, for resume
    state       TEXT NOT NULL,              -- working, needs_you, done, stopped, failed, closed
    created_at  TEXT NOT NULL,
    updated_at  TEXT NOT NULL
);
CREATE INDEX agents_state ON agents (state);

-- The conversation with an agent: your messages, its replies, and orch's
-- notes (a failed or stopped turn).
CREATE TABLE messages (
    id          INTEGER PRIMARY KEY,
    agent_id    INTEGER NOT NULL REFERENCES agents (id),
    author      TEXT NOT NULL,              -- you, agent, orch
    body        TEXT NOT NULL,
    files       TEXT NOT NULL DEFAULT '',   -- attached file names, one per line
    run_id      INTEGER REFERENCES runs (id),
    created_at  TEXT NOT NULL
);
CREATE INDEX messages_agent ON messages (agent_id, id);

ALTER TABLE runs ADD COLUMN agent_id INTEGER REFERENCES agents (id);
CREATE INDEX runs_agent ON runs (agent_id, id);

-- +goose Down
DROP INDEX runs_agent;
ALTER TABLE runs DROP COLUMN agent_id;
DROP TABLE messages;
DROP TABLE agents;
