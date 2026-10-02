-- +goose Up

-- Small runtime switches and cursors: the pause flag, the Telegram update
-- offset.
CREATE TABLE settings (
    key         TEXT PRIMARY KEY,
    value       TEXT NOT NULL,
    updated_at  TEXT NOT NULL
);

-- +goose Down
DROP TABLE settings;
