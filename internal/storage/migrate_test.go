// External test package, not `package storage`: internal/dbtest imports
// storage.Migrate to build its own harness, so a same-package test file
// importing dbtest back would be an import cycle. Nothing here needs
// unexported access anyway; Migrate, Connect, and Store.Pool are all
// exported.
package storage_test

import (
	"context"
	"errors"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/6586x57890143/merlin/internal/dbtest"
	"github.com/6586x57890143/merlin/internal/storage"
)

// embeddedMigrationVersions reads the same migrations/ directory Migrate
// embeds, returning the version numbers on disk. Read independently from
// loadMigrations (which is unexported and already exercised implicitly by
// Migrate itself) so the expected count in the tests below is derived from
// the filesystem, the same source of truth the embed directive uses, rather
// than a number that has to be remembered and updated by hand every time a
// migration is added.
func embeddedMigrationVersions(t *testing.T) map[int64]bool {
	t.Helper()
	entries, err := os.ReadDir("migrations")
	if err != nil {
		t.Fatalf("read migrations dir: %v", err)
	}
	versions := make(map[int64]bool)
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".up.sql") {
			continue
		}
		prefix, _, ok := strings.Cut(name, "_")
		if !ok {
			t.Fatalf("migration %q missing version prefix", name)
		}
		v, err := strconv.ParseInt(prefix, 10, 64)
		if err != nil {
			t.Fatalf("migration %q has non-numeric version: %v", name, err)
		}
		versions[v] = true
	}
	if len(versions) == 0 {
		t.Fatal("found no .up.sql migrations on disk; migrations dir path is probably wrong")
	}
	return versions
}

// This repo's whole reason to run rotation is not retaining content longer
// than necessary, and that promise is enforced entirely by SQL: retention
// windows, the notice idempotency constraint, and now the disclosure CHECK.
// None of it had ever been run against a real database before this file,
// including the CHECK this test is built around.
func TestMigrateAppliesEveryEmbeddedMigration(t *testing.T) {
	pool := dbtest.FreshSchema(t)
	ctx := context.Background()

	if err := storage.Migrate(ctx, pool); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	rows, err := pool.Query(ctx, `SELECT version FROM schema_migrations`)
	if err != nil {
		t.Fatalf("query schema_migrations: %v", err)
	}
	defer rows.Close()

	applied := map[int64]bool{}
	for rows.Next() {
		var v int64
		if err := rows.Scan(&v); err != nil {
			t.Fatalf("scan version: %v", err)
		}
		applied[v] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate schema_migrations: %v", err)
	}

	want := embeddedMigrationVersions(t)
	for v := range want {
		if !applied[v] {
			t.Errorf("migration %d is embedded but was not applied", v)
		}
	}
	for v := range applied {
		if !want[v] {
			t.Errorf("schema_migrations recorded version %d, which has no matching .up.sql on disk", v)
		}
	}
}

// Migrate's own doc comment promises this: "safe to call on every startup".
// Nothing had ever exercised that promise against a real database, including
// across two consecutive calls in the same process, which is closer to what
// actually happens on every restart than a single call ever proves.
func TestMigrateIsIdempotent(t *testing.T) {
	pool := dbtest.FreshSchema(t)
	ctx := context.Background()

	if err := storage.Migrate(ctx, pool); err != nil {
		t.Fatalf("first Migrate: %v", err)
	}
	var firstCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM schema_migrations`).Scan(&firstCount); err != nil {
		t.Fatalf("count after first migrate: %v", err)
	}

	if err := storage.Migrate(ctx, pool); err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
	var secondCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM schema_migrations`).Scan(&secondCount); err != nil {
		t.Fatalf("count after second migrate: %v", err)
	}

	if secondCount != firstCount {
		t.Errorf("schema_migrations grew from %d to %d rows on a repeat Migrate call", firstCount, secondCount)
	}
}

// validRotationChannelInsert satisfies every NOT NULL column on
// settings_rotation_channels except the ones the test itself wants to vary,
// so each disclosure test only has to say what it's actually testing.
const validRotationChannelInsert = `
	INSERT INTO settings_rotation_channels
		(guild_id, channel_id, interval_minutes, archive_category_id, archive_visibility, disclosure)
	VALUES ('g1', 'c1', 60, 'cat1', 'mod_only', $1)
`

// The disclosure column exists because a guild's retention disclosure is a
// promise about what it publishes about itself, and a value the Go layer
// doesn't recognize has to be rejected at the database, not just by
// whichever command handler happened to validate it that day. This is the
// one migration in the repo with a CHECK constraint precisely so a
// hand-edited row or a future bypass of validateRotationChannel cannot
// silently change what a channel discloses. It was added in the same change
// that added this test file, and until now nothing had run it.
func TestDisclosureColumnRejectsAnUnknownValue(t *testing.T) {
	pool := dbtest.FreshSchema(t)
	ctx := context.Background()
	if err := storage.Migrate(ctx, pool); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	_, err := pool.Exec(ctx, validRotationChannelInsert, "everything")
	if err == nil {
		t.Fatal("an unrecognized disclosure value was accepted by the database")
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("expected a Postgres error, got %v (%T)", err, err)
	}
	// 23514 is check_violation. Anything else (a syntax error, a missing
	// column) would mean this test is not actually exercising the
	// constraint it claims to.
	if pgErr.Code != "23514" {
		t.Errorf("expected a check_violation (23514), got code %s: %v", pgErr.Code, pgErr)
	}
}

