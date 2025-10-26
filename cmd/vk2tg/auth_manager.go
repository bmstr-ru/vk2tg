package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/rs/zerolog"
)

type authSuccessPayload struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	State        string `json:"state"`
	DeviceID     string `json:"device_id"`
	ExpiresIn    int    `json:"expires_in"`
}

const (
	vkRefreshURL   = "https://id.vk.ru/oauth2/auth"
	vkClientID     = "54260965"
	maxErrorBodyKB = 4
)

func (p authSuccessPayload) validate() error {
	if p.DeviceID == "" {
		return errors.New("device_id is required")
	}
	if p.AccessToken == "" {
		return errors.New("access_token is required")
	}
	if p.RefreshToken == "" {
		return errors.New("refresh_token is required")
	}
	if p.ExpiresIn <= 0 {
		return errors.New("expires_in must be a positive integer")
	}
	return nil
}

type tokenState struct {
	payload   authSuccessPayload
	updatedAt time.Time
	expiresAt time.Time
	lifetime  time.Duration
}

type tokenManager struct {
	logger     zerolog.Logger
	updateCh   chan authSuccessPayload
	requestCh  chan chan string
	httpClient *http.Client
	store      *storage
}

func newTokenManager(logger zerolog.Logger, store *storage) *tokenManager {
	if store == nil {
		panic("tokenManager requires non-nil storage")
	}
	m := &tokenManager{
		logger:    logger,
		updateCh:  make(chan authSuccessPayload),
		requestCh: make(chan chan string),
		store:     store,
		httpClient: &http.Client{
			Timeout: 10 * time.Second,
		},
	}
	go m.run()
	return m
}

func (m *tokenManager) Update(payload authSuccessPayload) {
	m.updateCh <- payload
}

func (m *tokenManager) AccessTokenRequests() chan<- chan string {
	return m.requestCh
}

func (m *tokenManager) RequestAccessToken(ctx context.Context) (string, error) {
	reply := make(chan string, 1)
	select {
	case m.requestCh <- reply:
	case <-ctx.Done():
		return "", ctx.Err()
	}

	select {
	case token := <-reply:
		return token, nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func (m *tokenManager) run() {
	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()

	state := m.loadInitialState()

	for {
		select {
		case payload := <-m.updateCh:
			newState, err := m.persistPayload(payload)
			if err != nil {
				m.logger.Error().
					Err(err).
					Msg("failed to persist auth success payload")
				continue
			}
			state = newState
			m.logger.Info().
				Dur("lifetime", newState.lifetime).
				Msg("received auth success payload")

		case reply := <-m.requestCh:
			token := ""
			if state != nil && state.payload.AccessToken != "" && time.Now().Before(state.expiresAt) {
				token = state.payload.AccessToken
			}
			reply <- token

		case <-ticker.C:
			if state == nil {
				m.logger.Info().
					Msg("state is null")
				continue
			}
			if state.payload.AccessToken == "" || state.payload.RefreshToken == "" {
				m.logger.Info().
					Msg("access or refresh token is empty")
				continue
			}
			eligible := state.lifetime <= 0
			if !eligible {
				remaining := time.Until(state.expiresAt)
				if remaining < 0 {
					remaining = 0
				}
				if state.lifetime > 0 {
					fraction := remaining.Seconds() / state.lifetime.Seconds()
					if fraction <= 0.15 {
						eligible = true
					}
				}
			}
			if !eligible {
				m.logger.Info().
					Msg("token is not eligible for refresh yet")
				continue
			}

			m.logger.Info().
				Msg("refresh token triggered")

			refreshed, err := m.refreshToken(state.payload)
			if err != nil {
				m.logger.Error().
					Err(err).
					Msg("token refresh failed")
				continue
			}

			newState, err := m.persistPayload(refreshed)
			if err != nil {
				m.logger.Error().
					Err(err).
					Msg("failed to persist refreshed token")
				continue
			}
			state = newState

			m.logger.Info().
				Dur("lifetime", newState.lifetime).
				Msg("token refresh succeeded")
		}
	}
}

func (m *tokenManager) loadInitialState() *tokenState {
	record, err := m.store.LoadTokenState(context.Background())
	if err != nil {
		m.logger.Error().
			Err(err).
			Msg("failed to load auth tokens from storage")
		return nil
	}
	if record == nil {
		return nil
	}

	lifetime := record.expiresAt.Sub(record.updatedAt)
	if lifetime < 0 {
		lifetime = 0
	}

	m.logger.Info().
		Dur("lifetime", lifetime).
		Msg("restored auth tokens from storage")

	return &tokenState{
		payload:   record.payload,
		updatedAt: record.updatedAt,
		expiresAt: record.expiresAt,
		lifetime:  lifetime,
	}
}

func (m *tokenManager) persistPayload(payload authSuccessPayload) (*tokenState, error) {
	now := time.Now()
	lifetime := time.Duration(payload.ExpiresIn) * time.Second
	if lifetime < 0 {
		lifetime = 0
	}
	expiresAt := now.Add(lifetime)

	if err := m.store.UpsertTokenState(context.Background(), payload, now, expiresAt); err != nil {
		return nil, err
	}

	return &tokenState{
		payload:   payload,
		updatedAt: now,
		expiresAt: expiresAt,
		lifetime:  lifetime,
	}, nil
}

func (m *tokenManager) refreshToken(payload authSuccessPayload) (authSuccessPayload, error) {
	if payload.RefreshToken == "" {
		return authSuccessPayload{}, errors.New("refresh_token is empty")
	}

	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", payload.RefreshToken)
	form.Set("client_id", vkClientID)
	if payload.DeviceID != "" {
		form.Set("device_id", payload.DeviceID)
	}
	if payload.State != "" {
		form.Set("state", payload.State)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, vkRefreshURL, strings.NewReader(form.Encode()))
	if err != nil {
		return authSuccessPayload{}, fmt.Errorf("build refresh request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := m.httpClient.Do(req)
	if err != nil {
		return authSuccessPayload{}, fmt.Errorf("execute refresh request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusBadRequest {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyKB*1024))
		return authSuccessPayload{}, fmt.Errorf("refresh request failed with %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}

	var refreshed authSuccessPayload
	if err := json.NewDecoder(resp.Body).Decode(&refreshed); err != nil {
		return authSuccessPayload{}, fmt.Errorf("decode refresh response: %w", err)
	}

	if refreshed.DeviceID == "" {
		refreshed.DeviceID = payload.DeviceID
	}
	if refreshed.State == "" {
		refreshed.State = payload.State
	}
	if refreshed.RefreshToken == "" {
		refreshed.RefreshToken = payload.RefreshToken
	}

	if err := refreshed.validate(); err != nil {
		return authSuccessPayload{}, fmt.Errorf("invalid refresh response: %w", err)
	}
	return refreshed, nil
}
