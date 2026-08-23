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
	"database/sql"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/6586x57890143/merlin/internal/core"
)

// staleRetryInterval is how often StartRetry re-reads guilds whose cache was
// dropped by a failed refresh. Short enough that a blip doesn't leave a guild
// on fail-closed defaults for long, long enough that a sustained outage isn't
// hammered.
const staleRetryInterval = 30 * time.Second

// GuildSettings is a guild's core bot-wide settings.
type GuildSettings struct {
	GuildID               string
	ModRoleIDs            []string
	AdminUserIDs          []string
	AuditLogChannelID     string
	StatusChannelID       string
	OnboardingNudgeSentAt *time.Time // nil until adminconfig's one-time "run /config setup" nudge has actually been posted
	DisabledPlugins       []string   // plugin Name()s disabled in this guild (see core.PluginGate)

	// JailAllowedChannelIDs is which channels stay visible to a jailed
	// member (internal/plugins/roles). Every other channel gets a deny
	// overwrite for the shared "Jailed" role. Empty means jail denies every
	// channel until a mod configures exceptions.
	JailAllowedChannelIDs []string
	// ArchiveViewerRoleIDs is which extra roles can see rotation's archive
	// channels, on top of ModRoleIDs and anyone holding Discord's own
	// Administrator bit. Guild-scoped rather than per rotating channel
	// because archive permissions live on the archive category, which
	// several rotating channels can share (migration 0020).
	ArchiveViewerRoleIDs []string
	// Optional pre-configured jail marker role ID. If set, roles plugin will
	// use this existing role as the marker rather than creating a new one.
	// Pointer so NULL in the DB scans correctly; accessor JailMarkerRoleID
	// returns the empty string when none is configured.
	JailMarkerRoleID *string

	// WritesPaused and WritesDryRun are this guild's emergency controls over
	// destructive Discord actions, enforced centrally by
	// internal/discordguard. Paused refuses every mutating call; dry-run
	// performs the full decision-making and audit trail but touches nothing.
	// Neither affects read/inspect commands, so an admin can always still see
	// what the bot thinks is going on while it is stopped.
	WritesPaused bool
	WritesDryRun bool
}

// IsConfigured reports whether a guild has begun any real configuration,
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
// ChannelID. rotation.rotate retargets ChannelID onto the new live channel
// after every successful rotation, but the Scheduler job tracking this
// slot's interval must key off something that never changes, or its
// persisted "last run" state resets every cycle (see migration 0009).
type RotationChannel struct {
	ID                      int64
	GuildID                 string
	ChannelID               string
	IntervalMinutes         int
	ArchiveCategoryID       string
	ArchiveVisibility       string // "mod_only" | "whitelist"
	ArchiveWhitelistRoleIDs []string
	ArchiveWhitelistUserIDs []string
	RetentionHours          *int // nil = keep forever, never swept
	StickyEnabled           bool
	StickyMessages          []string
	// NoticeLeadMinutes is how long before a rotation to warn the channel.
	// 0 disables the notice for this slot. Validation keeps it strictly
	// below the interval: a lead longer than the gap between rotations
	// would mean the warning for the next one arrives before the previous
	// one has happened.
	NoticeLeadMinutes int
	// Disclosure is how much the channel is told about its own rotation
	// (migration 0018). Empty is read as DisclosureFull; UpsertRotationChannel
	// normalizes it so a zero-valued struct can never write an illegal value.
	Disclosure Disclosure
}

// Disclosure is how much a freshly rotated channel is told about the rotation
// that just happened, and about how long the archived copy survives.
//
// This is the guild's published retention policy, so it is a setting rather
// than a wording choice: stating the deletion window is worth doing in a
// server that wants to point at what the bot actually does, and worth
// withholding in one that would rather not advertise the schedule. The voice
// catalog varies how each of these is said; it can never vary which facts
// appear, because internal/voice requires the matching placeholders in every
// single line of the corresponding key.
type Disclosure string

