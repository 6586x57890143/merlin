package roles

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/6586x57890143/merlin/internal/core"
)

// Action namespaces for whitelist grants (core.PermSpec.Action). jail is
// restriction-only (TierMod-eligible); grant can hand out any role the bot
// can manage, so it stays a separate, TierAdmin-only action, since a mod could
// otherwise use "grant" to hand themselves or an ally an escalated role.
// configure_jail_channels is its own action too: it changes what every
// jailed member (present and future) can see guild-wide, a bigger blast
// radius than a single jail/grant, so it stays Admin-only independent of
// the other two.
// actionJailRole is deliberately separate from actionJail, and Admin-tier by
// default. Jailing everyone holding a role is the same kind of action as
// jailing one person but with a blast radius closer to
// configure_jail_channels': one command can silence a large slice of the
// server, and getting the wrong role means undoing it member by member. That
// is the same reasoning that already keeps configure_jail_channels on its own
// Admin-only action. A guild that wants its mods to hold the raid button can
// say so explicitly with /config permissions set-tier roles.jail_role, which
// is a decision worth making on purpose rather than inheriting.
const (
	actionJail            = "roles.jail"
	actionJailRole        = "roles.jail_role"
	actionGrant           = "roles.grant"
	actionList            = "roles.list"
	actionConfigureJailCh = "roles.configure_jail_channels"
)

