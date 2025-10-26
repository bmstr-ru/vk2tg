package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/rs/zerolog"
	zlog "github.com/rs/zerolog/log"
)

func main() {
	zlog.Logger = zerolog.New(os.Stdout).With().Timestamp().Logger()

	addrFlag := flag.String("addr", defaultAddr(), "HTTP listen address, e.g. :8080")
	indexFlag := flag.String("index", defaultIndexPath(), "Path to index.html to serve on GET /")
	flag.Parse()

	handler, err := newIndexHandler(*indexFlag)
	if err != nil {
		zlog.Fatal().Err(err).Msg("failed to prepare index handler")
	}

	ctx := context.Background()

	store, err := newStorage(ctx, zlog.Logger)
	if err != nil {
		zlog.Fatal().Err(err).Msg("failed to initialize storage")
	}
	defer store.Close()

	tokenMgr := newTokenManager(zlog.Logger, store)

	groupID := os.Getenv("VK_GROUP_ID")
	botToken := os.Getenv("TG_BOT_TOKEN")
	channelID := os.Getenv("TG_CHANNEL_ID")

	if groupID == "" || botToken == "" || channelID == "" {
		zlog.Warn().Msg("VK to Telegram sync disabled: missing VK_GROUP_ID, TG_BOT_TOKEN, or TG_CHANNEL_ID")
	} else {
		startWallSync(ctx, zlog.Logger, tokenMgr, store, wallSyncConfig{
			GroupID:   groupID,
			BotToken:  botToken,
			ChannelID: channelID,
		})
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/auth/success", authSuccessHandler(tokenMgr))
	mux.HandleFunc("/auth", authHandler)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		handler(w, r)
	})

	server := &http.Server{
		Addr:              *addrFlag,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	zlog.Info().
		Str("index_path", *indexFlag).
		Str("addr", server.Addr).
		Msg("serving index")
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		zlog.Fatal().Err(err).Msg("server error")
	}
}

func defaultAddr() string {
	if port := os.Getenv("PORT"); port != "" {
		return ":" + port
	}
	return ":8080"
}

func defaultIndexPath() string {
	if path := os.Getenv("INDEX_HTML_PATH"); path != "" {
		return path
	}
	return "index.html"
}

func newIndexHandler(path string) (func(http.ResponseWriter, *http.Request), error) {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve absolute path: %w", err)
	}
	content, err := os.ReadFile(absPath)
	if err != nil {
		return nil, fmt.Errorf("read index file: %w", err)
	}
	info, err := os.Stat(absPath)
	if err != nil {
		return nil, fmt.Errorf("stat index file: %w", err)
	}

	modTime := info.ModTime()
	mediaType := mime.TypeByExtension(filepath.Ext(absPath))
	if mediaType == "" {
		mediaType = "text/html; charset=utf-8"
	}

	contentLength := strconv.Itoa(len(content))

	handler := func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", fmt.Sprintf("%s, %s", http.MethodGet, http.MethodHead))
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", mediaType)
		w.Header().Set("Last-Modified", modTime.UTC().Format(http.TimeFormat))
		w.Header().Set("Content-Length", contentLength)
		if r.Method == http.MethodHead {
			return
		}
		if _, err := w.Write(content); err != nil {
			zlog.Error().Err(err).Msg("error writing index response")
		}
	}
	return handler, nil
}

func authHandler(w http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()

	body, err := io.ReadAll(r.Body)
	if err != nil {
		zlog.Error().Err(err).Msg("read request body failed")
		http.Error(w, fmt.Sprintf("read body: %v", err), http.StatusInternalServerError)
		return
	}

	payload := map[string]any{
		"url":     r.URL.String(),
		"headers": r.Header,
		"body":    string(body),
	}

	response, err := json.Marshal(payload)
	if err != nil {
		zlog.Error().Err(err).Msg("marshal auth payload failed")
		http.Error(w, fmt.Sprintf("marshal payload: %v", err), http.StatusInternalServerError)
		return
	}

	zlog.Info().
		RawJSON("payload", response).
		Msg("auth payload")

	w.Header().Set("Content-Type", "application/json")
	if _, err := w.Write(response); err != nil {
		zlog.Error().Err(err).Msg("write auth response failed")
	}
}

func authSuccessHandler(manager *tokenManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		defer r.Body.Close()

		var payload authSuccessPayload
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			zlog.Error().Err(err).Msg("decode auth success payload failed")
			http.Error(w, "invalid JSON payload", http.StatusBadRequest)
			return
		}

		if err := payload.validate(); err != nil {
			zlog.Error().Err(err).Msg("invalid auth success payload")
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		manager.Update(payload)
		w.WriteHeader(http.StatusAccepted)
	}
}
