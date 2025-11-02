-- +goose Up
ALTER TABLE tg_post
	ADD COLUMN IF NOT EXISTS channel_id TEXT;

-- +goose Down
ALTER TABLE tg_post
	DROP COLUMN IF EXISTS channel_id;
