package rotation

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// ArchiveRecord tracks one archived (rotated-out) channel awaiting
// eventual permanent deletion. DeleteAfter is nil for "keep forever"
// archives, which the sweep never selects. ArchiveCategoryID is recorded at
// archive time (not re-derived from settings.RotationChannel at sweep time)
// so sweep's "was this channel rescued out of its archive category" check
// stays correct even after rotation.rotate retargets the live config's
// ChannelID onto the new channel — see migration 0008's comment for why a
// live settings lookup keyed by SourceChannelID stopped working.
type ArchiveRecord struct {
	ChannelID         string
	GuildID           string
	SourceChannelID   string
	ArchiveCategoryID string
	ArchivedAt        time.Time
	DeleteAfter       *time.Time
}

// ArchiveStore is the narrow persistence seam for the sweep-based permanent
// deletion design (see the Milestone 3 plan): rather than teach the
// Scheduler about one-shot jobs, Rotation tracks pending deletions itself
// and sweeps them on a normal recurring Scheduler job.
type ArchiveStore interface {
	Insert(ctx context.Context, rec ArchiveRecord) error
	DueForDeletion(ctx context.Context, guildID string, now time.Time) ([]ArchiveRecord, error)
	Delete(ctx context.Context, channelID string) error
}

type pgArchiveStore struct {
	pool *pgxpool.Pool
}

// NewPostgresArchiveStore backs ArchiveStore with the rotation_archives
// table (migration 0004).
func NewPostgresArchiveStore(pool *pgxpool.Pool) ArchiveStore {
	return &pgArchiveStore{pool: pool}
}

func (s *pgArchiveStore) Insert(ctx context.Context, rec ArchiveRecord) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO rotation_archives (channel_id, guild_id, source_channel_id, archive_category_id, archived_at, delete_after)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (channel_id) DO UPDATE
			SET archive_category_id = $4, archived_at = $5, delete_after = $6
	`, rec.ChannelID, rec.GuildID, rec.SourceChannelID, rec.ArchiveCategoryID, rec.ArchivedAt, rec.DeleteAfter)
	if err != nil {
		return fmt.Errorf("rotation archive store: insert: %w", err)
	}
	return nil
}

func (s *pgArchiveStore) DueForDeletion(ctx context.Context, guildID string, now time.Time) ([]ArchiveRecord, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT channel_id, guild_id, source_channel_id, archive_category_id, archived_at, delete_after
		FROM rotation_archives
		WHERE guild_id = $1 AND delete_after IS NOT NULL AND delete_after <= $2
	`, guildID, now)
	if err != nil {
		return nil, fmt.Errorf("rotation archive store: due for deletion: %w", err)
	}
	defer rows.Close()

	var out []ArchiveRecord
	for rows.Next() {
		var rec ArchiveRecord
		if err := rows.Scan(&rec.ChannelID, &rec.GuildID, &rec.SourceChannelID, &rec.ArchiveCategoryID, &rec.ArchivedAt, &rec.DeleteAfter); err != nil {
			return nil, fmt.Errorf("rotation archive store: scan: %w", err)
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rotation archive store: rows: %w", err)
	}
	return out, nil
}

func (s *pgArchiveStore) Delete(ctx context.Context, channelID string) error {
	if _, err := s.pool.Exec(ctx, `DELETE FROM rotation_archives WHERE channel_id = $1`, channelID); err != nil {
		return fmt.Errorf("rotation archive store: delete: %w", err)
	}
	return nil
}
