package storage

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Pool sizing for a single-instance bot: the workload is a handful of
// short settings/scheduler queries, never a request-per-user fan-out.
const (
	maxConns        = 10
	minConns        = 1
	maxConnIdleTime = 5 * time.Minute
	maxConnLifetime = time.Hour
)

// Postgres and the bot start together (docker compose, a host reboot), and
// the bot routinely loses the race. Retrying a bounded number of times turns
// "operator restarts the container by hand" into "it comes up on its own".
const (
	connectRetries    = 10
	connectRetryBase  = 500 * time.Millisecond
	connectRetryLimit = 5 * time.Second
)

// Store wraps the shared connection pool. Domain-specific queries (audit,
// rotation, factions) are added by the milestones that need them; this
// pass only establishes connectivity and a health check.
type Store struct {
	Pool *pgxpool.Pool
}

func Connect(ctx context.Context, dsn string) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse postgres dsn: %w", err)
	}
	cfg.MaxConns = maxConns
	cfg.MinConns = minConns
	cfg.MaxConnIdleTime = maxConnIdleTime
	cfg.MaxConnLifetime = maxConnLifetime

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("connect postgres: %w", err)
	}

	if err := pingWithRetry(ctx, pool); err != nil {
		pool.Close()
		return nil, err
	}
	return &Store{Pool: pool}, nil
}

func pingWithRetry(ctx context.Context, pool *pgxpool.Pool) error {
	var err error
	for attempt := range connectRetries {
		if err = pool.Ping(ctx); err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return fmt.Errorf("ping postgres: %w", ctx.Err())
		}
		delay := min(connectRetryBase<<attempt, connectRetryLimit)
		select {
		case <-ctx.Done():
			return fmt.Errorf("ping postgres: %w", ctx.Err())
		case <-time.After(delay):
		}
	}
	return fmt.Errorf("ping postgres after %d attempts: %w", connectRetries, err)
}

// Healthy reports whether the pool can still reach Postgres. Used by
// /config status so an operator can tell a DB outage apart from a bot bug
// without shell access to the host.
func (s *Store) Healthy(ctx context.Context) error {
	return s.Pool.Ping(ctx)
}

func (s *Store) Close() {
	s.Pool.Close()
}
