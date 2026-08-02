// Package adminconfig implements /config, the one deliberately cross-cutting
// command tree in the bot (spec.MD §4a): mod roles, admins, and per-action
// permission whitelists aren't owned by any single feature plugin, so they
// live here rather than being duplicated per plugin. Every other
// plugin-specific setting (rotation intervals, sticky content, ...) is
// configured through that plugin's own top-level command instead. See
// internal/plugins/rotation/configure.go for the pattern.
package adminconfig

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strings"

	"github.com/bwmarrin/discordgo"

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
	DisabledPlugins(guildID string) []string
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
	settings    SettingsAdmin
	session     *discordgo.Session
	auditWriter core.AuditWriter
	commands    *core.CommandRouter
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
	p.session = deps.Session
	p.auditWriter = deps.Audit
	p.commands = deps.Commands
	p.log = deps.Logger

	userOpt := func(name, desc string) *discordgo.ApplicationCommandOption {
		return &discordgo.ApplicationCommandOption{Type: discordgo.ApplicationCommandOptionUser, Name: name, Description: desc, Required: true}
	}
	roleOpt := func(name, desc string) *discordgo.ApplicationCommandOption {
		return &discordgo.ApplicationCommandOption{Type: discordgo.ApplicationCommandOptionRole, Name: name, Description: desc, Required: true}
	}
	actionOpt := &discordgo.ApplicationCommandOption{
		Type: discordgo.ApplicationCommandOptionString, Name: "action",
		Description: "The command action to customize, e.g. rotation.configure. See /config permissions list",
		Required:    true, Autocomplete: true,
	}
	tierOpt := &discordgo.ApplicationCommandOption{
		Type: discordgo.ApplicationCommandOptionString, Name: "tier",
		Description: "Who should be able to use this action",
		Required:    true,
		Choices: []*discordgo.ApplicationCommandOptionChoice{
			{Name: "Admins only", Value: "admin"},
			{Name: "Admins + Mods", Value: "mod"},
		},
	}
	pluginOpt := &discordgo.ApplicationCommandOption{
		Type: discordgo.ApplicationCommandOptionString, Name: "plugin",
		Description: "Plugin name. See /config plugins list",
		Required:    true, Autocomplete: true,
	}
	enabledOpt := func(desc string) *discordgo.ApplicationCommandOption {
		return &discordgo.ApplicationCommandOption{Type: discordgo.ApplicationCommandOptionBoolean, Name: "enabled", Description: desc, Required: true}
	}

	cmd := &discordgo.ApplicationCommand{
		Name:        "config",
		Description: "Configure the bird for this server: admins, mod roles, and per-command permission grants",
		Options: []*discordgo.ApplicationCommandOption{
			{
				Type: discordgo.ApplicationCommandOptionSubCommandGroup, Name: "admins",
				Description: "Manage who can run admin-tier commands",
				Options: []*discordgo.ApplicationCommandOption{
					{Type: discordgo.ApplicationCommandOptionSubCommand, Name: "add", Description: "Grant admin", Options: []*discordgo.ApplicationCommandOption{userOpt("user", "The user to grant admin")}},
					{Type: discordgo.ApplicationCommandOptionSubCommand, Name: "remove", Description: "Revoke admin", Options: []*discordgo.ApplicationCommandOption{userOpt("user", "The user to revoke admin from")}},
					{Type: discordgo.ApplicationCommandOptionSubCommand, Name: "list", Description: "List current admins"},
				},
			},
			{
				Type: discordgo.ApplicationCommandOptionSubCommandGroup, Name: "mod-roles",
				Description: "Manage which roles count as mod",
				Options: []*discordgo.ApplicationCommandOption{
					{Type: discordgo.ApplicationCommandOptionSubCommand, Name: "add", Description: "Add a mod role", Options: []*discordgo.ApplicationCommandOption{roleOpt("role", "The role to treat as mod")}},
					{Type: discordgo.ApplicationCommandOptionSubCommand, Name: "remove", Description: "Remove a mod role", Options: []*discordgo.ApplicationCommandOption{roleOpt("role", "The role to stop treating as mod")}},
					{Type: discordgo.ApplicationCommandOptionSubCommand, Name: "list", Description: "List current mod roles"},
				},
			},
			{
				Type: discordgo.ApplicationCommandOptionSubCommandGroup, Name: "permissions",
				Description: "Customize who can use a command action: tier, plus grant/block specific roles or users",
				Options: []*discordgo.ApplicationCommandOption{
					{
						Type: discordgo.ApplicationCommandOptionSubCommand, Name: "set-tier", Description: "Set whether an action requires Admins only or Admins+Mods",
						Options: []*discordgo.ApplicationCommandOption{actionOpt, tierOpt},
					},
					{
						Type: discordgo.ApplicationCommandOptionSubCommand, Name: "clear-tier", Description: "Reset an action back to its default tier requirement",
						Options: []*discordgo.ApplicationCommandOption{actionOpt},
					},
					{
						Type: discordgo.ApplicationCommandOptionSubCommand, Name: "allow", Description: "Grant or revoke an action for a role or user, independent of tier",
						Options: []*discordgo.ApplicationCommandOption{
							actionOpt,
							enabledOpt("true to grant, false to revoke a previous grant"),
							{Type: discordgo.ApplicationCommandOptionRole, Name: "role", Description: "Role to grant/revoke (either this or user)"},
							{Type: discordgo.ApplicationCommandOptionUser, Name: "user", Description: "User to grant/revoke (either this or role)"},
						},
					},
					{
						Type: discordgo.ApplicationCommandOptionSubCommand, Name: "deny", Description: "Block or unblock a specific role or user from an action, even if their tier would otherwise allow it",
						Options: []*discordgo.ApplicationCommandOption{
							actionOpt,
							enabledOpt("true to block, false to remove a previous block"),
							{Type: discordgo.ApplicationCommandOptionRole, Name: "role", Description: "Role to block/unblock (either this or user)"},
							{Type: discordgo.ApplicationCommandOptionUser, Name: "user", Description: "User to block/unblock (either this or role)"},
						},
					},
					{Type: discordgo.ApplicationCommandOptionSubCommand, Name: "list", Description: "List every action's tier override, grants, and blocks"},
				},
			},
			{
				Type: discordgo.ApplicationCommandOptionSubCommandGroup, Name: "plugins",
				Description: "Turn a whole plugin on or off for this server",
				Options: []*discordgo.ApplicationCommandOption{
					{Type: discordgo.ApplicationCommandOptionSubCommand, Name: "list", Description: "List every plugin and whether it's enabled here"},
					{
						Type: discordgo.ApplicationCommandOptionSubCommand, Name: "set", Description: "Enable or disable a plugin entirely for this server",
						Options: []*discordgo.ApplicationCommandOption{pluginOpt, enabledOpt("true to enable, false to disable for everyone")},
					},
				},
			},
			{
				Type: discordgo.ApplicationCommandOptionSubCommand, Name: "status",
				Description: "Health check: database, scheduled jobs, pause/dry-run state, configured channels",
			},
			{
				Type: discordgo.ApplicationCommandOptionSubCommand, Name: "pause",
				Description: "Emergency stop: refuse every destructive action (rotation, sweep, jail) in this server",
				Options: []*discordgo.ApplicationCommandOption{
					{Type: discordgo.ApplicationCommandOptionBoolean, Name: "paused", Description: "true to stop all destructive actions, false to resume", Required: true},
				},
			},
			{
				Type: discordgo.ApplicationCommandOptionSubCommand, Name: "dryrun",
				Description: "Rehearsal mode: log and audit what rotation/sweep/jail would do, without doing it",
				Options: []*discordgo.ApplicationCommandOption{
					{Type: discordgo.ApplicationCommandOptionBoolean, Name: "enabled", Description: "true to rehearse only, false to act for real", Required: true},
				},
			},
			{
				Type: discordgo.ApplicationCommandOptionSubCommand, Name: "setup",
				Description: "Guided setup: walk through the audit log, status channel, mod role, and admins",
			},
			{
				Type: discordgo.ApplicationCommandOptionSubCommand, Name: "import",
				Description: "One-time: seed this server's settings from the legacy config.yaml on disk",
			},
		},
	}

	p.commands.RegisterCommand(p.Name(), cmd)
	p.commands.Handle("config", "admins/add", core.PermSpec{Tier: core.TierAdmin, Action: actionMutate}, p.handleAdminsAdd)
	p.commands.Handle("config", "admins/remove", core.PermSpec{Tier: core.TierAdmin, Action: actionMutate}, p.handleAdminsRemove)
	p.commands.Handle("config", "admins/list", core.PermSpec{Tier: core.TierMod, Action: actionRead}, p.handleAdminsList)
	p.commands.Handle("config", "mod-roles/add", core.PermSpec{Tier: core.TierAdmin, Action: actionMutate}, p.handleModRolesAdd)
	p.commands.Handle("config", "mod-roles/remove", core.PermSpec{Tier: core.TierAdmin, Action: actionMutate}, p.handleModRolesRemove)
	p.commands.Handle("config", "mod-roles/list", core.PermSpec{Tier: core.TierMod, Action: actionRead}, p.handleModRolesList)
	p.commands.Handle("config", "permissions/set-tier", core.PermSpec{Tier: core.TierAdmin, Action: actionMutate}, p.handlePermissionsSetTier)
	p.commands.Handle("config", "permissions/clear-tier", core.PermSpec{Tier: core.TierAdmin, Action: actionMutate}, p.handlePermissionsClearTier)
	p.commands.Handle("config", "permissions/allow", core.PermSpec{Tier: core.TierAdmin, Action: actionMutate}, p.handlePermissionsAllow)
	p.commands.Handle("config", "permissions/deny", core.PermSpec{Tier: core.TierAdmin, Action: actionMutate}, p.handlePermissionsDeny)
	p.commands.Handle("config", "permissions/list", core.PermSpec{Tier: core.TierMod, Action: actionRead}, p.handlePermissionsList)
	p.commands.Handle("config", "plugins/list", core.PermSpec{Tier: core.TierMod, Action: actionRead}, p.handlePluginsList)
	p.commands.Handle("config", "plugins/set", core.PermSpec{Tier: core.TierAdmin, Action: actionMutate}, p.handlePluginsSet)
	p.commands.Handle("config", "status", core.PermSpec{Tier: core.TierMod, Action: actionRead}, p.handleStatus)
	p.commands.Handle("config", "pause", core.PermSpec{Tier: core.TierAdmin, Action: actionMutate}, p.handlePause)
	p.commands.Handle("config", "dryrun", core.PermSpec{Tier: core.TierAdmin, Action: actionMutate}, p.handleDryRun)
	p.commands.Handle("config", "setup", core.PermSpec{Tier: core.TierAdmin, Action: actionMutate}, p.handleSetup)
	p.commands.Handle("config", "import", core.PermSpec{Tier: core.TierAdmin, Action: actionMutate}, p.handleImport)
	p.commands.Autocomplete("config", "permissions/set-tier", p.autocompleteAction)
	p.commands.Autocomplete("config", "permissions/clear-tier", p.autocompleteAction)
	p.commands.Autocomplete("config", "permissions/allow", p.autocompleteAction)
	p.commands.Autocomplete("config", "permissions/deny", p.autocompleteAction)
	p.commands.Autocomplete("config", "plugins/set", p.autocompletePlugin)
	p.commands.HandleComponent(p.Name(), permissionsListComponentPrefix, core.PermSpec{Tier: core.TierMod, Action: actionRead}, p.handlePermissionsListPage)
	p.registerSetupComponents()
	return nil
}

