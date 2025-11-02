-- +goose Up
ALTER TABLE vk_post
	ADD COLUMN IF NOT EXISTS post_text TEXT;

ALTER TABLE tg_post
	ADD COLUMN IF NOT EXISTS post_text TEXT;

-- +goose Down
ALTER TABLE tg_post
	DROP COLUMN IF EXISTS post_text;

ALTER TABLE vk_post
	DROP COLUMN IF EXISTS post_text;
