package rotation

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// ArchiveRecord tracks one archived (rotated-out) channel awaiting
// eventual permanent deletion.
//
// RotationID links back to the settings_rotation_channels row that produced
// this archive (migration 0013). It is the sweep's authority for *when* this
// channel may be deleted: the deadline is re-derived on every pass as
// ArchivedAt + that slot's current RetentionHours, so changing retention
// applies to archives that already exist, not just future ones. Nil for rows
// written before migration 0013, which fall back to DeleteAfter.
//
// DeleteAfter is the deadline as computed at archive time. It is retained as
// the fallback for pre-0013 rows and for archives whose rotation slot has
// since been removed entirely, but it is no longer authoritative on its own;
// see archiveDeadline in sweep.go.
//
// ArchiveCategoryID is recorded at archive time (not re-derived from
// settings.RotationChannel at sweep time) so sweep's "was this channel
// rescued out of its archive category" check stays correct even after
// rotation.rotate retargets the live config's ChannelID onto the new channel.
// See migration 0008's comment for why a live settings lookup keyed by
// SourceChannelID stopped working.
type ArchiveRecord struct {
	ChannelID         string
	GuildID           string
	SourceChannelID   string
	ArchiveCategoryID string
	RotationID        *int64
	ArchivedAt        time.Time
	DeleteAfter       *time.Time
}

// ArchiveStore is the narrow persistence seam for the sweep-based permanent
// deletion design (see the Milestone 3 plan): rather than teach the
// Scheduler about one-shot jobs, Rotation tracks pending deletions itself
// and sweeps them on a normal recurring Scheduler job.
type ArchiveStore interface {
	Insert(ctx context.Context, rec ArchiveRecord) error
	// ListForGuild returns every archive still tracked for guildID,
	// deliberately *not* a "give me the due ones" query. Due-ness now depends
	// on the live retention setting rather than on a column frozen at archive
	// time, so the decision belongs in Go where that setting is readable (see
	// archiveDeadline). A guild's tracked archives are bounded by rotation
	// frequency times retention, so this stays a small result set.
	ListForGuild(ctx context.Context, guildID string) ([]ArchiveRecord, error)
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
		INSERT INTO rotation_archives (channel_id, guild_id, source_channel_id, archive_category_id, rotation_id, archived_at, delete_after)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (channel_id) DO UPDATE
			SET archive_category_id = $4, rotation_id = $5, archived_at = $6, delete_after = $7
	`, rec.ChannelID, rec.GuildID, rec.SourceChannelID, rec.ArchiveCategoryID, rec.RotationID, rec.ArchivedAt, rec.DeleteAfter)
	if err != nil {
		return fmt.Errorf("rotation archive store: insert: %w", err)
	}
	return nil
}

func (s *pgArchiveStore) ListForGuild(ctx context.Context, guildID string) ([]ArchiveRecord, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT channel_id, guild_id, source_channel_id, archive_category_id, rotation_id, archived_at, delete_after
		FROM rotation_archives
		WHERE guild_id = $1
	`, guildID)
	if err != nil {
		return nil, fmt.Errorf("rotation archive store: list for guild: %w", err)
	}
	defer rows.Close()

	var out []ArchiveRecord
	for rows.Next() {
		var rec ArchiveRecord
		if err := rows.Scan(&rec.ChannelID, &rec.GuildID, &rec.SourceChannelID, &rec.ArchiveCategoryID, &rec.RotationID, &rec.ArchivedAt, &rec.DeleteAfter); err != nil {
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