const (
	// DisclosureFull states the rotation cadence and the archival window.
	// The default, and what every channel did before migration 0018.
	DisclosureFull Disclosure = "full"
	// DisclosureCadence states only how often the channel resets, saying
	// nothing about what happens to the archived copy.
	DisclosureCadence Disclosure = "cadence"
	// DisclosureRetention states only how long the archive survives, saying
	// nothing about the rotation schedule.
	DisclosureRetention Disclosure = "retention"
	// DisclosureGeneric states neither: just that the channel has rotated.
	DisclosureGeneric Disclosure = "generic"
)

// Valid reports whether d is one of the four known modes. The empty value is
// deliberately *not* valid: callers that mean "unspecified" should resolve it
// to DisclosureFull explicitly (see Resolve), so that a mode arriving from a
// corrupt row or a hand-edited database is caught rather than silently read
// as the most disclosing option.
func (d Disclosure) Valid() bool {
	switch d {
	case DisclosureFull, DisclosureCadence, DisclosureRetention, DisclosureGeneric:
		return true
	}
	return false
}

// Resolve maps the empty value onto the default, leaving anything else
// (including an unrecognized value) untouched for Valid to reject.
func (d Disclosure) Resolve() Disclosure {
	if d == "" {
		return DisclosureFull
	}
	return d
}

type guildCache struct {
	settings  GuildSettings
	overrides map[string]ActionOverride  // by action
	rotations map[string]RotationChannel // by channel ID
}

// Store is the DB-backed settings store, with an in-memory read cache
// refreshed on every mutation and on demand (Refresh). Mirrors
// config.Loader's own reload-into-RWMutex-guarded-snapshot pattern, just
// sourced from Postgres instead of a YAML file, so it can be written to by
// running commands.
type Store struct {
	pool *pgxpool.Pool
	bus  *core.EventBus

	mu    sync.RWMutex
	cache map[string]*guildCache // by guild ID
	// stale holds guilds whose cache entry was dropped because a refresh
	// failed. RetryStale works through them in the background. Without it,
	// nothing would ever re-read them and the guild would stay locked into
	// fail-closed defaults until the next mutation or restart.
	stale map[string]bool
}