func (p *Plugin) registerCommands() {
	userOpt := func(name, desc string) *discordgo.ApplicationCommandOption {
		return &discordgo.ApplicationCommandOption{Type: discordgo.ApplicationCommandOptionUser, Name: name, Description: desc, Required: true}
	}
	optionalUserOpt := func(name, desc string) *discordgo.ApplicationCommandOption {
		return &discordgo.ApplicationCommandOption{Type: discordgo.ApplicationCommandOptionUser, Name: name, Description: desc}
	}
	roleOpt := func(name, desc string) *discordgo.ApplicationCommandOption {
		return &discordgo.ApplicationCommandOption{Type: discordgo.ApplicationCommandOptionRole, Name: name, Description: desc, Required: true}
	}
	channelOpt := func(name, desc string) *discordgo.ApplicationCommandOption {
		return &discordgo.ApplicationCommandOption{Type: discordgo.ApplicationCommandOptionChannel, Name: name, Description: desc, Required: true}
	}
	// optionalRoleOpt can be used to accept an existing role selection to
	// serve as the configured jail marker role for this guild.
	optionalRoleOpt := func(name, desc string) *discordgo.ApplicationCommandOption {
		return &discordgo.ApplicationCommandOption{Type: discordgo.ApplicationCommandOptionRole, Name: name, Description: desc}
	}
	durationOpt := func(name, desc string, required bool) *discordgo.ApplicationCommandOption {
		return &discordgo.ApplicationCommandOption{Type: discordgo.ApplicationCommandOptionString, Name: name, Description: desc, Required: required}
	}
	reasonOpt := &discordgo.ApplicationCommandOption{
		Type: discordgo.ApplicationCommandOptionString, Name: "reason", Description: "Why (recorded in the audit log)",
	}

	cmd := &discordgo.ApplicationCommand{
		Name:        "roles",
		Description: "Temporarily manage a member's roles, with full audit logging (spec.MD §4)",
		Options: []*discordgo.ApplicationCommandOption{
			{
				Type:        discordgo.ApplicationCommandOptionSubCommand,
				Name:        "jail",
				Description: "Strip up to 5 members' roles and channel access for a period, then automatically restore them",
				// Discord requires required options ahead of optional ones,
				// so the extra member slots follow duration. They are plain
				// optional User pickers rather than a free-text list of IDs.
				// See collectJailUserIDs.
				Options: []*discordgo.ApplicationCommandOption{
					userOpt("user", "The member to jail"),
					durationOpt("duration", "How long before automatic release. Needs a unit: \"3d\", \"24h\", \"90m\"", true),
					optionalUserOpt("user2", "A second member, jailed with the same duration and reason"),
					optionalUserOpt("user3", "A third member"),
					optionalUserOpt("user4", "A fourth member"),
					optionalUserOpt("user5", "A fifth member"),
					reasonOpt,
				},
			},
			{
				Type:        discordgo.ApplicationCommandOptionSubCommand,
				Name:        "jail-role",
				Description: "Jail everyone holding one role, for shutting down a raid",
				Options: []*discordgo.ApplicationCommandOption{
					roleOpt("role", "Every member holding this role will be jailed"),
					durationOpt("duration", "How long before automatic release. Needs a unit: \"3d\", \"24h\", \"90m\"", true),
					reasonOpt,
				},
			},
			{
				Type:        discordgo.ApplicationCommandOptionSubCommand,
				Name:        "release",
				Description: "Release a jailed member early, restoring their prior roles",
				Options:     []*discordgo.ApplicationCommandOption{userOpt("user", "The jailed member to release")},
			},
			{
				Type:        discordgo.ApplicationCommandOptionSubCommand,
				Name:        "grant",
				Description: "Grant a member one role, optionally for a limited time",
				Options: []*discordgo.ApplicationCommandOption{
					userOpt("user", "The member to grant a role to"),
					roleOpt("role", "The role to grant"),
					durationOpt("duration", "How long before automatic revocation. Needs a unit: \"3d\", \"24h\". Omit for permanent.", false),
					reasonOpt,
				},
			},
			{
				Type:        discordgo.ApplicationCommandOptionSubCommand,
				Name:        "revoke",
				Description: "Revoke a role Merlin previously granted",
				Options: []*discordgo.ApplicationCommandOption{
					userOpt("user", "The member to revoke a granted role from"),
					roleOpt("role", "The granted role to revoke"),
				},
			},
			{
				Type:        discordgo.ApplicationCommandOptionSubCommand,
				Name:        "list",
				Description: "Show a member's active jail (if any) and tracked role grants",
				Options:     []*discordgo.ApplicationCommandOption{userOpt("user", "The member to inspect")},
			},
			{
				Type:        discordgo.ApplicationCommandOptionSubCommandGroup,
				Name:        "configure",
				Description: "Configure which channels stay visible to a jailed member",
				Options: []*discordgo.ApplicationCommandOption{
					{
						Type:        discordgo.ApplicationCommandOptionSubCommand,
						Name:        "allow-channel",
						Description: "Keep a channel visible while jailed (e.g. appeals). Optional marker role.",
						Options:     []*discordgo.ApplicationCommandOption{channelOpt("channel", "The channel to keep visible while jailed"), optionalRoleOpt("marker_role", "Optional: choose an existing role to assign when jailing members")},
					},
					{
						Type:        discordgo.ApplicationCommandOptionSubCommand,
						Name:        "disallow-channel",
						Description: "Go back to hiding a channel from jailed members",
						Options:     []*discordgo.ApplicationCommandOption{channelOpt("channel", "The channel to hide from jailed members again")},
					},
					{
						Type:        discordgo.ApplicationCommandOptionSubCommand,
						Name:        "list-channels",
						Description: "List channels currently visible to jailed members",
					},
					{
						Type:        discordgo.ApplicationCommandOptionSubCommand,
						Name:        "marker-role",
						Description: "Choose an existing role to use for jailing, or omit to use Merlin's own fallback role.",
						Options:     []*discordgo.ApplicationCommandOption{optionalRoleOpt("marker_role", "The role to assign when jailing members")},
					},
					{
						Type:        discordgo.ApplicationCommandOptionSubCommand,
						Name:        "sync-channels",
						Description: "Recompute every channel's jail visibility (e.g. after creating new channels)",
					},
				},
			},
		},
	}

	p.commands.RegisterCommand(p.Name(), cmd)
	p.commands.Handle("roles", "jail", core.PermSpec{Tier: core.TierMod, Action: actionJail}, p.handleJail)
	p.commands.Handle("roles", "jail-role", core.PermSpec{Tier: core.TierAdmin, Action: actionJailRole}, p.handleJailRole)
	p.commands.Handle("roles", "release", core.PermSpec{Tier: core.TierMod, Action: actionJail}, p.handleRelease)
	p.commands.Handle("roles", "grant", core.PermSpec{Tier: core.TierAdmin, Action: actionGrant}, p.handleGrant)
	p.commands.Handle("roles", "revoke", core.PermSpec{Tier: core.TierAdmin, Action: actionGrant}, p.handleRevoke)
	p.commands.Handle("roles", "list", core.PermSpec{Tier: core.TierMod, Action: actionList}, p.handleList)
	p.commands.Handle("roles", "configure/allow-channel", core.PermSpec{Tier: core.TierAdmin, Action: actionConfigureJailCh}, p.handleAllowChannel)
	p.commands.Handle("roles", "configure/disallow-channel", core.PermSpec{Tier: core.TierAdmin, Action: actionConfigureJailCh}, p.handleDisallowChannel)
	p.commands.Handle("roles", "configure/list-channels", core.PermSpec{Tier: core.TierAdmin, Action: actionConfigureJailCh}, p.handleListChannels)
	p.commands.Handle("roles", "configure/marker-role", core.PermSpec{Tier: core.TierAdmin, Action: actionConfigureJailCh}, p.handleMarkerRole)
	p.commands.Handle("roles", "configure/sync-channels", core.PermSpec{Tier: core.TierAdmin, Action: actionConfigureJailCh}, p.handleSyncChannels)
}

