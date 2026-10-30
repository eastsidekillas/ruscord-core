package blob

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
)

// Register serves stored files at GET /cdn/<key>. Files are public, like
// avatars elsewhere: their keys are hard to guess but not secret.
//
// Headers make a file inert even if a browser opens it directly: the type
// is taken from the key (the pipeline wrote it), never sniffed; the CSP
// sandbox stops it from running anything; immutable caching follows from
// content-addressed keys.
func Register(mux *http.ServeMux, store Store, log *slog.Logger) {
	mux.HandleFunc("GET /cdn/{key...}", func(w http.ResponseWriter, r *http.Request) {
		key := r.PathValue("key")
		if !ValidKey(key) {
			http.NotFound(w, r)
			return
		}
		body, err := store.Get(r.Context(), key)
		if errors.Is(err, ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		if err != nil {
			log.Error("blob: serve", "key", key, "err", err)
			http.Error(w, "storage unavailable", http.StatusBadGateway)
			return
		}
		h := w.Header()
		h.Set("Content-Type", ContentType(key[strings.LastIndexByte(key, '.')+1:]))
		h.Set("Content-Length", strconv.Itoa(len(body)))
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Content-Security-Policy", "default-src 'none'; sandbox")
		h.Set("Cross-Origin-Resource-Policy", "cross-origin")
		h.Set("Cache-Control", "public, max-age=31536000, immutable")
		_, _ = w.Write(body)
	})
}
