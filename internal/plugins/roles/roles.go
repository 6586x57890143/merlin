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
)

// sweepInterval is how often each guild's due-jail/due-grant sweep runs.
// Jail/grant durations are realistically minutes-to-hours, not rotation's
// day-scale retention, so this runs every minute rather than matching
// rotation's hourly cadence — deliberately not "fixed" to match it later.
const sweepInterval = time.Minute

// jailRoleName is the role /roles jail resolves or auto-creates (mirroring
// rotation's defaultArchiveCategoryName pattern) — a guild's jailed members
// all share this one marker role, which every other role gets stripped in
// favor of.
const jailRoleName = "Jailed"

// Plugin implements core.Plugin: temporary role management (jail + timed
// grants). See the package doc comment (store.go) for the overall design.
type Plugin struct {
	ops               DiscordMemberOps
	store             Store
	jailChannelConfig JailChannelConfig
	perms             RoleManager
	audit             core.AuditWriter
	log               *slog.Logger
	sched             core.Scheduler
	commands          *core.CommandRouter
	now               func() time.Time

	mu              sync.Mutex
	sweepRegistered map[string]bool // guild ID -> sweep job registered

	jailRoleMu sync.Mutex
	jailRoleID map[string]string // guild ID -> resolved jail role ID, cached per process
}

// New constructs Plugin. store and jailChannelConfig are passed directly
// rather than through core.Deps: store is plugin-owned runtime state
// (mirrors rotation's ArchiveStore precedent), jailChannelConfig is a narrow
// slice of internal/settings.Store (mirrors rotation's own SettingsProvider
// parameter) for the one piece of guild configuration this plugin has —
// jail's channel-visibility allowlist.
func New(store Store, jailChannelConfig JailChannelConfig) *Plugin {
	return &Plugin{
		store:             store,
		jailChannelConfig: jailChannelConfig,
		now:               func() time.Time { return time.Now().UTC() },
		sweepRegistered:   make(map[string]bool),
		jailRoleID:        make(map[string]string),
	}
}

func (p *Plugin) Name() string { return "roles" }

func (p *Plugin) Init(deps core.Deps) error {
	p.ops = deps.Session
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
// rotation.Plugin.SyncGuild — this plugin has no configurable settings that
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

// resolveJailRole returns guildID's jail marker role ID, creating one named
// jailRoleName if none exists yet — mirrors rotation.resolveArchiveCategory's
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

	rolesList, err := p.ops.GuildRoles(guildID)
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
	role, err := p.ops.GuildRoleCreate(guildID, &discordgo.RoleParams{
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
	// (the allowlist is almost always empty at this point) — otherwise a
	// mod's very first /roles jail would strip roles but leave every
	// channel visible, until someone thought to run sync-channels by hand.
	if err := p.syncAllJailChannelOverwrites(guildID, role.ID); err != nil {
		p.log.Error("roles: initial jail channel overwrite sync failed", "guild", guildID, "err", err)
	}
	return role.ID, nil
}

// forgetJailRole drops guildID's cached jail role ID so the next
// resolveJailRole looks it up (or recreates it) from scratch. Called when
// Discord reports the cached role no longer exists — someone deleted it —
// which would otherwise keep every jail in that guild failing against a dead
// ID until the process restarted.
func (p *Plugin) forgetJailRole(guildID string) {
	p.jailRoleMu.Lock()
	defer p.jailRoleMu.Unlock()
	delete(p.jailRoleID, guildID)
}
