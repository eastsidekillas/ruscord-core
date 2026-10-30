package blob

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"github.com/johannesboyne/gofakes3"
	"github.com/johannesboyne/gofakes3/backend/s3mem"

	"github.com/eastsidekillas/ruscord-core/internal/platform/testdb"
)

// newS3 runs an in-process S3 server and returns a client for bucket.
func newS3(t *testing.T) *S3 {
	t.Helper()
	srv := httptest.NewServer(gofakes3.New(s3mem.New()).Server())
	t.Cleanup(srv.Close)
	return NewS3(S3Config{Endpoint: srv.URL, Region: "us-east-1", Bucket: "ruscord", AccessKey: "k", SecretKey: "s"})
}

func TestS3(t *testing.T) {
	s := newS3(t)
	ctx := t.Context()
	if err := s.Ping(ctx); err == nil {
		t.Fatal("ping of a missing bucket must fail")
	}
	if err := s.EnsureBucket(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.EnsureBucket(ctx); err != nil {
		t.Fatalf("second EnsureBucket: %v", err)
	}
	if err := s.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	key := Key(KindAvatar, uuid.New(), []byte("img"), ExtPNG)
	if _, err := s.Get(ctx, key); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing object: %v, want ErrNotFound", err)
	}
	if err := s.Put(ctx, key, []byte("img"), "image/png"); err != nil {
		t.Fatal(err)
	}
	if b, err := s.Get(ctx, key); err != nil || string(b) != "img" {
		t.Fatalf("get = %q, %v", b, err)
	}
	if err := s.Delete(ctx, key); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(ctx, key); err != nil {
		t.Fatalf("deleting a missing object: %v", err)
	}
	if _, err := s.Get(ctx, key); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after delete: %v", err)
	}
}

func TestKeys(t *testing.T) {
	owner := uuid.New()
	a, b := Key(KindAvatar, owner, []byte("a"), ExtPNG), Key(KindAvatar, owner, []byte("b"), ExtPNG)
	if a == b || a != Key(KindAvatar, owner, []byte("a"), ExtPNG) {
		t.Fatal("keys must be content-addressed")
	}
	if !ValidKey(a) || !ValidKey(Key(KindIcon, owner, []byte("x"), ExtJPEG)) {
		t.Fatal("produced keys must be valid")
	}
	hash := strings.Repeat("a", 64)
	for _, bad := range []string{
		"", "avatars", "avatars/" + owner.String() + "/" + hash + ".svg",
		"avatars/" + owner.String() + "/" + hash + ".png/../x",
		"../avatars/" + owner.String() + "/" + hash + ".png",
		"banners/" + owner.String() + "/" + hash + ".png",
		"avatars/not-a-uuid/" + hash + ".png",
		"avatars/" + owner.String() + "/" + strings.ToUpper(hash) + ".png",
		"avatars/" + owner.String() + "/" + hash + ".png\n",
	} {
		if ValidKey(bad) {
			t.Errorf("ValidKey(%q) = true", bad)
		}
	}
	if URL(a) != "/cdn/"+a || URLPtr(nil) != nil || *URLPtr(&a) != "/cdn/"+a {
		t.Fatal("URL")
	}
}

func TestHandler(t *testing.T) {
	store := NewMemory()
	mux := http.NewServeMux()
	Register(mux, store, testdb.Logger())
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	key := Key(KindIcon, uuid.New(), []byte("png bytes"), ExtPNG)
	_ = store.Put(t.Context(), key, []byte("png bytes"), "image/png")

	get := func(path string) (int, http.Header) {
		t.Helper()
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+path, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		return resp.StatusCode, resp.Header
	}
	code, h := get("/cdn/" + key)
	if code != 200 || h.Get("Content-Type") != "image/png" ||
		h.Get("X-Content-Type-Options") != "nosniff" ||
		!strings.Contains(h.Get("Content-Security-Policy"), "sandbox") ||
		!strings.Contains(h.Get("Cache-Control"), "immutable") {
		t.Fatalf("served: %d %v", code, h)
	}
	for _, path := range []string{
		"/cdn/" + Key(KindIcon, uuid.New(), []byte("missing"), ExtPNG),
		"/cdn/" + strings.Replace(key, ".png", ".svg", 1),
		"/cdn/%2e%2e/%2e%2e/etc/passwd",
		"/cdn/avatars/x",
	} {
		if code, _ := get(path); code != http.StatusNotFound {
			t.Errorf("GET %s: %d, want 404", path, code)
		}
	}
}

// Collector tests need RUSCORD_TEST_POSTGRES_URL.

type refs struct{ used atomic.Value } // map[string]bool

