// Package pg holds the database plumbing shared by every service: pool
// construction, migration, and the small helpers that keep service code from
// re-implementing them.
package pg

import (
	"context"
	"embed"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// Connect opens a pool and waits for the database to accept queries. Services
// start concurrently with Postgres in kind, so a bounded retry here is the
// difference between a clean rollout and a CrashLoopBackOff.
func Connect(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse dsn: %w", err)
	}
	// Kept small deliberately. The federated strategy's cost should show up as
	// network fan-out, not as connection pool contention masking it.
	cfg.MaxConns = 16
	cfg.MinConns = 2
	cfg.MaxConnLifetime = time.Hour

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("create pool: %w", err)
	}

	deadline := time.Now().Add(90 * time.Second)
	for {
		if err = pool.Ping(ctx); err == nil {
			return pool, nil
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			pool.Close()
			return nil, fmt.Errorf("database unreachable after 90s: %w", err)
		}
		select {
		case <-ctx.Done():
			pool.Close()
			return nil, ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

// Migrate applies every embedded migration in filename order, recording applied
// versions so reruns are cheap. Migrations must be idempotent regardless, since
// several services may race to run them on startup.
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	_, err := pool.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version    TEXT PRIMARY KEY,
			applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)`)
	if err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}

	entries, err := migrationFS.ReadDir("migrations")
	if err != nil {
		return fmt.Errorf("read migrations: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)

	// Advisory lock: several services run Migrate on startup and CREATE ROLE is
	// not safe to race. The number is arbitrary but must be shared.
	if _, err := pool.Exec(ctx, `SELECT pg_advisory_lock(8172345)`); err != nil {
		return fmt.Errorf("acquire migration lock: %w", err)
	}
	defer pool.Exec(ctx, `SELECT pg_advisory_unlock(8172345)`) //nolint:errcheck

	for _, name := range names {
		var exists bool
		err := pool.QueryRow(ctx,
			`SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version = $1)`, name).Scan(&exists)
		if err != nil {
			return fmt.Errorf("check migration %s: %w", name, err)
		}
		if exists {
			continue
		}
		body, err := migrationFS.ReadFile("migrations/" + name)
		if err != nil {
			return fmt.Errorf("read %s: %w", name, err)
		}
		if _, err := pool.Exec(ctx, string(body)); err != nil {
			return fmt.Errorf("apply %s: %w", name, err)
		}
		if _, err := pool.Exec(ctx,
			`INSERT INTO schema_migrations (version) VALUES ($1)`, name); err != nil {
			return fmt.Errorf("record %s: %w", name, err)
		}
	}

	return applyRolePasswords(ctx, pool)
}

// applyRolePasswords sets the per-service login passwords after the roles exist.
// Split out of 0007 because the password comes from the environment and must not
// be baked into a migration file.
func applyRolePasswords(ctx context.Context, pool *pgxpool.Pool) error {
	pw := os.Getenv("SERVICE_ROLE_PASSWORD")
	if pw == "" {
		// Local development default. Cluster manifests always set this.
		pw = "doclink-dev"
	}
	if strings.ContainsAny(pw, `'\`) {
		return fmt.Errorf("SERVICE_ROLE_PASSWORD must not contain quotes or backslashes")
	}
	for _, role := range []string{"svc_pim", "svc_subscriptions", "svc_shipping", "svc_doclink", "svc_bench"} {
		// ALTER ROLE cannot be parameterized; the value is validated above.
		stmt := fmt.Sprintf("ALTER ROLE %s WITH PASSWORD '%s'", role, pw)
		if _, err := pool.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("set password for %s: %w", role, err)
		}
	}
	return nil
}

// DSNFromEnv reads the service's connection string, falling back to a local
// Postgres so `go run` works without a cluster.
func DSNFromEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
