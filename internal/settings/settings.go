// Package settings is the DB-backed replacement for config.yaml's
// guild-scoped fields (spec.MD §4a): mod roles, admins, permission
// whitelists, and rotation config are all mutated through Discord commands
// (internal/plugins/adminconfig, internal/plugins/rotation) and persisted
// here, not hand-edited on the host. config.yaml/.env are left holding only
// process bootstrap (Discord token, DB DSN, log level, the bootstrap admin
// user ID).
package settings

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/6586x57890143/merlin/internal/core"
)

// GuildSettings is a guild's core bot-wide settings.
type GuildSettings struct {
	GuildID               string
	ModRoleIDs            []string
	AdminUserIDs          []string
	AuditLogChannelID     string
	StatusChannelID       string
	OnboardingNudgeSentAt *time.Time // nil until adminconfig's one-time "run /config setup" nudge has actually been posted
	DisabledPlugins       []string   // plugin Name()s disabled in this guild — see core.PluginGate
}

// IsConfigured reports whether a guild has begun any real configuration —
// used to decide whether the onboarding nudge (adminconfig.NudgeIfUnconfigured)
// still needs to fire, and to drive /config setup's own "what's still
// missing" guidance.
func (g GuildSettings) IsConfigured() bool {
	return g.AuditLogChannelID != "" || g.StatusChannelID != "" || len(g.ModRoleIDs) > 0 || len(g.AdminUserIDs) > 0
}

// ActionOverride is a guild's customization of one Action: an optional tier
// override plus allow/deny lists, independent of the command's compiled-in
// PermSpec.Tier (see core.PermSpec/core.ActionPolicy/core.Permissions.Authorize).
type ActionOverride struct {
	Action       string
	RequiredTier core.PermTier // zero value (tierUnset) = no override, use the command's own PermSpec.Tier
	RoleIDs      []string      // allow
	UserIDs      []string      // allow
	DenyRoleIDs  []string
	DenyUserIDs  []string
}

