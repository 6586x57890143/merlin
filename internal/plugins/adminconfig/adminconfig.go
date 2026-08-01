// Package adminconfig implements /config, the one deliberately cross-cutting
// command tree in the bot (spec.MD §4a): mod roles, admins, and per-action
// permission whitelists aren't owned by any single feature plugin, so they
// live here rather than being duplicated per plugin. Every other
// plugin-specific setting (rotation intervals, sticky content, ...) is
// configured through that plugin's own top-level command instead — see
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
// Postgres — mirrors the seams already used elsewhere in this codebase.
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
}

// New constructs Plugin. legacyConfigPath is the path /config import reads
// from — the same file the bootstrap loader reads (config.yaml by default),
// kept only for one-time migration/disaster-recovery, per spec.MD §4a.
func New(settingsStore SettingsAdmin, legacyConfigPath string) *Plugin {
	return &Plugin{settings: settingsStore, legacyPath: legacyConfigPath}
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
		Description: "The command action to customize, e.g. rotation.configure — see /config permissions list",
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
		Description: "Plugin name — see /config plugins list",
		Required:    true, Autocomplete: true,
	}

	cmd := &discordgo.ApplicationCommand{
		Name:        "config",
		Description: "Configure the bot for this server: admins, mod roles, and per-command permission grants",
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
						Type: discordgo.ApplicationCommandOptionSubCommand, Name: "grant", Description: "Grant an action to a role or user, independent of tier",
						Options: []*discordgo.ApplicationCommandOption{
							actionOpt,
							{Type: discordgo.ApplicationCommandOptionRole, Name: "role", Description: "Role to grant (either this or user)"},
							{Type: discordgo.ApplicationCommandOptionUser, Name: "user", Description: "User to grant (either this or role)"},
						},
					},
					{
						Type: discordgo.ApplicationCommandOptionSubCommand, Name: "revoke", Description: "Revoke a previously-granted action",
						Options: []*discordgo.ApplicationCommandOption{
							actionOpt,
							{Type: discordgo.ApplicationCommandOptionRole, Name: "role", Description: "Role to revoke (either this or user)"},
							{Type: discordgo.ApplicationCommandOptionUser, Name: "user", Description: "User to revoke (either this or role)"},
						},
					},
					{
						Type: discordgo.ApplicationCommandOptionSubCommand, Name: "block", Description: "Block a specific role or user from an action, even if their tier would otherwise allow it",
						Options: []*discordgo.ApplicationCommandOption{
							actionOpt,
							{Type: discordgo.ApplicationCommandOptionRole, Name: "role", Description: "Role to block (either this or user)"},
							{Type: discordgo.ApplicationCommandOptionUser, Name: "user", Description: "User to block (either this or role)"},
						},
					},
					{
						Type: discordgo.ApplicationCommandOptionSubCommand, Name: "unblock", Description: "Remove a previously-set block",
						Options: []*discordgo.ApplicationCommandOption{
							actionOpt,
							{Type: discordgo.ApplicationCommandOptionRole, Name: "role", Description: "Role to unblock (either this or user)"},
							{Type: discordgo.ApplicationCommandOptionUser, Name: "user", Description: "User to unblock (either this or role)"},
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
						Type: discordgo.ApplicationCommandOptionSubCommand, Name: "enable", Description: "Re-enable a disabled plugin",
						Options: []*discordgo.ApplicationCommandOption{pluginOpt},
					},
					{
						Type: discordgo.ApplicationCommandOptionSubCommand, Name: "disable", Description: "Disable a plugin entirely for this server, for everyone",
						Options: []*discordgo.ApplicationCommandOption{pluginOpt},
					},
				},
			},
			{
				Type: discordgo.ApplicationCommandOptionSubCommand, Name: "setup",
				Description: "First-time setup: create #bot-audit-log, #bot-status, and a Merlin Mod role if missing",
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
	p.commands.Handle("config", "permissions/grant", core.PermSpec{Tier: core.TierAdmin, Action: actionMutate}, p.handlePermissionsGrant)
	p.commands.Handle("config", "permissions/revoke", core.PermSpec{Tier: core.TierAdmin, Action: actionMutate}, p.handlePermissionsRevoke)
	p.commands.Handle("config", "permissions/block", core.PermSpec{Tier: core.TierAdmin, Action: actionMutate}, p.handlePermissionsBlock)
	p.commands.Handle("config", "permissions/unblock", core.PermSpec{Tier: core.TierAdmin, Action: actionMutate}, p.handlePermissionsUnblock)
	p.commands.Handle("config", "permissions/list", core.PermSpec{Tier: core.TierMod, Action: actionRead}, p.handlePermissionsList)
	p.commands.Handle("config", "plugins/list", core.PermSpec{Tier: core.TierMod, Action: actionRead}, p.handlePluginsList)
	p.commands.Handle("config", "plugins/enable", core.PermSpec{Tier: core.TierAdmin, Action: actionMutate}, p.handlePluginsEnable)
	p.commands.Handle("config", "plugins/disable", core.PermSpec{Tier: core.TierAdmin, Action: actionMutate}, p.handlePluginsDisable)
	p.commands.Handle("config", "setup", core.PermSpec{Tier: core.TierAdmin, Action: actionMutate}, p.handleSetup)
	p.commands.Handle("config", "import", core.PermSpec{Tier: core.TierAdmin, Action: actionMutate}, p.handleImport)
	p.commands.Autocomplete("config", "permissions/set-tier", p.autocompleteAction)
	p.commands.Autocomplete("config", "permissions/clear-tier", p.autocompleteAction)
	p.commands.Autocomplete("config", "permissions/grant", p.autocompleteAction)
	p.commands.Autocomplete("config", "permissions/revoke", p.autocompleteAction)
	p.commands.Autocomplete("config", "permissions/block", p.autocompleteAction)
	p.commands.Autocomplete("config", "permissions/unblock", p.autocompleteAction)
	p.commands.Autocomplete("config", "plugins/enable", p.autocompletePlugin)
	p.commands.Autocomplete("config", "plugins/disable", p.autocompletePlugin)
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
		core.RespondInfo(s, i, "Admins", "No admins configured beyond the break-glass bootstrap admin.")
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
	p.audit(ctx, i, "config.mod_role_added", "", roleID)
	core.RespondOK(s, i, "Mod role added", fmt.Sprintf("<@&%s> now counts as a mod role.", roleID))
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
	if err := p.settings.SetActionTier(ctx, i.GuildID, action, tier); err != nil {
		core.RespondErr(s, i, "Failed to set tier", err)
		return
	}
	p.audit(ctx, i, "config.permission_tier_set", "", fmt.Sprintf("action=%s tier=%s", action, tier))
	core.RespondOK(s, i, "Tier updated", fmt.Sprintf("`%s` now requires **%s**.", action, tier))
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

func (p *Plugin) handlePermissionsGrant(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate) {
	opts := core.LeafArgs(i)
	action := opts["action"].StringValue()
	roleID, userID := optionalID(opts, "role"), optionalID(opts, "user")
	if roleID == "" && userID == "" {
		core.RespondErr(s, i, "Nothing to grant", fmt.Errorf("provide a role or a user"))
		return
	}
	if err := p.settings.GrantOverride(ctx, i.GuildID, action, roleID, userID); err != nil {
		core.RespondErr(s, i, "Failed to grant", err)
		return
	}
	p.audit(ctx, i, "config.permission_granted", "", fmt.Sprintf("action=%s role=%s user=%s", action, roleID, userID))
	core.RespondOK(s, i, "Permission granted", fmt.Sprintf("Granted `%s`.", action))
}

func (p *Plugin) handlePermissionsRevoke(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate) {
	opts := core.LeafArgs(i)
	action := opts["action"].StringValue()
	roleID, userID := optionalID(opts, "role"), optionalID(opts, "user")
	if roleID == "" && userID == "" {
		core.RespondErr(s, i, "Nothing to revoke", fmt.Errorf("provide a role or a user"))
		return
	}
	if err := p.settings.RevokeOverride(ctx, i.GuildID, action, roleID, userID); err != nil {
		core.RespondErr(s, i, "Failed to revoke", err)
		return
	}
	p.audit(ctx, i, "config.permission_revoked", fmt.Sprintf("action=%s role=%s user=%s", action, roleID, userID), "")
	core.RespondOK(s, i, "Permission revoked", fmt.Sprintf("Revoked `%s`.", action))
}

func (p *Plugin) handlePermissionsBlock(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate) {
	opts := core.LeafArgs(i)
	action := opts["action"].StringValue()
	roleID, userID := optionalID(opts, "role"), optionalID(opts, "user")
	if roleID == "" && userID == "" {
		core.RespondErr(s, i, "Nothing to block", fmt.Errorf("provide a role or a user"))
		return
	}
	if err := p.settings.DenyOverride(ctx, i.GuildID, action, roleID, userID); err != nil {
		core.RespondErr(s, i, "Failed to block", err)
		return
	}
	p.audit(ctx, i, "config.permission_blocked", "", fmt.Sprintf("action=%s role=%s user=%s", action, roleID, userID))
	core.RespondOK(s, i, "Blocked", fmt.Sprintf("Blocked from `%s`. This wins over tier, Administrator, and any grant.", action))
}

func (p *Plugin) handlePermissionsUnblock(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate) {
	opts := core.LeafArgs(i)
	action := opts["action"].StringValue()
	roleID, userID := optionalID(opts, "role"), optionalID(opts, "user")
	if roleID == "" && userID == "" {
		core.RespondErr(s, i, "Nothing to unblock", fmt.Errorf("provide a role or a user"))
		return
	}
	if err := p.settings.UndenyOverride(ctx, i.GuildID, action, roleID, userID); err != nil {
		core.RespondErr(s, i, "Failed to unblock", err)
		return
	}
	p.audit(ctx, i, "config.permission_unblocked", fmt.Sprintf("action=%s role=%s user=%s", action, roleID, userID), "")
	core.RespondOK(s, i, "Unblocked", fmt.Sprintf("Unblocked from `%s`.", action))
}

func (p *Plugin) handlePermissionsList(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate) {
	overrides := p.settings.Overrides(i.GuildID)
	if len(overrides) == 0 {
		core.RespondInfo(s, i, "Permission grants", "No permission customizations configured.")
		return
	}
	var b strings.Builder
	for _, o := range overrides {
		tier := "default"
		if o.RequiredTier.IsSet() {
			tier = o.RequiredTier.String()
		}
		fmt.Fprintf(&b, "- `%s` — tier: %s, allow: roles=%v users=%v, block: roles=%v users=%v\n",
			o.Action, tier, o.RoleIDs, o.UserIDs, o.DenyRoleIDs, o.DenyUserIDs)
	}
	core.RespondInfo(s, i, "Permission grants", b.String())
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
		fmt.Fprintf(&b, "- `%s` — %s\n", name, status)
	}
	core.RespondInfo(s, i, "Plugins", b.String())
}

