-- +goose Up
DO $$
BEGIN
	IF to_regclass('vk_post') IS NULL THEN
		IF to_regclass('posted_mesage') IS NOT NULL THEN
			EXECUTE 'ALTER TABLE posted_mesage RENAME TO vk_post';
		ELSIF to_regclass('published_posts') IS NOT NULL THEN
			EXECUTE 'ALTER TABLE published_posts RENAME TO vk_post';
		ELSE
			RAISE NOTICE 'table posted_mesage / published_posts not found, creating vk_post from scratch';
			EXECUTE '
				CREATE TABLE vk_post (
					owner_id     BIGINT       NOT NULL,
					id           BIGINT       NOT NULL,
					published_at TIMESTAMPTZ,
					hash         TEXT         NOT NULL DEFAULT '''',
					PRIMARY KEY (owner_id, id)
				);
			';
		END IF;
	END IF;
END $$;

DO $$
BEGIN
	IF EXISTS (
		SELECT 1
		FROM information_schema.columns
		WHERE table_name = 'vk_post' AND column_name = 'post_id'
	) THEN
		EXECUTE 'ALTER TABLE vk_post RENAME COLUMN post_id TO id';
	END IF;
END $$;

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

DO $$
BEGIN
	IF EXISTS (
		SELECT 1
		FROM information_schema.columns
		WHERE table_name = 'vk_post' AND column_name = 'id'
	) AND NOT EXISTS (
		SELECT 1
		FROM information_schema.columns
		WHERE table_name = 'vk_post' AND column_name = 'post_id'
	) THEN
		EXECUTE 'ALTER TABLE vk_post RENAME COLUMN id TO post_id';
	END IF;
END $$;

ALTER TABLE vk_post
	ALTER COLUMN published_at SET NOT NULL,
	ALTER COLUMN published_at SET DEFAULT NOW();

DO $$
BEGIN
	IF to_regclass('published_posts') IS NULL THEN
		EXECUTE 'ALTER TABLE vk_post RENAME TO published_posts';
	END IF;
END $$;
