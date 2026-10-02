-- +goose Up

-- The reviewer model picked for the current review, so the review run and
-- the posted verdict name the model that actually reviewed.
ALTER TABLE tasks ADD COLUMN review_model TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE tasks DROP COLUMN review_model;
