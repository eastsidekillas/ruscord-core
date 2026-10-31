// Command ruscord runs the Ruscord core. One binary hosts every module
// (api, gateway, voice); RUSCORD_ROLES selects which of them this process runs.
//
//	ruscord                     run the server
//	ruscord migrate             apply database migrations and exit
//	ruscord files-backup DIR    copy stored files (S3) into DIR, new ones only
//	ruscord files-restore DIR   upload the files in DIR back to S3
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
	"github.com/eastsidekillas/ruscord-core/internal/gateway"
	"github.com/eastsidekillas/ruscord-core/internal/guild"
	"github.com/eastsidekillas/ruscord-core/internal/health"
	"github.com/eastsidekillas/ruscord-core/internal/message"
	"github.com/eastsidekillas/ruscord-core/internal/platform/blob"
	"github.com/eastsidekillas/ruscord-core/internal/platform/httpx"
	"github.com/eastsidekillas/ruscord-core/internal/platform/logx"
	"github.com/eastsidekillas/ruscord-core/internal/platform/metrics"
	"github.com/eastsidekillas/ruscord-core/internal/platform/store"
	"github.com/eastsidekillas/ruscord-core/internal/presence"
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

	if len(os.Args) == 3 && (os.Args[1] == "files-backup" || os.Args[1] == "files-restore") {
		return filesCommand(ctx, cfg, log, os.Args[1], os.Args[2])
	}

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
	reg := metrics.NewRegistry()
	reg.Runtime(version, log)
	gauge := func(name, help, query string) {
		reg.GaugeFunc(name, help, log, func(ctx context.Context) (float64, error) {
			var v float64
			err := clients.PG.QueryRow(ctx, query).Scan(&v)
			return v, err
		})
	}
	gauge("ruscord_outbox_backlog", "Outbox events not yet published to NATS.",
		`SELECT count(*) FROM outbox WHERE published_at IS NULL`)
	gauge("ruscord_outbox_oldest_age_seconds", "Age of the oldest unpublished outbox event; 0 when none.",
		`SELECT COALESCE(EXTRACT(EPOCH FROM now() - min(created_at)), 0) FROM outbox WHERE published_at IS NULL`)
	gauge("ruscord_blob_garbage_due", "Stored files due for the collector.",
		`SELECT count(*) FROM blob_garbage WHERE delete_after <= now()`)

	hc := health.New(2*time.Second, cfg.IsDev())
	hc.Add("postgres", func(ctx context.Context) error { return clients.PG.Ping(ctx) })
	hc.Add("redis", func(ctx context.Context) error { return clients.Redis.Ping(ctx).Err() })
	hc.Add("nats", func(context.Context) error {
		if clients.NATS.Status() != nats.CONNECTED {
			return errors.New(clients.NATS.Status().String())
		}
		return nil
	})
	var files *blob.Collector
	if cfg.FilesEnabled() {
		s3 := blob.NewS3(blob.S3Config{Endpoint: cfg.S3Endpoint, Region: cfg.S3Region, Bucket: cfg.S3Bucket,
			AccessKey: cfg.S3AccessKey, SecretKey: cfg.S3SecretKey})
		if cfg.IsDev() {
			if err := s3.EnsureBucket(ctx); err != nil {
				log.Warn("s3: could not create the bucket; is SeaweedFS up (make up)?", "err", err)
			}
		}
		hc.Add("s3", s3.Ping)
		files = blob.NewCollector(clients.PG, s3, log)
		blob.Register(mux, s3, log)
	} else {
		log.Warn("RUSCORD_S3_ACCESS_KEY is not set: avatar and icon uploads are off")
	}
	hc.Register(mux)

	// Modules register their routes here as they land (see docs/roadmap.md).
	// The gateway reads the same modules as the API, so both roles build them.
	if cfg.Has(config.RoleAPI) || cfg.Has(config.RoleGateway) {
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
			Revoked: auth.NewRevocations(clients.Redis),
			Files:   files,
		})
		if err != nil {
			return err
		}
		guilds := guild.NewService(clients.PG, svc, limiter)
		svc.SetGuildIDs(guilds)
		if files != nil {
			guilds.WithFiles(files)
		}
		socials := social.NewService(clients.PG, svc, guilds, limiter)
		svc.OnAccountDeleted(guilds, socials)
		eph := events.NewEphemeral(clients.NATS)
		gwOpts := gateway.Options{AllowedOrigins: cfg.AllowedOrigins}
		pres := presence.NewService(clients.Redis, eph, guilds, gateway.PresenceTTL(gwOpts))
		guilds.WithPresences(pres)
		guilds.OnChannelsDeleted(message.DeleteChannels)
		socials.OnChannelsDeleted(message.DeleteChannels)

		if cfg.Has(config.RoleAPI) {
			authHTTP := auth.NewHandler(svc, !cfg.IsDev())
			authHTTP.Register(mux)
			guild.NewHandler(guilds, authHTTP.RequireAuth).Register(mux)
			social.NewHandler(socials, authHTTP.RequireAuth).Register(mux)
			message.NewHandler(message.NewService(clients.PG, svc, guilds, socials, limiter, eph), authHTTP.RequireAuth).Register(mux)

			relay, err := events.NewRelay(clients.PG, clients.NATS, log)
			if err != nil {
				return err
			}
			go relay.Run(ctx)
			if files != nil {
				go files.Run(ctx, time.Minute)
			}
			go svc.RunAccountCleanup(ctx, time.Minute)
		}
		if cfg.Has(config.RoleGateway) {
			gw, err := gateway.New(ctx, gateway.Deps{Auth: svc, Guilds: guilds, Social: socials, Presence: pres,
				NATS: clients.NATS, Log: log}, gwOpts)
			if err != nil {
				return err
			}
			gw.Register(mux)
			reg.GaugeFunc("ruscord_gateway_sessions", "Gateway sessions on this node, attached or waiting for RESUME.", log,
				func(context.Context) (float64, error) { return float64(gw.SessionCount()), nil })
			go gw.Run(ctx)
		}
	}

	if cfg.MetricsAddr != "off" {
		mm := http.NewServeMux()
		mm.Handle("GET /metrics", reg.Handler())
		go func() {
			if err := httpx.Run(ctx, log, cfg.MetricsAddr, mm, cfg.ShutdownTimeout); err != nil {
				log.Error("metrics server", "err", err)
			}
		}()
	}

	// metrics.HTTP sits right around the mux: it reads the matched pattern,
	// which the mux sets on the request it is given.
	handler := httpx.Recover(httpx.TrustProxies(cfg.TrustedProxies, httpx.CheckOrigin(cfg.AllowedOrigins, metrics.HTTP(reg, mux))))
	return httpx.Run(ctx, log, cfg.HTTPAddr, handler, cfg.ShutdownTimeout)
}

// filesCommand backs up or restores stored files (scripts/backup.sh).
func filesCommand(ctx context.Context, cfg config.Config, log *slog.Logger, cmd, dir string) error {
	if !cfg.FilesEnabled() {
		return errors.New("RUSCORD_S3_ACCESS_KEY is not set")
	}
	s3 := blob.NewS3(blob.S3Config{Endpoint: cfg.S3Endpoint, Region: cfg.S3Region, Bucket: cfg.S3Bucket,
		AccessKey: cfg.S3AccessKey, SecretKey: cfg.S3SecretKey})
	if cmd == "files-restore" {
		n, err := blob.Restore(ctx, s3, dir)
		log.Info("files restored", "files", n, "from", dir)
		return err
	}
	copied, skipped, err := blob.Backup(ctx, s3, dir)
	log.Info("files backed up", "copied", copied, "already there", skipped, "to", dir)
	return err
}
