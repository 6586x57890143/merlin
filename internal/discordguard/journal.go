package discordguard

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// journalTimeout bounds each journal write. The journal is diagnostic, so it
// must never be what makes a Discord mutation hang.
const journalTimeout = 5 * time.Second

// Journal records destructive Discord calls as they are attempted.
//
// It is strictly a record. Nothing reads it to decide what to do, nothing
// retries from it, and plugin logic must never consult it — rotation and
// roles already re-derive their own idempotency from live Discord state, and
// a second source of truth driving recovery alongside the first would be two
// mechanisms that can disagree rather than one that works.
//
// An interface so Guard doesn't depend on Postgres in tests, matching the
// JobStateStore and ArchiveStore seams elsewhere in this codebase.
type Journal interface {
	// Begin records an attempt and returns its row ID. A zero ID means the
	// attempt wasn't recorded and Finish should do nothing.
	Begin(ctx context.Context, guildID, op, targetID string) (int64, error)
	// Finish closes out an attempt with its outcome.
	Finish(ctx context.Context, id int64, err error) error
	// PruneBefore deletes entries older than cutoff, returning how many.
	PruneBefore(ctx context.Context, cutoff time.Time) (int64, error)
}

// WithJournal attaches j to the guard. Optional: with no journal the guard
// behaves identically minus the record, which is what the unit tests use.
func (g *Guard) WithJournal(j Journal) *Guard {
	g.journal = j
	return g
}

// begin opens a journal entry, if journalling is enabled. A failure to
// record is logged and ignored: losing the diagnostic record of a mutation is
// bad, but refusing to perform the mutation because we couldn't write a note
// about it would be considerably worse.
func (o *GuildOps) beginJournal(op, targetID string) int64 {
	if o.guard.journal == nil {
		return 0
	}
	ctx, cancel := context.WithTimeout(context.Background(), journalTimeout)
	defer cancel()
	id, err := o.guard.journal.Begin(ctx, o.guildID, op, targetID)
	if err != nil {
		o.guard.log.Error("discordguard: journal begin failed", "guild", o.guildID, "op", op, "err", err)
		return 0
	}
	return id
}

func (o *GuildOps) finishJournal(id int64, err error) {
	if o.guard.journal == nil || id == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), journalTimeout)
	defer cancel()
	if ferr := o.guard.journal.Finish(ctx, id, err); ferr != nil {
		o.guard.log.Error("discordguard: journal finish failed", "guild", o.guildID, "id", id, "err", ferr)
	}
}

// PostgresJournal is the Journal backed by the action_journal table
// (migration 0015).
type PostgresJournal struct {
	pool *pgxpool.Pool
}

func NewPostgresJournal(pool *pgxpool.Pool) *PostgresJournal {
	return &PostgresJournal{pool: pool}
}

func (j *PostgresJournal) Begin(ctx context.Context, guildID, op, targetID string) (int64, error) {
	var id int64
	if err := j.pool.QueryRow(ctx, `
		INSERT INTO action_journal (guild_id, op, target_id, state)
		VALUES ($1, $2, $3, 'pending')
		RETURNING id`, guildID, op, targetID).Scan(&id); err != nil {
		return 0, fmt.Errorf("journal: begin: %w", err)
	}
	return id, nil
}

func (j *PostgresJournal) Finish(ctx context.Context, id int64, callErr error) error {
	state := "done"
	msg := ""
	if callErr != nil {
		state = "failed"
		msg = truncate(callErr.Error(), 500)
	}
	if _, err := j.pool.Exec(ctx, `
		UPDATE action_journal SET state = $2, error = $3, ended_at = now()
		WHERE id = $1`, id, state, msg); err != nil {
		return fmt.Errorf("journal: finish: %w", err)
	}
	return nil
}

func (j *PostgresJournal) PruneBefore(ctx context.Context, cutoff time.Time) (int64, error) {
	tag, err := j.pool.Exec(ctx, `DELETE FROM action_journal WHERE started_at < $1`, cutoff)
	if err != nil {
		return 0, fmt.Errorf("journal: prune: %w", err)
	}
	return tag.RowsAffected(), nil
}

// truncate keeps a stored error message bounded. Discord error bodies can be
// long, and this column is diagnostic, not evidentiary.
func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}
