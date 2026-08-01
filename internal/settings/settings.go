// Package settings is the DB-backed replacement for config.yaml's
// guild-scoped fields (spec.MD §4a): mod roles, admins, permission
// whitelists, and rotation config are all mutated through Discord commands
// (internal/plugins/adminconfig, internal/plugins/rotation) and persisted
// here, not hand-edited on the host. config.yaml/.env are left holding only
// process bootstrap (Discord token, DB DSN, log level, the break-glass
// admin user ID).
package settings

import (
	"context"
	"fmt"
	"sync"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/6586x57890143/merlin/internal/core"
)

// GuildSettings is a guild's core bot-wide settings.
type GuildSettings struct {
	GuildID           string
	ModRoleIDs        []string
	AdminUserIDs      []string
	AuditLogChannelID string
	StatusChannelID   string
}

// ActionOverride is a per-action whitelist grant, independent of mod/admin
// tier (see core.PermSpec/core.Permissions.Authorize).
type ActionOverride struct {
	Action  string
	RoleIDs []string
	UserIDs []string
}

// RotationChannel is one guild's configured rotating channel (spec.MD §6),
// the DB-backed replacement for config.yaml's rotating_channels[] entries.
type RotationChannel struct {
	GuildID                 string
	ChannelID               string
	IntervalHours           int
	ArchiveCategoryID       string
	ArchiveVisibility       string // "mod_only" | "whitelist"
	ArchiveWhitelistRoleIDs []string
	ArchiveWhitelistUserIDs []string
	RetentionDays           *int // nil = keep forever, never swept
	StickyEnabled           bool
	StickyMessages          []string
}

type guildCache struct {
	settings  GuildSettings
	overrides map[string]ActionOverride  // by action
	rotations map[string]RotationChannel // by channel ID
}

// Store is the DB-backed settings store, with an in-memory read cache
// refreshed on every mutation and on demand (Refresh) — mirrors
// config.Loader's own reload-into-RWMutex-guarded-snapshot pattern, just
// sourced from Postgres instead of a YAML file, so it can be written to by
// running commands.
type Store struct {
	pool *pgxpool.Pool
	bus  *core.EventBus

	mu    sync.RWMutex
	cache map[string]*guildCache // by guild ID
}

func New(pool *pgxpool.Pool, bus *core.EventBus) *Store {
	return &Store{pool: pool, bus: bus, cache: make(map[string]*guildCache)}
}

// Refresh reloads guildID's settings from Postgres into the in-memory
// cache. Call it once per guild at startup (for every guild the bot is
// currently in) and again whenever the bot joins a new guild; every
// mutating method below also calls it after writing, so the cache is never
// more than one round-trip stale.
func (s *Store) Refresh(ctx context.Context, guildID string) error {
	gc := &guildCache{
		settings:  GuildSettings{GuildID: guildID},
		overrides: make(map[string]ActionOverride),
		rotations: make(map[string]RotationChannel),
	}

	row := s.pool.QueryRow(ctx, `SELECT mod_role_ids, admin_user_ids, audit_log_channel_id, status_channel_id
		FROM settings_guild WHERE guild_id = $1`, guildID)
	switch err := row.Scan(&gc.settings.ModRoleIDs, &gc.settings.AdminUserIDs, &gc.settings.AuditLogChannelID, &gc.settings.StatusChannelID); err {
	case nil, pgx.ErrNoRows:
	default:
		return fmt.Errorf("settings: load guild %s: %w", guildID, err)
	}

	rows, err := s.pool.Query(ctx, `SELECT action, role_ids, user_ids FROM settings_permission_overrides WHERE guild_id = $1`, guildID)
	if err != nil {
		return fmt.Errorf("settings: load overrides for %s: %w", guildID, err)
	}
	for rows.Next() {
		var o ActionOverride
		if err := rows.Scan(&o.Action, &o.RoleIDs, &o.UserIDs); err != nil {
			rows.Close()
			return fmt.Errorf("settings: scan override for %s: %w", guildID, err)
		}
		gc.overrides[o.Action] = o
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("settings: iterate overrides for %s: %w", guildID, err)
	}
	rows.Close()

	rcRows, err := s.pool.Query(ctx, `SELECT channel_id, interval_hours, archive_category_id, archive_visibility,
		archive_whitelist_role_ids, archive_whitelist_user_ids, retention_days, sticky_enabled, sticky_messages
		FROM settings_rotation_channels WHERE guild_id = $1`, guildID)
	if err != nil {
		return fmt.Errorf("settings: load rotation channels for %s: %w", guildID, err)
	}
	for rcRows.Next() {
		rc := RotationChannel{GuildID: guildID}
		if err := rcRows.Scan(&rc.ChannelID, &rc.IntervalHours, &rc.ArchiveCategoryID, &rc.ArchiveVisibility,
			&rc.ArchiveWhitelistRoleIDs, &rc.ArchiveWhitelistUserIDs, &rc.RetentionDays, &rc.StickyEnabled, &rc.StickyMessages); err != nil {
			rcRows.Close()
			return fmt.Errorf("settings: scan rotation channel for %s: %w", guildID, err)
		}
		gc.rotations[rc.ChannelID] = rc
	}
	if err := rcRows.Err(); err != nil {
		return fmt.Errorf("settings: iterate rotation channels for %s: %w", guildID, err)
	}
	rcRows.Close()

	s.mu.Lock()
	s.cache[guildID] = gc
	s.mu.Unlock()
	return nil
}