func actorID(i *discordgo.InteractionCreate) string {
	if i.Member != nil && i.Member.User != nil {
		return i.Member.User.ID
	}
	return ""
}

func (p *Plugin) handleList(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate) {
	userID := core.LeafArgs(i)["user"].Value.(string)

	var lines []string
	if rec, ok, err := p.store.GetJail(ctx, i.GuildID, userID); err != nil {
		core.RespondErr(s, i, "Failed to look up jail", err)
		return
	} else if ok {
		release := "indefinite"
		if rec.ReleaseAt != nil {
			release = rec.ReleaseAt.Format(time.RFC3339)
		}
		lines = append(lines, fmt.Sprintf("**Jailed:** released at %s", release))
	}

	grants, err := p.store.ListGrants(ctx, i.GuildID, userID)
	if err != nil {
		core.RespondErr(s, i, "Failed to list grants", err)
		return
	}
	slices.SortFunc(grants, func(a, b GrantRecord) int { return cmp.Compare(a.ID, b.ID) })
	for _, g := range grants {
		expiry := "permanent"
		if g.ExpiresAt != nil {
			expiry = "expires " + g.ExpiresAt.Format(time.RFC3339)
		}
		lines = append(lines, fmt.Sprintf("<@&%s> · %s", g.RoleID, expiry))
	}

	if len(lines) == 0 {
		core.RespondInfo(s, i, "No active jail or grants", fmt.Sprintf("<@%s> has no active jail and no tracked role grants.", userID))
		return
	}
	core.RespondInfo(s, i, fmt.Sprintf("Status for <@%s>", userID), strings.Join(lines, "\n"))
}

func (p *Plugin) handleAllowChannel(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate) {
	args := core.LeafArgs(i)
	channelID := args["channel"].Value.(string)
	// Optional marker role: if provided, persist it as the configured jail
	// marker role for this guild so jailing uses that role instead of
	// auto-creating one.
	if opt, ok := args["marker_role"]; ok {
		if roleID, _ := opt.Value.(string); roleID != "" {
			if err := p.jailChannelConfig.SetJailMarkerRole(ctx, i.GuildID, roleID); err != nil {
				core.RespondErr(s, i, "Failed to save marker role", err)
				return
			}
			p.forgetJailRole(i.GuildID)
			if err := p.audit.Record(ctx, i.GuildID, actorID(i), "roles.configure_jail_channels", "", "marker_role="+core.MentionRole(roleID)); err != nil {
				p.log.Error("roles: audit set marker role failed", "guild", i.GuildID, "err", err)
			}
		}
	}

	if err := p.jailChannelConfig.AddJailAllowedChannel(ctx, i.GuildID, channelID); err != nil {
		core.RespondErr(s, i, "Failed to save", err)
		return
	}
	p.syncOneChannelBestEffort(s, i, channelID)
	if err := p.audit.Record(ctx, i.GuildID, actorID(i), "roles.configure_jail_channels", "", "allow="+core.MentionChannel(channelID)); err != nil {
		p.log.Error("roles: audit allow-channel failed", "guild", i.GuildID, "err", err)
	}
	core.RespondOK(s, i, "Channel allowed", fmt.Sprintf("<#%s> will stay visible to jailed members.", channelID))
}

