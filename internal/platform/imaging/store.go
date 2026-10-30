package imaging

import (
	"context"
	"errors"
	"net/http"

	"github.com/google/uuid"

	"github.com/eastsidekillas/ruscord-core/internal/platform/blob"
	"github.com/eastsidekillas/ruscord-core/internal/platform/httpx"
)

var (
	errNoFiles  = httpx.NewError(http.StatusServiceUnavailable, "files_unavailable", "Загрузка файлов сейчас недоступна")
	errBadImage = httpx.NewError(http.StatusUnprocessableEntity, "invalid_image", "Нужна картинка PNG, JPEG, WebP или GIF")
	errBigImage = httpx.NewError(http.StatusUnprocessableEntity, "image_too_large", "Картинка больше 4096 пикселей по стороне")
)

// Store runs an upload through Square and stores the result under a
// content-addressed key of kind and owner, returning the key. Refusals are
// API errors (422) that explain what was wrong with the picture; without
// files (storage not configured) uploads answer 503.
func Store(ctx context.Context, files *blob.Collector, kind string, owner uuid.UUID, upload []byte) (string, error) {
	if files == nil {
		return "", errNoFiles
	}
	img, ext, err := Square(ctx, upload)
	switch {
	case errors.Is(err, ErrTooLarge):
		return "", errBigImage
	case errors.Is(err, ErrFormat):
		return "", errBadImage
	case err != nil:
		return "", err
	}
	key := blob.Key(kind, owner, img, ext)
	if err := files.Upload(ctx, key, img); err != nil {
		return "", err
	}
	return key, nil
}
