// Package dbtest is the shared harness for tests that need a real Postgres
// rather than an in-memory fake.
//
// Every Postgres-backed store in this repo (internal/settings,
// internal/storage, and the plugin-owned stores in internal/plugins/roles
// and internal/plugins/rotation, internal/scheduler, internal/discordguard,
// internal/audit) is deliberately built behind a narrow interface so its
// consumers can be unit tested against a tiny fake. That leaves the store
// itself untested against the one thing that actually enforces its
// contracts: Postgres's own constraints, its array/NULL semantics, and its
// ON CONFLICT behavior. This package exists to close that gap without
// pulling a container-orchestration dependency into go.mod: it is built
// entirely on pgx/v5, already the repo's only database dependency.
//
// Not a _test.go file, so it is importable from every package's test files
// while never entering the production binary (nothing outside a _test.go
// file imports it).
package dbtest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/6586x57890143/merlin/internal/storage"
)

// testDatabaseURLEnv is read by both Pool and FreshSchema. Not the same
// variable as the application's DATABASE_URL: this one is test-only,
// documented in CLAUDE.md, and never read outside this package.
const testDatabaseURLEnv = "TEST_DATABASE_URL"

// skipIfUnset is the first t.Skip in this repo, and deliberately so: every
// other package's tests run with zero setup, and a database-backed test
// suite that failed outright without a database would break that on every
// contributor's machine and in every environment that doesn't happen to
// have Postgres reachable. CI sets TEST_DATABASE_URL via a services:
// container (see .github/workflows/ci.yml) so the real assertions still run
// on every PR; anywhere else, skipping is the only choice that doesn't
// regress "go test ./... just works."
func skipIfUnset(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv(testDatabaseURLEnv)
	if dsn == "" {
		t.Skipf("%s not set, skipping Postgres-backed test (see CLAUDE.md)", testDatabaseURLEnv)
	}
	return dsn
}

// Pool connects to TEST_DATABASE_URL, runs storage.Migrate against it, and
// returns the pool with a t.Cleanup registered to close it. Skips the test
// if TEST_DATABASE_URL is unset.
//
// Migrate is idempotent by design ("safe to call on every startup," per its
// own doc comment), so calling it from every test that needs a pool is the
// right primitive here rather than migrating once behind a sync.Once: it
// costs one no-op pass over already-applied versions per test, and in
// exchange no test has to coordinate ordering with any other. Callers get
// one shared, already-migrated database, the same shape production runs
// against (one schema, many guilds), and are expected to give each test its
// own row space (a unique guild ID derived from t.Name() is the convention
// used throughout internal/settings's tests) rather than relying on
// isolation the pool does not provide.
func Pool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := skipIfUnset(t)
	ctx := context.Background()

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("dbtest: connect to %s: %v", testDatabaseURLEnv, err)
	}
	t.Cleanup(pool.Close)

	if err := storage.Migrate(ctx, pool); err != nil {
		t.Fatalf("dbtest: migrate: %v", err)
	}
	return pool
}

// FreshSchema returns a pool scoped to a brand-new, empty Postgres schema,
// for tests that need to observe storage.Migrate running against a database
// it has never touched. Pool's database is already migrated by the first
// test that calls it, so it cannot exercise "applies every migration from
// scratch"; this can. Skips if TEST_DATABASE_URL is unset.
//
// Isolation is via a fresh schema plus search_path, not a fresh database:
// creating a schema needs only CREATE on the already-connected database,
// which the configured role has by default, whereas CREATE DATABASE would
// need a superuser or an explicit CREATEDB grant that a deployment's
// TEST_DATABASE_URL role has no other reason to carry. Migrate's SQL never
// qualifies a table name, so setting search_path on every pooled connection
// (via AfterConnect, since the pool can open more than one) is sufficient
// for every statement it runs to land in the fresh schema.
func FreshSchema(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := skipIfUnset(t)
	ctx := context.Background()

	schema := randomSchemaName(t)

	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("dbtest: admin connect: %v", err)
	}
	if _, err := admin.Exec(ctx, `CREATE SCHEMA `+schema); err != nil {
		_ = admin.Close(ctx)
		t.Fatalf("dbtest: create schema %s: %v", schema, err)
	}
	if err := admin.Close(ctx); err != nil {
		t.Fatalf("dbtest: close admin connection: %v", err)
	}

	// Registered before the pool exists, so on test cleanup (LIFO) the
	// pool's own Close (registered second, below) runs first and the schema
	// drop runs second. Dropping a schema out from under an open connection
	// pool is the kind of ordering bug that only shows up as a flake under
	// load, so the order is pinned here rather than left to be discovered.
	t.Cleanup(func() {
		dropCtx := context.Background()
		conn, err := pgx.Connect(dropCtx, dsn)
		if err != nil {
			t.Logf("dbtest: could not reconnect to drop schema %s: %v", schema, err)
			return
		}
		defer func() { _ = conn.Close(dropCtx) }()
		if _, err := conn.Exec(dropCtx, `DROP SCHEMA IF EXISTS `+schema+` CASCADE`); err != nil {
			t.Logf("dbtest: drop schema %s: %v", schema, err)
		}
	})

	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("dbtest: parse config: %v", err)
	}
	config.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		_, err := conn.Exec(ctx, `SET search_path TO `+schema)
		return err
	}
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatalf("dbtest: connect pool for schema %s: %v", schema, err)
	}
	t.Cleanup(pool.Close)

	return pool
}

// randomSchemaName produces a name safe to interpolate directly into DDL
// without quoting: lowercase hex only, starting with a letter so it can
// never be mistaken for a numeric literal.
func randomSchemaName(t *testing.T) string {
	t.Helper()
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		t.Fatalf("dbtest: generate schema name: %v", err)
	}
	return fmt.Sprintf("dbtest_%s", hex.EncodeToString(buf))
}
