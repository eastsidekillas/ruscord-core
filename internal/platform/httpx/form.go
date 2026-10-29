package httpx

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"
)

// ErrNotFound is the answer for missing objects and for objects the caller
// may not see, so existence cannot be probed.
var ErrNotFound = NewError(http.StatusNotFound, "not_found", "Не найдено")

// Fields collects per-field validation messages.
type Fields map[string]string

// Length checks that v has between minLen and maxLen characters and no NUL,
// which Postgres text cannot store (it would surface as a 500).
func (f Fields) Length(key, v string, minLen, maxLen int) {
	switch n := utf8.RuneCountInString(v); {
	case n < minLen || n > maxLen:
		f[key] = "Длина от " + strconv.Itoa(minLen) + " до " + strconv.Itoa(maxLen) + " символов"
	case strings.ContainsRune(v, 0):
		f[key] = "Недопустимый символ"
	}
}

// Err is nil when no field failed, otherwise the 422 answer.
func (f Fields) Err() error {
	if len(f) == 0 {
		return nil
	}
	return Validation(f)
}

// Validation is the 422 answer for a form with per-field messages.
func Validation(fields map[string]string) *Error {
	e := NewError(http.StatusUnprocessableEntity, "validation_failed", "Проверьте поля формы")
	e.Fields = fields
	return e
}

// Optional distinguishes an absent JSON field (Set false) from an explicit
// null (Set true, Value nil).
type Optional[T any] struct {
	Set   bool
	Value *T
}

func (o *Optional[T]) UnmarshalJSON(b []byte) error {
	o.Set = true
	if string(b) == "null" {
		o.Value = nil
		return nil
	}
	var v T
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	o.Value = &v
	return nil
}