func New(pool *pgxpool.Pool, bus *core.EventBus) *Store {
	return &Store{
		pool:  pool,
		bus:   bus,
		cache: make(map[string]*guildCache),
		stale: make(map[string]bool),
	}
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

	row := s.pool.QueryRow(ctx, `SELECT mod_role_ids, admin_user_ids, audit_log_channel_id, status_channel_id, onboarding_nudge_sent_at, disabled_plugins, jail_allowed_channel_ids, jail_marker_role_id, writes_paused, writes_dry_run, archive_viewer_role_ids
		FROM settings_guild WHERE guild_id = $1`, guildID)
	var marker sql.NullString
	switch err := row.Scan(&gc.settings.ModRoleIDs, &gc.settings.AdminUserIDs, &gc.settings.AuditLogChannelID, &gc.settings.StatusChannelID, &gc.settings.OnboardingNudgeSentAt, &gc.settings.DisabledPlugins, &gc.settings.JailAllowedChannelIDs, &marker, &gc.settings.WritesPaused, &gc.settings.WritesDryRun, &gc.settings.ArchiveViewerRoleIDs); err {
	case nil, pgx.ErrNoRows:
		if marker.Valid {
			v := marker.String
			gc.settings.JailMarkerRoleID = &v
		} else {
			gc.settings.JailMarkerRoleID = nil
		}
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

	rcRows, err := s.pool.Query(ctx, `SELECT id, channel_id, interval_minutes, archive_category_id, archive_visibility,
		archive_whitelist_role_ids, archive_whitelist_user_ids, retention_hours, sticky_enabled, sticky_messages,
		notice_lead_minutes, disclosure
		FROM settings_rotation_channels WHERE guild_id = $1`, guildID)
	if err != nil {
		return fmt.Errorf("settings: load rotation channels for %s: %w", guildID, err)
	}
	for rcRows.Next() {
		rc := RotationChannel{GuildID: guildID}
		// Scanned through a plain string rather than straight into
		// rc.Disclosure. pgx does handle a named string type, but it reaches
		// it by reflection over the underlying type rather than by a
		// registered codec, and nothing in this repo tests against a real
		// database. The blast radius of being wrong is out of proportion to
		// the tidiness: a failing Scan fails Refresh, which drops the guild
		// to fail-closed defaults and, on the GuildCreate path, skips
		// rotation's reconcile entirely. One conversion avoids the question.
		var disclosure string
		if err := rcRows.Scan(&rc.ID, &rc.ChannelID, &rc.IntervalMinutes, &rc.ArchiveCategoryID, &rc.ArchiveVisibility,
			&rc.ArchiveWhitelistRoleIDs, &rc.ArchiveWhitelistUserIDs, &rc.RetentionHours, &rc.StickyEnabled, &rc.StickyMessages,
			&rc.NoticeLeadMinutes, &disclosure); err != nil {
			rcRows.Close()
			return fmt.Errorf("settings: scan rotation channel for %s: %w", guildID, err)
		}
		rc.Disclosure = Disclosure(disclosure)
		gc.rotations[rc.ChannelID] = rc
	}
	if err := rcRows.Err(); err != nil {
		return fmt.Errorf("settings: iterate rotation channels for %s: %w", guildID, err)
	}
	rcRows.Close()

	s.mu.Lock()
	s.cache[guildID] = gc
	delete(s.stale, guildID)
	s.mu.Unlock()
	return nil
}

// invalidate drops guildID's cached settings after a failed refresh, rather
// than leaving the pre-mutation copy in place.
//
// Leaving it was a quiet fail-open. Every mutator writes to Postgres and
// then refreshes; if that refresh failed, the row had already changed but
// the cache still answered with the old values, and nothing ever retried,
// so a /config permissions deny could be committed to the database and go on
// being ignored by Authorize indefinitely, with the only evidence a single
// error return the admin may well have read as "it didn't work."
//
// Dropping the entry instead means reads fall back to zero-value defaults:
// no mod roles, no admins, no overrides. That is fail-closed for Mod and
// Admin tiers, and deliberately still leaves the bootstrap identity and
// Discord's own Administrator bit working, so a guild can never be locked
// out of /config by a database blip.
func (s *Store) invalidate(guildID string) {
	s.mu.Lock()
	delete(s.cache, guildID)
	s.stale[guildID] = true
	s.mu.Unlock()
}

// MarkStale queues guildID for re-read by RetryStale without any mutation
// having happened.
//
// The mutation paths reach the same set through invalidate, but a guild whose
// very first load failed never went through a mutator, so nothing had it
// queued: the failure was logged once at GuildCreate and then forgotten for
// the life of the process. Exported so cmd/bot/main.go can hand a guild it
// could not load to the retry loop instead of dropping it.
func (s *Store) MarkStale(guildID string) { s.invalidate(guildID) }

// Forget drops guildID from the cache entirely, without marking it stale,
// for when the bot has left the guild and there is nothing to retry. The
// Postgres rows are deliberately left alone: being removed from a server is
// often temporary (a kick and re-invite, a permissions mistake), and
// deleting a guild's mod roles, admins, permission policy, and rotation
// config on the way out would turn that into a silent, unrecoverable wipe.
func (s *Store) Forget(guildID string) {
	s.mu.Lock()
	delete(s.cache, guildID)
	delete(s.stale, guildID)
	s.mu.Unlock()
}

// RetryStale re-reads every guild whose cache was dropped by a failed
// refresh, returning the number still stale afterwards. Called on a ticker
// by StartRetry.
func (s *Store) RetryStale(ctx context.Context) int {
	s.mu.RLock()
	pending := make([]string, 0, len(s.stale))
	for guildID := range s.stale {
		pending = append(pending, guildID)
	}
	s.mu.RUnlock()

	remaining := 0
	for _, guildID := range pending {
		if err := s.Refresh(ctx, guildID); err != nil {
			remaining++
			continue
		}
		// Recovering the cache is only half the job: while the guild was
		// unreadable every consumer saw fail-closed defaults, and plugins that
		// derive registered work from settings (rotation.reconcile) acted on
		// them. They rebuild from EventConfigChanged, and nothing else is going
		// to publish one: a successful retry is not a mutation. Without this a
		// guild recovered its settings but kept whatever job set it had
		// computed while it had none.
		s.publishChanged(ctx, guildID)
	}
	return remaining
}

// StartRetry runs RetryStale until ctx is cancelled. A guild is only in the
// stale set because the database was unreachable at exactly the wrong
// moment, so this is idle almost always; it exists so recovery doesn't wait
// on the next mutation or a restart.
func (s *Store) StartRetry(ctx context.Context, log *slog.Logger) {
	go func() {
		ticker := time.NewTicker(staleRetryInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if remaining := s.RetryStale(ctx); remaining > 0 {
					log.Warn("settings: guilds still unreadable, running on fail-closed defaults", "count", remaining)
				}
			}
		}
	}()
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

// JailAllowedChannelIDs satisfies roles.JailChannelConfig, the narrow view
// internal/plugins/roles depends on for its jail channel-visibility
// allowlist.
func (s *Store) JailAllowedChannelIDs(guildID string) []string {
	return s.guild(guildID).settings.JailAllowedChannelIDs
}

// ArchiveViewerRoleIDs satisfies rotation.SettingsProvider: the extra roles
// allowed to see this guild's archive channels, on top of the mod roles.
func (s *Store) ArchiveViewerRoleIDs(guildID string) []string {
	return s.guild(guildID).settings.ArchiveViewerRoleIDs
}

// JailMarkerRoleID returns the configured jail marker role for the guild,
// if any. Empty string when none configured.
func (s *Store) JailMarkerRoleID(guildID string) string {
	v := s.guild(guildID).settings.JailMarkerRoleID
	if v == nil {
		return ""
	}
	return *v
}

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

// WritesPaused and WritesDryRun together satisfy discordguard's GuildGate.
// Both read through the same in-memory cache as every other setting, so a
// pause takes effect on the next destructive call with no DB round trip.
func (s *Store) WritesPaused(guildID string) bool {
	return s.guild(guildID).settings.WritesPaused
}

func (s *Store) WritesDryRun(guildID string) bool {
	return s.guild(guildID).settings.WritesDryRun
}

// --- read accessors beyond GuildAuthData ---

func (s *Store) GuildSettings(guildID string) GuildSettings { return s.guild(guildID).settings }

// AuditLogChannelID is the narrow accessor internal/audit needs: it only
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
// than its current (mutable) ChannelID, used by rotation's Scheduler job
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
// re-derive its own view. This is spec.MD Design Principle 4, "config changes are
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
		s.invalidate(guildID)
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
		s.invalidate(guildID)
		return err
	}
	s.publishChanged(ctx, guildID)
	return nil
}

