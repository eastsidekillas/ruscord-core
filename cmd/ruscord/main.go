// Command ruscord runs the Ruscord core. One binary hosts every module
// (api, gateway, voice); RUSCORD_ROLES selects which of them this process runs.
//
//	ruscord           run the server
//	ruscord migrate   apply database migrations and exit
package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/eastsidekillas/ruscord-core/internal/auth"
	"github.com/eastsidekillas/ruscord-core/internal/config"
	"github.com/eastsidekillas/ruscord-core/internal/events"
	"github.com/eastsidekillas/ruscord-core/internal/guild"
	"github.com/eastsidekillas/ruscord-core/internal/health"
	"github.com/eastsidekillas/ruscord-core/internal/message"
	"github.com/eastsidekillas/ruscord-core/internal/platform/httpx"
	"github.com/eastsidekillas/ruscord-core/internal/platform/logx"
	"github.com/eastsidekillas/ruscord-core/internal/platform/store"
	"github.com/eastsidekillas/ruscord-core/internal/ratelimit"
	"github.com/eastsidekillas/ruscord-core/internal/social"
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

	migrateOnly := len(os.Args) > 1 && os.Args[1] == "migrate"
	if migrateOnly || cfg.AutoMigrate {
		if err := store.Migrate(ctx, clients.PG, log); err != nil {
			return err
		}
		if migrateOnly {
			return nil
		}
	}

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
	if cfg.Has(config.RoleAPI) {
		seed := cfg.JWTSeed
		if seed == nil {
			seed = make([]byte, ed25519.SeedSize)
			if _, err := rand.Read(seed); err != nil {
				return err
			}
			log.Warn("RUSCORD_JWT_ED25519_SEED is not set: using an ephemeral key, sessions reset on restart")
		}
		signer, err := auth.NewSigner(seed)
		if err != nil {
			return err
		}
		limiter := ratelimit.New(clients.Redis)
		svc, err := auth.NewService(ctx, auth.Deps{
			PG:      clients.PG,
			Signer:  signer,
			Limiter: limiter,
			Tickets: auth.NewTickets(clients.Redis),
		})
		if err != nil {
			return err
		}
		authHTTP := auth.NewHandler(svc, !cfg.IsDev())
		authHTTP.Register(mux)
		guilds := guild.NewService(clients.PG, svc, limiter)
		guild.NewHandler(guilds, authHTTP.RequireAuth).Register(mux)
		socials := social.NewService(clients.PG, svc, guilds, limiter)
		social.NewHandler(socials, authHTTP.RequireAuth).Register(mux)
		guilds.OnChannelsDeleted(message.DeleteChannels)
		socials.OnChannelsDeleted(message.DeleteChannels)
		message.NewHandler(message.NewService(clients.PG, svc, guilds, socials, limiter), authHTTP.RequireAuth).Register(mux)

		relay, err := events.NewRelay(clients.PG, clients.NATS, log)
		if err != nil {
			return err
		}
		go relay.Run(ctx)
	}

	handler := httpx.Recover(httpx.CheckOrigin(cfg.AllowedOrigins, mux))
	return httpx.Run(ctx, log, cfg.HTTPAddr, handler, cfg.ShutdownTimeout)
}