func (s *Store) guild(guildID string) *guildCache {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if gc, ok := s.cache[guildID]; ok {
		return gc
	}
	return &guildCache{settings: GuildSettings{GuildID: guildID}}
}

// --- core.GuildAuthData ---

func (s *Store) ModRoleIDs(guildID string) []string   { return s.guild(guildID).settings.ModRoleIDs }
func (s *Store) AdminUserIDs(guildID string) []string { return s.guild(guildID).settings.AdminUserIDs }

func (s *Store) ActionOverride(guildID, action string) (roleIDs, userIDs []string) {
	gc := s.guild(guildID)
	if gc.overrides == nil {
		return nil, nil
	}
	o, ok := gc.overrides[action]
	if !ok {
		return nil, nil
	}
	return o.RoleIDs, o.UserIDs
}

// --- read accessors beyond GuildAuthData ---

func (s *Store) GuildSettings(guildID string) GuildSettings { return s.guild(guildID).settings }

// AuditLogChannelID is the narrow accessor internal/audit needs — it only
// ever wants this one field of a guild's settings.
func (s *Store) AuditLogChannelID(guildID string) string {
	return s.guild(guildID).settings.AuditLogChannelID
}

// StatusChannelID is the narrow accessor internal/scheduler needs for
// alerting on repeated job failure.
func (s *Store) StatusChannelID(guildID string) string {
	return s.guild(guildID).settings.StatusChannelID
}

func (s *Store) Overrides(guildID string) []ActionOverride {
	gc := s.guild(guildID)
	out := make([]ActionOverride, 0, len(gc.overrides))
	for _, o := range gc.overrides {
		out = append(out, o)
	}
	return out
}

func (s *Store) RotationChannels(guildID string) []RotationChannel {
	gc := s.guild(guildID)
	out := make([]RotationChannel, 0, len(gc.rotations))
	for _, rc := range gc.rotations {
		out = append(out, rc)
	}
	return out
}

func (s *Store) RotationChannel(guildID, channelID string) (RotationChannel, bool) {
	gc := s.guild(guildID)
	rc, ok := gc.rotations[channelID]
	return rc, ok
}

// --- mutations ---
//
// Every mutation writes to Postgres, calls Refresh so the cache reflects it
// immediately, and publishes core.EventConfigChanged so any other
// in-process subscriber (none yet; wired for future plugins) knows to
// re-derive its own view — spec.MD Design Principle 4, "config changes are
// audited, not silent." Callers (adminconfig, rotation command handlers) are
// responsible for the actual audit-log write via Deps.Audit, since only they
// know the human-readable old/new values worth recording.