// PruneDeletedRole removes roleID from everywhere a guild's settings can
// still be pointing at it: the mod-role list, and every action's allow and
// deny lists. It returns a description of what it actually removed, empty
// when the role was not referenced, so a caller can audit a real change
// without writing an entry every time anybody deletes any role.
//
// Deleting a role in Discord tells this bot nothing on its own, so without
// this the entries sit there permanently. Snowflakes are never reused, so a
// stale entry cannot silently grant a stranger anything later. The cost is
// to the operator instead: /config permissions list accumulates rules
// naming roles that no longer exist, and the one time somebody reads that
// list carefully is while working out why a person can or cannot do
// something, which is exactly when a screenful of dead entries is most
// expensive.
//
// Built on the ordinary mutators rather than one bulk statement so each
// removal takes the same SQL, cache refresh, and EventConfigChanged path a
// mod-initiated removal takes. A deleted role is usually in none or one of
// these lists, so the extra round trips cost nothing in practice.
func (s *Store) PruneDeletedRole(ctx context.Context, guildID, roleID string) ([]string, error) {
	var removed []string

	if slices.Contains(s.ModRoleIDs(guildID), roleID) {
		if err := s.RemoveModRole(ctx, guildID, roleID); err != nil {
			return removed, err
		}
		removed = append(removed, "mod role")
	}

	if slices.Contains(s.ArchiveViewerRoleIDs(guildID), roleID) {
		if err := s.RemoveArchiveViewerRole(ctx, guildID, roleID); err != nil {
			return removed, err
		}
		removed = append(removed, "archive viewer role")
	}

	for _, o := range s.Overrides(guildID) {
		if slices.Contains(o.RoleIDs, roleID) {
			if err := s.RevokeOverride(ctx, guildID, o.Action, roleID, ""); err != nil {
				return removed, err
			}
			removed = append(removed, "allow on "+o.Action)
		}
		if slices.Contains(o.DenyRoleIDs, roleID) {
			if err := s.UndenyOverride(ctx, guildID, o.Action, roleID, ""); err != nil {
				return removed, err
			}
			removed = append(removed, "deny on "+o.Action)
		}
	}
	return removed, nil
}