func (p *Plugin) Start(ctx context.Context) error    { return nil }
func (p *Plugin) Shutdown(ctx context.Context) error { return nil }

func (p *Plugin) autocompleteAction(ctx context.Context, i *discordgo.InteractionCreate, focusedOption, focusedValue string) []*discordgo.ApplicationCommandOptionChoice {
	var choices []*discordgo.ApplicationCommandOptionChoice
	for _, action := range p.commands.Actions() {
		if focusedValue != "" && !strings.Contains(action, focusedValue) {
			continue
		}
		choices = append(choices, &discordgo.ApplicationCommandOptionChoice{Name: action, Value: action})
	}
	return choices
}

func (p *Plugin) autocompletePlugin(ctx context.Context, i *discordgo.InteractionCreate, focusedOption, focusedValue string) []*discordgo.ApplicationCommandOptionChoice {
	var choices []*discordgo.ApplicationCommandOptionChoice
	for _, name := range p.commands.Plugins() {
		if focusedValue != "" && !strings.Contains(name, focusedValue) {
			continue
		}
		choices = append(choices, &discordgo.ApplicationCommandOptionChoice{Name: name, Value: name})
	}
	return choices
}

func (p *Plugin) handleAdminsAdd(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate) {
	userID := core.LeafArgs(i)["user"].Value.(string)
	if err := p.settings.AddAdmin(ctx, i.GuildID, userID); err != nil {
		core.RespondErr(s, i, "Failed to add admin", err)
		return
	}
	p.audit(ctx, i, "config.admin_added", "", userID)
	core.RespondOK(s, i, "Admin added", fmt.Sprintf("<@%s> is now an admin.", userID))
}

