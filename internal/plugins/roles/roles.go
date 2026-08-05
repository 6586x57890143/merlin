package roles

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/6586x57890143/merlin/internal/core"
	"github.com/6586x57890143/merlin/internal/scheduler"
	"github.com/6586x57890143/merlin/internal/voice"
)

// sweepInterval is how often each guild's due-jail/due-grant sweep runs.
// Jail/grant durations are realistically minutes-to-hours, not rotation's
// day-scale retention, so this runs every minute rather than matching
// rotation's hourly cadence, deliberately not "fixed" to match it later.
const sweepInterval = time.Minute

// jailRoleName is the role /roles jail resolves or auto-creates (mirroring
// rotation's defaultArchiveCategoryName pattern). A guild's jailed members
// all share this one marker role, which every other role gets stripped in
// favor of.
const jailRoleName = "Jailed"

// Plugin implements core.Plugin: temporary role management (jail + timed
// grants). See the package doc comment (store.go) for the overall design.
type Plugin struct {
	ops               OpsProvider
	dryRun            func(guildID string) bool
	store             Store
	jailChannelConfig JailChannelConfig
	perms             RoleManager
	audit             core.AuditWriter
	log               *slog.Logger
	sched             core.Scheduler
	commands          *core.CommandRouter
	now               func() time.Time
	// voice supplies the DM wording sent to a jailed or released member.
	// An interface, not the concrete catalog, so a generator can replace it
	// later without this plugin knowing (see internal/voice).
	voice voice.Source

	mu              sync.Mutex
	sweepRegistered map[string]bool // guild ID -> sweep job registered

	jailRoleMu sync.Mutex
	jailRoleID map[string]string // guild ID -> resolved jail role ID, cached per process
}

// OpsProvider yields the Discord ops view for one guild. See
// rotation.OpsProvider for why the guild is bound explicitly rather than
// inferred at call time.
type OpsProvider func(guildID string) DiscordMemberOps

// New constructs Plugin. store and jailChannelConfig are passed directly
// rather than through core.Deps: store is plugin-owned runtime state
// (mirrors rotation's ArchiveStore precedent), jailChannelConfig is a narrow
// slice of internal/settings.Store (mirrors rotation's own SettingsProvider
// parameter) for the one piece of guild configuration this plugin has:
// jail's channel-visibility allowlist.
func New(store Store, jailChannelConfig JailChannelConfig, ops OpsProvider, dryRun func(guildID string) bool, speaker voice.Source) *Plugin {
	return &Plugin{
		store:             store,
		jailChannelConfig: jailChannelConfig,
		ops:               ops,
		dryRun:            dryRun,
		voice:             speaker,
		now:               func() time.Time { return time.Now().UTC() },
		sweepRegistered:   make(map[string]bool),
		jailRoleID:        make(map[string]string),
	}
}

func (p *Plugin) Name() string { return "roles" }

func (p *Plugin) Init(deps core.Deps) error {
	p.perms = deps.Perms
	p.audit = deps.Audit
	p.log = deps.Logger
	p.sched = deps.Scheduler
	p.commands = deps.Commands

	p.registerCommands()
	return nil
}

func (p *Plugin) Start(ctx context.Context) error { return nil }

func (p *Plugin) Shutdown(ctx context.Context) error { return nil }

// SyncGuild ensures guildID has its sweep job registered. Call once per
// guild right after startup/GuildCreate (cmd/bot/main.go), mirroring
// rotation.Plugin.SyncGuild. This plugin has no configurable settings that
// change job existence, just one fixed sweep per known guild.
func (p *Plugin) SyncGuild(guildID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.sweepRegistered[guildID] {
		return
	}
	sweepKey := scheduler.JobKey(guildID, "roles-sweep")
	if err := p.sched.Register(sweepKey, core.CronSpec{Schedule: core.IntervalSchedule{Interval: sweepInterval}}, p.makeSweepJob(guildID)); err != nil {
		p.log.Error("roles: register sweep job", "guild", guildID, "err", err)
		return
	}
	p.sweepRegistered[guildID] = true
}

// ForgetGuild drops guildID's sweep-registration bookkeeping and its cached
// jail role after the bot has been removed from it, so a later re-add
// re-registers the sweep and re-resolves the role rather than trusting state
// from a membership that has since ended. Jail and grant rows in Postgres are
// left alone: they hold the role snapshots needed to restore members, and a
// kick-and-re-invite must not be what silently discards them.
func (p *Plugin) ForgetGuild(guildID string) {
	p.mu.Lock()
	delete(p.sweepRegistered, guildID)
	p.mu.Unlock()
	p.forgetJailRole(guildID)
}

// HandleRoleDeleted reacts to a role disappearing from guildID.
//
// The only role this plugin caches is the jail marker, and that cache is
// held for the process lifetime once resolved, deliberately, so that
// renaming the role does not spawn a duplicate. The flip side is that a
// *deleted* marker role leaves the cache pointing at an ID Discord no
// longer knows, and every subsequent jail in that guild fails against it
// until the process restarts.
//
// There is already a recovery path: applyJail drops the cache when Discord
// answers Unknown Role. This closes the same hole a step earlier, so the
// first jail after the deletion succeeds instead of being the one that pays
// for the discovery. Nothing else is touched: the role is gone, so no
// member still holds it, and the jail records in Postgres are still the
// only copy of what those members held before being jailed.
func (p *Plugin) HandleRoleDeleted(guildID, roleID string) {
	p.jailRoleMu.Lock()
	cached, known := p.jailRoleID[guildID]
	p.jailRoleMu.Unlock()
	if !known || cached != roleID {
		return
	}
	p.forgetJailRole(guildID)
	p.log.Warn("roles: jail role was deleted, will be recreated on the next jail",
		"guild", guildID, "role", roleID)
}

