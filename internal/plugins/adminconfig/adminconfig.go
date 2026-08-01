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
	ImportFromLegacyYAML(ctx context.Context, path string) ([]string, error)
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
		Description: "The command action to grant/revoke, e.g. rotation.configure — see /config permissions list",
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
				Description: "Whitelist a specific role/user for one command action, independent of mod/admin status",
				Options: []*discordgo.ApplicationCommandOption{
					{
						Type: discordgo.ApplicationCommandOptionSubCommand, Name: "grant", Description: "Grant an action to a role or user",
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
					{Type: discordgo.ApplicationCommandOptionSubCommand, Name: "list", Description: "List current permission grants"},
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

	p.commands.RegisterCommand(cmd)
	p.commands.Handle("config", "admins/add", core.PermSpec{Tier: core.TierAdmin, Action: actionMutate}, p.handleAdminsAdd)
	p.commands.Handle("config", "admins/remove", core.PermSpec{Tier: core.TierAdmin, Action: actionMutate}, p.handleAdminsRemove)
	p.commands.Handle("config", "admins/list", core.PermSpec{Tier: core.TierMod, Action: actionRead}, p.handleAdminsList)
	p.commands.Handle("config", "mod-roles/add", core.PermSpec{Tier: core.TierAdmin, Action: actionMutate}, p.handleModRolesAdd)
	p.commands.Handle("config", "mod-roles/remove", core.PermSpec{Tier: core.TierAdmin, Action: actionMutate}, p.handleModRolesRemove)
	p.commands.Handle("config", "mod-roles/list", core.PermSpec{Tier: core.TierMod, Action: actionRead}, p.handleModRolesList)
	p.commands.Handle("config", "permissions/grant", core.PermSpec{Tier: core.TierAdmin, Action: actionMutate}, p.handlePermissionsGrant)
	p.commands.Handle("config", "permissions/revoke", core.PermSpec{Tier: core.TierAdmin, Action: actionMutate}, p.handlePermissionsRevoke)
	p.commands.Handle("config", "permissions/list", core.PermSpec{Tier: core.TierMod, Action: actionRead}, p.handlePermissionsList)
	p.commands.Handle("config", "setup", core.PermSpec{Tier: core.TierAdmin, Action: actionMutate}, p.handleSetup)
	p.commands.Handle("config", "import", core.PermSpec{Tier: core.TierAdmin, Action: actionMutate}, p.handleImport)
	p.commands.Autocomplete("config", "permissions/grant", p.autocompleteAction)
	p.commands.Autocomplete("config", "permissions/revoke", p.autocompleteAction)
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

func (p *Plugin) handleAdminsAdd(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate) {
	userID := core.LeafArgs(i)["user"].Value.(string)
	if err := p.settings.AddAdmin(ctx, i.GuildID, userID); err != nil {
		respond(s, i, fmt.Sprintf("Failed: %v", err))
		return
	}
	p.audit(ctx, i, "config.admin_added", "", userID)
	respond(s, i, fmt.Sprintf("<@%s> is now an admin.", userID))
}

func (p *Plugin) handleAdminsRemove(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate) {
	userID := core.LeafArgs(i)["user"].Value.(string)
	if err := p.settings.RemoveAdmin(ctx, i.GuildID, userID); err != nil {
		respond(s, i, fmt.Sprintf("Failed: %v", err))
		return
	}
	p.audit(ctx, i, "config.admin_removed", userID, "")
	respond(s, i, fmt.Sprintf("<@%s> is no longer an admin.", userID))
}

func (p *Plugin) handleAdminsList(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate) {
	gs := p.settings.GuildSettings(i.GuildID)
	if len(gs.AdminUserIDs) == 0 {
		respond(s, i, "No admins configured beyond the break-glass bootstrap admin.")
		return
	}
	var b strings.Builder
	b.WriteString("**Admins**\n")
	for _, id := range gs.AdminUserIDs {
		fmt.Fprintf(&b, "- <@%s>\n", id)
	}
	respond(s, i, b.String())
}

func (p *Plugin) handleModRolesAdd(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate) {
	roleID := core.LeafArgs(i)["role"].Value.(string)
	if err := p.settings.AddModRole(ctx, i.GuildID, roleID); err != nil {
		respond(s, i, fmt.Sprintf("Failed: %v", err))
		return
	}
	p.audit(ctx, i, "config.mod_role_added", "", roleID)
	respond(s, i, fmt.Sprintf("<@&%s> now counts as a mod role.", roleID))
}

func (p *Plugin) handleModRolesRemove(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate) {
	roleID := core.LeafArgs(i)["role"].Value.(string)
	if err := p.settings.RemoveModRole(ctx, i.GuildID, roleID); err != nil {
		respond(s, i, fmt.Sprintf("Failed: %v", err))
		return
	}
	p.audit(ctx, i, "config.mod_role_removed", roleID, "")
	respond(s, i, fmt.Sprintf("<@&%s> no longer counts as a mod role.", roleID))
}

func (p *Plugin) handleModRolesList(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate) {
	gs := p.settings.GuildSettings(i.GuildID)
	if len(gs.ModRoleIDs) == 0 {
		respond(s, i, "No mod roles configured yet.")
		return
	}
	var b strings.Builder
	b.WriteString("**Mod roles**\n")
	for _, id := range gs.ModRoleIDs {
		fmt.Fprintf(&b, "- <@&%s>\n", id)
	}
	respond(s, i, b.String())
}

func (p *Plugin) handlePermissionsGrant(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate) {
	opts := core.LeafArgs(i)
	action := opts["action"].StringValue()
	roleID, userID := optionalID(opts, "role"), optionalID(opts, "user")
	if roleID == "" && userID == "" {
		respond(s, i, "Provide a role or a user to grant.")
		return
	}
	if err := p.settings.GrantOverride(ctx, i.GuildID, action, roleID, userID); err != nil {
		respond(s, i, fmt.Sprintf("Failed: %v", err))
		return
	}
	p.audit(ctx, i, "config.permission_granted", "", fmt.Sprintf("action=%s role=%s user=%s", action, roleID, userID))
	respond(s, i, fmt.Sprintf("Granted %q.", action))
}

func (p *Plugin) handlePermissionsRevoke(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate) {
	opts := core.LeafArgs(i)
	action := opts["action"].StringValue()
	roleID, userID := optionalID(opts, "role"), optionalID(opts, "user")
	if roleID == "" && userID == "" {
		respond(s, i, "Provide a role or a user to revoke.")
		return
	}
	if err := p.settings.RevokeOverride(ctx, i.GuildID, action, roleID, userID); err != nil {
		respond(s, i, fmt.Sprintf("Failed: %v", err))
		return
	}
	p.audit(ctx, i, "config.permission_revoked", fmt.Sprintf("action=%s role=%s user=%s", action, roleID, userID), "")
	respond(s, i, fmt.Sprintf("Revoked %q.", action))
}

func (p *Plugin) handlePermissionsList(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate) {
	overrides := p.settings.Overrides(i.GuildID)
	if len(overrides) == 0 {
		respond(s, i, "No permission grants configured.")
		return
	}
	var b strings.Builder
	b.WriteString("**Permission grants**\n")
	for _, o := range overrides {
		fmt.Fprintf(&b, "- `%s`: roles=%v users=%v\n", o.Action, o.RoleIDs, o.UserIDs)
	}
	respond(s, i, b.String())
}

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
			respond(s, i, fmt.Sprintf("Failed to create #bot-audit-log: %v", err))
			return
		}
		if err := p.settings.SetAuditLogChannel(ctx, i.GuildID, ch.ID); err != nil {
			respond(s, i, fmt.Sprintf("Created #bot-audit-log but failed to save it: %v", err))
			return
		}
		created = append(created, "<#"+ch.ID+">  (audit log)")
	}

	if gs.StatusChannelID == "" {
		ch, err := s.GuildChannelCreateComplex(i.GuildID, discordgo.GuildChannelCreateData{
			Name: "bot-status",
			PermissionOverwrites: []*discordgo.PermissionOverwrite{
				{ID: i.GuildID, Type: discordgo.PermissionOverwriteTypeRole, Deny: discordgo.PermissionViewChannel},
			},
		})
		if err != nil {
			respond(s, i, fmt.Sprintf("Failed to create #bot-status: %v", err))
			return
		}
		if err := p.settings.SetStatusChannel(ctx, i.GuildID, ch.ID); err != nil {
			respond(s, i, fmt.Sprintf("Created #bot-status but failed to save it: %v", err))
			return
		}
		created = append(created, "<#"+ch.ID+"> (status)")
	}

	if len(gs.ModRoleIDs) == 0 {
		mentionable := false
		role, err := s.GuildRoleCreate(i.GuildID, &discordgo.RoleParams{Name: "Merlin Mod", Mentionable: &mentionable})
		if err != nil {
			respond(s, i, fmt.Sprintf("Failed to create Merlin Mod role: %v", err))
			return
		}
		if err := p.settings.AddModRole(ctx, i.GuildID, role.ID); err != nil {
			respond(s, i, fmt.Sprintf("Created Merlin Mod role but failed to save it: %v", err))
			return
		}
		created = append(created, "<@&"+role.ID+"> (mod role — assign it to your moderators)")
	}

	p.audit(ctx, i, "config.setup", "", strings.Join(created, ", "))
	if len(created) == 0 {
		respond(s, i, "Nothing to set up — audit log, status channel, and a mod role are all already configured.")
		return
	}
	respond(s, i, "Created: "+strings.Join(created, ", "))
}

func (p *Plugin) handleImport(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate) {
	if _, err := os.Stat(p.legacyPath); err != nil {
		respond(s, i, fmt.Sprintf("No legacy config file found at %s: %v", p.legacyPath, err))
		return
	}
	guilds, err := p.settings.ImportFromLegacyYAML(ctx, p.legacyPath)
	if err != nil {
		respond(s, i, fmt.Sprintf("Import failed: %v", err))
		return
	}
	p.audit(ctx, i, "config.imported", "", strings.Join(guilds, ", "))
	respond(s, i, fmt.Sprintf("Imported settings for %d guild(s): %s", len(guilds), strings.Join(guilds, ", ")))
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

func respond(s *discordgo.Session, i *discordgo.InteractionCreate, msg string) {
	_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Content: msg,
			Flags:   discordgo.MessageFlagsEphemeral,
		},
	})
}
