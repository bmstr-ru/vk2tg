package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/rs/zerolog"
)

const (
	vkWallGetURL       = "https://api.vk.com/method/wall.get"
	vkAPIVersion       = "5.199"
	telegramSendURLFmt = "https://api.telegram.org/bot%s/sendMessage"
)

type wallSyncConfig struct {
	GroupID   string
	BotToken  string
	ChannelID string
}

func startWallSync(ctx context.Context, logger zerolog.Logger, manager *tokenManager, cfg wallSyncConfig) {
	logger.Info().
		Str("vk_group_id", cfg.GroupID).
		Msg("starting VK to Telegram sync worker")

	syncer := &wallSyncer{
		logger:      logger,
		manager:     manager,
		cfg:         cfg,
		httpClient:  &http.Client{Timeout: 10 * time.Second},
		publishedID: make(map[string]struct{}),
	}

	go syncer.run(ctx)
}

type wallSyncer struct {
	logger      zerolog.Logger
	manager     *tokenManager
	cfg         wallSyncConfig
	httpClient  *http.Client
	publishedID map[string]struct{}
}

func (s *wallSyncer) run(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	s.sync(ctx)

	for {
		select {
		case <-ctx.Done():
			s.logger.Info().Msg("VK to Telegram sync worker stopped")
			return
		case <-ticker.C:
			s.sync(ctx)
		}
	}
}

func (s *wallSyncer) sync(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	accessToken, err := s.manager.RequestAccessToken(ctx)
	if err != nil {
		s.logger.Error().Err(err).Msg("failed to get access token for sync")
		return
	}

	if accessToken == "" {
		s.logger.Debug().Msg("access token not yet available, skipping sync")
		return
	}

	posts, err := s.fetchVKPosts(ctx, accessToken)
	if err != nil {
		s.logger.Error().Err(err).Msg("failed to fetch posts from VK")
		return
	}

	if len(posts) == 0 {
		return
	}

	for i := len(posts) - 1; i >= 0; i-- {
		post := posts[i]
		if post.ID == 0 {
			continue
		}
		key := fmt.Sprintf("%d_%d", post.OwnerID, post.ID)
		if _, exists := s.publishedID[key]; exists {
			continue
		}

		text := strings.TrimSpace(post.Text)
		link := fmt.Sprintf("https://vk.com/wall-%s_%d", s.cfg.GroupID, post.ID)
		if text == "" {
			text = link
		} else {
			text = fmt.Sprintf("%s\n\n%s", text, link)
		}

		if err := s.publishToTelegram(ctx, text); err != nil {
			s.logger.Error().Err(err).Msg("failed to publish message to Telegram")
			continue
		}

		s.publishedID[key] = struct{}{}
	}
}

func (s *wallSyncer) fetchVKPosts(ctx context.Context, accessToken string) ([]vkPost, error) {
	params := url.Values{}
	params.Set("access_token", accessToken)
	params.Set("v", vkAPIVersion)
	params.Set("count", "20")
	params.Set("domain", "club232382073")

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("%s?%s", vkWallGetURL, params.Encode()), nil)
	if err != nil {
		return nil, fmt.Errorf("build VK request: %w", err)
	}

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("execute VK request: %w", err)
	}
	defer resp.Body.Close()

	var result vkWallResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode VK response: %w", err)
	}

	if result.Error.Code != 0 {
		return nil, fmt.Errorf("vk api error %d: %s", result.Error.Code, result.Error.Msg)
	}

	return result.Response.Items, nil
}

func (s *wallSyncer) publishToTelegram(ctx context.Context, text string) error {
	time.Sleep(5 * time.Second)
	params := url.Values{}
	params.Set("chat_id", s.cfg.ChannelID)
	params.Set("text", text)
	params.Set("disable_web_page_preview", "false")

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, fmt.Sprintf(telegramSendURLFmt, s.cfg.BotToken), strings.NewReader(params.Encode()))
	if err != nil {
		return fmt.Errorf("build Telegram request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("execute Telegram request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusBadRequest {
		return fmt.Errorf("telegram API returned %s", resp.Status)
	}

	return nil
}

type vkPost struct {
	ID      int    `json:"id"`
	OwnerID int    `json:"owner_id"`
	Text    string `json:"text"`
}

type vkWallResponse struct {
	Response struct {
		Items []vkPost `json:"items"`
	} `json:"response"`
	Error struct {
		Code int    `json:"error_code"`
		Msg  string `json:"error_msg"`
	} `json:"error"`
}
