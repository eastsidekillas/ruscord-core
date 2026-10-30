package gateway

import (
	"slices"
	"strconv"
	"testing"
)

func TestReplayBuffer(t *testing.T) {
	// fill pushes frames numbered from base+1; each frame is its number as text.
	fill := func(b *replayBuffer, n int) {
		for range n {
			b.push([]byte(strconv.FormatUint(b.last()+1, 10)))
		}
	}
	text := func(fs [][]byte) []string {
		out := make([]string, len(fs))
		for i, f := range fs {
			out[i] = string(f)
		}
		return out
	}
	cases := []struct {
		name      string
		maxFrames int
		maxBytes  int
		push      int
		ack       uint64
		since     uint64
		want      []string
		ok        bool
	}{
		{name: "nothing sent after READY", maxFrames: 10, maxBytes: 1000, since: 1, want: []string{}, ok: true},
		{name: "everything after READY", maxFrames: 10, maxBytes: 1000, push: 3, since: 1, want: []string{"2", "3", "4"}, ok: true},
		{name: "from the middle", maxFrames: 10, maxBytes: 1000, push: 3, since: 3, want: []string{"4"}, ok: true},
		{name: "up to date", maxFrames: 10, maxBytes: 1000, push: 3, since: 4, want: []string{}, ok: true},
		{name: "seq never sent", maxFrames: 10, maxBytes: 1000, push: 3, since: 5},
		{name: "seq zero", maxFrames: 10, maxBytes: 1000, push: 3, since: 0},
		{name: "frame cap drops the oldest", maxFrames: 2, maxBytes: 1000, push: 4, since: 3, want: []string{"4", "5"}, ok: true},
		{name: "dropped frames cannot be replayed", maxFrames: 2, maxBytes: 1000, push: 4, since: 2},
		{name: "byte cap drops the oldest", maxFrames: 10, maxBytes: 3, push: 5, since: 3, want: []string{"4", "5", "6"}, ok: true},
		{name: "ack frees confirmed frames", maxFrames: 10, maxBytes: 1000, push: 4, ack: 3, since: 3, want: []string{"4", "5"}, ok: true},
		{name: "acked frames are gone", maxFrames: 10, maxBytes: 1000, push: 4, ack: 3, since: 2},
		{name: "ack beyond last empties the buffer", maxFrames: 10, maxBytes: 1000, push: 2, ack: 99, since: 3, want: []string{}, ok: true},
		{name: "stale ack changes nothing", maxFrames: 10, maxBytes: 1000, push: 2, ack: 1, since: 1, want: []string{"2", "3"}, ok: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b := newReplayBuffer(1, c.maxFrames, c.maxBytes)
			fill(b, c.push)
			if c.ack > 0 {
				b.ack(c.ack)
			}
			got, ok := b.since(c.since)
			if ok != c.ok || (ok && !slices.Equal(text(got), c.want)) {
				t.Fatalf("since(%d) = %v, %v; want %v, %v", c.since, text(got), ok, c.want, c.ok)
			}
			if b.bytes < 0 || len(b.frames) > c.maxFrames || b.bytes > c.maxBytes {
				t.Fatalf("over the caps: %d frames, %d bytes", len(b.frames), b.bytes)
			}
		})
	}
}
