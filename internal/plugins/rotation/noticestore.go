package rotation

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// NoticeStore records which pre-rotation notices have already been posted.
//
// Runtime state, so it is plugin-owned rather than living in
// internal/settings, which holds guild configuration only. Same split as
// ArchiveStore and roles' jail records.
type NoticeStore interface {
	// ClaimNotice records that the notice for rotationID's rotation at
	// noticeFor is being sent, and reports whether this caller is the one
	// that gets to send it.
	//
	// This is the whole idempotency mechanism, and it is a database
	// constraint rather than an in-process check on purpose: the notice job
	// runs every minute, and a slow run overlapping the next tick would
	// otherwise let two runs both see "not sent yet" and both post. An
	// INSERT that either wins or reports zero rows cannot do that regardless
	// of how the callers interleave.
	ClaimNotice(ctx context.Context, rotationID int64, noticeFor time.Time) (bool, error)

	// PruneNotices drops claims for rotations that have already happened.
	// They are only interesting until the rotation they refer to is past.
	PruneNotices(ctx context.Context, before time.Time) error
}

type pgNoticeStore struct{ pool *pgxpool.Pool }

func NewPostgresNoticeStore(pool *pgxpool.Pool) NoticeStore {
	return &pgNoticeStore{pool: pool}
}

func (s *pgNoticeStore) ClaimNotice(ctx context.Context, rotationID int64, noticeFor time.Time) (bool, error) {
	tag, err := s.pool.Exec(ctx, `
		INSERT INTO rotation_notices (rotation_id, notice_for) VALUES ($1, $2)
		ON CONFLICT (rotation_id, notice_for) DO NOTHING`, rotationID, noticeFor)
	if err != nil {
		return false, fmt.Errorf("rotation: claim notice: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

func (s *pgNoticeStore) PruneNotices(ctx context.Context, before time.Time) error {
	if _, err := s.pool.Exec(ctx, `DELETE FROM rotation_notices WHERE notice_for < $1`, before); err != nil {
		return fmt.Errorf("rotation: prune notices: %w", err)
	}
	return nil
}
