package httpx

import (
	"net/http"
	"time"
)

// Server-wide limits on reading a request and writing an answer. They suit
// API calls; large bodies get more time with AllowRead and AllowWrite.
const (
	readTimeout  = 30 * time.Second
	writeTimeout = 60 * time.Second
)

// MinTransferRate is the slowest line a file upload or download is waited
// for: about 256 kbit/s. A 25 MiB attachment then has 30 s + 13.5 min to
// arrive; a slower or stalled peer is still cut off (R3 of
// docs/audit/2026-11-04.md).
const MinTransferRate = 32 << 10 // bytes per second

// TransferTime is how long n bytes take at MinTransferRate.
func TransferTime(n int64) time.Duration {
	return time.Duration(max(n, 0)/MinTransferRate) * time.Second
}

// AllowRead gives the request body of n bytes time to arrive at
// MinTransferRate on top of the usual limit. Writers that cannot set
// deadlines (tests) keep theirs.
func AllowRead(w http.ResponseWriter, n int64) {
	_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(readTimeout + TransferTime(n)))
}

// AllowWrite is AllowRead for an answer of n bytes.
func AllowWrite(w http.ResponseWriter, n int64) {
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(writeTimeout + TransferTime(n)))
}

// BodySize is what a body may be: its Content-Length, or limit when it is
// unknown or larger (MaxBytesReader stops it there).
func BodySize(r *http.Request, limit int64) int64 {
	if r.ContentLength < 0 || r.ContentLength > limit {
		return limit
	}
	return r.ContentLength
}
