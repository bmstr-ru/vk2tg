package main

import (
	"flag"
	"fmt"
	"log"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

func main() {
	addrFlag := flag.String("addr", defaultAddr(), "HTTP listen address, e.g. :8080")
	indexFlag := flag.String("index", defaultIndexPath(), "Path to index.html to serve on GET /")
	flag.Parse()

	handler, modTime, err := newIndexHandler(*indexFlag)
	if err != nil {
		log.Fatalf("failed to prepare index handler: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		handler(w, r, modTime)
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

func newIndexHandler(path string) (func(http.ResponseWriter, *http.Request, time.Time), time.Time, error) {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("resolve absolute path: %w", err)
	}
	content, err := os.ReadFile(absPath)
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("read index file: %w", err)
	}
	info, err := os.Stat(absPath)
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("stat index file: %w", err)
	}

	modTime := info.ModTime()
	mediaType := mime.TypeByExtension(filepath.Ext(absPath))
	if mediaType == "" {
		mediaType = "text/html; charset=utf-8"
	}

	return func(w http.ResponseWriter, r *http.Request, mod time.Time) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
	}
		w.Header().Set("Content-Type", mediaType)
		http.ServeContent(w, r, filepath.Base(absPath), mod, bytesReader(content))
	}, modTime, nil
}

func bytesReader(b []byte) *byteReader {
	return &byteReader{data: b}
}

type byteReader struct {
	data []byte
	pos  int64
}

func (r *byteReader) Read(p []byte) (int, error) {
	if r.pos >= int64(len(r.data)) {
		return 0, io.EOF
	}
	n := copy(p, r.data[r.pos:])
	r.pos += int64(n)
	return n, nil
}

func (r *byteReader) Seek(offset int64, whence int) (int64, error) {
	var newPos int64
	switch whence {
	case io.SeekStart:
		newPos = offset
	case io.SeekCurrent:
		newPos = r.pos + offset
	case io.SeekEnd:
		newPos = int64(len(r.data)) + offset
	default:
		return 0, fmt.Errorf("invalid whence %d", whence)
	}
	if newPos < 0 {
		return 0, fmt.Errorf("negative position")
	}
	r.pos = newPos
	return newPos, nil
}