// AddJailAllowedChannel and RemoveJailAllowedChannel mirror Add/RemoveModRole
// exactly: same upsert-or-append / array_remove shape, just a different
// column.
func (s *Store) AddJailAllowedChannel(ctx context.Context, guildID, channelID string) error {
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO settings_guild (guild_id, jail_allowed_channel_ids, updated_at) VALUES ($1, ARRAY[$2], now())
		ON CONFLICT (guild_id) DO UPDATE SET
			jail_allowed_channel_ids = (SELECT array_agg(DISTINCT r) FROM unnest(settings_guild.jail_allowed_channel_ids || $2) AS r),
			updated_at = now()`,
		guildID, channelID); err != nil {
		return fmt.Errorf("settings: add jail allowed channel: %w", err)
	}
	if err := s.Refresh(ctx, guildID); err != nil {
		s.invalidate(guildID)
		return err
	}
	s.publishChanged(ctx, guildID)
	return nil
}

func (s *Store) RemoveJailAllowedChannel(ctx context.Context, guildID, channelID string) error {
	if _, err := s.pool.Exec(ctx, `
		UPDATE settings_guild SET jail_allowed_channel_ids = array_remove(jail_allowed_channel_ids, $2), updated_at = now()
		WHERE guild_id = $1`, guildID, channelID); err != nil {
		return fmt.Errorf("settings: remove jail allowed channel: %w", err)
	}
	if err := s.Refresh(ctx, guildID); err != nil {
		s.invalidate(guildID)
		return err
	}
	s.publishChanged(ctx, guildID)
	return nil
}

// AddArchiveViewerRole and RemoveArchiveViewerRole are the same shape again,
// for the roles allowed to see rotation's archives (migration 0020).
func (s *Store) AddArchiveViewerRole(ctx context.Context, guildID, roleID string) error {
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO settings_guild (guild_id, archive_viewer_role_ids, updated_at) VALUES ($1, ARRAY[$2], now())
		ON CONFLICT (guild_id) DO UPDATE SET
			archive_viewer_role_ids = (SELECT array_agg(DISTINCT r) FROM unnest(settings_guild.archive_viewer_role_ids || $2) AS r),
			updated_at = now()`,
		guildID, roleID); err != nil {
		return fmt.Errorf("settings: add archive viewer role: %w", err)
	}
	if err := s.Refresh(ctx, guildID); err != nil {
		s.invalidate(guildID)
		return err
	}
	s.publishChanged(ctx, guildID)
	return nil
}

func (s *Store) RemoveArchiveViewerRole(ctx context.Context, guildID, roleID string) error {
	if _, err := s.pool.Exec(ctx, `
		UPDATE settings_guild SET archive_viewer_role_ids = array_remove(archive_viewer_role_ids, $2), updated_at = now()
		WHERE guild_id = $1`, guildID, roleID); err != nil {
		return fmt.Errorf("settings: remove archive viewer role: %w", err)
	}
	if err := s.Refresh(ctx, guildID); err != nil {
		s.invalidate(guildID)
		return err
	}
	s.publishChanged(ctx, guildID)
	return nil
}