func TestDisclosureColumnAcceptsAllFourKnownValues(t *testing.T) {
	pool := dbtest.FreshSchema(t)
	ctx := context.Background()
	if err := storage.Migrate(ctx, pool); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	for _, mode := range []string{"full", "cadence", "retention", "generic"} {
		if _, err := pool.Exec(ctx,
			`INSERT INTO settings_rotation_channels
				(guild_id, channel_id, interval_minutes, archive_category_id, archive_visibility, disclosure)
			VALUES ('g1', $1, 60, 'cat1', 'mod_only', $2)`,
			"c-"+mode, mode,
		); err != nil {
			t.Errorf("disclosure %q was rejected: %v", mode, err)
		}
	}
}

// pingWithRetry backs off for up to ~10 attempts against an unreachable
// database, which is the right behavior for a container that has not
// finished starting yet and the wrong behavior for a caller with a bounded
// context: it must bail out on ctx cancellation rather than working through
// the whole retry budget regardless. Nothing had exercised that path before.
// Deliberately no TEST_DATABASE_URL dependency: a connection refused by a
// closed local port is immediate and needs no live Postgres to produce.
func TestConnectFailsWithoutHangingForever(t *testing.T) {
	// Port 1 is a reserved, unassignable port that nothing binds to, so the
	// connection attempt fails fast (refused) rather than timing out on its
	// own, which would defeat the point of asserting ctx bounds the wait.
	const unreachableDSN = "postgres://nobody:nobody@127.0.0.1:1/nowhere?sslmode=disable&connect_timeout=1"

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	start := time.Now()
	_, err := storage.Connect(ctx, unreachableDSN)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("Connect against an unreachable database returned no error")
	}
	// Generous margin over the 2s context budget: pingWithRetry's own
	// backoff step can overshoot a cancelled context by up to one select
	// iteration, and this is asserting "it did not run the full ~10-attempt,
	// multi-second backoff schedule," not timing it to the millisecond.
	if elapsed > 4*time.Second {
		t.Errorf("Connect took %v against a 2s context, did not respect cancellation", elapsed)
	}
}

func TestHealthyReflectsConnectivity(t *testing.T) {
	pool := dbtest.Pool(t)
	store := &storage.Store{Pool: pool}
	ctx := context.Background()

	if err := store.Healthy(ctx); err != nil {
		t.Errorf("Healthy on a live pool returned an error: %v", err)
	}

	pool.Close()
	if err := store.Healthy(ctx); err == nil {
		t.Error("Healthy on a closed pool returned nil")
	}
}

// Two migrators against one empty database, which is what actually happens:
// in production more than one instance can start at once, and in the tests
// every package using internal/dbtest migrates the same database while Go runs
// those packages in parallel.
//
// Without the advisory lock the version check is a check-then-act, so both
// callers see a version as unapplied, both run its DDL, and the loser fails on
// a duplicate object. That was a real CI flake, and it got likelier every time
// another package started using the shared test database: it surfaced as
// "duplicate key value violates unique constraint pg_type_typname_nsp_index"
// out of migration 0002, from a test that had nothing to do with migrations.
func TestMigrateIsSafeUnderConcurrency(t *testing.T) {
	pool := dbtest.FreshSchema(t)
	ctx := context.Background()

	const racers = 4
	errs := make(chan error, racers)
	var start sync.WaitGroup
	start.Add(1)
	for i := 0; i < racers; i++ {
		go func() {
			// Released together, so the calls genuinely overlap rather than
			// finishing one after another and never touching the bug.
			start.Wait()
			errs <- storage.Migrate(ctx, pool)
		}()
	}
	start.Done()

	for i := 0; i < racers; i++ {
		if err := <-errs; err != nil {
			t.Fatalf("concurrent migrator %d failed: %v", i, err)
		}
	}

	// And exactly one row per migration: a lock that serialised the runs but
	// let both record their work would leave duplicates behind.
	var versions, distinct int
	if err := pool.QueryRow(ctx,
		`SELECT count(*), count(DISTINCT version) FROM schema_migrations`).Scan(&versions, &distinct); err != nil {
		t.Fatalf("count applied migrations: %v", err)
	}
	if versions != distinct || versions == 0 {
		t.Fatalf("schema_migrations holds %d rows across %d versions", versions, distinct)
	}
}

// The lock has to be released back to the pool, or a pooled connection carries
// an orphaned session-level lock and every later migrator in the process
// blocks on it. Sequential calls after a completed run must simply return.
func TestMigrateReleasesItsLock(t *testing.T) {
	pool := dbtest.FreshSchema(t)
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		done := make(chan error, 1)
		go func() { done <- storage.Migrate(ctx, pool) }()
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("run %d: %v", i, err)
			}
		case <-time.After(30 * time.Second):
			t.Fatalf("run %d blocked, so the previous run did not release its lock", i)
		}
	}
}