func (p *Plugin) handleAdminsRemove(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate) {
	userID := core.LeafArgs(i)["user"].Value.(string)
	if err := p.settings.RemoveAdmin(ctx, i.GuildID, userID); err != nil {
		core.RespondErr(s, i, "Failed to remove admin", err)
		return
	}
	p.audit(ctx, i, "config.admin_removed", userID, "")
	core.RespondOK(s, i, "Admin removed", fmt.Sprintf("<@%s> is no longer an admin.", userID))
}

func (p *Plugin) handleAdminsList(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate) {
	gs := p.settings.GuildSettings(i.GuildID)
	if len(gs.AdminUserIDs) == 0 {
		core.RespondInfo(s, i, "Admins", "No admins configured yet.")
		return
	}
	var b strings.Builder
	for _, id := range gs.AdminUserIDs {
		fmt.Fprintf(&b, "- <@%s>\n", id)
	}
	core.RespondInfo(s, i, "Admins", b.String())
}

func (p *Plugin) handleModRolesAdd(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate) {
	roleID := core.LeafArgs(i)["role"].Value.(string)
	if err := p.settings.AddModRole(ctx, i.GuildID, roleID); err != nil {
		core.RespondErr(s, i, "Failed to add mod role", err)
		return
	}
	p.grantModRoleChannelAccess(s, i.GuildID, roleID)
	p.audit(ctx, i, "config.mod_role_added", "", roleID)
	core.RespondOK(s, i, "Mod role added", fmt.Sprintf("<@&%s> now counts as a mod role.", roleID))
}

