package scheduler

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// JobState is a job's persisted execution state.
type JobState struct {
	// LastRun is the timestamp of the last successful completion. Only
	// meaningful when HasLastRun is true (a job that has never succeeded
	// has no last-run anchor to compute its next normal-cadence due time
	// from).
	LastRun    time.Time
	HasLastRun bool

	// LastAttempt is the timestamp of the last attempt, success or
	// failure, used as the backoff anchor while ConsecutiveFailures > 0.
	LastAttempt time.Time

	ConsecutiveFailures int
}

// JobStateStore is the narrow persistence seam the Scheduler depends on, so
// unit tests can use an in-memory fake instead of a live Postgres.
type JobStateStore interface {
	// Get returns the zero JobState (HasLastRun=false, ConsecutiveFailures=0)
	// for a job that has never been attempted.
	Get(ctx context.Context, jobKey string) (JobState, error)
	// RecordSuccess sets last_run=at, updated_at=at, and resets
	// consecutive_failures to 0.
	RecordSuccess(ctx context.Context, jobKey string, at time.Time) error
	// RecordFailure sets updated_at=at (last_run untouched) and increments
	// consecutive_failures, returning the new count.
	RecordFailure(ctx context.Context, jobKey string, at time.Time) (int, error)
}

type pgJobStateStore struct {
	pool *pgxpool.Pool
}

// NewPostgresJobStateStore backs JobStateStore with the
// scheduler_job_state table (migration 0002).
func NewPostgresJobStateStore(pool *pgxpool.Pool) JobStateStore {
	return &pgJobStateStore{pool: pool}
}

func (s *pgJobStateStore) Get(ctx context.Context, jobKey string) (JobState, error) {
	var (
		lastRun   *time.Time
		updatedAt time.Time
		failures  int
	)
	err := s.pool.QueryRow(ctx,
		`SELECT last_run, consecutive_failures, updated_at FROM scheduler_job_state WHERE job_key = $1`,
		jobKey,
	).Scan(&lastRun, &failures, &updatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return JobState{}, nil
		}
		return JobState{}, fmt.Errorf("get job state %q: %w", jobKey, err)
	}

	st := JobState{ConsecutiveFailures: failures, LastAttempt: updatedAt}
	if lastRun != nil {
		st.LastRun = *lastRun
		st.HasLastRun = true
	}
	return st, nil
}

func (s *pgJobStateStore) RecordSuccess(ctx context.Context, jobKey string, at time.Time) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO scheduler_job_state (job_key, last_run, consecutive_failures, updated_at)
		VALUES ($1, $2, 0, $2)
		ON CONFLICT (job_key) DO UPDATE
			SET last_run = $2, consecutive_failures = 0, updated_at = $2
	`, jobKey, at)
	if err != nil {
		return fmt.Errorf("record success %q: %w", jobKey, err)
	}
	return nil
}

func (s *pgJobStateStore) RecordFailure(ctx context.Context, jobKey string, at time.Time) (int, error) {
	var count int
	err := s.pool.QueryRow(ctx, `
		INSERT INTO scheduler_job_state (job_key, consecutive_failures, updated_at)
		VALUES ($1, 1, $2)
		ON CONFLICT (job_key) DO UPDATE
			SET consecutive_failures = scheduler_job_state.consecutive_failures + 1, updated_at = $2
		RETURNING consecutive_failures
	`, jobKey, at).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("record failure %q: %w", jobKey, err)
	}
	return count, nil
}
