package apitest

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"net/http"
)

// PNG encodes a w×h picture filled with c, for upload tests. Different
// colors give different files (and so different content-addressed keys).
func PNG(w, h int, c color.Color) []byte {
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for x := range w {
		for y := range h {
			img.Set(x, y, c)
		}
	}
	var b bytes.Buffer
	_ = png.Encode(&b, img)
	return b.Bytes()
}

// Garbage lists the keys the blob collector has scheduled, with how long
// until each is due, in whole hours.
func (e *Env) Garbage() map[string]int {
	e.T.Helper()
	rows, err := e.PG.Query(e.T.Context(),
		`SELECT key, round(extract(epoch FROM delete_after - now()) / 3600)::int FROM blob_garbage`)
	if err != nil {
		e.T.Fatal(err)
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var k string
		var h int
		_ = rows.Scan(&k, &h)
		out[k] = h
	}
	return out
}

// Sweep makes every scheduled file due and runs the collector once.
func (e *Env) Sweep() {
	e.T.Helper()
	if _, err := e.PG.Exec(e.T.Context(), `UPDATE blob_garbage SET delete_after = now() - interval '1 second'`); err != nil {
		e.T.Fatal(err)
	}
	if _, err := e.Files.Sweep(e.T.Context()); err != nil {
		e.T.Fatal(err)
	}
}

// Outbox lists the written events of one type as (topic, payload) pairs,
// oldest first.
func (e *Env) Outbox(typ string) []OutboxEvent {
	e.T.Helper()
	rows, err := e.PG.Query(e.T.Context(), `SELECT topic, payload FROM outbox WHERE type = $1 ORDER BY id`, typ)
	if err != nil {
		e.T.Fatal(err)
	}
	defer rows.Close()
	var out []OutboxEvent
	for rows.Next() {
		var ev OutboxEvent
		if err := rows.Scan(&ev.Topic, &ev.Data); err != nil {
			e.T.Fatal(err)
		}
		out = append(out, ev)
	}
	return out
}

// OutboxEvent is one written event.
type OutboxEvent struct {
	Topic string
	Data  map[string]any
}

// Fetch GETs a path without credentials (files under /cdn are public) and
// returns the status and headers.
func (e *Env) Fetch(path string) (int, http.Header) {
	e.T.Helper()
	req, _ := http.NewRequestWithContext(e.T.Context(), http.MethodGet, e.srv.URL+path, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.T.Fatal(err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode, resp.Header
}