// RotationChannel is one guild's configured rotating channel (spec.MD §6),
// the DB-backed replacement for config.yaml's rotating_channels[] entries.
// ID is a stable identity for this configured rotation slot, independent of
// ChannelID — rotation.rotate retargets ChannelID onto the new live channel
// after every successful rotation, but the Scheduler job tracking this
// slot's interval must key off something that never changes, or its
// persisted "last run" state resets every cycle (see migration 0009).
type RotationChannel struct {
	ID                      int64
	GuildID                 string
	ChannelID               string
	IntervalHours           int
	ArchiveCategoryID       string
	ArchiveVisibility       string // "mod_only" | "whitelist"
	ArchiveWhitelistRoleIDs []string
	ArchiveWhitelistUserIDs []string
	RetentionHours          *int // nil = keep forever, never swept
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

	row := s.pool.QueryRow(ctx, `SELECT mod_role_ids, admin_user_ids, audit_log_channel_id, status_channel_id, onboarding_nudge_sent_at, disabled_plugins
		FROM settings_guild WHERE guild_id = $1`, guildID)
	switch err := row.Scan(&gc.settings.ModRoleIDs, &gc.settings.AdminUserIDs, &gc.settings.AuditLogChannelID, &gc.settings.StatusChannelID, &gc.settings.OnboardingNudgeSentAt, &gc.settings.DisabledPlugins); err {
	case nil, pgx.ErrNoRows:
	default:
		return fmt.Errorf("settings: load guild %s: %w", guildID, err)
	}

	rows, err := s.pool.Query(ctx, `SELECT action, role_ids, user_ids, required_tier, deny_role_ids, deny_user_ids
		FROM settings_permission_overrides WHERE guild_id = $1`, guildID)
	if err != nil {
		return fmt.Errorf("settings: load overrides for %s: %w", guildID, err)
	}
	for rows.Next() {
		var o ActionOverride
		var requiredTier *int16
		if err := rows.Scan(&o.Action, &o.RoleIDs, &o.UserIDs, &requiredTier, &o.DenyRoleIDs, &o.DenyUserIDs); err != nil {
			rows.Close()
			return fmt.Errorf("settings: scan override for %s: %w", guildID, err)
		}
		if requiredTier != nil {
			o.RequiredTier = core.PermTier(*requiredTier)
		}
		gc.overrides[o.Action] = o
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("settings: iterate overrides for %s: %w", guildID, err)
	}
	rows.Close()

	rcRows, err := s.pool.Query(ctx, `SELECT id, channel_id, interval_hours, archive_category_id, archive_visibility,
		archive_whitelist_role_ids, archive_whitelist_user_ids, retention_hours, sticky_enabled, sticky_messages
		FROM settings_rotation_channels WHERE guild_id = $1`, guildID)
	if err != nil {
		return fmt.Errorf("settings: load rotation channels for %s: %w", guildID, err)
	}
	for rcRows.Next() {
		rc := RotationChannel{GuildID: guildID}
		if err := rcRows.Scan(&rc.ID, &rc.ChannelID, &rc.IntervalHours, &rc.ArchiveCategoryID, &rc.ArchiveVisibility,
			&rc.ArchiveWhitelistRoleIDs, &rc.ArchiveWhitelistUserIDs, &rc.RetentionHours, &rc.StickyEnabled, &rc.StickyMessages); err != nil {
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

// ActionPolicy satisfies core.GuildAuthData: guildID's customization of
// action (tier override, allow-list, deny-list), or the zero value if the
// guild hasn't customized this action at all.
func (s *Store) ActionPolicy(guildID, action string) core.ActionPolicy {
	gc := s.guild(guildID)
	o, ok := gc.overrides[action]
	if !ok {
		return core.ActionPolicy{}
	}
	return core.ActionPolicy{
		RequiredTier: o.RequiredTier,
		AllowRoleIDs: o.RoleIDs,
		AllowUserIDs: o.UserIDs,
		DenyRoleIDs:  o.DenyRoleIDs,
		DenyUserIDs:  o.DenyUserIDs,
	}
}

// DisabledPlugins returns the plugin Name()s disabled in guildID.
func (s *Store) DisabledPlugins(guildID string) []string {
	return s.guild(guildID).settings.DisabledPlugins
}

// PluginEnabled satisfies core.PluginGate.
func (s *Store) PluginEnabled(guildID, pluginName string) bool {
	return !slices.Contains(s.DisabledPlugins(guildID), pluginName)
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

// RotationChannelByID looks up a rotation config by its stable ID rather
// than its current (mutable) ChannelID — used by rotation's Scheduler job
// closures, which must keep re-resolving the same logical rotation slot
// across retargets. A guild's rotation count is always small, so a linear
// scan over the already-cached slice needs no dedicated index.
func (s *Store) RotationChannelByID(guildID string, id int64) (RotationChannel, bool) {
	for _, rc := range s.RotationChannels(guildID) {
		if rc.ID == id {
			return rc, true
		}
	}
	return RotationChannel{}, false
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

// MarkOnboardingNudgeSent records that adminconfig's one-time "run /config
// setup" nudge has actually been posted for guildID, so NudgeIfUnconfigured
// never re-sends it. Callers should only call this after a successful
// channel post — leaving it unmarked on failure lets a transient permission
// issue self-heal on the bot's next restart instead of silently giving up.
func (s *Store) MarkOnboardingNudgeSent(ctx context.Context, guildID string) error {
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO settings_guild (guild_id, onboarding_nudge_sent_at, updated_at) VALUES ($1, now(), now())
		ON CONFLICT (guild_id) DO UPDATE SET onboarding_nudge_sent_at = now(), updated_at = now()`,
		guildID); err != nil {
		return fmt.Errorf("settings: mark onboarding nudge sent: %w", err)
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

// SetActionTier sets a per-guild override of action's required tier,
// replacing the command's own compiled-in PermSpec.Tier for that action in
// this guild only (core.Permissions.Authorize applies it). tier must be
// core.TierMod or core.TierAdmin — TierAdmin-only at the command layer, see
// internal/plugins/adminconfig.
func (s *Store) SetActionTier(ctx context.Context, guildID, action string, tier core.PermTier) error {
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO settings_permission_overrides (guild_id, action, required_tier, updated_at)
		VALUES ($1, $2, $3, now())
		ON CONFLICT (guild_id, action) DO UPDATE SET required_tier = $3, updated_at = now()`,
		guildID, action, int16(tier)); err != nil {
		return fmt.Errorf("settings: set action tier: %w", err)
	}
	if err := s.Refresh(ctx, guildID); err != nil {
		return err
	}
	s.publishChanged(ctx, guildID)
	return nil
}

// ClearActionTier resets action back to its command's compiled-in
// PermSpec.Tier, undoing a prior SetActionTier for this guild.
func (s *Store) ClearActionTier(ctx context.Context, guildID, action string) error {
	if _, err := s.pool.Exec(ctx, `
		UPDATE settings_permission_overrides SET required_tier = NULL, updated_at = now()
		WHERE guild_id = $1 AND action = $2`,
		guildID, action); err != nil {
		return fmt.Errorf("settings: clear action tier: %w", err)
	}
	if err := s.Refresh(ctx, guildID); err != nil {
		return err
	}
	s.publishChanged(ctx, guildID)
	return nil
}

// DenyOverride adds roleID and/or userID (either may be empty) to action's
// deny-list — see core.Permissions.Authorize for why deny always wins over
// tier/Administrator-bit/allow. TierAdmin-only at the command layer.
func (s *Store) DenyOverride(ctx context.Context, guildID, action, roleID, userID string) error {
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO settings_permission_overrides (guild_id, action, deny_role_ids, deny_user_ids, updated_at)
		VALUES ($1, $2,
			CASE WHEN $3 = '' THEN '{}'::TEXT[] ELSE ARRAY[$3] END,
			CASE WHEN $4 = '' THEN '{}'::TEXT[] ELSE ARRAY[$4] END,
			now())
		ON CONFLICT (guild_id, action) DO UPDATE SET
			deny_role_ids = CASE WHEN $3 = '' THEN settings_permission_overrides.deny_role_ids
				ELSE (SELECT array_agg(DISTINCT r) FROM unnest(settings_permission_overrides.deny_role_ids || $3) AS r) END,
			deny_user_ids = CASE WHEN $4 = '' THEN settings_permission_overrides.deny_user_ids
				ELSE (SELECT array_agg(DISTINCT r) FROM unnest(settings_permission_overrides.deny_user_ids || $4) AS r) END,
			updated_at = now()`,
		guildID, action, roleID, userID); err != nil {
		return fmt.Errorf("settings: deny override: %w", err)
	}
	if err := s.Refresh(ctx, guildID); err != nil {
		return err
	}
	s.publishChanged(ctx, guildID)
	return nil
}

func (s *Store) UndenyOverride(ctx context.Context, guildID, action, roleID, userID string) error {
	if _, err := s.pool.Exec(ctx, `
		UPDATE settings_permission_overrides SET
			deny_role_ids = CASE WHEN $3 = '' THEN deny_role_ids ELSE array_remove(deny_role_ids, $3) END,
			deny_user_ids = CASE WHEN $4 = '' THEN deny_user_ids ELSE array_remove(deny_user_ids, $4) END,
			updated_at = now()
		WHERE guild_id = $1 AND action = $2`,
		guildID, action, roleID, userID); err != nil {
		return fmt.Errorf("settings: undeny override: %w", err)
	}
	if err := s.Refresh(ctx, guildID); err != nil {
		return err
	}
	s.publishChanged(ctx, guildID)
	return nil
}

// DisablePlugin/EnablePlugin toggle a whole plugin off/on for guildID (see
// core.PluginGate) — coarser than any per-action policy, checked by
// core.CommandRouter before a disabled plugin's commands are even
// authorized. internal/plugins/adminconfig itself must never be passed here
// (enforced at the command layer, not here) — disabling it would
// permanently lock a guild out of ever re-enabling anything.
func (s *Store) DisablePlugin(ctx context.Context, guildID, pluginName string) error {
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO settings_guild (guild_id, disabled_plugins, updated_at) VALUES ($1, ARRAY[$2], now())
		ON CONFLICT (guild_id) DO UPDATE SET
			disabled_plugins = (SELECT array_agg(DISTINCT r) FROM unnest(settings_guild.disabled_plugins || $2) AS r),
			updated_at = now()`,
		guildID, pluginName); err != nil {
		return fmt.Errorf("settings: disable plugin: %w", err)
	}
	if err := s.Refresh(ctx, guildID); err != nil {
		return err
	}
	s.publishChanged(ctx, guildID)
	return nil
}

func (s *Store) EnablePlugin(ctx context.Context, guildID, pluginName string) error {
	if _, err := s.pool.Exec(ctx, `
		UPDATE settings_guild SET disabled_plugins = array_remove(disabled_plugins, $2), updated_at = now()
		WHERE guild_id = $1`,
		guildID, pluginName); err != nil {
		return fmt.Errorf("settings: enable plugin: %w", err)
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
	// A nil Go slice binds as SQL NULL via pgx, which every array column here
	// rejects (NOT NULL DEFAULT '{}') — the DEFAULT only applies when a
	// column is omitted entirely, not when NULL is explicitly passed, and
	// this INSERT always lists every column. Callers building a
	// RotationChannel from scratch (e.g. rotation's /rotation configure add,
	// which only sets the fields its own options cover) leave these nil by
	// default, so normalize here once rather than requiring every caller to
	// remember.
	if rc.ArchiveWhitelistRoleIDs == nil {
		rc.ArchiveWhitelistRoleIDs = []string{}
	}
	if rc.ArchiveWhitelistUserIDs == nil {
		rc.ArchiveWhitelistUserIDs = []string{}
	}
	if rc.StickyMessages == nil {
		rc.StickyMessages = []string{}
	}
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO settings_rotation_channels (guild_id, channel_id, interval_hours, archive_category_id,
			archive_visibility, archive_whitelist_role_ids, archive_whitelist_user_ids, retention_hours,
			sticky_enabled, sticky_messages, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10, now())
		ON CONFLICT (guild_id, channel_id) DO UPDATE SET
			interval_hours = $3, archive_category_id = $4, archive_visibility = $5,
			archive_whitelist_role_ids = $6, archive_whitelist_user_ids = $7, retention_hours = $8,
			sticky_enabled = $9, sticky_messages = $10, updated_at = now()`,
		rc.GuildID, rc.ChannelID, rc.IntervalHours, rc.ArchiveCategoryID, rc.ArchiveVisibility,
		rc.ArchiveWhitelistRoleIDs, rc.ArchiveWhitelistUserIDs, rc.RetentionHours, rc.StickyEnabled, rc.StickyMessages,
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

// RetargetRotationChannel repoints a rotation config from oldChannelID to
// newChannelID in place, preserving every other column (interval, archive
// settings, sticky messages, ...) — used by rotation.rotate immediately
// after a successful swap, since oldChannelID has just become an archived
// channel and must never be looked up again as a live rotation target.
// (guild_id, channel_id) is the row's key, so this is a plain UPDATE rather
// than the delete+upsert dance a naive read-modify-write would need.
func (s *Store) RetargetRotationChannel(ctx context.Context, guildID, oldChannelID, newChannelID string) error {
	if _, err := s.pool.Exec(ctx, `
		UPDATE settings_rotation_channels SET channel_id = $3, updated_at = now()
		WHERE guild_id = $1 AND channel_id = $2`,
		guildID, oldChannelID, newChannelID); err != nil {
		return fmt.Errorf("settings: retarget rotation channel: %w", err)
	}
	if err := s.Refresh(ctx, guildID); err != nil {
		return err
	}
	s.publishChanged(ctx, guildID)
	return nil
}
