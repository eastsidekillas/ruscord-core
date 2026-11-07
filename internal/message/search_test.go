package message

import (
	"errors"
	"image/color"
	"net/http"
	"net/url"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/eastsidekillas/ruscord-core/internal/perm"
	"github.com/eastsidekillas/ruscord-core/internal/platform/apitest"
)

// find runs a search as u and returns the total and the contents found.
// Search is rate-limited per second; tests clear the limits between calls.
func (e *env) find(u *apitest.User, path string, q url.Values, want int) (int, []string) {
	e.T.Helper()
	if err := e.Redis.FlushAll(e.T.Context()).Err(); err != nil {
		e.T.Fatal(err)
	}
	out := obj(u.Expect(want, "GET", path+"?"+q.Encode(), nil))
	texts := []string{}
	for _, m := range list(out["messages"]) {
		texts = append(texts, str(obj(m)["content"]))
	}
	n, _ := out["total_results"].(float64)
	return int(n), texts
}

func TestSearch(t *testing.T) {
	e := newEnv(t)
	owner, bob, eve := e.User("owner"), e.User("bob"), e.User("eve")
	gid, text, _ := e.server(owner, bob)
	g := "/api/v1/guilds/" + gid
	search := g + "/messages/search"
	hidden := str(obj(owner.Expect(http.StatusCreated, "POST", g+"/channels", map[string]any{"type": 0, "name": "тайный"}))["id"])
	owner.Expect(http.StatusOK, "PUT", "/api/v1/channels/"+hidden+"/permissions/"+gid, map[string]any{"type": 1, "deny": permStr(perm.ViewChannel)})

	send(owner, text, "Рыжие кошки спят")
	send(bob, text, "кошка на окне https://cat.example")
	send(owner, text, "собака")
	send(owner, hidden, "секретная кошка")
	pic := upload(t, bob, text, "a.png", apitest.PNG(4, 4, color.Black))
	bob.Expect(http.StatusCreated, "POST", msgs(text), map[string]any{"content": "фото", "attachments": []any{pic["id"]}})
	send(owner, text, mention(bob)+" посмотри")

	// Russian stems find other forms; hidden channels are never searched.
	if n, got := e.find(bob, search, url.Values{"q": {"кошки"}}, http.StatusOK); n != 2 || !slices.Equal(got, []string{"кошка на окне https://cat.example", "Рыжие кошки спят"}) {
		t.Fatalf("кошки: %d %v", n, got)
	}
	// The owner sees the hidden channel too.
	if n, _ := e.find(owner, search, url.Values{"q": {"кошка"}}, http.StatusOK); n != 3 {
		t.Fatalf("owner: %d", n)
	}
	// Words as written: a name the stemmer does not know, an English word.
	if n, _ := e.find(bob, search, url.Values{"q": {"рыжие"}}, http.StatusOK); n != 1 {
		t.Fatalf("рыжие: %d", n)
	}
	// Filters, alone and together.
	cases := []struct {
		name string
		q    url.Values
		want []string
	}{
		{"author", url.Values{"q": {"кошка"}, "author_id": {bob.ID.String()}}, []string{"кошка на окне https://cat.example"}},
		{"link", url.Values{"has": {"link"}}, []string{"кошка на окне https://cat.example"}},
		{"image", url.Values{"has": {"image"}}, []string{"фото"}},
		{"file and author", url.Values{"has": {"file"}, "author_id": {owner.ID.String()}}, []string{}},
		{"mentions", url.Values{"mentions": {bob.ID.String()}}, []string{mention(bob) + " посмотри"}},
		{"in a channel", url.Values{"q": {"собака"}, "channel_id": {text}}, []string{"собака"}},
		{"in a hidden channel", url.Values{"q": {"кошка"}, "channel_id": {hidden}}, []string{}},
		{"after now", url.Values{"q": {"кошка"}, "after": {"2999-01-01T00:00:00Z"}}, []string{}},
		{"before then", url.Values{"q": {"кошка"}, "before": {"2999-01-01T00:00:00Z"}}, []string{"кошка на окне https://cat.example", "Рыжие кошки спят"}},
		{"nothing", url.Values{"q": {"жираф"}}, []string{}},
	}
	for _, c := range cases {
		if _, got := e.find(bob, search, c.q, http.StatusOK); !slices.Equal(got, c.want) {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
	// Pages: offset past the end gives the total and nothing else.
	if n, got := e.find(bob, search, url.Values{"q": {"кошка"}, "offset": {"5"}}, http.StatusOK); n != 2 || len(got) != 0 {
		t.Fatalf("past the end: %d %v", n, got)
	}
	for _, bad := range []url.Values{{}, {"q": {"  "}}, {"has": {"pdf"}}, {"q": {"x"}, "offset": {"6000"}}, {"q": {"x"}, "after": {"вчера"}}} {
		e.find(bob, search, bad, http.StatusUnprocessableEntity)
	}
	e.find(eve, search, url.Values{"q": {"кошка"}}, http.StatusNotFound)

	// A DM or one channel by itself.
	ann := e.User("ann")
	dm := e.dm(bob, ann)
	send(ann, dm, "лично про кошку")
	if n, _ := e.find(bob, "/api/v1/channels/"+dm+"/messages/search", url.Values{"q": {"кошка"}}, http.StatusOK); n != 1 {
		t.Fatalf("DM: %d", n)
	}
	e.find(eve, "/api/v1/channels/"+dm+"/messages/search", url.Values{"q": {"кошка"}}, http.StatusNotFound)

	// Rate limit: a second search within a second waits.
	e.find(bob, search, url.Values{"q": {"кошка"}}, http.StatusOK)
	bob.Expect(http.StatusTooManyRequests, "GET", search+"?q=x", nil)
}

func TestHistoryAround(t *testing.T) {
	e := newEnv(t)
	owner := e.User("owner")
	_, text, _ := e.server(owner)
	ids := []string{}
	for _, c := range []string{"1", "2", "3", "4", "5"} {
		ids = append(ids, str(send(owner, text, c)["id"]))
	}
	got := []string{}
	for _, m := range list(owner.Expect(http.StatusOK, "GET", msgs(text)+"?limit=3&around="+ids[2], nil)) {
		got = append(got, str(obj(m)["content"]))
	}
	if !slices.Equal(got, []string{"4", "3", "2"}) {
		t.Fatalf("around 3: %v", got)
	}
	owner.Expect(http.StatusBadRequest, "GET", msgs(text)+"?around="+ids[2]+"&before="+ids[3], nil)
}

// A statement stopped by statement_timeout is told apart from other errors.
func TestIsTimeout(t *testing.T) {
	e := newEnv(t)
	err := pgx.BeginFunc(t.Context(), e.PG, func(tx pgx.Tx) error {
		if _, err := tx.Exec(t.Context(), `SET LOCAL statement_timeout = 10`); err != nil {
			return err
		}
		_, err := tx.Exec(t.Context(), `SELECT pg_sleep(1)`)
		return err
	})
	if !isTimeout(err) {
		t.Fatalf("a timeout: %v", err)
	}
	if isTimeout(errors.New("other")) || isTimeout(nil) || isTimeout(errSearchTooSlow) {
		t.Fatal("not a timeout")
	}
}
