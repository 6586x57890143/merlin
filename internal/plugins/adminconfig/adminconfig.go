// Package adminconfig implements /config, the one deliberately cross-cutting
// command tree in the bot (spec.MD §4a): mod roles, admins, and per-action
// permission whitelists aren't owned by any single feature plugin, so they
// live here rather than being duplicated per plugin. Every other
// plugin-specific setting (rotation intervals, sticky content, ...) is
// configured through that plugin's own top-level command instead. The
// whole surface is one panel (panel.go).
package adminconfig

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/bwmarrin/discordgo"
	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/rest"
	"github.com/disgoorg/snowflake/v2"

	"github.com/6586x57890143/merlin/internal/core"
	"github.com/6586x57890143/merlin/internal/settings"
)

// SettingsAdmin is the narrow slice of internal/settings.Store this plugin
// mutates, so unit tests can use an in-memory fake instead of a live
// Postgres. Mirrors the seams already used elsewhere in this codebase.
type SettingsAdmin interface {
	GuildSettings(guildID string) settings.GuildSettings
	Overrides(guildID string) []settings.ActionOverride
	AddModRole(ctx context.Context, guildID, roleID string) error
	RemoveModRole(ctx context.Context, guildID, roleID string) error
	AddAdmin(ctx context.Context, guildID, userID string) error
	RemoveAdmin(ctx context.Context, guildID, userID string) error
	SetAuditLogChannel(ctx context.Context, guildID, channelID string) error
	SetStatusChannel(ctx context.Context, guildID, channelID string) error
	GrantOverride(ctx context.Context, guildID, action, roleID, userID string) error
	RevokeOverride(ctx context.Context, guildID, action, roleID, userID string) error
	DenyOverride(ctx context.Context, guildID, action, roleID, userID string) error
	UndenyOverride(ctx context.Context, guildID, action, roleID, userID string) error
	SetActionTier(ctx context.Context, guildID, action string, tier core.PermTier) error
	ClearActionTier(ctx context.Context, guildID, action string) error
	PluginEnabled(guildID, pluginName string) bool
	DisablePlugin(ctx context.Context, guildID, pluginName string) error
	EnablePlugin(ctx context.Context, guildID, pluginName string) error
	ImportFromLegacyYAML(ctx context.Context, path string) ([]string, error)
	MarkOnboardingNudgeSent(ctx context.Context, guildID string) error
	SetWritesPaused(ctx context.Context, guildID string, paused bool) error
	SetWritesDryRun(ctx context.Context, guildID string, dryRun bool) error
}

const (
	actionMutate = "config.mutate" // admins/mod-roles/permissions/setup/import
	actionRead   = "config.read"   // *.list
)

type Plugin struct {
	settings SettingsAdmin
	// rest is what adminconfig does as merlin, outside the guard: setup
	// creating its channels and role, granting mods view of them, and the
	// onboarding DM.
	rest        discordREST
	botID       snowflake.ID
	auditWriter core.AuditWriter
	commands    *core.CommandRouter
	perms       *core.Permissions
	log         *slog.Logger
	legacyPath  string
	// db and jobs back /config status. Narrow interfaces taken as
	// constructor parameters rather than pulled off core.Deps, so this
	// plugin can be tested without a database or a live Scheduler.
	db   DBHealth
	jobs JobHealth
}

// New constructs Plugin. legacyConfigPath is the path /config import reads
// from: the same file the bootstrap loader reads (config.yaml by default),
// kept only for one-time migration/disaster-recovery, per spec.MD §4a.
func New(settingsStore SettingsAdmin, legacyConfigPath string, db DBHealth, jobs JobHealth) *Plugin {
	return &Plugin{settings: settingsStore, legacyPath: legacyConfigPath, db: db, jobs: jobs}
}

func (p *Plugin) Name() string { return "adminconfig" }