// SetJailMarkerRole records an explicit role ID to use as the guild's jail
// marker role. It upserts the settings row so a fresh guild gets the column
// set without a separate create step.
func (s *Store) SetJailMarkerRole(ctx context.Context, guildID, roleID string) error {
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO settings_guild (guild_id, jail_marker_role_id, updated_at) VALUES ($1, $2, now())
		ON CONFLICT (guild_id) DO UPDATE SET jail_marker_role_id = $2, updated_at = now()`,
		guildID, roleID); err != nil {
		return fmt.Errorf("settings: set jail marker role: %w", err)
	}
	if err := s.Refresh(ctx, guildID); err != nil {
		s.invalidate(guildID)
		return err
	}
	s.publishChanged(ctx, guildID)
	return nil
}

// ClearJailMarkerRole removes any configured jail marker role from a guild.
func (s *Store) ClearJailMarkerRole(ctx context.Context, guildID string) error {
	if _, err := s.pool.Exec(ctx, `
		UPDATE settings_guild SET jail_marker_role_id = NULL, updated_at = now()
		WHERE guild_id = $1`, guildID); err != nil {
		return fmt.Errorf("settings: clear jail marker role: %w", err)
	}
	if err := s.Refresh(ctx, guildID); err != nil {
		s.invalidate(guildID)
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
		s.invalidate(guildID)
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
		s.invalidate(guildID)
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
		s.invalidate(guildID)
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
		s.invalidate(guildID)
		return err
	}
	s.publishChanged(ctx, guildID)
	return nil
}

// MarkOnboardingNudgeSent records that adminconfig's one-time "run /config
// setup" nudge has actually been posted for guildID, so NudgeIfUnconfigured
// never re-sends it. Callers should only call this after a successful
// channel post. Leaving it unmarked on failure lets a transient permission
// issue self-heal on the bot's next restart instead of silently giving up.
func (s *Store) MarkOnboardingNudgeSent(ctx context.Context, guildID string) error {
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO settings_guild (guild_id, onboarding_nudge_sent_at, updated_at) VALUES ($1, now(), now())
		ON CONFLICT (guild_id) DO UPDATE SET onboarding_nudge_sent_at = now(), updated_at = now()`,
		guildID); err != nil {
		return fmt.Errorf("settings: mark onboarding nudge sent: %w", err)
	}
	if err := s.Refresh(ctx, guildID); err != nil {
		s.invalidate(guildID)
		return err
	}
	s.publishChanged(ctx, guildID)
	return nil
}

// GrantOverride adds roleID and/or userID (either may be empty) to action's
// whitelist. TierAdmin-only at the command layer, see
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
		s.invalidate(guildID)
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
		s.invalidate(guildID)
		return err
	}
	s.publishChanged(ctx, guildID)
	return nil
}

// SetActionTier sets a per-guild override of action's required tier,
// replacing the command's own compiled-in PermSpec.Tier for that action in
// this guild only (core.Permissions.Authorize applies it). tier must be
// core.TierMod or core.TierAdmin. TierAdmin-only at the command layer, see
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
		s.invalidate(guildID)
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
		s.invalidate(guildID)
		return err
	}
	s.publishChanged(ctx, guildID)
	return nil
}

// DenyOverride adds roleID and/or userID (either may be empty) to action's
// deny-list. See core.Permissions.Authorize for why deny always wins over
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
		s.invalidate(guildID)
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
		s.invalidate(guildID)
		return err
	}
	s.publishChanged(ctx, guildID)
	return nil
}

// DisablePlugin/EnablePlugin toggle a whole plugin off/on for guildID (see
// core.PluginGate). Coarser than any per-action policy, checked by
// core.CommandRouter before a disabled plugin's commands are even
// authorized. internal/plugins/adminconfig itself must never be passed here
// (enforced at the command layer, not here): disabling it would
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
		s.invalidate(guildID)
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
		s.invalidate(guildID)
		return err
	}
	s.publishChanged(ctx, guildID)
	return nil
}

