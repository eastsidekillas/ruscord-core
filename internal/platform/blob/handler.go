package blob

import (
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"strconv"
	"strings"
)

// Register serves stored files at GET /cdn/<key>. Avatars and icons are
// public: their keys are hard to guess but not secret. Attachments need a
// link signed by signer (docs/adr/0036-attachments.md).
//
// Headers make a file inert even if a browser opens it directly: the type
// is taken from the key (the server wrote it), never sniffed; the CSP
// sandbox stops it from running anything; anything that is not a picture or
// a video is a download. Immutable caching follows from keys never being
// reused. Ranges are served for seeking in videos.
func Register(mux *http.ServeMux, store Store, signer *Signer, log *slog.Logger) {
	mux.HandleFunc("GET /cdn/{key...}", func(w http.ResponseWriter, r *http.Request) {
		key := r.PathValue("key")
		attachment := strings.HasPrefix(key, KindAttachment+"/")
		if !ValidKey(key) || (attachment && !signer.Valid(key, r.URL.Query())) {
			http.NotFound(w, r)
			return
		}
		obj, err := store.Open(r.Context(), key, r.Header.Get("Range"))
		switch {
		case errors.Is(err, ErrNotFound):
			http.NotFound(w, r)
			return
		case errors.Is(err, ErrRange):
			http.Error(w, "range not satisfiable", http.StatusRequestedRangeNotSatisfiable)
			return
		case err != nil:
			log.Error("blob: serve", "key", key, "err", err)
			http.Error(w, "storage unavailable", http.StatusBadGateway)
			return
		}
		defer func() { _ = obj.Body.Close() }()
		ext := Ext(key)
		h := w.Header()
		h.Set("Content-Type", ContentType(ext))
		h.Set("Content-Length", strconv.FormatInt(obj.Size, 10))
		h.Set("Accept-Ranges", "bytes")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Content-Security-Policy", "default-src 'none'; sandbox")
		h.Set("Cross-Origin-Resource-Policy", "cross-origin")
		if attachment {
			// The link expires, so caches may keep it until then.
			h.Set("Cache-Control", "private, max-age=86400, immutable")
			disposition := "inline"
			if ext == ExtBin {
				disposition = "attachment"
			}
			// FormatMediaType gives "" for a name it cannot encode; then the
			// browser picks a name itself.
			if d := mime.FormatMediaType(disposition, map[string]string{"filename": r.URL.Query().Get("name")}); d != "" {
				disposition = d
			}
			h.Set("Content-Disposition", disposition)
		} else {
			h.Set("Cache-Control", "public, max-age=31536000, immutable")
		}
		status := http.StatusOK
		if obj.Range != "" {
			h.Set("Content-Range", obj.Range)
			status = http.StatusPartialContent
		}
		w.WriteHeader(status)
		_, _ = io.Copy(w, obj.Body) //nolint:gosec // G705: served inert (fixed type, nosniff, sandbox CSP)
	})
}