func (p *Plugin) Init(deps core.Deps) error {
	if deps.Rest != nil {
		p.rest = deps.Rest
	}
	p.botID = deps.BotID
	p.auditWriter = deps.Audit
	p.commands = deps.Commands
	p.log = deps.Logger

	p.perms = deps.Perms

	// One command, no subcommands: the panel is the whole surface (panel.go).
	p.commands.RegisterCommand(p.Name(), &discordgo.ApplicationCommand{
		Name:        "config",
		Description: "merlin's settings for this server: admins, mod roles, channels, plugins, permissions",
	})
	p.commands.HandleIx("config", "", readSpec, p.handleConfig)
	p.registerPanel()
	p.registerSetupComponents()
	return nil
}

func (p *Plugin) Start(ctx context.Context) error    { return nil }
func (p *Plugin) Shutdown(ctx context.Context) error { return nil }

// grantModRoleChannelAccess gives roleID VIEW_CHANNEL on whichever of the
// guild's audit-log/status channels are currently configured. Called
// whenever a role becomes (or already is) a mod role, from here and from
// the setup wizard, so mods can actually see the moderation trail they're
// meant to have access to; the channels themselves only deny @everyone by
// default (see core.DenyEveryoneExceptBot), they don't proactively grant mod
// roles, since a mod role may not exist yet at channel-creation time.
func (p *Plugin) grantModRoleChannelAccess(guildID, roleID string) {
	p.grantRolesChannelAccess(guildID, []string{roleID})
}

// discordREST is the slice of disgo's REST client adminconfig uses.
type discordREST interface {
	GetChannel(channelID snowflake.ID, opts ...rest.RequestOpt) (discord.Channel, error)
	UpdatePermissionOverwrite(channelID, overwriteID snowflake.ID, update discord.PermissionOverwriteUpdate, opts ...rest.RequestOpt) error
	CreateGuildChannel(guildID snowflake.ID, create discord.GuildChannelCreate, opts ...rest.RequestOpt) (discord.GuildChannel, error)
	CreateRole(guildID snowflake.ID, create discord.RoleCreate, opts ...rest.RequestOpt) (*discord.Role, error)
	CreateDMChannel(userID snowflake.ID, opts ...rest.RequestOpt) (*discord.DMChannel, error)
	CreateMessage(channelID snowflake.ID, create discord.MessageCreate, opts ...rest.RequestOpt) (*discord.Message, error)
}

// guildChannel reads channelID, nil if it cannot be read or is not a guild
// channel.
func (p *Plugin) guildChannel(channelID string) (discord.GuildChannel, error) {
	id, err := snowflake.Parse(channelID)
	if err != nil {
		return nil, err
	}
	ch, err := p.rest.GetChannel(id)
	if err != nil {
		return nil, err
	}
	gc, ok := ch.(discord.GuildChannel)
	if !ok {
		return nil, fmt.Errorf("channel %s is not a server channel", channelID)
	}
	return gc, nil
}

// grantRolesChannelAccess is the shared body: for each configured channel it
// fetches the channel once and writes only the roles that are actually
// missing the view grant.
//
// Every permission write shows up in the guild's own Discord audit log, and
// grantModRolesChannelAccess re-runs the whole mod-role list every time
// either channel is reconfigured, so a guild with six mod roles used to post
// twelve identical "permission overwrite update" entries for a change that
// altered nothing. One channel read replaces up to one write per role.
//
// A channel that cannot be read is granted anyway: the point of this is that
// mods can see their own moderation trail, which matters more than a tidy
// audit log, so an unreadable channel falls back to the old unconditional
// write rather than skipping it.
func (p *Plugin) grantRolesChannelAccess(guildID string, roleIDs []string) {
	gs := p.settings.GuildSettings(guildID)
	for _, channelID := range []string{gs.AuditLogChannelID, gs.StatusChannelID} {
		if channelID == "" {
			continue
		}
		ch, err := p.guildChannel(channelID)
		if err != nil {
			p.log.Warn("adminconfig: could not read channel before granting mod access", "guild", guildID, "channel", channelID, "err", err)
			ch = nil
		}
		cid, _ := snowflake.Parse(channelID)
		for _, roleID := range rolesMissingViewAccess(ch, roleIDs) {
			rid, perr := snowflake.Parse(roleID)
			if perr != nil {
				continue
			}
			view, none := discord.PermissionViewChannel, discord.Permissions(0)
			if err := p.rest.UpdatePermissionOverwrite(cid, rid, discord.RolePermissionOverwriteUpdate{Allow: &view, Deny: &none}); err != nil {
				p.log.Error("adminconfig: grant mod role channel access failed", "guild", guildID, "channel", channelID, "role", roleID, "err", err)
			}
		}
	}
}

