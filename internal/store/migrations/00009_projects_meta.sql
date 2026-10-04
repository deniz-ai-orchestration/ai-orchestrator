-- +goose Up

-- Projects get a display name and an optional description, asked on
-- creation. Existing rows keep theirs empty; the panel falls back to the
-- directory's last folder.
ALTER TABLE projects ADD COLUMN name TEXT NOT NULL DEFAULT '';
ALTER TABLE projects ADD COLUMN description TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE projects DROP COLUMN description;
ALTER TABLE projects DROP COLUMN name;