// grantModRoleChannelAccess gives roleID VIEW_CHANNEL on whichever of the
// guild's audit-log/status channels are currently configured. Called
// whenever a role becomes (or already is) a mod role, from here and from
// the setup wizard, so mods can actually see the moderation trail they're
// meant to have access to; the channels themselves only deny @everyone by
// default (see core.DenyEveryoneExceptBot), they don't proactively grant mod
// roles, since a mod role may not exist yet at channel-creation time.
func (p *Plugin) grantModRoleChannelAccess(s *discordgo.Session, guildID, roleID string) {
	gs := p.settings.GuildSettings(guildID)
	for _, channelID := range []string{gs.AuditLogChannelID, gs.StatusChannelID} {
		if channelID == "" {
			continue
		}
		if err := s.ChannelPermissionSet(channelID, roleID, discordgo.PermissionOverwriteTypeRole, discordgo.PermissionViewChannel, 0); err != nil {
			p.log.Error("adminconfig: grant mod role channel access failed", "guild", guildID, "channel", channelID, "role", roleID, "err", err)
		}
	}
}

func (p *Plugin) handleModRolesRemove(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate) {
	roleID := core.LeafArgs(i)["role"].Value.(string)
	if err := p.settings.RemoveModRole(ctx, i.GuildID, roleID); err != nil {
		core.RespondErr(s, i, "Failed to remove mod role", err)
		return
	}
	p.audit(ctx, i, "config.mod_role_removed", roleID, "")
	core.RespondOK(s, i, "Mod role removed", fmt.Sprintf("<@&%s> no longer counts as a mod role.", roleID))
}

