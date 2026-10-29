package httpx

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestDecodeJSON(t *testing.T) {
	type in struct {
		Name string `json:"name"`
		N    int    `json:"n"`
	}
	for _, c := range []struct {
		name, body string
		status     int
		msg        string
	}{
		{"ok", `{"name": "x", "n": 1}`, 0, ""},
		{"empty", ``, 400, "Пустое тело запроса, ожидается JSON-объект"},
		{"syntax", `{"name": `, 400, "Некорректный JSON"},
		{"wrong type", `{"n": "1"}`, 400, `Поле "n": неверный тип значения`},
		{"unknown field", `{"is_admin": true}`, 400, `Неизвестное поле "is_admin"`},
		{"two objects", `{} {}`, 400, "Тело запроса должно содержать один JSON-объект"},
		{"too large", `{"name": "` + strings.Repeat("a", maxBodyBytes) + `"}`, 413, "Тело запроса больше 1 МиБ"},
	} {
		r := httptest.NewRequestWithContext(t.Context(), "POST", "/", strings.NewReader(c.body))
		var dst in
		err := DecodeJSON(httptest.NewRecorder(), r, &dst)
		if c.status == 0 {
			if err != nil {
				t.Errorf("%s: %v", c.name, err)
			}
			continue
		}
		var e *Error
		if !errors.As(err, &e) || e.Status != c.status || e.Message != c.msg {
			t.Errorf("%s: got %v, want %d %q", c.name, err, c.status, c.msg)
		}
	}
}

func TestQueryParams(t *testing.T) {
	q := url.Values{"after": {"not-a-uuid"}, "limit": {"0"}}
	if _, err := QueryUUID(q, "after", "id"); err == nil {
		t.Error("bad uuid accepted")
	}
	if id, err := QueryUUID(q, "before", "id"); id != nil || err != nil {
		t.Errorf("absent uuid: %v %v", id, err)
	}
	for v, ok := range map[string]bool{"": true, "1": true, "100": true, "0": false, "101": false, "x": false} {
		n, err := QueryLimit(url.Values{"limit": {v}}, 100)
		if (err == nil) != ok {
			t.Errorf("limit %q: %d %v", v, n, err)
		}
		var e *Error
		if !ok && (!errors.As(err, &e) || e.Status != http.StatusBadRequest) {
			t.Errorf("limit %q: %v", v, err)
		}
	}
}