func (s *Store) publishChanged(ctx context.Context, guildID string) {
	s.bus.Publish(ctx, core.Event{Type: core.EventConfigChanged, GuildID: guildID})
}

func (s *Store) AddModRole(ctx context.Context, guildID, roleID string) error {
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO settings_guild (guild_id, mod_role_ids, updated_at) VALUES ($1, ARRAY[$2], now())
		ON CONFLICT (guild_id) DO UPDATE SET
			mod_role_ids = (SELECT array_agg(DISTINCT r) FROM unnest(settings_guild.mod_role_ids || $2) AS r),
			updated_at = now()`,
		guildID, roleID); err != nil {
		return fmt.Errorf("settings: add mod role: %w", err)
	}
	if err := s.Refresh(ctx, guildID); err != nil {
		return err
	}
	s.publishChanged(ctx, guildID)
	return nil
}

func (s *Store) RemoveModRole(ctx context.Context, guildID, roleID string) error {
	if _, err := s.pool.Exec(ctx, `
		UPDATE settings_guild SET mod_role_ids = array_remove(mod_role_ids, $2), updated_at = now()
		WHERE guild_id = $1`, guildID, roleID); err != nil {
		return fmt.Errorf("settings: remove mod role: %w", err)
	}
	if err := s.Refresh(ctx, guildID); err != nil {
		return err
	}
	s.publishChanged(ctx, guildID)
	return nil
}

func (s *Store) AddAdmin(ctx context.Context, guildID, userID string) error {
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO settings_guild (guild_id, admin_user_ids, updated_at) VALUES ($1, ARRAY[$2], now())
		ON CONFLICT (guild_id) DO UPDATE SET
			admin_user_ids = (SELECT array_agg(DISTINCT r) FROM unnest(settings_guild.admin_user_ids || $2) AS r),
			updated_at = now()`,
		guildID, userID); err != nil {
		return fmt.Errorf("settings: add admin: %w", err)
	}
	if err := s.Refresh(ctx, guildID); err != nil {
		return err
	}
	s.publishChanged(ctx, guildID)
	return nil
}

func (s *Store) RemoveAdmin(ctx context.Context, guildID, userID string) error {
	if _, err := s.pool.Exec(ctx, `
		UPDATE settings_guild SET admin_user_ids = array_remove(admin_user_ids, $2), updated_at = now()
		WHERE guild_id = $1`, guildID, userID); err != nil {
		return fmt.Errorf("settings: remove admin: %w", err)
	}
	if err := s.Refresh(ctx, guildID); err != nil {
		return err
	}
	s.publishChanged(ctx, guildID)
	return nil
}

func (s *Store) SetAuditLogChannel(ctx context.Context, guildID, channelID string) error {
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO settings_guild (guild_id, audit_log_channel_id, updated_at) VALUES ($1, $2, now())
		ON CONFLICT (guild_id) DO UPDATE SET audit_log_channel_id = $2, updated_at = now()`,
		guildID, channelID); err != nil {
		return fmt.Errorf("settings: set audit log channel: %w", err)
	}
	if err := s.Refresh(ctx, guildID); err != nil {
		return err
	}
	s.publishChanged(ctx, guildID)
	return nil
}

func (s *Store) SetStatusChannel(ctx context.Context, guildID, channelID string) error {
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO settings_guild (guild_id, status_channel_id, updated_at) VALUES ($1, $2, now())
		ON CONFLICT (guild_id) DO UPDATE SET status_channel_id = $2, updated_at = now()`,
		guildID, channelID); err != nil {
		return fmt.Errorf("settings: set status channel: %w", err)
	}
	if err := s.Refresh(ctx, guildID); err != nil {
		return err
	}
	s.publishChanged(ctx, guildID)
	return nil
}