func (p *Plugin) handleModRolesList(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate) {
	gs := p.settings.GuildSettings(i.GuildID)
	if len(gs.ModRoleIDs) == 0 {
		core.RespondInfo(s, i, "Mod roles", "No mod roles configured yet.")
		return
	}
	var b strings.Builder
	for _, id := range gs.ModRoleIDs {
		fmt.Fprintf(&b, "- <@&%s>\n", id)
	}
	core.RespondInfo(s, i, "Mod roles", b.String())
}

func (p *Plugin) handlePermissionsSetTier(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate) {
	opts := core.LeafArgs(i)
	action := opts["action"].StringValue()
	var tier core.PermTier
	switch opts["tier"].StringValue() {
	case "admin":
		tier = core.TierAdmin
	case "mod":
		tier = core.TierMod
	default:
		core.RespondErr(s, i, "Invalid tier", fmt.Errorf("tier must be one of the offered choices"))
		return
	}

	if err := validateTierChange(action, tier); err != nil {
		core.RespondErr(s, i, "Can't lower that action", err)
		return
	}

	if err := p.settings.SetActionTier(ctx, i.GuildID, action, tier); err != nil {
		core.RespondErr(s, i, "Failed to set tier", err)
		return
	}
	p.audit(ctx, i, "config.permission_tier_set", "", fmt.Sprintf("action=%s tier=%s", action, tier))
	core.RespondOK(s, i, "Tier updated", fmt.Sprintf("`%s` now requires **%s**.", action, tier))
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
				"would let any mod grant themselves admin. Grant a specific person or role instead: "+
				"`/config permissions allow action:%s`", actionMutate, actionMutate)
	}
	return nil
}

func (p *Plugin) handlePermissionsClearTier(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate) {
	action := core.LeafArgs(i)["action"].StringValue()
	if err := p.settings.ClearActionTier(ctx, i.GuildID, action); err != nil {
		core.RespondErr(s, i, "Failed to clear tier", err)
		return
	}
	p.audit(ctx, i, "config.permission_tier_cleared", action, "")
	core.RespondOK(s, i, "Tier reset", fmt.Sprintf("`%s` now uses its default tier requirement again.", action))
}