// rolesMissingViewAccess returns which of roleIDs still need a View Channel
// grant on ch. A nil ch (the channel could not be read) returns all of them:
// this must fail towards granting access, never towards silently not.
//
// Only the View Channel bit is compared, not the whole overwrite: a guild
// that has added its own extra allows or denies to a mod role on the audit
// channel is expressing a preference, and rewriting the overwrite to exactly
// view-and-nothing-else would quietly undo it.
func rolesMissingViewAccess(ch discord.GuildChannel, roleIDs []string) []string {
	if ch == nil {
		return roleIDs
	}
	granted := map[string]bool{}
	for _, ow := range ch.PermissionOverwrites() {
		if r, ok := ow.(discord.RolePermissionOverwrite); ok && r.Allow.Has(discord.PermissionViewChannel) {
			granted[r.RoleID.String()] = true
		}
	}
	var out []string
	for _, id := range roleIDs {
		if !granted[id] {
			out = append(out, id)
		}
	}
	return out
}

// validateTierChange rejects a per-guild tier override that would break a
// tier invariant this bot relies on. Today that's exactly one: config.mutate
// governs /config admins add, mod-roles, permission grants, and the plugin
// toggle, so lowering it to Admins+Mods would let any mod run
// `/config admins add @themselves`, a one-command collapse of the whole
// model, and precisely the escalation the tiers exist to prevent.
//
// Refused for the same reason adminconfig can't disable itself via
// /config plugins set: it only ever reads as a mistake, and an invariant the
// codebase states in prose is worth enforcing in code. A pure function so
// the rule is testable without a Discord session, mirroring roles.jailRoles.
func validateTierChange(action string, tier core.PermTier) error {
	if action == actionMutate && tier != core.TierAdmin {
		return fmt.Errorf(
			"`%s` controls who can add admins, mod roles, and permission grants. Leaving it below **Admins only** "+
				"would let any mod grant themselves admin; allow a specific person or role on it instead", actionMutate)
	}
	return nil
}

func permissionOverrideLines(overrides []settings.ActionOverride) []string {
	lines := make([]string, 0, len(overrides))
	for _, o := range overrides {
		tier := "default"
		if o.RequiredTier.IsSet() {
			tier = o.RequiredTier.String()
		}
		lines = append(lines, fmt.Sprintf("`%s` · tier: %s, allow: %s, block: %s",
			o.Action, tier, mentionsOrNone(o.RoleIDs, o.UserIDs), mentionsOrNone(o.DenyRoleIDs, o.DenyUserIDs)))
	}
	return lines
}

// mentionsOrNone renders an allow or block list as clickable role and user
// mentions rather than %v's bracketed snowflakes.
func mentionsOrNone(roleIDs, userIDs []string) string {
	if s := strings.TrimSpace(core.MentionRoles(roleIDs) + " " + core.MentionUsers(userIDs)); s != "" {
		return s
	}
	return "none"
}

func (p *Plugin) audit(ctx context.Context, ix *core.Ix, action, oldValue, newValue string) {
	actorID := ""
	if ix.Member() != nil {
		actorID = ix.UserID
	}
	if err := p.auditWriter.Record(ctx, ix.GuildID, actorID, action, oldValue, newValue); err != nil {
		p.log.Error("adminconfig: audit record failed", "action", action, "err", err)
	}
}

