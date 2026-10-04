-- +goose Up

-- Projects are trusted directories on PC2 that orch may work in; workflows
-- are the jobs inside them. A workflow owns its branch, test rounds and
-- PR/CI state and survives agent replacement; agents are the replaceable
-- workers inside it.
CREATE TABLE projects (
    path        TEXT PRIMARY KEY,            -- absolute, cleaned directory
    trusted     INTEGER NOT NULL DEFAULT 0,
    has_git     INTEGER NOT NULL DEFAULT 0,
    autonomous  INTEGER NOT NULL DEFAULT 0,  -- autonomous loop; git projects only
    github_repo TEXT NOT NULL DEFAULT '',    -- owner/name of origin, if GitHub
    created_at  TEXT NOT NULL,
    updated_at  TEXT NOT NULL
);

CREATE TABLE workflows (
    id           INTEGER PRIMARY KEY,
    project_path TEXT NOT NULL REFERENCES projects (path),
    title        TEXT NOT NULL DEFAULT '',
    branch       TEXT NOT NULL,
    base         TEXT NOT NULL DEFAULT '',     -- commit the branch started from
    state        TEXT NOT NULL DEFAULT 'open', -- open, closed
    round        INTEGER NOT NULL DEFAULT 0,   -- test rounds (review_cycles limit)
    dev_runs     INTEGER NOT NULL DEFAULT 0,   -- dev agents summoned (dev_runs limit)
    ci_attempts  INTEGER NOT NULL DEFAULT 0,   -- CI fix attempts (ci_attempts limit)
    pr_number    INTEGER NOT NULL DEFAULT 0,
    pr_url       TEXT NOT NULL DEFAULT '',
    head_sha     TEXT NOT NULL DEFAULT '',     -- pushed head CI is watched on
    ci_state     TEXT NOT NULL DEFAULT '',     -- '', pending, green, red
    workspace    TEXT NOT NULL DEFAULT '',
    prompt       TEXT NOT NULL DEFAULT '',
    model        TEXT NOT NULL DEFAULT '',
    phase        TEXT NOT NULL DEFAULT 'idle',
    review_cycles INTEGER NOT NULL DEFAULT 0,
    created_at   TEXT NOT NULL,
    updated_at   TEXT NOT NULL
);
CREATE INDEX workflows_project ON workflows (project_path, state, id);
CREATE UNIQUE INDEX workflows_branch ON workflows(project_path, branch) WHERE branch != '' AND state = 'open';

ALTER TABLE agents ADD COLUMN workflow_id INTEGER REFERENCES workflows (id);
CREATE INDEX agents_workflow ON agents (workflow_id, id);
ALTER TABLE runs ADD COLUMN workflow_id INTEGER REFERENCES workflows (id);
CREATE INDEX runs_workflow ON runs (workflow_id, id);

-- Backfill: agents summoned before workflows each used a project folder
-- name and a branch. Every project name becomes a trusted legacy project
-- row, every distinct (project, branch) of a root agent becomes one
-- workflow (a root agent whose branch never got created gets one of its
-- own), testers join their developer's workflow and runs their agent's.
INSERT INTO projects (path, trusted, has_git, autonomous, github_repo, created_at, updated_at)
SELECT project, 1, 1, 0, '', MIN(created_at), MIN(created_at) FROM agents GROUP BY project;

INSERT INTO workflows (project_path, title, branch, base, state, round, pr_number, pr_url, created_at, updated_at)
SELECT a.project, '', a.branch, a.base, 'open', a.round, a.pr_number, a.pr_url, a.created_at, a.updated_at
FROM agents a
WHERE a.parent_id IS NULL AND (a.branch = '' OR a.id = (
    SELECT MIN(b.id) FROM agents b
    WHERE b.parent_id IS NULL AND b.project = a.project AND b.branch = a.branch AND b.branch != ''));

UPDATE agents SET workflow_id = (
    SELECT w.id FROM workflows w
    WHERE w.project_path = agents.project AND w.branch = agents.branch AND w.created_at = agents.created_at
    LIMIT 1)
WHERE parent_id IS NULL;

UPDATE agents SET workflow_id = (
    SELECT MIN(w.id) FROM workflows w
    WHERE w.project_path = agents.project AND w.branch = agents.branch)
WHERE parent_id IS NULL AND workflow_id IS NULL;

UPDATE agents SET workflow_id = (
    SELECT p.workflow_id FROM agents p WHERE p.id = agents.parent_id)
WHERE parent_id IS NOT NULL;

UPDATE runs SET workflow_id = (
    SELECT a.workflow_id FROM agents a WHERE a.id = runs.agent_id)
WHERE agent_id IS NOT NULL;

-- Carry the latest root's workspace and PR metadata, not the first session's.
UPDATE workflows SET
 workspace = COALESCE((SELECT workspace FROM agents WHERE workflow_id=workflows.id AND parent_id IS NULL AND workspace!='' ORDER BY id DESC LIMIT 1),''),
 model = COALESCE((SELECT model FROM agents WHERE workflow_id=workflows.id AND parent_id IS NULL ORDER BY id DESC LIMIT 1),''),
 round = COALESCE((SELECT MAX(round) FROM agents WHERE workflow_id=workflows.id),0),
 dev_runs = (SELECT COUNT(*) FROM agents WHERE workflow_id=workflows.id AND role='developer'),
 pr_number = COALESCE((SELECT pr_number FROM agents WHERE workflow_id=workflows.id AND pr_number>0 ORDER BY id DESC LIMIT 1),0),
 pr_url = COALESCE((SELECT pr_url FROM agents WHERE workflow_id=workflows.id AND pr_number>0 ORDER BY id DESC LIMIT 1),''),
 prompt = COALESCE((SELECT body FROM messages JOIN agents ON agents.id=messages.agent_id WHERE agents.workflow_id=workflows.id AND agents.parent_id IS NULL AND messages.author='you' ORDER BY messages.id LIMIT 1),'');

-- +goose Down
DROP INDEX runs_workflow;
ALTER TABLE runs DROP COLUMN workflow_id;
DROP INDEX agents_workflow;
ALTER TABLE agents DROP COLUMN workflow_id;
DROP TABLE workflows;
DROP TABLE projects;