func (p *Plugin) handleMarkerRole(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate) {
	args := core.LeafArgs(i)
	if opt, ok := args["marker_role"]; ok {
		if roleID, _ := opt.Value.(string); roleID != "" {
			if err := p.jailChannelConfig.SetJailMarkerRole(ctx, i.GuildID, roleID); err != nil {
				core.RespondErr(s, i, "Failed to save marker role", err)
				return
			}
			p.forgetJailRole(i.GuildID)
			if err := p.syncAllJailChannelOverwrites(i.GuildID, roleID); err != nil {
				p.log.Error("roles: failed to sync jail overwrites for configured role", "guild", i.GuildID, "role", roleID, "err", err)
			}
			if err := p.audit.Record(ctx, i.GuildID, actorID(i), "roles.configure_jail_channels", "", "marker_role="+core.MentionRole(roleID)); err != nil {
				p.log.Error("roles: audit set marker role failed", "guild", i.GuildID, "err", err)
			}
			core.RespondOK(s, i, "Configured jail role", fmt.Sprintf("Jailed members will be assigned <@&%s>.", roleID))
			return
		}
	}

	if err := p.jailChannelConfig.ClearJailMarkerRole(ctx, i.GuildID); err != nil {
		core.RespondErr(s, i, "Failed to clear marker role", err)
		return
	}
	p.forgetJailRole(i.GuildID)
	if err := p.audit.Record(ctx, i.GuildID, actorID(i), "roles.configure_jail_channels", "", "marker_role=none"); err != nil {
		p.log.Error("roles: audit clear marker role failed", "guild", i.GuildID, "err", err)
	}
	core.RespondOK(s, i, "Cleared jail role", "Merlin will now use its own birdjailed role again.")
}

func (p *Plugin) handleDisallowChannel(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate) {
	channelID := core.LeafArgs(i)["channel"].Value.(string)
	if err := p.jailChannelConfig.RemoveJailAllowedChannel(ctx, i.GuildID, channelID); err != nil {
		core.RespondErr(s, i, "Failed to save", err)
		return
	}
	p.syncOneChannelBestEffort(s, i, channelID)
	if err := p.audit.Record(ctx, i.GuildID, actorID(i), "roles.configure_jail_channels", "allow="+core.MentionChannel(channelID), ""); err != nil {
		p.log.Error("roles: audit disallow-channel failed", "guild", i.GuildID, "err", err)
	}
	core.RespondOK(s, i, "Channel hidden", fmt.Sprintf("<#%s> is hidden from jailed members again.", channelID))
}

// syncOneChannelBestEffort applies the just-changed allowlist to channelID's
// live permission overwrite. If the jail role hasn't been created yet in
// this guild (nobody has ever run /roles jail), there's nothing to sync.
// The allowlist is still recorded and will apply once resolveJailRole
// eventually creates the role and runs its own full sync.
func (p *Plugin) syncOneChannelBestEffort(s *discordgo.Session, i *discordgo.InteractionCreate, channelID string) {
	p.jailRoleMu.Lock()
	jailRoleID, known := p.jailRoleID[i.GuildID]
	p.jailRoleMu.Unlock()
	if !known {
		return
	}
	if err := p.syncJailChannelOverwrite(i.GuildID, jailRoleID, channelID); err != nil {
		p.log.Error("roles: sync single jail channel overwrite failed", "guild", i.GuildID, "channel", channelID, "err", err)
	}
}

func (p *Plugin) handleListChannels(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate) {
	ids := p.jailChannelConfig.JailAllowedChannelIDs(i.GuildID)
	if len(ids) == 0 {
		core.RespondInfo(s, i, "No allowed channels", "No channels are configured to stay visible to jailed members, so jail currently hides every channel.")
		return
	}
	lines := make([]string, len(ids))
	for idx, id := range ids {
		lines[idx] = fmt.Sprintf("<#%s>", id)
	}
	core.RespondInfo(s, i, "Channels visible while jailed", strings.Join(lines, "\n"))
}

// handleSyncChannels is the one deliberately O(channels) command here: it
// writes an overwrite per managed channel, so a large guild takes far longer
// than Discord's 3-second response deadline allows. It defers first and
// answers with a follow-up.
func (p *Plugin) handleSyncChannels(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate) {
	if err := core.DeferResponse(s, i); err != nil {
		p.log.Error("roles: defer sync-channels response failed", "guild", i.GuildID, "err", err)
		return
	}

	var followUpErr error
	jailRoleID, err := p.resolveJailRole(i.GuildID)
	switch {
	case err != nil:
		followUpErr = core.FollowUpErr(s, i, "Failed to resolve jail role", err)
	default:
		syncErr := p.syncAllJailChannelOverwrites(i.GuildID, jailRoleID)
		if memberErr := p.syncActiveJailMemberOverwrites(ctx, i.GuildID); memberErr != nil && syncErr == nil {
			syncErr = memberErr
		}
		if syncErr != nil {
			followUpErr = core.FollowUpErr(s, i, "Sync completed with errors", syncErr)
		} else {
			followUpErr = core.FollowUpOK(s, i, "Channels synced", "Every channel's jail visibility now matches the current allowlist.")
		}
	}
	if followUpErr != nil {
		p.log.Error("roles: sync-channels follow-up failed", "guild", i.GuildID, "err", followUpErr)
	}
}