func (r *refs) set(keys ...string) {
	m := map[string]bool{}
	for _, k := range keys {
		m[k] = true
	}
	r.used.Store(m)
}

func (r *refs) check(_ context.Context, key string) (bool, error) {
	m, _ := r.used.Load().(map[string]bool)
	return m[key], nil
}

func newCollector(t *testing.T) (*Collector, *Memory, *refs) {
	t.Helper()
	pg := testdb.New(t)
	store := NewMemory()
	c := NewCollector(pg, store, testdb.Logger())
	r := &refs{}
	r.set()
	c.Register(KindAvatar, r.check)
	return c, store, r
}

// due makes every scheduled row due now.
func due(t *testing.T, c *Collector) {
	t.Helper()
	if _, err := c.pg.Exec(t.Context(), `UPDATE blob_garbage SET delete_after = now() - interval '1 second'`); err != nil {
		t.Fatal(err)
	}
}

func rowsLeft(t *testing.T, c *Collector) int {
	t.Helper()
	var n int
	_ = c.pg.QueryRow(t.Context(), `SELECT count(*) FROM blob_garbage`).Scan(&n)
	return n
}

func TestCollector(t *testing.T) {
	c, store, r := newCollector(t)
	ctx := t.Context()
	owner := uuid.New()
	kept, orphan, failing := Key(KindAvatar, owner, []byte("kept"), ExtPNG), Key(KindAvatar, owner, []byte("orphan"), ExtPNG),
		Key(KindAvatar, owner, []byte("failing"), ExtPNG)
	unknown := Key(KindIcon, owner, []byte("icon"), ExtPNG) // no check registered for icons here
	for _, k := range []string{kept, orphan, unknown} {
		if err := c.Upload(ctx, k, []byte("x")); err != nil {
			t.Fatal(err)
		}
	}
	r.set(kept) // the transaction referring to "kept" committed; "orphan"'s did not

	// Not due yet: nothing happens.
	if n, err := c.Sweep(ctx); err != nil || n != 0 {
		t.Fatalf("early sweep: %d %v", n, err)
	}
	due(t, c)
	if _, err := c.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	if got := store.Keys(); !slices.Contains(got, kept) || slices.Contains(got, orphan) || !slices.Contains(got, unknown) {
		t.Fatalf("after sweep the store has %v", got)
	}
	// "kept" and "orphan" are done; the unknown kind stays leased, untouched.
	if n := rowsLeft(t, c); n != 1 {
		t.Fatalf("%d rows left, want only the unknown kind", n)
	}

	// A failed delete keeps the row for a retry after the lease.
	_ = c.Upload(ctx, failing, []byte("x"))
	store.SetFailDelete(true)
	due(t, c)
	_, _ = c.Sweep(ctx)
	if !slices.Contains(store.Keys(), failing) {
		t.Fatal("object gone although delete failed")
	}
	store.SetFailDelete(false)
	if n, _ := c.Sweep(ctx); n != 0 {
		t.Fatal("a leased row must wait for its lease")
	}
	due(t, c)
	_, _ = c.Sweep(ctx)
	if slices.Contains(store.Keys(), failing) {
		t.Fatal("retry after the lease did not delete")
	}
}

// The race the lease exists for: the sweep takes a row, then the same
// content is uploaded again and referred to before the sweep deletes it.
func TestCollectorSkipsReuploaded(t *testing.T) {
	c, store, r := newCollector(t)
	ctx := t.Context()
	key := Key(KindAvatar, uuid.New(), []byte("same"), ExtPNG)
	_ = c.Upload(ctx, key, []byte("same"))
	due(t, c)

	// Take the row like Sweep does, but stop before collecting.
	var lease [16]byte
	if err := c.pg.QueryRow(ctx,
		`UPDATE blob_garbage SET delete_after = now() + interval '10 minutes', lease = gen_random_uuid()
		 WHERE key = $1 RETURNING lease`, key).Scan(&lease); err != nil {
		t.Fatal(err)
	}
	// Meanwhile: uploaded again, but the referring transaction has not
	// committed yet, so a reference check alone would say "unused".
	if err := c.Upload(ctx, key, []byte("same")); err != nil {
		t.Fatal(err)
	}
	if err := c.collect(ctx, key, lease); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(store.Keys(), key) {
		t.Fatal("the sweep deleted an object uploaded again after it took the row")
	}
	r.set(key) // now the reference commits; a later sweep keeps the object
	due(t, c)
	_, _ = c.Sweep(ctx)
	if !slices.Contains(store.Keys(), key) || rowsLeft(t, c) != 0 {
		t.Fatal("a referenced object must be kept and its row cleared")
	}
}
