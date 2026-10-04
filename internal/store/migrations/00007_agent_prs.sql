-- +goose Up

-- The pull request orch opened for a developer agent's branch.
ALTER TABLE agents ADD COLUMN pr_number INTEGER NOT NULL DEFAULT 0;
ALTER TABLE agents ADD COLUMN pr_url TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE agents DROP COLUMN pr_url;
ALTER TABLE agents DROP COLUMN pr_number;
