-- +goose Up
ALTER TABLE published_posts RENAME TO vk_post;
ALTER TABLE vk_post RENAME COLUMN post_id TO id;

ALTER TABLE vk_post
	ADD COLUMN IF NOT EXISTS hash TEXT NOT NULL DEFAULT '';

ALTER TABLE vk_post
	ALTER COLUMN published_at DROP NOT NULL,
	ALTER COLUMN published_at DROP DEFAULT;

CREATE TABLE IF NOT EXISTS tg_post (
	vk_owner_id  BIGINT       NOT NULL,
	vk_post_id   BIGINT       NOT NULL,
	id           BIGINT       NOT NULL,
	published_at TIMESTAMPTZ  NOT NULL,
	PRIMARY KEY (vk_owner_id, vk_post_id, id),
	FOREIGN KEY (vk_owner_id, vk_post_id) REFERENCES vk_post (owner_id, id)
);

-- +goose Down
DROP TABLE IF EXISTS tg_post;

ALTER TABLE vk_post
	DROP COLUMN IF EXISTS hash;

ALTER TABLE vk_post RENAME COLUMN id TO post_id;

ALTER TABLE vk_post
	ALTER COLUMN published_at SET NOT NULL,
	ALTER COLUMN published_at SET DEFAULT NOW();

ALTER TABLE vk_post RENAME TO published_posts;