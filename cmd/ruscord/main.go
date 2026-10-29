// Command ruscord runs the Ruscord core. One binary hosts every module
// (api, gateway, voice); RUSCORD_ROLES selects which of them this process runs.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/eastsidekillas/ruscord-core/internal/config"
	"github.com/eastsidekillas/ruscord-core/internal/health"
	"github.com/eastsidekillas/ruscord-core/internal/platform/httpx"
	"github.com/eastsidekillas/ruscord-core/internal/platform/logx"
	"github.com/eastsidekillas/ruscord-core/internal/platform/store"
)

// version is set at build time: -ldflags "-X main.version=..."
var version = "dev"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "ruscord:", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}

	log := logx.New(os.Stdout, cfg.LogLevel, !cfg.IsDev())
	slog.SetDefault(log)
	log.Info("starting ruscord", "version", version, "env", cfg.Env, "roles", cfg.Roles)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	clients, err := store.Open(ctx, store.Options{
		PostgresURL:   cfg.PostgresURL,
		RedisAddr:     cfg.RedisAddr,
		RedisPassword: cfg.RedisPassword,
		NATSURL:       cfg.NATSURL,
		ClientName:    "ruscord-" + version,
	})
	if err != nil {
		return err
	}
	defer clients.Close()

	mux := http.NewServeMux()

	hc := health.New(2*time.Second, cfg.IsDev())
	hc.Add("postgres", func(ctx context.Context) error { return clients.PG.Ping(ctx) })
	hc.Add("redis", func(ctx context.Context) error { return clients.Redis.Ping(ctx).Err() })
	hc.Add("nats", func(context.Context) error {
		if clients.NATS.Status() != nats.CONNECTED {
			return errors.New(clients.NATS.Status().String())
		}
		return nil
	})
	hc.Register(mux)

	// Modules register their routes here as they land (see docs/roadmap.md).
	// if cfg.Has(config.RoleAPI)     { api.Register(mux, ...) }
	// if cfg.Has(config.RoleGateway) { gateway.Register(mux, ...) }
	// if cfg.Has(config.RoleVoice)   { voice.Register(mux, ...) }

	return httpx.Run(ctx, log, cfg.HTTPAddr, mux, cfg.ShutdownTimeout)
}
