// Package roles implements temporary role management (spec.MD §4 design
// principles): "jail" a member (snapshot and strip their current roles,
// restoring them later, automatically or on demand) and single-role timed
// grants. Mirrors internal/plugins/rotation's shape end to end: a
// plugin-owned Postgres store for runtime state (pending reversals aren't
// guild configuration, so they don't belong in internal/settings.Store —
// see rotation.ArchiveStore for the precedent), a per-guild Scheduler sweep
// job that finds due work and re-derives idempotency from live Discord
// state rather than trusting stored assumptions, and full audit logging.
package roles

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// JailRecord tracks one member currently jailed in a guild: the roles they
// held before being stripped (so release can restore exactly what was
// removed), and the marker role applied in their place.
type JailRecord struct {
	GuildID         string
	UserID          string
	SnapshotRoleIDs []string
	JailRoleID      string
	JailedAt        time.Time
	ReleaseAt       *time.Time // nil = indefinite, never auto-released
	JailedBy        string
	Reason          string
}

// GrantRecord tracks one role Merlin herself granted to a member, optionally
// with an expiry. A member can hold several independent tracked grants at
// once (unlike a jail, which is one all-or-nothing state per member).
type GrantRecord struct {
	ID        int64
	GuildID   string
	UserID    string
	RoleID    string
	GrantedAt time.Time
	ExpiresAt *time.Time // nil = permanent, never auto-removed
	GrantedBy string
	Reason    string
}

// Store is the narrow persistence seam for this plugin's own runtime
// state — mirrors rotation.ArchiveStore's role: pending future actions
// tracked here, not in internal/settings (guild configuration only).
type Store interface {
	InsertJail(ctx context.Context, rec JailRecord) error
	GetJail(ctx context.Context, guildID, userID string) (JailRecord, bool, error)
	DeleteJail(ctx context.Context, guildID, userID string) error
	DueJails(ctx context.Context, guildID string, now time.Time) ([]JailRecord, error)

	InsertGrant(ctx context.Context, rec GrantRecord) error
	GetGrant(ctx context.Context, guildID, userID, roleID string) (GrantRecord, bool, error)
	DeleteGrant(ctx context.Context, guildID, userID, roleID string) error
	DueGrants(ctx context.Context, guildID string, now time.Time) ([]GrantRecord, error)
	ListGrants(ctx context.Context, guildID, userID string) ([]GrantRecord, error)
}

type pgStore struct {
	pool *pgxpool.Pool
}

// NewPostgresStore backs Store with the role_jails/role_grants tables
// (migration 0011).
func NewPostgresStore(pool *pgxpool.Pool) Store {
	return &pgStore{pool: pool}
}