// resolveJailRole returns guildID's jail marker role ID, creating one named
// jailRoleName if none exists yet. Mirrors rotation.resolveArchiveCategory's
// find-by-name-or-create-if-missing pattern, so a mod never has to go create
// this role by hand in Discord's UI before /roles jail works at all. Cached
// per guild for the process lifetime once resolved: recreating it on every
// jail would be wasteful, and a mod renaming it is expected to still resolve
// to the same role ID via the cache rather than spawning a duplicate.
func (p *Plugin) resolveJailRole(guildID string) (string, error) {
	p.jailRoleMu.Lock()
	defer p.jailRoleMu.Unlock()
	if id, ok := p.jailRoleID[guildID]; ok {
		return id, nil
	}

	// Prefer a configured marker role if present in settings.
	if marker := p.jailChannelConfig.JailMarkerRoleID(guildID); marker != "" {
		// Verify the role exists in the guild.
		rolesList, err := p.ops(guildID).GuildRoles(guildID)
		if err != nil {
			return "", fmt.Errorf("roles: list guild roles: %w", err)
		}
		var markerRole *discordgo.Role
		for _, r := range rolesList {
			if r.ID == marker {
				markerRole = r
				break
			}
		}
		if markerRole != nil {
			// Ensure a dedicated plugin jail role exists (find or create by name).
			var jailRole *discordgo.Role
			for _, r := range rolesList {
				if r.Name == jailRoleName {
					jailRole = r
					break
				}
			}
			if jailRole == nil {
				perms := int64(0)
				hoist := false
				mentionable := false
				created, err := p.ops(guildID).GuildRoleCreate(guildID, &discordgo.RoleParams{
					Name:        jailRoleName,
					Permissions: &perms,
					Hoist:       &hoist,
					Mentionable: &mentionable,
				})
				if err != nil {
					return "", fmt.Errorf("roles: create jail role: %w", err)
				}
				jailRole = created
			}
			// Mirror permissions from the configured role onto the dedicated
			// plugin jail role. Prefer to log-and-continue on failure rather
			// than abort the operation entirely: permission sync is important
			// for the intended behaviour but not fatal to the jail flow.
			params := &discordgo.RoleParams{
				Permissions: &markerRole.Permissions,
				Hoist:       &markerRole.Hoist,
				Mentionable: &markerRole.Mentionable,
			}
			if _, err := p.ops(guildID).GuildRoleEdit(guildID, jailRole.ID, params); err != nil {
				p.log.Error("roles: failed to mirror configured role permissions to jail role", "guild", guildID, "err", err)
			}
			p.jailRoleID[guildID] = jailRole.ID
			return jailRole.ID, nil
		}
		// Configured role not present: clear the configured value and fall
		// through to the create/find-by-name path. This avoids repeatedly
		// returning a missing-role error while keeping stored config
		// self-healing when an admin fixes it.
		if err := p.jailChannelConfig.ClearJailMarkerRole(context.Background(), guildID); err != nil {
			p.log.Error("roles: failed to clear missing configured jail role", "guild", guildID, "role", marker, "err", err)
		}
	}

	rolesList, err := p.ops(guildID).GuildRoles(guildID)
	if err != nil {
		return "", fmt.Errorf("roles: list guild roles: %w", err)
	}
	for _, r := range rolesList {
		if r.Name == jailRoleName {
			p.jailRoleID[guildID] = r.ID
			return r.ID, nil
		}
	}

	perms := int64(0)
	hoist := false
	mentionable := false
	role, err := p.ops(guildID).GuildRoleCreate(guildID, &discordgo.RoleParams{
		Name:        jailRoleName,
		Permissions: &perms,
		Hoist:       &hoist,
		Mentionable: &mentionable,
	})
	if err != nil {
		return "", fmt.Errorf("roles: create jail role: %w", err)
	}
	p.jailRoleID[guildID] = role.ID

	// A freshly created jail role starts deny-by-default on every channel
	// (the allowlist is almost always empty at this point). Otherwise a
	// mod's very first /roles jail would strip roles but leave every
	// channel visible, until someone thought to run sync-channels by hand.
	if err := p.syncAllJailChannelOverwrites(guildID, role.ID); err != nil {
		p.log.Error("roles: initial jail channel overwrite sync failed", "guild", guildID, "err", err)
	}
	return role.ID, nil
}

// forgetJailRole drops guildID's cached jail role ID so the next
// resolveJailRole looks it up (or recreates it) from scratch. Called when
// Discord reports the cached role no longer exists (someone deleted it),
// which would otherwise keep every jail in that guild failing against a dead
// ID until the process restarted.
func (p *Plugin) forgetJailRole(guildID string) {
	p.jailRoleMu.Lock()
	defer p.jailRoleMu.Unlock()
	delete(p.jailRoleID, guildID)
}
