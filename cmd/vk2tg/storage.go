package main

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/rs/zerolog"
)

//go:embed migrations/*.sql
var embeddedMigrations embed.FS

type dbConfig struct {
	Host     string
	Port     string
	Username string
	Password string
	Database string
	Schema   string
}

func (c dbConfig) dsn() (string, error) {
	if c.Host == "" || c.Port == "" || c.Username == "" || c.Password == "" || c.Database == "" {
		return "", errors.New("incomplete database configuration")
	}

	u := &url.URL{
		Scheme: "postgres",
		User:   url.UserPassword(c.Username, c.Password),
		Host:   fmt.Sprintf("%s:%s", c.Host, c.Port),
		Path:   "/" + c.Database,
	}
	q := u.Query()
	q.Set("sslmode", "disable")
	u.RawQuery = q.Encode()
	return u.String(), nil
}

func loadDBConfigFromEnv() (dbConfig, error) {
	cfg := dbConfig{
		Host:     os.Getenv("DB_HOST"),
		Port:     os.Getenv("DB_PORT"),
		Username: os.Getenv("DB_USERNAME"),
		Password: os.Getenv("DB_PASSWORD"),
		Database: os.Getenv("DB_DATABASE"),
		Schema:   os.Getenv("DB_SCHEMA"),
	}

	var missing []string
	if cfg.Host == "" {
		missing = append(missing, "DB_HOST")
	}
	if cfg.Port == "" {
		missing = append(missing, "DB_PORT")
	}
	if cfg.Username == "" {
		missing = append(missing, "DB_USERNAME")
	}
	if cfg.Password == "" {
		missing = append(missing, "DB_PASSWORD")
	}
	if cfg.Database == "" {
		missing = append(missing, "DB_DATABASE")
	}
	if cfg.Schema == "" {
		missing = append(missing, "DB_SCHEMA")
	}

	if len(missing) > 0 {
		return dbConfig{}, fmt.Errorf("missing required database env vars: %s", strings.Join(missing, ", "))
	}

	return cfg, nil
}

type storage struct {
	db      *sql.DB
	timeout time.Duration
}

func newStorage(ctx context.Context, logger zerolog.Logger) (*storage, error) {
	cfg, err := loadDBConfigFromEnv()
	if err != nil {
		return nil, err
	}

	dsn, err := cfg.dsn()
	if err != nil {
		return nil, err
	}

	baseCfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse postgres config: %w", err)
	}

	setupDB := stdlib.OpenDB(*baseCfg)
	defer setupDB.Close()

	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	if err := setupDB.PingContext(ctx); err != nil {
		return nil, fmt.Errorf("connect to postgres: %w", err)
	}

	createSchemaSQL := fmt.Sprintf("CREATE SCHEMA IF NOT EXISTS %s", quoteIdentifier(cfg.Schema))
	if _, err := setupDB.ExecContext(ctx, createSchemaSQL); err != nil {
		return nil, fmt.Errorf("ensure schema %s: %w", cfg.Schema, err)
	}

	baseCfg.RuntimeParams["search_path"] = cfg.Schema

	db := stdlib.OpenDB(*baseCfg)
	db.SetMaxIdleConns(4)
	db.SetMaxOpenConns(10)
	db.SetConnMaxLifetime(30 * time.Minute)

	migrateCtx, cancelMigrate := context.WithTimeout(ctx, 30*time.Second)
	defer cancelMigrate()

	goose.SetBaseFS(embeddedMigrations)
	if err := goose.SetDialect("postgres"); err != nil {
		db.Close()
		return nil, fmt.Errorf("configure migrations: %w", err)
	}

	if err := goose.UpContext(migrateCtx, db, "migrations"); err != nil {
		db.Close()
		return nil, fmt.Errorf("apply migrations: %w", err)
	}

	logger.Info().
		Str("schema", cfg.Schema).
		Str("database", cfg.Database).
		Msg("database migrations applied")

	return &storage{
		db:      db,
		timeout: 5 * time.Second,
	}, nil
}

func (s *storage) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

func (s *storage) withContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithTimeout(ctx, s.timeout)
}

type tokenRecord struct {
	payload   authSuccessPayload
	updatedAt time.Time
	expiresAt time.Time
}

