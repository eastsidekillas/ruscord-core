package gateway

// replayBuffer keeps the encoded frames a client may not have received yet,
// so a RESUME can send them again. Frames are numbered consecutively; the
// buffer holds base+1 … last. Heartbeats acknowledge frames and free them,
// so a healthy connection keeps about one heartbeat interval of frames; the
// caps only matter while a client is gone or stops acknowledging.
type replayBuffer struct {
	base      uint64
	frames    [][]byte
	bytes     int
	maxFrames int
	maxBytes  int
}

func newReplayBuffer(base uint64, maxFrames, maxBytes int) *replayBuffer {
	return &replayBuffer{base: base, maxFrames: maxFrames, maxBytes: maxBytes}
}

// last is the sequence number of the newest frame (base when empty).
func (b *replayBuffer) last() uint64 { return b.base + uint64(len(b.frames)) } //nolint:gosec // a length is never negative

// push stores the frame numbered last()+1 and drops the oldest frames when
// over a cap.
func (b *replayBuffer) push(frame []byte) {
	b.frames = append(b.frames, frame)
	b.bytes += len(frame)
	for len(b.frames) > 0 && (len(b.frames) > b.maxFrames || b.bytes > b.maxBytes) {
		b.dropFirst()
	}
}

// ack frees the frames up to seq, which the client confirmed it has.
func (b *replayBuffer) ack(seq uint64) {
	for len(b.frames) > 0 && b.base < seq {
		b.dropFirst()
	}
}

func (b *replayBuffer) dropFirst() {
	b.bytes -= len(b.frames[0])
	b.frames[0] = nil
	b.frames = b.frames[1:]
	b.base++
}

// since returns the frames after seq. ok is false when seq is older than
// the buffer (frames were dropped) or newer than anything sent.
func (b *replayBuffer) since(seq uint64) (frames [][]byte, ok bool) {
	if seq < b.base || seq > b.last() {
		return nil, false
	}
	return append([][]byte(nil), b.frames[seq-b.base:]...), true
}
