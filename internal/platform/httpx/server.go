// Package httpx runs an HTTP server with sane timeouts and graceful shutdown.
package httpx

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"time"
)

// Run serves h on addr until ctx is cancelled, then drains connections for
// up to shutdownTimeout.
//
// Read and write timeouts bound slow clients on REST (bodies are at most
// 1 MiB). They do not apply to WebSockets: net/http clears deadlines when a
// connection is hijacked, and hijacked connections are not drained by
// Shutdown, so the gateway closes its own on ctx.
func Run(ctx context.Context, log *slog.Logger, addr string, h http.Handler, shutdownTimeout time.Duration) error {
	srv := &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}

	errc := make(chan error, 1)
	go func() {
		log.Info("http server listening", "addr", addr)
		errc <- srv.ListenAndServe()
	}()

	select {
	case err := <-errc:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
	}

	log.Info("http server shutting down", "timeout", shutdownTimeout)
	sctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	return srv.Shutdown(sctx)
}
