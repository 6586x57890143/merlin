package storage

import (
	"context"
	"embed"
	"fmt"
	"path"
	"sort"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.up.sql
var migrationFiles embed.FS

type migration struct {
	version int64
	name    string
	sql     string
}

// migrateLockKey namespaces this application's advisory lock. Arbitrary but
// fixed: any value works as long as nothing else in the database picks the
// same one, and it is spelled in hex so it is recognisable in pg_locks.
const migrateLockKey int64 = 0x6D65726C696E // "merlin"

// Migrate applies every embedded *.up.sql migration not yet recorded in
// schema_migrations, in ascending version order, each in its own
// transaction. Safe to call on every startup; already-applied versions are
// skipped.
//
// Safe to call concurrently, which it has to be for two separate reasons. In
// production more than one instance can start at once, and in the tests every
// package using internal/dbtest migrates the same database while Go runs those
// packages in parallel. The version check below is a check-then-act: without
// serialising, two callers both see a version as unapplied and both run its
// DDL, and the loser fails on a duplicate object rather than skipping. That
// surfaced as a flake in CI that got likelier every time another package
// started using the shared test database.
//
// A session-level advisory lock rather than a table lock or a transaction:
// each migration deliberately runs in its own transaction, so there is no
// single transaction to scope a lock to, and this serialises the whole run
// including the schema_migrations bookkeeping between them.
//
// Every statement below runs on the one connection holding that lock, which is
// not a stylistic choice. Holding a pooled connection for the lock while asking
// the pool for another to do the work deadlocks as soon as the number of
// concurrent migrators reaches the pool size: each one holds a connection and
// waits forever for a second. pgxpool sizes itself from the CPU count, so that
// is a bug that hides on a developer laptop and appears on a small CI runner,
// which is exactly how it was found. One connection per migrator means one is
// always enough to make progress.
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	migrations, err := loadMigrations()
	if err != nil {
		return err
	}

	// Pinned for the duration, and every statement below uses it. The lock is
	// session-scoped, so it has to be released explicitly before the
	// connection goes back to the pool: a pooled connection carrying an
	// orphaned advisory lock would block every later migrator in the process
	// for as long as the pool kept it.
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire migration lock connection: %w", err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, migrateLockKey); err != nil {
		return fmt.Errorf("take migration lock: %w", err)
	}
	defer func() {
		// Detached, so a cancelled or timed-out ctx still releases the lock
		// rather than leaving it held until the connection is closed. Same
		// reasoning as the Scheduler's post-run bookkeeping.
		if _, err := conn.Exec(context.WithoutCancel(ctx),
			`SELECT pg_advisory_unlock($1)`, migrateLockKey); err != nil {
			// Nothing useful to do about it here, and the lock dies with the
			// connection regardless, so this must not mask a real error from
			// the migration run itself.
			_ = err
		}
	}()

	// 0001_init creates schema_migrations itself, so it can't be gated on
	// that table's existence, so apply it unconditionally if the table is
	// missing, then let every later migration go through the normal check.
	if _, err := conn.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version BIGINT PRIMARY KEY,
		applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
	)`); err != nil {
		return fmt.Errorf("ensure schema_migrations: %w", err)
	}

	for _, m := range migrations {
		var applied bool
		if err := conn.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE version = $1)`, m.version).Scan(&applied); err != nil {
			return fmt.Errorf("check migration %d: %w", m.version, err)
		}
		if applied {
			continue
		}

		tx, err := conn.Begin(ctx)
		if err != nil {
			return fmt.Errorf("begin migration %d: %w", m.version, err)
		}
		if _, err := tx.Exec(ctx, m.sql); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("apply migration %d (%s): %w", m.version, m.name, err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO schema_migrations (version) VALUES ($1)`, m.version); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("record migration %d: %w", m.version, err)
		}
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("commit migration %d: %w", m.version, err)
		}
	}
	return nil
}

func loadMigrations() ([]migration, error) {
	entries, err := migrationFiles.ReadDir("migrations")
	if err != nil {
		return nil, fmt.Errorf("read migrations dir: %w", err)
	}

	migrations := make([]migration, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".up.sql") {
			continue
		}
		prefix, _, ok := strings.Cut(name, "_")
		if !ok {
			return nil, fmt.Errorf("migration %q missing version prefix", name)
		}
		version, err := strconv.ParseInt(prefix, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("migration %q has non-numeric version: %w", name, err)
		}
		content, err := migrationFiles.ReadFile(path.Join("migrations", name))
		if err != nil {
			return nil, fmt.Errorf("read migration %q: %w", name, err)
		}
		migrations = append(migrations, migration{version: version, name: name, sql: string(content)})
	}

	sort.Slice(migrations, func(i, j int) bool { return migrations[i].version < migrations[j].version })
	return migrations, nil
}