// handlePermissionsAllow grants (enabled:true) or revokes (enabled:false) an
// additive per-action whitelist entry for a role/user. Grant and revoke are
// the same underlying toggle (settings.GrantOverride/RevokeOverride), so
// consolidating them into one command with an enabled flag halves the
// number of near-identical "action+role+user" commands this group had.
func (p *Plugin) handlePermissionsAllow(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate) {
	opts := core.LeafArgs(i)
	action := opts["action"].StringValue()
	roleID, userID := optionalID(opts, "role"), optionalID(opts, "user")
	if roleID == "" && userID == "" {
		core.RespondErr(s, i, "Nothing to change", fmt.Errorf("provide a role or a user"))
		return
	}

	if opts["enabled"].BoolValue() {
		if err := p.settings.GrantOverride(ctx, i.GuildID, action, roleID, userID); err != nil {
			core.RespondErr(s, i, "Failed to grant", err)
			return
		}
		p.audit(ctx, i, "config.permission_granted", "", fmt.Sprintf("action=%s role=%s user=%s", action, roleID, userID))
		core.RespondOK(s, i, "Permission granted", fmt.Sprintf("Granted `%s`.", action))
		return
	}
	if err := p.settings.RevokeOverride(ctx, i.GuildID, action, roleID, userID); err != nil {
		core.RespondErr(s, i, "Failed to revoke", err)
		return
	}
	p.audit(ctx, i, "config.permission_revoked", fmt.Sprintf("action=%s role=%s user=%s", action, roleID, userID), "")
	core.RespondOK(s, i, "Permission revoked", fmt.Sprintf("Revoked `%s`.", action))
}

// handlePermissionsDeny blocks (enabled:true) or unblocks (enabled:false) a
// role/user from an action, same consolidation rationale as
// handlePermissionsAllow above, for the deny-list's own toggle pair.
func (p *Plugin) handlePermissionsDeny(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate) {
	opts := core.LeafArgs(i)
	action := opts["action"].StringValue()
	roleID, userID := optionalID(opts, "role"), optionalID(opts, "user")
	if roleID == "" && userID == "" {
		core.RespondErr(s, i, "Nothing to change", fmt.Errorf("provide a role or a user"))
		return
	}

	if opts["enabled"].BoolValue() {
		if err := p.settings.DenyOverride(ctx, i.GuildID, action, roleID, userID); err != nil {
			core.RespondErr(s, i, "Failed to block", err)
			return
		}
		p.audit(ctx, i, "config.permission_blocked", "", fmt.Sprintf("action=%s role=%s user=%s", action, roleID, userID))
		core.RespondOK(s, i, "Blocked", fmt.Sprintf("Blocked from `%s`. This wins over tier, Administrator, and any grant.", action))
		return
	}
	if err := p.settings.UndenyOverride(ctx, i.GuildID, action, roleID, userID); err != nil {
		core.RespondErr(s, i, "Failed to unblock", err)
		return
	}
	p.audit(ctx, i, "config.permission_unblocked", fmt.Sprintf("action=%s role=%s user=%s", action, roleID, userID), "")
	core.RespondOK(s, i, "Unblocked", fmt.Sprintf("Unblocked from `%s`.", action))
}

// permissionsListComponentPrefix namespaces this list's pagination buttons
// (core.CommandRouter.HandleComponent, spec.MD §4a).
const permissionsListComponentPrefix = "config:permissions:list:page:"

func (p *Plugin) handlePermissionsList(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate) {
	lines := permissionOverrideLines(p.settings.Overrides(i.GuildID))
	if len(lines) == 0 {
		core.RespondInfo(s, i, "Permission grants", "No permission customizations configured.")
		return
	}
	embed, components := renderPermissionsPage(lines, 0)
	if err := core.RespondEmbedWithComponents(s, i, embed, components); err != nil {
		p.log.Error("adminconfig: permissions list response failed", "err", err)
	}
}

func (p *Plugin) handlePermissionsListPage(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate, customID string) {
	page, err := core.ParsePaginationPage(customID, permissionsListComponentPrefix)
	if err != nil {
		p.log.Error("adminconfig: parse pagination page", "custom_id", customID, "err", err)
		page = 0
	}
	embed, components := renderPermissionsPage(permissionOverrideLines(p.settings.Overrides(i.GuildID)), page)
	if err := core.UpdateEmbedWithComponents(s, i, embed, components); err != nil {
		p.log.Error("adminconfig: permissions list page update failed", "err", err)
	}
}

