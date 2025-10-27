package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/rs/zerolog"
)

const (
	vkWallGetURL            = "https://api.vk.com/method/wall.get"
	vkAPIVersion            = "5.199"
	telegramSendURLFmt      = "https://api.telegram.org/bot%s/sendMessage"
	telegramSendPhotoURLFmt = "https://api.telegram.org/bot%s/sendPhoto"
)

type wallSyncConfig struct {
	GroupID   string
	BotToken  string
	ChannelID string
	ThreadID  string
}

func startWallSync(ctx context.Context, logger zerolog.Logger, manager *tokenManager, store *storage, cfg wallSyncConfig) {
	logger.Info().
		Str("vk_group_id", cfg.GroupID).
		Msg("starting VK to Telegram sync worker")

	syncer := &wallSyncer{
		logger:     logger,
		manager:    manager,
		store:      store,
		cfg:        cfg,
		httpClient: &http.Client{Timeout: 10 * time.Second},
	}

	go syncer.run(ctx)
}

type wallSyncer struct {
	logger     zerolog.Logger
	manager    *tokenManager
	store      *storage
	cfg        wallSyncConfig
	httpClient *http.Client
}

func (s *wallSyncer) run(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()

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
		s.logger.Error().Err(err).Stack().Msg("failed to get access token for sync")
		return
	}

	if accessToken == "" {
		s.logger.Debug().Msg("access token not yet available, skipping sync")
		return
	}

	posts, err := s.fetchVKPosts(ctx, accessToken)
	if err != nil {
		s.logger.Error().Err(err).Stack().Msg("failed to fetch posts from VK")
		return
	}

	if len(posts) == 0 {
		s.logger.Info().Msg("no posts received from VK")
		return
	}

	sort.Slice(posts, func(i, j int) bool {
		return posts[i].ID < posts[j].ID
	})

	for _, post := range posts {
		if post.ID == 0 {
			continue
		}

		published, err := s.store.IsPostPublished(ctx, post.OwnerID, post.ID)
		if err != nil {
			s.logger.Error().
				Err(err).
				Stack().
				Int("owner_id", post.OwnerID).
				Int("post_id", post.ID).
				Msg("failed to check published status")
			continue
		}
		if published {
			s.logger.Info().Int("postId", post.ID).Msg("post already published")
			continue
		}

		text := strings.TrimSpace(post.Text)
		link := fmt.Sprintf("https://vk.com/wall-%s_%d", s.cfg.GroupID, post.ID)
		if text == "" {
			text = link
		} else {
			text = fmt.Sprintf("%s\n\n%s", text, link)
		}

		if photoURL, ok := singlePhotoAttachmentURL(post); ok && len(text) < 1024 {
			if err := s.publishPhotoToTelegram(ctx, photoURL, text); err != nil {
				s.logger.Error().Err(err).Stack().Msg("failed to publish photo to Telegram")
				continue
			}
		} else if err := s.publishTextToTelegram(ctx, text); err != nil {
			s.logger.Error().Err(err).Stack().Msg("failed to publish message to Telegram")
			continue
		}

		if err := s.store.MarkPostPublished(ctx, post.OwnerID, post.ID); err != nil {
			s.logger.Error().
				Err(err).
				Stack().
				Int("owner_id", post.OwnerID).
				Int("post_id", post.ID).
				Msg("failed to mark post as published")
		}
	}
}

func (s *wallSyncer) fetchVKPosts(ctx context.Context, accessToken string) ([]vkPost, error) {
	params := url.Values{}
	params.Set("access_token", accessToken)
	params.Set("v", vkAPIVersion)
	params.Set("count", "20")
	params.Set("domain", "club"+s.cfg.GroupID)

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

func (s *wallSyncer) publishTextToTelegram(ctx context.Context, text string) error {
	time.Sleep(5 * time.Second)
	params := url.Values{}
	params.Set("chat_id", s.cfg.ChannelID)
	params.Set("text", text)
	params.Set("disable_web_page_preview", "false")
	if s.cfg.ThreadID != "" {
		params.Set("message_thread_id", s.cfg.ThreadID)
	}

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

func (s *wallSyncer) publishPhotoToTelegram(ctx context.Context, photoURL, caption string) error {
	time.Sleep(5 * time.Second)
	params := url.Values{}
	params.Set("chat_id", s.cfg.ChannelID)
	params.Set("photo", photoURL)
	if caption != "" {
		params.Set("caption", caption)
	}
	if s.cfg.ThreadID != "" {
		params.Set("message_thread_id", s.cfg.ThreadID)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, fmt.Sprintf(telegramSendPhotoURLFmt, s.cfg.BotToken), strings.NewReader(params.Encode()))
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
		bodyBytes, err := io.ReadAll(resp.Body)
		if err != nil {
			return fmt.Errorf("error reading response body: %v", err)
		}
		return fmt.Errorf("telegram API returned %s: %s", resp.Status, string(bodyBytes))
	}

	return nil
}

type vkPost struct {
	ID          int            `json:"id"`
	OwnerID     int            `json:"owner_id"`
	Text        string         `json:"text"`
	Attachments []vkAttachment `json:"attachments"`
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

type vkAttachment struct {
	Type  string   `json:"type"`
	Photo *vkPhoto `json:"photo"`
}

type vkPhoto struct {
	Sizes []vkPhotoSize `json:"sizes"`
}

type vkPhotoSize struct {
	URL    string `json:"url"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
	Type   string `json:"type"`
}

func singlePhotoAttachmentURL(post vkPost) (string, bool) {
	if len(post.Attachments) != 1 {
		return "", false
	}

	att := post.Attachments[0]
	if att.Type != "photo" || att.Photo == nil {
		return "", false
	}

	return selectLargestPhotoURL(att.Photo.Sizes)
}

func selectLargestPhotoURL(sizes []vkPhotoSize) (string, bool) {
	if len(sizes) == 0 {
		return "", false
	}

	best := sizes[0]
	bestArea := best.Width * best.Height

	for _, size := range sizes[1:] {
		area := size.Width * size.Height
		if area > bestArea {
			best = size
			bestArea = area
		}
	}

	if best.URL == "" {
		return "", false
	}

	return best.URL, true
}
