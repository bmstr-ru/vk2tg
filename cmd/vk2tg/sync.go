package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/rs/zerolog"
)

const (
	vkWallGetURL                 = "https://api.vk.com/method/wall.get"
	vkAPIVersion                 = "5.199"
	telegramSendURLFmt           = "https://api.telegram.org/bot%s/sendMessage"
	telegramSendPhotoURLFmt      = "https://api.telegram.org/bot%s/sendPhoto"
	telegramSendMediaGroupURLFmt = "https://api.telegram.org/bot%s/sendMediaGroup"
	telegramEditTextURLFmt       = "https://api.telegram.org/bot%s/editMessageText"
	telegramEditCaptionURLFmt    = "https://api.telegram.org/bot%s/editMessageCaption"
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

		postText := strings.TrimSpace(post.Text)

		state, err := s.store.EnsureVKPost(ctx, post.OwnerID, post.ID, post.Hash, postText)
		if err != nil {
			s.logger.Error().
				Err(err).
				Stack().
				Int("owner_id", post.OwnerID).
				Int("post_id", post.ID).
				Msg("failed to check published status")
			continue
		}

		text := postText
		link := fmt.Sprintf("https://vk.com/wall-%s_%d", s.cfg.GroupID, post.ID)
		if text == "" {
			text = link
		} else {
			text = fmt.Sprintf("%s\n\n%s", text, link)
		}

		if state.Published {
			if state.Hash == post.Hash {
				s.logger.Info().
					Int("postId", post.ID).
					Msg("post already published and hash unchanged")
				continue
			}

			updated, err := s.updateTelegramPostContent(ctx, post, text)
			if err != nil {
				s.logger.Error().
					Err(err).
					Stack().
					Int("owner_id", post.OwnerID).
					Int("post_id", post.ID).
					Msg("failed to update Telegram post content")
				continue
			}
			if !updated {
				s.logger.Warn().
					Int("owner_id", post.OwnerID).
					Int("post_id", post.ID).
					Msg("skipped Telegram post update after edit failure")
				continue
			}

			if err := s.store.UpdateVKPostAfterEdit(ctx, post.OwnerID, post.ID, post.Hash, postText); err != nil {
				s.logger.Error().
					Err(err).
					Stack().
					Int("owner_id", post.OwnerID).
					Int("post_id", post.ID).
					Msg("failed to persist updated VK post hash")
			}
			continue
		}

		messages, err := s.publishPost(ctx, post, text)
		if err != nil {
			s.logger.Error().
				Err(err).
				Stack().
				Int("owner_id", post.OwnerID).
				Int("post_id", post.ID).
				Msg("failed to publish post to Telegram")
			continue
		}

		for _, msg := range messages {
			if err := s.store.RecordTelegramPost(ctx, post.OwnerID, post.ID, msg.ID, s.cfg.ChannelID, msg.Text, msg.PublishedAt); err != nil {
				s.logger.Error().
					Err(err).
					Stack().
					Int("owner_id", post.OwnerID).
					Int("post_id", post.ID).
					Int64("telegram_message_id", msg.ID).
					Msg("failed to record Telegram post")
			}
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

func (s *wallSyncer) publishPost(ctx context.Context, post vkPost, text string) ([]telegramMessage, error) {
	photoURLs := photoAttachmentURLs(post)
	textLen := utf8.RuneCountInString(text)

	var messages []telegramMessage

	switch len(photoURLs) {
	case 0:
		msg, err := s.publishTextToTelegram(ctx, text)
		if err != nil {
			return nil, err
		}
		messages = append(messages, msg)
	case 1:
		photoURL := photoURLs[0]
		if textLen < 1024 {
			msg, err := s.publishPhotoToTelegram(ctx, photoURL, text)
			if err != nil {
				return nil, err
			}
			messages = append(messages, msg)
		} else {
			msg, err := s.publishPhotoToTelegram(ctx, photoURL, "")
			if err != nil {
				return nil, err
			}
			messages = append(messages, msg)

			msg, err = s.publishTextToTelegram(ctx, text)
			if err != nil {
				return nil, err
			}
			messages = append(messages, msg)
		}
	default:
		var (
			groupMessages []telegramMessage
			err           error
		)
		if textLen < 1024 {
			groupMessages, err = s.publishMediaGroupToTelegram(ctx, photoURLs, text)
		} else {
			groupMessages, err = s.publishMediaGroupToTelegram(ctx, photoURLs, "")
		}
		if err != nil {
			return nil, err
		}
		messages = append(messages, groupMessages...)

		if textLen >= 1024 {
			msg, err := s.publishTextToTelegram(ctx, text)
			if err != nil {
				return nil, err
			}
			messages = append(messages, msg)
		}
	}

	return messages, nil
}

func (s *wallSyncer) updateTelegramPostContent(ctx context.Context, post vkPost, text string) (bool, error) {
	rec, err := s.store.LatestTelegramPost(ctx, post.OwnerID, post.ID)
	if err != nil {
		return false, fmt.Errorf("lookup latest Telegram post: %w", err)
	}
	if rec == nil {
		return false, fmt.Errorf("no Telegram messages recorded for vk post %d", post.ID)
	}

	chatID := rec.ChannelID
	if chatID == "" {
		chatID = s.cfg.ChannelID
	}
	if chatID == "" {
		return false, fmt.Errorf("missing Telegram channel ID for vk post %d", post.ID)
	}

	edited, err := s.tryEditTelegramMessage(ctx, chatID, rec.MessageID, text)
	if err != nil {
		return false, err
	}
	if !edited {
		return false, nil
	}

	if err := s.store.UpdateTelegramPostText(ctx, post.OwnerID, post.ID, rec.MessageID, text); err != nil {
		return false, fmt.Errorf("update stored Telegram post text: %w", err)
	}
	return true, nil
}

func (s *wallSyncer) tryEditTelegramMessage(ctx context.Context, chatID string, messageID int64, text string) (bool, error) {
	if _, err := s.editTelegramMessageText(ctx, chatID, messageID, text); err == nil {
		return true, nil
	} else if !isTelegramBadRequest(err) {
		return false, err
	}

	if _, err := s.editTelegramMessageCaption(ctx, chatID, messageID, text); err == nil {
		return true, nil
	} else if isTelegramBadRequest(err) {
		return false, nil
	} else {
		return false, err
	}
}

func (s *wallSyncer) publishTextToTelegram(ctx context.Context, text string) (telegramMessage, error) {
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
		return telegramMessage{}, fmt.Errorf("build Telegram request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return telegramMessage{}, fmt.Errorf("execute Telegram request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return telegramMessage{}, fmt.Errorf("read Telegram response: %w", err)
	}

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusBadRequest {
		return telegramMessage{}, fmt.Errorf("telegram API returned %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}

	msg, err := parseTelegramSendResponse(body)
	if err != nil {
		return telegramMessage{}, err
	}
	msg.Text = text
	return msg, nil
}

func (s *wallSyncer) publishPhotoToTelegram(ctx context.Context, photoURL, caption string) (telegramMessage, error) {
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
		return telegramMessage{}, fmt.Errorf("build Telegram request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return telegramMessage{}, fmt.Errorf("execute Telegram request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return telegramMessage{}, fmt.Errorf("read Telegram response: %w", err)
	}

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusBadRequest {
		return telegramMessage{}, fmt.Errorf("telegram API returned %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}

	msg, err := parseTelegramSendResponse(body)
	if err != nil {
		return telegramMessage{}, err
	}
	msg.Text = caption
	return msg, nil
}

func (s *wallSyncer) publishMediaGroupToTelegram(ctx context.Context, photoURLs []string, caption string) ([]telegramMessage, error) {
	time.Sleep(5 * time.Second)

	media := make([]telegramInputMediaPhoto, 0, len(photoURLs))
	for idx, url := range photoURLs {
		item := telegramInputMediaPhoto{
			Type:  "photo",
			Media: url,
		}
		if idx == 0 && caption != "" {
			item.Caption = caption
		}
		media = append(media, item)
	}

	if len(media) == 0 {
		return nil, fmt.Errorf("sendMediaGroup requires at least one media item")
	}

	mediaPayload, err := json.Marshal(media)
	if err != nil {
		return nil, fmt.Errorf("encode media group payload: %w", err)
	}

	params := url.Values{}
	params.Set("chat_id", s.cfg.ChannelID)
	params.Set("media", string(mediaPayload))
	if s.cfg.ThreadID != "" {
		params.Set("message_thread_id", s.cfg.ThreadID)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, fmt.Sprintf(telegramSendMediaGroupURLFmt, s.cfg.BotToken), strings.NewReader(params.Encode()))
	if err != nil {
		return nil, fmt.Errorf("build Telegram media group request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("execute Telegram media group request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read Telegram media group response: %w", err)
	}

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusBadRequest {
		return nil, fmt.Errorf("telegram API returned %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}

	msgs, err := parseTelegramSendMediaGroupResponse(body)
	if err != nil {
		return nil, err
	}
	if caption != "" && len(msgs) > 0 {
		msgs[0].Text = caption
	}
	return msgs, nil
}

func (s *wallSyncer) editTelegramMessageText(ctx context.Context, chatID string, messageID int64, text string) (telegramMessage, error) {
	params := url.Values{}
	params.Set("chat_id", chatID)
	params.Set("message_id", fmt.Sprintf("%d", messageID))
	params.Set("text", text)
	params.Set("disable_web_page_preview", "false")
	if s.cfg.ThreadID != "" {
		params.Set("message_thread_id", s.cfg.ThreadID)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, fmt.Sprintf(telegramEditTextURLFmt, s.cfg.BotToken), strings.NewReader(params.Encode()))
	if err != nil {
		return telegramMessage{}, fmt.Errorf("build Telegram edit text request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return telegramMessage{}, fmt.Errorf("execute Telegram edit text request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return telegramMessage{}, fmt.Errorf("read Telegram edit text response: %w", err)
	}

	if resp.StatusCode == http.StatusBadRequest {
		return telegramMessage{}, &telegramAPIError{Code: http.StatusBadRequest, Description: strings.TrimSpace(string(body))}
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusBadRequest {
		return telegramMessage{}, fmt.Errorf("telegram API returned %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}

	msg, err := parseTelegramSendResponse(body)
	if err != nil {
		return telegramMessage{}, err
	}
	msg.Text = text
	return msg, nil
}

func (s *wallSyncer) editTelegramMessageCaption(ctx context.Context, chatID string, messageID int64, caption string) (telegramMessage, error) {
	params := url.Values{}
	params.Set("chat_id", chatID)
	params.Set("message_id", fmt.Sprintf("%d", messageID))
	params.Set("caption", caption)
	if s.cfg.ThreadID != "" {
		params.Set("message_thread_id", s.cfg.ThreadID)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, fmt.Sprintf(telegramEditCaptionURLFmt, s.cfg.BotToken), strings.NewReader(params.Encode()))
	if err != nil {
		return telegramMessage{}, fmt.Errorf("build Telegram edit caption request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return telegramMessage{}, fmt.Errorf("execute Telegram edit caption request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return telegramMessage{}, fmt.Errorf("read Telegram edit caption response: %w", err)
	}

	if resp.StatusCode == http.StatusBadRequest {
		return telegramMessage{}, &telegramAPIError{Code: http.StatusBadRequest, Description: strings.TrimSpace(string(body))}
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusBadRequest {
		return telegramMessage{}, fmt.Errorf("telegram API returned %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}

	msg, err := parseTelegramSendResponse(body)
	if err != nil {
		return telegramMessage{}, err
	}
	msg.Text = caption
	return msg, nil
}

func isTelegramBadRequest(err error) bool {
	var apiErr *telegramAPIError
	if errors.As(err, &apiErr) {
		return apiErr.Code == http.StatusBadRequest
	}
	return false
}

type vkPost struct {
	ID          int            `json:"id"`
	OwnerID     int            `json:"owner_id"`
	Text        string         `json:"text"`
	Hash        string         `json:"hash"`
	Attachments []vkAttachment `json:"attachments"`
}

type telegramMessagePayload struct {
	MessageID int64 `json:"message_id"`
	Date      int64 `json:"date"`
}

type telegramMessage struct {
	ID          int64
	Text        string
	PublishedAt time.Time
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

type telegramResponseEnvelope struct {
	OK          bool            `json:"ok"`
	Result      json.RawMessage `json:"result"`
	Description string          `json:"description"`
	ErrorCode   int             `json:"error_code"`
}

type telegramInputMediaPhoto struct {
	Type    string `json:"type"`
	Media   string `json:"media"`
	Caption string `json:"caption,omitempty"`
}

type telegramAPIError struct {
	Code        int
	Description string
}

func (e *telegramAPIError) Error() string {
	if e == nil {
		return ""
	}
	if e.Description == "" {
		return fmt.Sprintf("telegram API error %d", e.Code)
	}
	return fmt.Sprintf("telegram API error %d: %s", e.Code, e.Description)
}

func parseTelegramSendResponse(body []byte) (telegramMessage, error) {
	env, err := parseTelegramResponseEnvelope(body)
	if err != nil {
		return telegramMessage{}, err
	}

	var payload telegramMessagePayload
	if err := json.Unmarshal(env.Result, &payload); err != nil {
		return telegramMessage{}, fmt.Errorf("decode Telegram message: %w", err)
	}

	return telegramMessageFromPayload(payload)
}

func parseTelegramSendMediaGroupResponse(body []byte) ([]telegramMessage, error) {
	env, err := parseTelegramResponseEnvelope(body)
	if err != nil {
		return nil, err
	}

	var payloads []telegramMessagePayload
	if err := json.Unmarshal(env.Result, &payloads); err != nil {
		return nil, fmt.Errorf("decode Telegram media group: %w", err)
	}

	if len(payloads) == 0 {
		return nil, fmt.Errorf("telegram media group response missing messages")
	}

	messages := make([]telegramMessage, 0, len(payloads))
	for _, payload := range payloads {
		msg, err := telegramMessageFromPayload(payload)
		if err != nil {
			return nil, err
		}
		messages = append(messages, msg)
	}
	return messages, nil
}

func parseTelegramResponseEnvelope(body []byte) (telegramResponseEnvelope, error) {
	var env telegramResponseEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		return telegramResponseEnvelope{}, fmt.Errorf("decode Telegram envelope: %w", err)
	}

	if !env.OK {
		desc := env.Description
		if desc == "" {
			desc = strings.TrimSpace(string(body))
		}
		return telegramResponseEnvelope{}, &telegramAPIError{
			Code:        env.ErrorCode,
			Description: desc,
		}
	}
	if len(env.Result) == 0 {
		return telegramResponseEnvelope{}, fmt.Errorf("telegram API response missing result payload")
	}

	return env, nil
}

func telegramMessageFromPayload(payload telegramMessagePayload) (telegramMessage, error) {
	if payload.MessageID == 0 {
		return telegramMessage{}, fmt.Errorf("telegram API response missing message_id")
	}

	publishedAt := time.Unix(payload.Date, 0).UTC()
	if payload.Date == 0 {
		publishedAt = time.Now().UTC()
	}

	return telegramMessage{
		ID:          payload.MessageID,
		PublishedAt: publishedAt,
	}, nil
}

func photoAttachmentURLs(post vkPost) []string {
	urls := make([]string, 0, len(post.Attachments))
	for _, att := range post.Attachments {
		if att.Type != "photo" || att.Photo == nil {
			continue
		}
		if url, ok := selectLargestPhotoURL(att.Photo.Sizes); ok {
			urls = append(urls, url)
		}
	}
	return urls
}
