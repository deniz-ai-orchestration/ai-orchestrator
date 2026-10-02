-- +goose Up

-- The outbox item (start_dev_run / start_review_run) a run executes, so a
-- restart can tell a pending effect that never started from one whose run
-- was interrupted.
ALTER TABLE runs ADD COLUMN outbox_id INTEGER REFERENCES outbox (id);
CREATE INDEX runs_outbox ON runs (outbox_id);

-- +goose Down
DROP INDEX runs_outbox;
ALTER TABLE runs DROP COLUMN outbox_id;