// permissionTarget renders who a permission grant or denial applies to, for
// the audit trail.
//
// The four permission audit lines used to interpolate "action=%s role=%s
// user=%s" unconditionally, and exactly one of role/user is ever set (the
// command takes either), so every one of those records carried a dangling
// "user=" or "role=" and two raw snowflakes for a reader to go and look up.
// Skipping the empty half and rendering the other as a mention is the whole
// difference between a row somebody can read and one they have to decode.
func permissionTarget(action, roleID, userID string) string {
	parts := []string{"action=" + action}
	if roleID != "" {
		parts = append(parts, "role="+core.MentionRole(roleID))
	}
	if userID != "" {
		parts = append(parts, "user="+core.MentionUser(userID))
	}
	return strings.Join(parts, " ")
}

// NudgeIfUnconfigured DMs a fresh guild's owner pointing at /config setup,
// once, if the guild hasn't configured anything yet and hasn't already been
// nudged. Called from cmd/bot/main.go's GuildCreate handler, right after the
// settings cache is refreshed for gc, but only if command registration for
// this guild actually succeeded (see main.go); telling someone to run a
// command that isn't registered yet would be actively misleading, and
// worse, would burn the one-time nudge for nothing.
//
// DMs the owner rather than posting publicly: Discord's API has no way to
// find out who actually invited the bot, and guild.OwnerID is the best
// available, always-present proxy: the owner always implicitly holds
// Discord's Administrator permission, so once they receive this they can run
// /config setup immediately (see core.Permissions.Authorize's Administrator
// path). If the owner has DMs closed or blocks the bot, this logs and
// returns without marking the nudge as sent, so it retries on the next
// restart. No public fallback, by design, since a private nudge was the
// whole point.
func (p *Plugin) NudgeIfUnconfigured(ctx context.Context, gc *discordgo.GuildCreate) {
	gs := p.settings.GuildSettings(gc.ID)
	if gs.IsConfigured() || gs.OnboardingNudgeSentAt != nil {
		return
	}
	if gc.OwnerID == "" {
		p.log.Warn("adminconfig: guild has no owner ID, skipping onboarding nudge", "guild", gc.ID)
		return
	}

	owner, err := snowflake.Parse(gc.OwnerID)
	if err != nil {
		p.log.Warn("adminconfig: guild owner ID unreadable, skipping onboarding nudge", "guild", gc.ID, "owner", gc.OwnerID)
		return
	}
	dmChannel, err := p.rest.CreateDMChannel(owner)
	if err != nil {
		p.log.Warn("adminconfig: could not open a DM with the guild owner for the onboarding nudge",
			"guild", gc.ID, "owner", gc.OwnerID, "err", err)
		return
	}

	embed := core.LandmarkEmbed(core.ColorInfo, "Thanks for adding merlin!",
		fmt.Sprintf("Run **/config setup** in **%s** and I'll walk you through it: an audit-log channel, "+
			"a status channel, a mod role, and admins, one step at a time. I only ever change what you pick on each "+
			"step, and it's safe to re-run any time.", gc.Name))
	_, err = p.rest.CreateMessage(dmChannel.ID(), discord.MessageCreate{
		Embeds: []discord.Embed{embed},
		Files:  core.EmbedUploads(embed),
		// Outside the guard, so stated here as everywhere else.
		AllowedMentions: core.NoPings(),
	})
	if err != nil {
		p.log.Warn("adminconfig: onboarding DM failed, owner may have DMs closed",
			"guild", gc.ID, "owner", gc.OwnerID, "err", err)
		return
	}
	if err := p.settings.MarkOnboardingNudgeSent(ctx, gc.ID); err != nil {
		p.log.Error("adminconfig: failed to record onboarding nudge sent", "guild", gc.ID, "err", err)
	}
}

// Perms is what guided setup does as merlin: create the audit and status
// channels and the mod role.
func (p *Plugin) Perms(string) discord.Permissions {
	return discord.PermissionManageChannels | discord.PermissionManageRoles
}
