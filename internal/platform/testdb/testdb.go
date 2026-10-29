// Package testdb gives integration tests an isolated, migrated Postgres schema.
package testdb

import (
	"context"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/eastsidekillas/ruscord-core/internal/platform/store"
)

// EnvURL names the variable with the test database URL. Tests skip without it.
const EnvURL = "RUSCORD_TEST_POSTGRES_URL"

// New creates a fresh schema with all migrations applied and returns a pool
// bound to it. The schema is dropped when the test ends, so tests that each
// call New can run in parallel.
func New(t testing.TB) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv(EnvURL)
	if url == "" {
		t.Skip(EnvURL + " not set")
	}
	ctx := context.Background()
	schema := "t_" + strings.ReplaceAll(uuid.NewString(), "-", "")

	admin, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	// Extensions live in public, shared by every test schema.
	if _, err := admin.Exec(ctx, "CREATE EXTENSION IF NOT EXISTS citext"); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE")
		admin.Close()
	})

	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema + ",public"
	pg, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pg.Close)
	if err := store.Migrate(ctx, pg, Logger()); err != nil {
		t.Fatal(err)
	}
	return pg
}

// Logger discards output; for components that require a logger in tests.
func Logger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }
