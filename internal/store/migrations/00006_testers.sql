-- +goose Up

-- Testers are agents too: parent_id names the developer agent they test,
-- round counts the developer's test rounds. A message's data keeps a
-- tester's structured verdict.
ALTER TABLE agents ADD COLUMN parent_id INTEGER REFERENCES agents (id);
ALTER TABLE agents ADD COLUMN round INTEGER NOT NULL DEFAULT 0;
CREATE INDEX agents_parent ON agents (parent_id, round);
ALTER TABLE messages ADD COLUMN data TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE messages DROP COLUMN data;
DROP INDEX agents_parent;
ALTER TABLE agents DROP COLUMN round;
ALTER TABLE agents DROP COLUMN parent_id;