// SetWritesPaused and SetWritesDryRun toggle this guild's emergency controls
// over destructive Discord actions. Both are TierAdmin-gated at the command
// layer, like every other setting that changes what the bot is allowed to do.
func (s *Store) SetWritesPaused(ctx context.Context, guildID string, paused bool) error {
	return s.setWriteControl(ctx, guildID, "writes_paused", paused)
}

func (s *Store) SetWritesDryRun(ctx context.Context, guildID string, dryRun bool) error {
	return s.setWriteControl(ctx, guildID, "writes_dry_run", dryRun)
}

// setWriteControl takes the column name from its two callers above, never
// from user input. The value is the only parameterized part, and no path
// reaches here with an attacker-influenced column.
func (s *Store) setWriteControl(ctx context.Context, guildID, column string, value bool) error {
	sql := fmt.Sprintf(`
		INSERT INTO settings_guild (guild_id, %s, updated_at) VALUES ($1, $2, now())
		ON CONFLICT (guild_id) DO UPDATE SET %s = $2, updated_at = now()`, column, column)
	if _, err := s.pool.Exec(ctx, sql, guildID, value); err != nil {
		return fmt.Errorf("settings: set %s: %w", column, err)
	}
	if err := s.Refresh(ctx, guildID); err != nil {
		s.invalidate(guildID)
		return err
	}
	s.publishChanged(ctx, guildID)
	return nil
}

// UpsertRotationChannel inserts or fully replaces one rotating channel's
// config. Callers read-modify-write via RotationChannel/RotationChannels
// above, then call this with the whole updated struct (mirrors the simple
// full-replace pattern already used by internal/plugins/rotation's other
// state, not a field-by-field setter for every option).
func (s *Store) UpsertRotationChannel(ctx context.Context, rc RotationChannel) error {
	// A nil Go slice binds as SQL NULL via pgx, which every array column here
	// rejects (NOT NULL DEFAULT '{}'). The DEFAULT only applies when a
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
	// Same reasoning as the slices above, one column over: a caller building
	// the struct from scratch leaves Disclosure empty, and the column's
	// DEFAULT never applies because this INSERT always names every column.
	// Normalizing to the default here rather than rejecting keeps the
	// "unspecified means full" rule in exactly one place.
	rc.Disclosure = rc.Disclosure.Resolve()
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO settings_rotation_channels (guild_id, channel_id, interval_minutes, archive_category_id,
			archive_visibility, archive_whitelist_role_ids, archive_whitelist_user_ids, retention_hours,
			sticky_enabled, sticky_messages, notice_lead_minutes, disclosure, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12, now())
		ON CONFLICT (guild_id, channel_id) DO UPDATE SET
			interval_minutes = $3, archive_category_id = $4, archive_visibility = $5,
			archive_whitelist_role_ids = $6, archive_whitelist_user_ids = $7, retention_hours = $8,
			sticky_enabled = $9, sticky_messages = $10, notice_lead_minutes = $11,
			disclosure = $12, updated_at = now()`,
		rc.GuildID, rc.ChannelID, rc.IntervalMinutes, rc.ArchiveCategoryID, rc.ArchiveVisibility,
		rc.ArchiveWhitelistRoleIDs, rc.ArchiveWhitelistUserIDs, rc.RetentionHours, rc.StickyEnabled, rc.StickyMessages,
		rc.NoticeLeadMinutes, string(rc.Disclosure),
	); err != nil {
		return fmt.Errorf("settings: upsert rotation channel: %w", err)
	}
	if err := s.Refresh(ctx, rc.GuildID); err != nil {
		s.invalidate(rc.GuildID)
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
		s.invalidate(guildID)
		return err
	}
	s.publishChanged(ctx, guildID)
	return nil
}

// RetargetRotationChannel repoints a rotation config from oldChannelID to
// newChannelID in place, preserving every other column (interval, archive
// settings, sticky messages, ...), used by rotation.rotate immediately
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
		s.invalidate(guildID)
		return err
	}
	s.publishChanged(ctx, guildID)
	return nil
}
