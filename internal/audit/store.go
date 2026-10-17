package audit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/6586x57890143/cygnet/ledger"

	"github.com/6586x57890143/merlin/internal/core"
)

// Store is audit_log as a ledger.Store: each guild's rows since migration
// 0047 are one sha256 chain (seq, prev, hash), with the entry itself in
// entry. Rows from before it have no seq and are outside every chain; they
// are still pruned and still readable by SQL.
//
// The old columns are still written: actor_id, action and the call site's
// own old/new strings are the greppable copy, and the strings are the one
// thing the entry may have dropped on the way through toEntry.
type Store struct {
	pool *pgxpool.Pool
}

func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// Append is a ledger.Store append with no legacy strings.
func (s *Store) Append(ctx context.Context, e ledger.Entry) (ledger.Record, error) {
	return s.appendRaw(ctx, e, "", "")
}

// appendRaw writes e as the next record in its guild's chain. A per-guild
// advisory lock held for the transaction is what keeps two appends from
// reading the same tail, and the unique (guild_id, seq) index is the
// backstop if anything ever writes around it.
func (s *Store) appendRaw(ctx context.Context, e ledger.Entry, oldValue, newValue string) (ledger.Record, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return ledger.Record{}, fmt.Errorf("audit: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('audit_log:' || $1, 0))`, e.Guild); err != nil {
		return ledger.Record{}, fmt.Errorf("audit: lock chain: %w", err)
	}
	var last ledger.Record
	var hash []byte
	err = tx.QueryRow(ctx, `
		SELECT seq, hash FROM audit_log
		WHERE guild_id = $1 AND seq IS NOT NULL
		ORDER BY seq DESC LIMIT 1`, e.Guild).Scan(&last.Seq, &hash)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
	case err != nil:
		return ledger.Record{}, fmt.Errorf("audit: read chain tail: %w", err)
	default:
		copy(last.Hash[:], hash)
	}
	r := ledger.Next(last, ledger.Number(e, last.Seq+1))
	body, err := json.Marshal(r.Entry)
	if err != nil {
		return ledger.Record{}, fmt.Errorf("audit: encode entry: %w", err)
	}
	actor := r.Entry.Actor.ID
	if r.Entry.Actor.Kind == ledger.ActorAuto {
		actor = core.ActorSystem
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO audit_log (guild_id, actor_id, action, old_value, new_value, created_at, seq, entry, prev, hash)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
		r.Entry.Guild, actor, r.Entry.Action, oldValue, newValue, r.Entry.At, r.Seq, body, r.Prev[:], r.Hash[:]); err != nil {
		return ledger.Record{}, fmt.Errorf("audit: record: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return ledger.Record{}, fmt.Errorf("audit: commit: %w", err)
	}
	return r, nil
}

// Query answers a ledger.Filter: the action and subject tests run in SQL,
// so a page is a page and not a page of candidates. The subject test is
// Filter.Matches': the actor, any subject's ID, or a message subject in
// that channel.
func (s *Store) Query(ctx context.Context, f ledger.Filter) ([]ledger.Record, error) {
	return s.read(ctx, `
		SELECT seq, entry, prev, hash FROM audit_log
		WHERE guild_id = $1 AND seq IS NOT NULL
		  AND ($2 = 0 OR seq < $2)
		  AND ($3 = '' OR action = $3 OR (right($3, 1) = '.' AND starts_with(action, $3)))
		  AND ($4 = '' OR entry->'actor'->>'id' = $4 OR EXISTS (
		        SELECT 1 FROM jsonb_array_elements(entry->'subjects') sub
		        WHERE sub->>'id' = $4 OR starts_with(sub->>'id', $4 || '/')))
		ORDER BY seq DESC LIMIT $5`, f.Guild, f.Before, f.Action, f.Subject, f.Size())
}

// Range is guild's chain after seq after, oldest first.
func (s *Store) Range(ctx context.Context, guild string, after int64, n int) ([]ledger.Record, error) {
	return s.read(ctx, `
		SELECT seq, entry, prev, hash FROM audit_log
		WHERE guild_id = $1 AND seq > $2
		ORDER BY seq LIMIT $3`, guild, after, n)
}

func (s *Store) read(ctx context.Context, sql string, args ...any) ([]ledger.Record, error) {
	rows, err := s.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("audit: query: %w", err)
	}
	defer rows.Close()
	var out []ledger.Record
	for rows.Next() {
		var r ledger.Record
		var body, prev, hash []byte
		if err := rows.Scan(&r.Seq, &body, &prev, &hash); err != nil {
			return nil, fmt.Errorf("audit: scan: %w", err)
		}
		if err := json.Unmarshal(body, &r.Entry); err != nil {
			return nil, fmt.Errorf("audit: decode entry %d: %w", r.Seq, err)
		}
		copy(r.Prev[:], prev)
		copy(r.Hash[:], hash)
		out = append(out, r)
	}
	return out, rows.Err()
}
