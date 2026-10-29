package httpx

import (
	"net/http"
	"net/url"
	"strconv"

	"github.com/google/uuid"
)

func badQuery(msg string) *Error { return NewError(http.StatusBadRequest, "invalid_request", msg) }

// QueryUUID reads an optional id from the query string; nil when absent.
// what names the object in the error, e.g. "id сообщения".
func QueryUUID(q url.Values, key, what string) (*uuid.UUID, error) {
	v := q.Get(key)
	if v == "" {
		return nil, nil
	}
	id, err := uuid.Parse(v)
	if err != nil {
		return nil, badQuery(key + " должен быть " + what)
	}
	return &id, nil
}

// QueryLimit reads an optional limit in [1, maxLimit]; 0 when absent, so
// the service applies its default.
func QueryLimit(q url.Values, maxLimit int) (int, error) {
	v := q.Get("limit")
	if v == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 || n > maxLimit {
		return 0, badQuery("limit от 1 до " + strconv.Itoa(maxLimit))
	}
	return n, nil
}
