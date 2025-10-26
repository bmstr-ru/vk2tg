-- +goose Up
CREATE TABLE IF NOT EXISTS published_posts (
    owner_id    BIGINT       NOT NULL,
    post_id     BIGINT       NOT NULL,
    published_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (owner_id, post_id)
);

CREATE TABLE IF NOT EXISTS auth_tokens (
    id            SMALLINT     PRIMARY KEY,
    access_token  TEXT         NOT NULL,
    refresh_token TEXT         NOT NULL,
    state         TEXT         NOT NULL DEFAULT '',
    device_id     TEXT         NOT NULL,
    expires_in    INTEGER      NOT NULL CHECK (expires_in >= 0),
    updated_at    TIMESTAMPTZ  NOT NULL,
    expires_at    TIMESTAMPTZ  NOT NULL,
    CHECK (id = 1)
);

-- +goose Down
DROP TABLE IF EXISTS auth_tokens;
DROP TABLE IF EXISTS published_posts;
