package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

func main() {
	addrFlag := flag.String("addr", defaultAddr(), "HTTP listen address, e.g. :8080")
	indexFlag := flag.String("index", defaultIndexPath(), "Path to index.html to serve on GET /")
	flag.Parse()

	handler, err := newIndexHandler(*indexFlag)
	if err != nil {
		log.Fatalf("failed to prepare index handler: %v", err)
	}

	mux := http.NewServeMux()
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

	log.Printf("Serving %s on %s", *indexFlag, server.Addr)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("server error: %v", err)
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
			log.Printf("error writing response: %v", err)
		}
	}
	return handler, nil
}

func authHandler(w http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()

	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, fmt.Sprintf("read body: %v", err), http.StatusInternalServerError)
		return
	}

	payload := map[string]any{
		"url":     r.URL.String(),
		"headers": r.Header,
		"body":    string(body),
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(payload); err != nil {
		log.Printf("error writing JSON response: %v", err)
	}
}
