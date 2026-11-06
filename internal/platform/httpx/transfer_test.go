package httpx

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestTransferTime(t *testing.T) {
	for _, tt := range []struct {
		n    int64
		want time.Duration
	}{
		{-1, 0}, {0, 0}, {MinTransferRate - 1, 0}, {MinTransferRate, time.Second},
		{25 << 20, 800 * time.Second},
	} {
		if got := TransferTime(tt.n); got != tt.want {
			t.Errorf("TransferTime(%d) = %v, want %v", tt.n, got, tt.want)
		}
	}
}

func TestBodySize(t *testing.T) {
	for _, tt := range []struct{ length, limit, want int64 }{
		{-1, 100, 100}, {0, 100, 0}, {50, 100, 50}, {500, 100, 100},
	} {
		r := httptest.NewRequest("POST", "/", nil)
		r.ContentLength = tt.length
		if got := BodySize(r, tt.limit); got != tt.want {
			t.Errorf("BodySize(%d, %d) = %d, want %d", tt.length, tt.limit, got, tt.want)
		}
	}
}

// A body slower than the server's read limit arrives when the handler
// allows it, and is cut off when it does not.
func TestAllowReadOutlastsTheServerLimit(t *testing.T) {
	for _, allow := range []bool{true, false} {
		srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if allow {
				AllowRead(w, 0)
			}
			b, err := io.ReadAll(r.Body)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			_, _ = w.Write(b)
		}))
		srv.Config.ReadTimeout = 300 * time.Millisecond
		srv.Start()

		pr, pw := io.Pipe()
		go func() {
			for _, part := range []string{"мед", "лен", "но"} {
				_, _ = pw.Write([]byte(part))
				time.Sleep(200 * time.Millisecond)
			}
			_ = pw.Close()
		}()
		req, _ := http.NewRequestWithContext(t.Context(), "POST", srv.URL, pr)
		resp, err := srv.Client().Do(req)
		got := ""
		if err == nil {
			b, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				got = string(b)
			}
		}
		srv.Close()
		if ok := got == "медленно"; ok != allow {
			t.Errorf("allow=%v: got %q, %v", allow, got, err)
		}
	}
}