// GrantOverride adds roleID and/or userID (either may be empty) to action's
// whitelist. TierAdmin-only at the command layer — see
// internal/plugins/adminconfig.
func (s *Store) GrantOverride(ctx context.Context, guildID, action, roleID, userID string) error {
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO settings_permission_overrides (guild_id, action, role_ids, user_ids, updated_at)
		VALUES ($1, $2,
			CASE WHEN $3 = '' THEN '{}'::TEXT[] ELSE ARRAY[$3] END,
			CASE WHEN $4 = '' THEN '{}'::TEXT[] ELSE ARRAY[$4] END,
			now())
		ON CONFLICT (guild_id, action) DO UPDATE SET
			role_ids = CASE WHEN $3 = '' THEN settings_permission_overrides.role_ids
				ELSE (SELECT array_agg(DISTINCT r) FROM unnest(settings_permission_overrides.role_ids || $3) AS r) END,
			user_ids = CASE WHEN $4 = '' THEN settings_permission_overrides.user_ids
				ELSE (SELECT array_agg(DISTINCT r) FROM unnest(settings_permission_overrides.user_ids || $4) AS r) END,
			updated_at = now()`,
		guildID, action, roleID, userID); err != nil {
		return fmt.Errorf("settings: grant override: %w", err)
	}
	if err := s.Refresh(ctx, guildID); err != nil {
		return err
	}
	s.publishChanged(ctx, guildID)
	return nil
}

func (s *Store) RevokeOverride(ctx context.Context, guildID, action, roleID, userID string) error {
	if _, err := s.pool.Exec(ctx, `
		UPDATE settings_permission_overrides SET
			role_ids = CASE WHEN $3 = '' THEN role_ids ELSE array_remove(role_ids, $3) END,
			user_ids = CASE WHEN $4 = '' THEN user_ids ELSE array_remove(user_ids, $4) END,
			updated_at = now()
		WHERE guild_id = $1 AND action = $2`,
		guildID, action, roleID, userID); err != nil {
		return fmt.Errorf("settings: revoke override: %w", err)
	}
	if err := s.Refresh(ctx, guildID); err != nil {
		return err
	}
	s.publishChanged(ctx, guildID)
	return nil
}

// UpsertRotationChannel inserts or fully replaces one rotating channel's
// config — callers read-modify-write via RotationChannel/RotationChannels
// above, then call this with the whole updated struct (mirrors the simple
// full-replace pattern already used by internal/plugins/rotation's other
// state, not a field-by-field setter for every option).
func (s *Store) UpsertRotationChannel(ctx context.Context, rc RotationChannel) error {
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO settings_rotation_channels (guild_id, channel_id, interval_hours, archive_category_id,
			archive_visibility, archive_whitelist_role_ids, archive_whitelist_user_ids, retention_days,
			sticky_enabled, sticky_messages, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10, now())
		ON CONFLICT (guild_id, channel_id) DO UPDATE SET
			interval_hours = $3, archive_category_id = $4, archive_visibility = $5,
			archive_whitelist_role_ids = $6, archive_whitelist_user_ids = $7, retention_days = $8,
			sticky_enabled = $9, sticky_messages = $10, updated_at = now()`,
		rc.GuildID, rc.ChannelID, rc.IntervalHours, rc.ArchiveCategoryID, rc.ArchiveVisibility,
		rc.ArchiveWhitelistRoleIDs, rc.ArchiveWhitelistUserIDs, rc.RetentionDays, rc.StickyEnabled, rc.StickyMessages,
	); err != nil {
		return fmt.Errorf("settings: upsert rotation channel: %w", err)
	}
	if err := s.Refresh(ctx, rc.GuildID); err != nil {
		return err
	}
	s.publishChanged(ctx, rc.GuildID)
	return nil
}

func (s *Store) RemoveRotationChannel(ctx context.Context, guildID, channelID string) error {
	if _, err := s.pool.Exec(ctx, `DELETE FROM settings_rotation_channels WHERE guild_id = $1 AND channel_id = $2`,
		guildID, channelID); err != nil {
		return fmt.Errorf("settings: remove rotation channel: %w", err)
	}
	if err := s.Refresh(ctx, guildID); err != nil {
		return err
	}
	s.publishChanged(ctx, guildID)
	return nil
}