func permissionOverrideLines(overrides []settings.ActionOverride) []string {
	lines := make([]string, 0, len(overrides))
	for _, o := range overrides {
		tier := "default"
		if o.RequiredTier.IsSet() {
			tier = o.RequiredTier.String()
		}
		lines = append(lines, fmt.Sprintf("`%s` · tier: %s, allow: roles=%v users=%v, block: roles=%v users=%v",
			o.Action, tier, o.RoleIDs, o.UserIDs, o.DenyRoleIDs, o.DenyUserIDs))
	}
	return lines
}

func renderPermissionsPage(lines []string, page int) (*discordgo.MessageEmbed, []discordgo.MessageComponent) {
	pageLines, clampedPage, totalPages := core.Paginate(lines, page)
	embed := core.NewEmbed(core.ColorInfo, "Permission grants", strings.Join(pageLines, "\n"))
	return embed, core.PaginationRow(permissionsListComponentPrefix, clampedPage, totalPages)
}

func (p *Plugin) handlePluginsList(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate) {
	disabled := make(map[string]bool)
	for _, name := range p.settings.DisabledPlugins(i.GuildID) {
		disabled[name] = true
	}
	names := p.commands.Plugins()
	sort.Strings(names)

	var b strings.Builder
	for _, name := range names {
		status := "enabled"
		if disabled[name] {
			status = "disabled"
		}
		fmt.Fprintf(&b, "- `%s`: %s\n", name, status)
	}
	core.RespondInfo(s, i, "Plugins", b.String())
}

// handlePluginsSet enables (enabled:true) or disables (enabled:false) a
// whole plugin for this guild, consolidated from separate enable/disable
// commands for the same "grant/revoke toggle" reason as
// handlePermissionsAllow/Deny above. Disabling adminconfig itself is
// rejected before ever touching settings, not left as a doc-only warning:
// doing so would permanently lock the guild out of ever re-enabling
// anything, including itself.
func (p *Plugin) handlePluginsSet(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate) {
	opts := core.LeafArgs(i)
	name := opts["plugin"].StringValue()
	enabled := opts["enabled"].BoolValue()

	if !enabled && name == p.Name() {
		core.RespondErr(s, i, "Can't disable this", fmt.Errorf(
			"%s can't be disabled: doing so would permanently lock this server out of ever re-enabling anything", p.Name()))
		return
	}

	if enabled {
		if err := p.settings.EnablePlugin(ctx, i.GuildID, name); err != nil {
			core.RespondErr(s, i, "Failed to enable", err)
			return
		}
		p.audit(ctx, i, "config.plugin_enabled", "", name)
		core.RespondOK(s, i, "Plugin enabled", fmt.Sprintf("`%s` is enabled for this server.", name))
		return
	}
	if err := p.settings.DisablePlugin(ctx, i.GuildID, name); err != nil {
		core.RespondErr(s, i, "Failed to disable", err)
		return
	}
	p.audit(ctx, i, "config.plugin_disabled", "", name)
	core.RespondOK(s, i, "Plugin disabled", fmt.Sprintf("`%s` is disabled for this server. Nobody, including admins, can use its commands here until re-enabled.", name))
}

// handlePause is the in-Discord emergency stop. It is TierAdmin like every
// other setting that changes what the bot may do, and deliberately does not
// touch command dispatch: read and inspect commands keep working while
// paused, so whoever pulled the lever can still see what the bot believes is
// going on. The process-wide equivalent is MERLIN_PAUSE_ALL_WRITES, which
// exists for when the database itself is the problem.
func (p *Plugin) handlePause(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate) {
	paused := core.LeafArgs(i)["paused"].BoolValue()
	if err := p.settings.SetWritesPaused(ctx, i.GuildID, paused); err != nil {
		core.RespondErr(s, i, "Failed to change pause state", err)
		return
	}
	if paused {
		p.audit(ctx, i, "config.writes_paused", "", "true")
		core.RespondWarn(s, i, "Destructive actions paused",
			"Merlin will refuse every channel rotation, archive deletion, jail, and role change in this server until you run `/config pause paused:false`. Scheduled jobs stay due and resume where they left off.")
		return
	}
	p.audit(ctx, i, "config.writes_paused", "", "false")
	core.RespondOK(s, i, "Destructive actions resumed",
		"Merlin will act normally again. Anything that came due while paused runs on its next scheduled tick.")
}