func (p *Plugin) handlePluginsEnable(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate) {
	name := core.LeafArgs(i)["plugin"].StringValue()
	if err := p.settings.EnablePlugin(ctx, i.GuildID, name); err != nil {
		core.RespondErr(s, i, "Failed to enable", err)
		return
	}
	p.audit(ctx, i, "config.plugin_enabled", "", name)
	core.RespondOK(s, i, "Plugin enabled", fmt.Sprintf("`%s` is enabled for this server.", name))
}

// handlePluginsDisable disables a whole plugin for this guild — except
// itself: disabling adminconfig would permanently lock the guild out of
// ever re-enabling anything (including itself), so that's rejected here
// before ever touching settings, not left as a doc-only warning.
func (p *Plugin) handlePluginsDisable(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate) {
	name := core.LeafArgs(i)["plugin"].StringValue()
	if name == p.Name() {
		core.RespondErr(s, i, "Can't disable this", fmt.Errorf(
			"%s can't be disabled — doing so would permanently lock this server out of ever re-enabling anything", p.Name()))
		return
	}
	if err := p.settings.DisablePlugin(ctx, i.GuildID, name); err != nil {
		core.RespondErr(s, i, "Failed to disable", err)
		return
	}
	p.audit(ctx, i, "config.plugin_disabled", "", name)
	core.RespondOK(s, i, "Plugin disabled", fmt.Sprintf("`%s` is disabled for this server — nobody, including admins, can use its commands here until re-enabled.", name))
}