func (s *pgStore) InsertJail(ctx context.Context, rec JailRecord) error {
	if rec.SnapshotRoleIDs == nil {
		rec.SnapshotRoleIDs = []string{}
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO role_jails (guild_id, user_id, snapshot_role_ids, jail_role_id, jailed_at, release_at, jailed_by, reason)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
		ON CONFLICT (guild_id, user_id) DO UPDATE SET
			snapshot_role_ids = $3, jail_role_id = $4, jailed_at = $5, release_at = $6, jailed_by = $7, reason = $8
	`, rec.GuildID, rec.UserID, rec.SnapshotRoleIDs, rec.JailRoleID, rec.JailedAt, rec.ReleaseAt, rec.JailedBy, rec.Reason)
	if err != nil {
		return fmt.Errorf("roles store: insert jail: %w", err)
	}
	return nil
}

func (s *pgStore) GetJail(ctx context.Context, guildID, userID string) (JailRecord, bool, error) {
	rec := JailRecord{GuildID: guildID, UserID: userID}
	err := s.pool.QueryRow(ctx, `
		SELECT snapshot_role_ids, jail_role_id, jailed_at, release_at, jailed_by, reason
		FROM role_jails WHERE guild_id = $1 AND user_id = $2
	`, guildID, userID).Scan(&rec.SnapshotRoleIDs, &rec.JailRoleID, &rec.JailedAt, &rec.ReleaseAt, &rec.JailedBy, &rec.Reason)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return JailRecord{}, false, nil
		}
		return JailRecord{}, false, fmt.Errorf("roles store: get jail: %w", err)
	}
	return rec, true, nil
}

func (s *pgStore) DeleteJail(ctx context.Context, guildID, userID string) error {
	if _, err := s.pool.Exec(ctx, `DELETE FROM role_jails WHERE guild_id = $1 AND user_id = $2`, guildID, userID); err != nil {
		return fmt.Errorf("roles store: delete jail: %w", err)
	}
	return nil
}

func (s *pgStore) DueJails(ctx context.Context, guildID string, now time.Time) ([]JailRecord, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT user_id, snapshot_role_ids, jail_role_id, jailed_at, release_at, jailed_by, reason
		FROM role_jails WHERE guild_id = $1 AND release_at IS NOT NULL AND release_at <= $2
	`, guildID, now)
	if err != nil {
		return nil, fmt.Errorf("roles store: due jails: %w", err)
	}
	defer rows.Close()

	var out []JailRecord
	for rows.Next() {
		rec := JailRecord{GuildID: guildID}
		if err := rows.Scan(&rec.UserID, &rec.SnapshotRoleIDs, &rec.JailRoleID, &rec.JailedAt, &rec.ReleaseAt, &rec.JailedBy, &rec.Reason); err != nil {
			return nil, fmt.Errorf("roles store: scan due jail: %w", err)
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("roles store: iterate due jails: %w", err)
	}
	return out, nil
}

func (s *pgStore) InsertGrant(ctx context.Context, rec GrantRecord) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO role_grants (guild_id, user_id, role_id, granted_at, expires_at, granted_by, reason)
		VALUES ($1,$2,$3,$4,$5,$6,$7)
		ON CONFLICT (guild_id, user_id, role_id) DO UPDATE SET
			granted_at = $4, expires_at = $5, granted_by = $6, reason = $7
	`, rec.GuildID, rec.UserID, rec.RoleID, rec.GrantedAt, rec.ExpiresAt, rec.GrantedBy, rec.Reason)
	if err != nil {
		return fmt.Errorf("roles store: insert grant: %w", err)
	}
	return nil
}

func (s *pgStore) GetGrant(ctx context.Context, guildID, userID, roleID string) (GrantRecord, bool, error) {
	rec := GrantRecord{GuildID: guildID, UserID: userID, RoleID: roleID}
	err := s.pool.QueryRow(ctx, `
		SELECT id, granted_at, expires_at, granted_by, reason
		FROM role_grants WHERE guild_id = $1 AND user_id = $2 AND role_id = $3
	`, guildID, userID, roleID).Scan(&rec.ID, &rec.GrantedAt, &rec.ExpiresAt, &rec.GrantedBy, &rec.Reason)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return GrantRecord{}, false, nil
		}
		return GrantRecord{}, false, fmt.Errorf("roles store: get grant: %w", err)
	}
	return rec, true, nil
}

func (s *pgStore) DeleteGrant(ctx context.Context, guildID, userID, roleID string) error {
	if _, err := s.pool.Exec(ctx, `DELETE FROM role_grants WHERE guild_id = $1 AND user_id = $2 AND role_id = $3`,
		guildID, userID, roleID); err != nil {
		return fmt.Errorf("roles store: delete grant: %w", err)
	}
	return nil
}

func (s *pgStore) DueGrants(ctx context.Context, guildID string, now time.Time) ([]GrantRecord, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, user_id, role_id, granted_at, expires_at, granted_by, reason
		FROM role_grants WHERE guild_id = $1 AND expires_at IS NOT NULL AND expires_at <= $2
	`, guildID, now)
	if err != nil {
		return nil, fmt.Errorf("roles store: due grants: %w", err)
	}
	defer rows.Close()

	var out []GrantRecord
	for rows.Next() {
		rec := GrantRecord{GuildID: guildID}
		if err := rows.Scan(&rec.ID, &rec.UserID, &rec.RoleID, &rec.GrantedAt, &rec.ExpiresAt, &rec.GrantedBy, &rec.Reason); err != nil {
			return nil, fmt.Errorf("roles store: scan due grant: %w", err)
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("roles store: iterate due grants: %w", err)
	}
	return out, nil
}

func (s *pgStore) ListGrants(ctx context.Context, guildID, userID string) ([]GrantRecord, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, role_id, granted_at, expires_at, granted_by, reason
		FROM role_grants WHERE guild_id = $1 AND user_id = $2
	`, guildID, userID)
	if err != nil {
		return nil, fmt.Errorf("roles store: list grants: %w", err)
	}
	defer rows.Close()

	var out []GrantRecord
	for rows.Next() {
		rec := GrantRecord{GuildID: guildID, UserID: userID}
		if err := rows.Scan(&rec.ID, &rec.RoleID, &rec.GrantedAt, &rec.ExpiresAt, &rec.GrantedBy, &rec.Reason); err != nil {
			return nil, fmt.Errorf("roles store: scan grant: %w", err)
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("roles store: iterate grants: %w", err)
	}
	return out, nil
}