// handleDryRun turns on rehearsal mode: rotation, sweep, and jail make their
// full decision and write their audit trail but touch nothing in Discord.
// This exists because the operations it covers have no undo (a permanently
// deleted archive channel is gone), so there needs to be a way to watch a
// real guild's real schedule play out once before trusting it.
func (p *Plugin) handleDryRun(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate) {
	enabled := core.LeafArgs(i)["enabled"].BoolValue()
	if err := p.settings.SetWritesDryRun(ctx, i.GuildID, enabled); err != nil {
		core.RespondErr(s, i, "Failed to change dry-run state", err)
		return
	}
	if enabled {
		p.audit(ctx, i, "config.writes_dry_run", "", "true")
		core.RespondWarn(s, i, "Dry-run enabled",
			"Rotations, archive sweeps, and jails will be logged to the audit channel as what they *would* have done, and nothing will actually change. Turn it off with `/config dryrun enabled:false`.")
		return
	}
	p.audit(ctx, i, "config.writes_dry_run", "", "false")
	core.RespondOK(s, i, "Dry-run disabled", "Merlin is acting for real again.")
}

func (p *Plugin) handleImport(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate) {
	if _, err := os.Stat(p.legacyPath); err != nil {
		core.RespondErr(s, i, "Import failed", fmt.Errorf("no legacy config file found at %s: %w", p.legacyPath, err))
		return
	}
	guilds, err := p.settings.ImportFromLegacyYAML(ctx, p.legacyPath)
	if err != nil {
		core.RespondErr(s, i, "Import failed", err)
		return
	}
	p.audit(ctx, i, "config.imported", "", strings.Join(guilds, ", "))
	core.RespondOK(s, i, "Import complete", fmt.Sprintf("Imported settings for %d guild(s): %s", len(guilds), strings.Join(guilds, ", ")))
}

func (p *Plugin) audit(ctx context.Context, i *discordgo.InteractionCreate, action, oldValue, newValue string) {
	actorID := ""
	if i.Member != nil && i.Member.User != nil {
		actorID = i.Member.User.ID
	}
	if err := p.auditWriter.Record(ctx, i.GuildID, actorID, action, oldValue, newValue); err != nil {
		p.log.Error("adminconfig: audit record failed", "action", action, "err", err)
	}
}

func optionalID(opts map[string]*discordgo.ApplicationCommandInteractionDataOption, name string) string {
	o, ok := opts[name]
	if !ok {
		return ""
	}
	return o.Value.(string)
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

	dmChannel, err := p.session.UserChannelCreate(gc.OwnerID)
	if err != nil {
		p.log.Warn("adminconfig: could not open a DM with the guild owner for the onboarding nudge",
			"guild", gc.ID, "owner", gc.OwnerID, "err", err)
		return
	}

	embed := core.NewLandmarkEmbed(core.ColorInfo, "Thanks for adding Merlin!",
		fmt.Sprintf("Run **/config setup** in **%s** to get started. It walks you through an audit-log channel, "+
			"a status channel, a mod role, and admins, one step at a time. It only ever changes what you pick on each "+
			"step, and it's safe to re-run any time.", gc.Name))
	_, err = p.session.ChannelMessageSendComplex(dmChannel.ID, &discordgo.MessageSend{
		Embeds: []*discordgo.MessageEmbed{embed},
		Files:  []*discordgo.File{core.AvatarFile(), core.BannerFile()},
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