// handleSetup auto-provisions whatever a guild is still missing (audit log
// channel, status channel, a default mod role) and always responds with a
// full status summary plus concrete next steps — safe and useful to
// re-run any time as a guided "how's my setup going" check, not just once
// on first join.
func (p *Plugin) handleSetup(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate) {
	gs := p.settings.GuildSettings(i.GuildID)
	var created []string

	if gs.AuditLogChannelID == "" {
		ch, err := s.GuildChannelCreateComplex(i.GuildID, discordgo.GuildChannelCreateData{
			Name: "bot-audit-log",
			PermissionOverwrites: []*discordgo.PermissionOverwrite{
				{ID: i.GuildID, Type: discordgo.PermissionOverwriteTypeRole, Deny: discordgo.PermissionViewChannel},
			},
		})
		if err != nil {
			core.RespondErr(s, i, "Setup failed", fmt.Errorf("create #bot-audit-log: %w", err))
			return
		}
		if err := p.settings.SetAuditLogChannel(ctx, i.GuildID, ch.ID); err != nil {
			core.RespondErr(s, i, "Setup failed", fmt.Errorf("#bot-audit-log created but not saved: %w", err))
			return
		}
		gs.AuditLogChannelID = ch.ID
		created = append(created, "<#"+ch.ID+"> (audit log)")
	}

	if gs.StatusChannelID == "" {
		ch, err := s.GuildChannelCreateComplex(i.GuildID, discordgo.GuildChannelCreateData{
			Name: "bot-status",
			PermissionOverwrites: []*discordgo.PermissionOverwrite{
				{ID: i.GuildID, Type: discordgo.PermissionOverwriteTypeRole, Deny: discordgo.PermissionViewChannel},
			},
		})
		if err != nil {
			core.RespondErr(s, i, "Setup failed", fmt.Errorf("create #bot-status: %w", err))
			return
		}
		if err := p.settings.SetStatusChannel(ctx, i.GuildID, ch.ID); err != nil {
			core.RespondErr(s, i, "Setup failed", fmt.Errorf("#bot-status created but not saved: %w", err))
			return
		}
		gs.StatusChannelID = ch.ID
		created = append(created, "<#"+ch.ID+"> (status)")
	}

	if len(gs.ModRoleIDs) == 0 {
		mentionable := false
		role, err := s.GuildRoleCreate(i.GuildID, &discordgo.RoleParams{Name: "Merlin Mod", Mentionable: &mentionable})
		if err != nil {
			core.RespondErr(s, i, "Setup failed", fmt.Errorf("create Merlin Mod role: %w", err))
			return
		}
		if err := p.settings.AddModRole(ctx, i.GuildID, role.ID); err != nil {
			core.RespondErr(s, i, "Setup failed", fmt.Errorf("merlin mod role created but not saved: %w", err))
			return
		}
		gs.ModRoleIDs = append(gs.ModRoleIDs, role.ID)
		created = append(created, "<@&"+role.ID+"> (mod role — assign it to your moderators)")
	}

	p.audit(ctx, i, "config.setup", "", strings.Join(created, ", "))

	status := fmt.Sprintf(
		"Audit log: <#%s>\nStatus channel: <#%s>\nMod roles: %d configured\nAdmins beyond break-glass: %d configured",
		gs.AuditLogChannelID, gs.StatusChannelID, len(gs.ModRoleIDs), len(gs.AdminUserIDs),
	)
	fields := []*discordgo.MessageEmbedField{{Name: "Current status", Value: status}}
	if len(created) > 0 {
		fields = append(fields, &discordgo.MessageEmbedField{Name: "Created this run", Value: strings.Join(created, "\n")})
	}
	fields = append(fields, &discordgo.MessageEmbedField{Name: "Next steps", Value: strings.Join([]string{
		"Assign the mod role to your actual moderators.",
		"`/config admins add` for anyone who should always have admin access beyond the break-glass identity.",
		"`/rotation configure add` to start rotating a channel.",
		"Safe to re-run `/config setup` any time — it only creates what's still missing.",
	}, "\n"), Inline: false})

	desc := "Everything's already set up."
	if len(created) > 0 {
		desc = "Filled in what was missing."
	}
	if err := core.RespondEmbed(s, i, core.NewEmbed(core.ColorSuccess, "Server setup", desc, fields...)); err != nil {
		p.log.Error("adminconfig: setup response failed", "err", err)
	}
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
// settings cache is refreshed for gc — but only if command registration for
// this guild actually succeeded (see main.go); telling someone to run a
// command that isn't registered yet would be actively misleading, and
// worse, would burn the one-time nudge for nothing.
//
// DMs the owner rather than posting publicly: Discord's API has no way to
// find out who actually invited the bot, and guild.OwnerID is the best
// available, always-present proxy — the owner always implicitly holds
// Discord's Administrator permission, so once they receive this they can run
// /config setup immediately (see core.Permissions.Authorize's Administrator
// path), with no need to explain the break-glass bootstrap admin at all. If
// the owner has DMs closed or blocks the bot, this logs and returns without
// marking the nudge as sent, so it retries on the next restart — no public
// fallback, by design, since a private nudge was the whole point.
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

	embed := core.NewEmbed(core.ColorInfo, "Thanks for adding Merlin!",
		fmt.Sprintf("Run **/config setup** in **%s** to get started — it creates a private audit-log channel, "+
			"a status channel, and a default mod role automatically. It's safe to re-run any time.", gc.Name))
	if _, err := p.session.ChannelMessageSendEmbed(dmChannel.ID, embed); err != nil {
		p.log.Warn("adminconfig: onboarding DM failed, owner may have DMs closed",
			"guild", gc.ID, "owner", gc.OwnerID, "err", err)
		return
	}
	if err := p.settings.MarkOnboardingNudgeSent(ctx, gc.ID); err != nil {
		p.log.Error("adminconfig: failed to record onboarding nudge sent", "guild", gc.ID, "err", err)
	}
}