func (s *storage) LoadTokenState(ctx context.Context) (*tokenRecord, error) {
	ctx, cancel := s.withContext(ctx)
	defer cancel()

	const query = `
		SELECT access_token, refresh_token, state, device_id, expires_in, updated_at, expires_at
		FROM auth_tokens
		WHERE id = 1
	`

	var (
		rec       tokenRecord
		expiresIn int
	)
	if err := s.db.QueryRowContext(ctx, query).Scan(
		&rec.payload.AccessToken,
		&rec.payload.RefreshToken,
		&rec.payload.State,
		&rec.payload.DeviceID,
		&expiresIn,
		&rec.updatedAt,
		&rec.expiresAt,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("query auth token: %w", err)
	}

	rec.payload.ExpiresIn = expiresIn
	return &rec, nil
}

func (s *storage) UpsertTokenState(ctx context.Context, payload authSuccessPayload, updatedAt, expiresAt time.Time) error {
	ctx, cancel := s.withContext(ctx)
	defer cancel()

	const query = `
		INSERT INTO auth_tokens (
			id, access_token, refresh_token, state, device_id, expires_in, updated_at, expires_at
		) VALUES (
			1, $1, $2, $3, $4, $5, $6, $7
		)
		ON CONFLICT (id) DO UPDATE
		SET access_token = EXCLUDED.access_token,
			refresh_token = EXCLUDED.refresh_token,
			state = EXCLUDED.state,
			device_id = EXCLUDED.device_id,
			expires_in = EXCLUDED.expires_in,
			updated_at = EXCLUDED.updated_at,
			expires_at = EXCLUDED.expires_at
	`

	if _, err := s.db.ExecContext(ctx, query,
		payload.AccessToken,
		payload.RefreshToken,
		payload.State,
		payload.DeviceID,
		payload.ExpiresIn,
		updatedAt.UTC(),
		expiresAt.UTC(),
	); err != nil {
		return fmt.Errorf("upsert auth token: %w", err)
	}
	return nil
}

func (s *storage) EnsureVKPost(ctx context.Context, ownerID, postID int, hash string) (bool, error) {
	ctx, cancel := s.withContext(ctx)
	defer cancel()

	const query = `
		INSERT INTO vk_post (owner_id, id, hash)
		VALUES ($1, $2, $3)
		ON CONFLICT (owner_id, id) DO UPDATE
		SET hash = CASE
			WHEN vk_post.hash = '' AND EXCLUDED.hash <> '' THEN EXCLUDED.hash
			ELSE vk_post.hash
		END
		RETURNING published_at
	`

	var publishedAt sql.NullTime
	if err := s.db.QueryRowContext(ctx, query, ownerID, postID, hash).Scan(&publishedAt); err != nil {
		return false, fmt.Errorf("ensure vk post: %w", err)
	}

	return publishedAt.Valid, nil
}

func (s *storage) RecordTelegramPost(ctx context.Context, ownerID, postID int, messageID int64, publishedAt time.Time) error {
	ctx, cancel := s.withContext(ctx)
	defer cancel()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	const insertTGPost = `
		INSERT INTO tg_post (vk_owner_id, vk_post_id, id, published_at)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (vk_owner_id, vk_post_id, id) DO NOTHING
	`
	if _, err = tx.ExecContext(ctx, insertTGPost, ownerID, postID, messageID, publishedAt.UTC()); err != nil {
		return fmt.Errorf("insert telegram post: %w", err)
	}

	const upsertVKPost = `
		INSERT INTO vk_post (owner_id, id, hash, published_at)
		VALUES ($1, $2, '', $3)
		ON CONFLICT (owner_id, id) DO UPDATE
		SET published_at = COALESCE(vk_post.published_at, EXCLUDED.published_at)
	`
	if _, err = tx.ExecContext(ctx, upsertVKPost, ownerID, postID, publishedAt.UTC()); err != nil {
		return fmt.Errorf("update vk post timestamp: %w", err)
	}

	if err = tx.Commit(); err != nil {
		return fmt.Errorf("commit telegram post tx: %w", err)
	}
	return nil
}

func quoteIdentifier(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}
