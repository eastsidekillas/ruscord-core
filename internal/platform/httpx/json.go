package httpx

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/google/uuid"
)

const maxBodyBytes = 1 << 20

// Error is an API error returned to clients as {"code": ..., "message": ...}.
// Code is stable and machine-readable; Message is for humans.
type Error struct {
	Status  int               `json:"-"`
	Code    string            `json:"code"`
	Message string            `json:"message"`
	Fields  map[string]string `json:"fields,omitempty"`
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

func NewError(status int, code, msg string) *Error {
	return &Error{Status: status, Code: code, Message: msg}
}

var (
	ErrUnauthorized = NewError(http.StatusUnauthorized, "unauthorized", "Требуется вход")
	ErrForbidden    = NewError(http.StatusForbidden, "forbidden", "Недостаточно прав")
)

var errBodyTooLarge = NewError(http.StatusRequestEntityTooLarge, "body_too_large", "Тело запроса больше 1 МиБ")

// DecodeJSON reads a JSON body of at most 1 MiB and rejects unknown fields.
// Errors name the field but never echo decoder internals.
func DecodeJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return decodeError(err)
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return errBodyTooLarge
		}
		return NewError(http.StatusBadRequest, "invalid_request", "Тело запроса должно содержать один JSON-объект")
	}
	return nil
}

func decodeError(err error) error {
	var (
		tooLarge *http.MaxBytesError
		typeErr  *json.UnmarshalTypeError
		synErr   *json.SyntaxError
	)
	msg := "Некорректный JSON"
	switch {
	case errors.As(err, &tooLarge):
		return errBodyTooLarge
	case errors.Is(err, io.EOF):
		msg = "Пустое тело запроса, ожидается JSON-объект"
	case errors.As(err, &typeErr) && typeErr.Field != "":
		msg = fmt.Sprintf("Поле %q: неверный тип значения", typeErr.Field)
	case errors.As(err, &typeErr), errors.As(err, &synErr), errors.Is(err, io.ErrUnexpectedEOF):
	case strings.HasPrefix(err.Error(), "json: unknown field "):
		// encoding/json has no typed error for this one.
		msg = "Неизвестное поле " + strings.TrimPrefix(err.Error(), "json: unknown field ")
	default:
		// Custom UnmarshalJSON errors, e.g. a permissions string that is not a number.
		msg = "Некорректное значение в JSON"
	}
	return NewError(http.StatusBadRequest, "invalid_request", msg)
}

func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if v != nil {
		_ = json.NewEncoder(w).Encode(v)
	}
}

// WriteError writes err as an API error. Unknown errors are logged and
// reported as a generic 500 so internals never reach the client.
func WriteError(w http.ResponseWriter, r *http.Request, err error) {
	var apiErr *Error
	if errors.As(err, &apiErr) {
		WriteJSON(w, apiErr.Status, apiErr)
		return
	}
	slog.ErrorContext(r.Context(), "request failed", "method", r.Method, "path", r.URL.Path, "err", err)
	WriteJSON(w, http.StatusInternalServerError, NewError(http.StatusInternalServerError, "internal", "Внутренняя ошибка"))
}

// NoContent answers 204 unless err is set; handlers end with
// `return httpx.NoContent(w, svc.Do(...))`.
func NoContent(w http.ResponseWriter, err error) error {
	if err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// PathUUID parses a path value as a UUID. A malformed id is just another
// missing object, so it answers 404.
func PathUUID(r *http.Request, name string) (uuid.UUID, error) {
	id, err := uuid.Parse(r.PathValue(name))
	if err != nil {
		return uuid.Nil, ErrNotFound
	}
	return id, nil
}

// Handle adapts a handler that returns an error.
func Handle(fn func(w http.ResponseWriter, r *http.Request) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := fn(w, r); err != nil {
			WriteError(w, r, err)
		}
	}
}
