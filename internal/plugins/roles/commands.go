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
// can manage, so it stays a separate, TierAdmin-only action — a mod could
// otherwise use "grant" to hand themselves or an ally an escalated role.
// configure_jail_channels is its own action too: it changes what every
// jailed member (present and future) can see guild-wide, a bigger blast
// radius than a single jail/grant, so it stays Admin-only independent of
// the other two.
const (
	actionJail            = "roles.jail"
	actionGrant           = "roles.grant"
	actionList            = "roles.list"
	actionConfigureJailCh = "roles.configure_jail_channels"
)

func (p *Plugin) registerCommands() {
	userOpt := func(name, desc string) *discordgo.ApplicationCommandOption {
		return &discordgo.ApplicationCommandOption{Type: discordgo.ApplicationCommandOptionUser, Name: name, Description: desc, Required: true}
	}
	roleOpt := func(name, desc string) *discordgo.ApplicationCommandOption {
		return &discordgo.ApplicationCommandOption{Type: discordgo.ApplicationCommandOptionRole, Name: name, Description: desc, Required: true}
	}
	channelOpt := func(name, desc string) *discordgo.ApplicationCommandOption {
		return &discordgo.ApplicationCommandOption{Type: discordgo.ApplicationCommandOptionChannel, Name: name, Description: desc, Required: true}
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
				Description: "Strip a member's roles and channel access for a period, then automatically restore both",
				Options: []*discordgo.ApplicationCommandOption{
					userOpt("user", "The member to jail"),
					durationOpt("duration", "How long before automatic release — e.g. \"24h\" or \"3d\"", true),
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
					durationOpt("duration", "How long before automatic revocation — e.g. \"24h\" or \"3d\". Omit for permanent.", false),
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
						Description: "Keep a channel visible to jailed members (e.g. an appeals channel)",
						Options:     []*discordgo.ApplicationCommandOption{channelOpt("channel", "The channel to keep visible while jailed")},
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
						Name:        "sync-channels",
						Description: "Recompute every channel's jail visibility (e.g. after creating new channels)",
					},
				},
			},
		},
	}

	p.commands.RegisterCommand(p.Name(), cmd)
	p.commands.Handle("roles", "jail", core.PermSpec{Tier: core.TierMod, Action: actionJail}, p.handleJail)
	p.commands.Handle("roles", "release", core.PermSpec{Tier: core.TierMod, Action: actionJail}, p.handleRelease)
	p.commands.Handle("roles", "grant", core.PermSpec{Tier: core.TierAdmin, Action: actionGrant}, p.handleGrant)
	p.commands.Handle("roles", "revoke", core.PermSpec{Tier: core.TierAdmin, Action: actionGrant}, p.handleRevoke)
	p.commands.Handle("roles", "list", core.PermSpec{Tier: core.TierMod, Action: actionList}, p.handleList)
	p.commands.Handle("roles", "configure/allow-channel", core.PermSpec{Tier: core.TierAdmin, Action: actionConfigureJailCh}, p.handleAllowChannel)
	p.commands.Handle("roles", "configure/disallow-channel", core.PermSpec{Tier: core.TierAdmin, Action: actionConfigureJailCh}, p.handleDisallowChannel)
	p.commands.Handle("roles", "configure/list-channels", core.PermSpec{Tier: core.TierAdmin, Action: actionConfigureJailCh}, p.handleListChannels)
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
		lines = append(lines, fmt.Sprintf("**Jailed** — released at %s", release))
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
		lines = append(lines, fmt.Sprintf("<@&%s> — %s", g.RoleID, expiry))
	}

	if len(lines) == 0 {
		core.RespondInfo(s, i, "No active jail or grants", fmt.Sprintf("<@%s> has no active jail and no tracked role grants.", userID))
		return
	}
	core.RespondInfo(s, i, fmt.Sprintf("Status for <@%s>", userID), strings.Join(lines, "\n"))
}

func (p *Plugin) handleAllowChannel(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate) {
	channelID := core.LeafArgs(i)["channel"].Value.(string)
	if err := p.jailChannelConfig.AddJailAllowedChannel(ctx, i.GuildID, channelID); err != nil {
		core.RespondErr(s, i, "Failed to save", err)
		return
	}
	p.syncOneChannelBestEffort(s, i, channelID)
	if err := p.audit.Record(ctx, i.GuildID, actorID(i), "roles.configure_jail_channels", "", fmt.Sprintf("allow=%s", channelID)); err != nil {
		p.log.Error("roles: audit allow-channel failed", "guild", i.GuildID, "err", err)
	}
	core.RespondOK(s, i, "Channel allowed", fmt.Sprintf("<#%s> will stay visible to jailed members.", channelID))
}

func (p *Plugin) handleDisallowChannel(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate) {
	channelID := core.LeafArgs(i)["channel"].Value.(string)
	if err := p.jailChannelConfig.RemoveJailAllowedChannel(ctx, i.GuildID, channelID); err != nil {
		core.RespondErr(s, i, "Failed to save", err)
		return
	}
	p.syncOneChannelBestEffort(s, i, channelID)
	if err := p.audit.Record(ctx, i.GuildID, actorID(i), "roles.configure_jail_channels", fmt.Sprintf("allow=%s", channelID), ""); err != nil {
		p.log.Error("roles: audit disallow-channel failed", "guild", i.GuildID, "err", err)
	}
	core.RespondOK(s, i, "Channel hidden", fmt.Sprintf("<#%s> is hidden from jailed members again.", channelID))
}

// syncOneChannelBestEffort applies the just-changed allowlist to channelID's
// live permission overwrite. If the jail role hasn't been created yet in
// this guild (nobody has ever run /roles jail), there's nothing to sync —
// the allowlist is still recorded and will apply once resolveJailRole
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
		core.RespondInfo(s, i, "No allowed channels", "No channels are configured to stay visible to jailed members — jail currently hides every channel.")
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
		if syncErr := p.syncAllJailChannelOverwrites(i.GuildID, jailRoleID); syncErr != nil {
			followUpErr = core.FollowUpErr(s, i, "Sync completed with errors", syncErr)
		} else {
			followUpErr = core.FollowUpOK(s, i, "Channels synced", "Every channel's jail visibility now matches the current allowlist.")
		}
	}
	if followUpErr != nil {
		p.log.Error("roles: sync-channels follow-up failed", "guild", i.GuildID, "err", followUpErr)
	}
}
