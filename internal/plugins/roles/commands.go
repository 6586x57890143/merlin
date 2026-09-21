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
	"github.com/6586x57890143/merlin/internal/scripts"
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
	actionJail = "roles.jail"
	// actionVacation is the vacation script's command (script_vacation.go):
	// jail's shape, the island's marker, so it sits at jail's tier and gets
	// its own action for a guild that wants the beach on a different key.
	actionVacation        = "roles.vacation"
	actionJailRole        = "roles.jail_role"
	actionGrant           = "roles.grant"
	actionList            = "roles.list"
	actionConfigureJailCh = "roles.configure_jail_channels"
	// actionScripts turns scripts on and off. Admin-only and never lowered
	// by this plugin: a script on is merlin overriding admins, and the one
	// lever that stops it should not be a mod's.
	actionScripts = "roles.scripts"
	// actionScriptsDefine is adding and removing eternal roles. TierAdmin is
	// only the coarse floor: the handler additionally requires the guild
	// owner or the bootstrap operator (canDefineEternal), which PermSpec
	// cannot express, exactly as /aimod funding set-address does.
	actionScriptsDefine = "roles.scripts_define"
)

// pluginScripts is every script this plugin ships, for the fixed choice
// list on /roles scripts set. A compile-time set, so a plain Choices option
// rather than autocomplete (spec.MD §4a's autocomplete rule is for values
// that come from bot state).
var pluginScripts = []string{scriptEternalRole, scriptVacation}

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
				Name:        "vacation",
				Description: "vacation script: strip up to 5 members to the island's role for a period, then restore them",
				Options: []*discordgo.ApplicationCommandOption{
					userOpt("user", "The member to send on vacation"),
					durationOpt("duration", "How long before they come back. Needs a unit: \"3d\", \"24h\", \"90m\"", true),
					optionalUserOpt("user2", "A second member, same duration and reason"),
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
					durationOpt("duration", "How long before automatic revocation. Needs a unit: \"3d\", \"24h\", \"90m\". Omit for permanent.", false),
					reasonOpt,
				},
			},
			{
				Type:        discordgo.ApplicationCommandOptionSubCommand,
				Name:        "revoke",
				Description: "Revoke a role merlin previously granted",
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
						Name:        "announce-channel",
						Description: "Where jail/release notices are echoed, besides the channel the command was run in. Omit to clear.",
						Options: []*discordgo.ApplicationCommandOption{{
							Type:         discordgo.ApplicationCommandOptionChannel,
							Name:         "channel",
							Description:  "The one channel to echo jail and release notices into (usually the jail channel)",
							ChannelTypes: []discordgo.ChannelType{discordgo.ChannelTypeGuildText, discordgo.ChannelTypeGuildNews},
						}},
					},
					{
						Type:        discordgo.ApplicationCommandOptionSubCommand,
						Name:        "list-channels",
						Description: "List channels currently visible to jailed members",
					},
					{
						Type:        discordgo.ApplicationCommandOptionSubCommand,
						Name:        "marker-role",
						Description: "Choose an existing role to use for jailing, or omit to use merlin's own fallback role.",
						Options:     []*discordgo.ApplicationCommandOption{optionalRoleOpt("marker_role", "The role to assign when jailing members")},
					},
					{
						Type:        discordgo.ApplicationCommandOptionSubCommand,
						Name:        "member-role",
						Description: "The plain member role; jailed/vacation members never get more than it has. Omit to clear.",
						Options:     []*discordgo.ApplicationCommandOption{optionalRoleOpt("role", "The role a plain member holds besides @everyone")},
					},
					{
						Type:        discordgo.ApplicationCommandOptionSubCommand,
						Name:        "vacation-role",
						Description: "vacation script: the existing role a member on vacation holds (never created). Omit to clear.",
						Options:     []*discordgo.ApplicationCommandOption{optionalRoleOpt("role", "The existing role a member on vacation holds")},
					},
					{
						Type:        discordgo.ApplicationCommandOptionSubCommand,
						Name:        "vacation-allow-channel",
						Description: "vacation script: keep a channel visible to members on vacation (everything else is hidden)",
						Options:     []*discordgo.ApplicationCommandOption{channelOpt("channel", "The channel to keep visible while on vacation")},
					},
					{
						Type:        discordgo.ApplicationCommandOptionSubCommand,
						Name:        "vacation-disallow-channel",
						Description: "vacation script: go back to hiding a channel from members on vacation",
						Options:     []*discordgo.ApplicationCommandOption{channelOpt("channel", "The channel to hide from members on vacation again")},
					},
					{
						Type:        discordgo.ApplicationCommandOptionSubCommand,
						Name:        "sync-channels",
						Description: "Recompute every channel's jail and vacation visibility (e.g. after creating new channels)",
					},
				},
			},
			{
				Type:        discordgo.ApplicationCommandOptionSubCommandGroup,
				Name:        "scripts",
				Description: "Server-specific scripts. Off by default; while on, merlin overrides admins on what they protect",
				Options: []*discordgo.ApplicationCommandOption{
					{
						Type:        discordgo.ApplicationCommandOptionSubCommand,
						Name:        "set",
						Description: "Turn a script on or off for this server",
						Options: []*discordgo.ApplicationCommandOption{
							{Type: discordgo.ApplicationCommandOptionString, Name: "script", Description: "Which script", Required: true, Choices: scriptChoices()},
							{Type: discordgo.ApplicationCommandOptionBoolean, Name: "enabled", Description: "true to turn on, false to turn off", Required: true},
						},
					},
					{
						Type:        discordgo.ApplicationCommandOptionSubCommand,
						Name:        "list",
						Description: "Show which scripts are on in this server, and what they protect",
					},
					{
						Type:        discordgo.ApplicationCommandOptionSubCommand,
						Name:        "eternal-add",
						Description: "eternal-role: make a member always hold a role, exactly as it is now. Server owner only.",
						Options:     []*discordgo.ApplicationCommandOption{userOpt("user", "The member"), roleOpt("role", "The role they keep, copied as it is right now")},
					},
					{
						Type:        discordgo.ApplicationCommandOptionSubCommand,
						Name:        "eternal-remove",
						Description: "eternal-role: stop keeping a role on a member. Server owner only.",
						Options:     []*discordgo.ApplicationCommandOption{userOpt("user", "The member"), roleOpt("role", "The role to stop keeping (the original or its current copy)")},
					},
				},
			},
		},
	}

	p.commands.RegisterCommand(p.Name(), cmd)
	p.commands.Handle("roles", "jail", core.PermSpec{Tier: core.TierMod, Action: actionJail}, p.handleJail)
	p.commands.Handle("roles", "vacation", core.PermSpec{Tier: core.TierMod, Action: actionVacation}, p.handleVacation)
	p.commands.Handle("roles", "jail-role", core.PermSpec{Tier: core.TierAdmin, Action: actionJailRole}, p.handleJailRole)
	p.commands.Handle("roles", "release", core.PermSpec{Tier: core.TierMod, Action: actionJail}, p.handleRelease)
	p.commands.Handle("roles", "grant", core.PermSpec{Tier: core.TierAdmin, Action: actionGrant}, p.handleGrant)
	p.commands.Handle("roles", "revoke", core.PermSpec{Tier: core.TierAdmin, Action: actionGrant}, p.handleRevoke)
	p.commands.Handle("roles", "list", core.PermSpec{Tier: core.TierMod, Action: actionList}, p.handleList)
	p.commands.Handle("roles", "configure/allow-channel", core.PermSpec{Tier: core.TierAdmin, Action: actionConfigureJailCh}, p.handleAllowChannel)
	p.commands.Handle("roles", "configure/disallow-channel", core.PermSpec{Tier: core.TierAdmin, Action: actionConfigureJailCh}, p.handleDisallowChannel)
	p.commands.Handle("roles", "configure/announce-channel", core.PermSpec{Tier: core.TierAdmin, Action: actionConfigureJailCh}, p.handleAnnounceChannel)
	p.commands.Handle("roles", "configure/list-channels", core.PermSpec{Tier: core.TierAdmin, Action: actionConfigureJailCh}, p.handleListChannels)
	p.commands.Handle("roles", "configure/marker-role", core.PermSpec{Tier: core.TierAdmin, Action: actionConfigureJailCh}, p.handleMarkerRole)
	p.commands.Handle("roles", "configure/member-role", core.PermSpec{Tier: core.TierAdmin, Action: actionConfigureJailCh}, p.handleMemberRole)
	p.commands.Handle("roles", "configure/vacation-role", core.PermSpec{Tier: core.TierAdmin, Action: actionConfigureJailCh}, p.handleVacationRole)
	p.commands.Handle("roles", "configure/vacation-allow-channel", core.PermSpec{Tier: core.TierAdmin, Action: actionConfigureJailCh}, p.handleVacationAllowChannel)
	p.commands.Handle("roles", "configure/vacation-disallow-channel", core.PermSpec{Tier: core.TierAdmin, Action: actionConfigureJailCh}, p.handleVacationDisallowChannel)
	p.commands.Handle("roles", "configure/sync-channels", core.PermSpec{Tier: core.TierAdmin, Action: actionConfigureJailCh}, p.handleSyncChannels)
	p.commands.Handle("roles", "scripts/set", core.PermSpec{Tier: core.TierAdmin, Action: actionScripts}, p.handleScriptsSet)
	p.commands.Handle("roles", "scripts/list", core.PermSpec{Tier: core.TierAdmin, Action: actionScripts}, p.handleScriptsList)
	p.commands.Handle("roles", "scripts/eternal-add", core.PermSpec{Tier: core.TierAdmin, Action: actionScriptsDefine}, p.handleEternalAdd)
	p.commands.Handle("roles", "scripts/eternal-remove", core.PermSpec{Tier: core.TierAdmin, Action: actionScriptsDefine}, p.handleEternalRemove)
}

func scriptChoices() []*discordgo.ApplicationCommandOptionChoice {
	out := make([]*discordgo.ApplicationCommandOptionChoice, 0, len(pluginScripts))
	for _, name := range pluginScripts {
		out = append(out, &discordgo.ApplicationCommandOptionChoice{Name: name, Value: name})
	}
	return out
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
		state := "Jailed"
		if p.sentenceFor(i.GuildID, rec.JailRoleID) == vacationSentence {
			state = "On vacation"
		}
		lines = append(lines, fmt.Sprintf("**%s:** released at %s", state, release))
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
	newMarkerRole := ""
	if opt, ok := args["marker_role"]; ok {
		if roleID, _ := opt.Value.(string); roleID != "" {
			if err := p.jailChannelConfig.SetJailMarkerRole(ctx, i.GuildID, roleID); err != nil {
				core.RespondErr(s, i, "Failed to save marker role", err)
				return
			}
			p.forgetJailRole(i.GuildID)
			newMarkerRole = roleID
			if err := p.audit.Record(ctx, i.GuildID, actorID(i), "roles.configure_jail_channels", "", "marker_role="+core.MentionRole(roleID)); err != nil {
				p.log.Error("roles: audit set marker role failed", "guild", i.GuildID, "err", err)
			}
		}
	}

	if err := p.jailChannelConfig.AddJailAllowedChannel(ctx, i.GuildID, channelID); err != nil {
		core.RespondErr(s, i, "Failed to save", err)
		return
	}
	var withheld int64
	if newMarkerRole != "" {
		// The marker role just changed in this same command, so the cache
		// forgetJailRole just cleared means syncOneChannelBestEffort's direct
		// cache read would find nothing and silently no-op. Sync every
		// managed channel against the new role and the allowlist (which
		// already includes channelID from the AddJailAllowedChannel call
		// above) in one pass instead, matching handleMarkerRole's own
		// behavior: the sync happens exactly once, here, at the moment the
		// role actually changes, not as a side effect of some later cold
		// cache resolve.
		if err := p.syncAllJailChannelOverwrites(i.GuildID, newMarkerRole); err != nil {
			p.log.Error("roles: failed to sync jail overwrites for configured role", "guild", i.GuildID, "role", newMarkerRole, "err", err)
		}
		withheld, _ = p.syncJailChannelOverwrite(i.GuildID, newMarkerRole, channelID)
	} else {
		withheld = p.syncOneChannelBestEffort(s, i, channelID)
	}
	if err := p.audit.Record(ctx, i.GuildID, actorID(i), "roles.configure_jail_channels", "", "allow="+core.MentionChannel(channelID)+withheldDetail(withheld)); err != nil {
		p.log.Error("roles: audit allow-channel failed", "guild", i.GuildID, "err", err)
	}
	p.respondAllowed(s, i, "jailed members", channelID, withheld)
}

// respondAllowed answers an allow-channel leaf: a plain success, or a
// warning naming what the marker was refused because an ordinary member
// cannot do it in that channel either (memberBaseline). The warning is the
// point of the check: an admin who allowlists an announcements room for
// jailed members is told they can read it and not post in it, rather than
// finding out from the post.
func (p *Plugin) respondAllowed(s *discordgo.Session, i *discordgo.InteractionCreate, who, channelID string, withheld int64) {
	if withheld == 0 {
		core.RespondOK(s, i, "Channel allowed", fmt.Sprintf("<#%s> will stay visible to %s.", channelID, who))
		return
	}
	baseline := "@everyone"
	if id := p.jailChannelConfig.MemberRoleID(i.GuildID); id != "" {
		baseline = core.MentionRole(id)
	}
	if withheld&discordgo.PermissionViewChannel != 0 {
		core.RespondWarn(s, i, "Channel allowed, but hidden anyway",
			fmt.Sprintf("<#%s> is on the allowlist, but ordinary members (%s) cannot see it, so %s are not allowed to either. The entry does nothing until that changes.", channelID, baseline, who))
		return
	}
	core.RespondWarn(s, i, "Channel allowed, with limits",
		fmt.Sprintf("<#%s> will stay visible to %s, but they get no %s there: ordinary members (%s) do not have it in that channel, and a marker role is never allowed more than they are.",
			channelID, who, namePermissions(withheld), baseline))
}

func withheldDetail(withheld int64) string {
	if withheld == 0 {
		return ""
	}
	return " withheld=" + namePermissions(withheld)
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
	core.RespondOK(s, i, "Cleared jail role", "merlin will now use its own birdjailed role again.")
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
func (p *Plugin) syncOneChannelBestEffort(s *discordgo.Session, i *discordgo.InteractionCreate, channelID string) (withheld int64) {
	p.jailRoleMu.Lock()
	jailRoleID, known := p.jailRoleID[i.GuildID]
	p.jailRoleMu.Unlock()
	if !known {
		return 0
	}
	withheld, err := p.syncJailChannelOverwrite(i.GuildID, jailRoleID, channelID)
	if err != nil {
		p.log.Error("roles: sync single jail channel overwrite failed", "guild", i.GuildID, "channel", channelID, "err", err)
	}
	return withheld
}

// handleMemberRole sets, or with the option omitted clears, the ordinary
// member role that memberBaseline caps both markers against. The baseline
// just moved on every channel, so both markers are re-synced; that is
// O(channels), hence the deferral.
func (p *Plugin) handleMemberRole(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate) {
	roleID := ""
	if opt, ok := core.LeafArgs(i)["role"]; ok {
		roleID, _ = opt.Value.(string)
	}
	if err := core.DeferResponse(s, i); err != nil {
		p.log.Error("roles: defer member-role response failed", "guild", i.GuildID, "err", err)
		return
	}
	if err := p.jailChannelConfig.SetMemberRole(ctx, i.GuildID, roleID); err != nil {
		_ = core.FollowUpErr(s, i, "Failed to save", err)
		return
	}
	detail := "none"
	if roleID != "" {
		detail = core.MentionRole(roleID)
	}
	if err := p.audit.Record(ctx, i.GuildID, actorID(i), "roles.configure_jail_channels", "", "member_role="+detail); err != nil {
		p.log.Error("roles: audit member-role failed", "guild", i.GuildID, "err", err)
	}
	p.jailRoleMu.Lock()
	jailRoleID, known := p.jailRoleID[i.GuildID]
	p.jailRoleMu.Unlock()
	if known {
		if err := p.syncAllJailChannelOverwrites(i.GuildID, jailRoleID); err != nil {
			p.log.Error("roles: sync jail overwrites after member-role change", "guild", i.GuildID, "err", err)
		}
	}
	if err := p.syncAllVacationOverwrites(i.GuildID); err != nil {
		p.log.Error("roles: sync vacation overwrites after member-role change", "guild", i.GuildID, "err", err)
	}
	if roleID == "" {
		_ = core.FollowUpOK(s, i, "Member role cleared", "Jailed and vacationing members are now capped at what @everyone can do in each allowlisted channel.")
		return
	}
	_ = core.FollowUpOK(s, i, "Member role set",
		fmt.Sprintf("Jailed and vacationing members are now capped at what %s can do in each allowlisted channel. Every channel has been re-synced against it.", core.MentionRole(roleID)))
}

// handleAnnounceChannel sets, or with the option omitted clears, the one
// extra channel jail and release notices are echoed into
// (announceDestinations). Omitting to clear mirrors configure/marker-role,
// rather than spending a second subcommand on it.
func (p *Plugin) handleAnnounceChannel(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate) {
	channelID := ""
	if opt, ok := core.LeafArgs(i)["channel"]; ok {
		channelID, _ = opt.Value.(string)
	}
	if err := p.jailChannelConfig.SetJailAnnounceChannel(ctx, i.GuildID, channelID); err != nil {
		core.RespondErr(s, i, "Failed to save", err)
		return
	}
	detail := "none"
	if channelID != "" {
		detail = core.MentionChannel(channelID)
	}
	if err := p.audit.Record(ctx, i.GuildID, actorID(i), "roles.configure_jail_channels", "", "announce_channel="+detail); err != nil {
		p.log.Error("roles: audit announce-channel failed", "guild", i.GuildID, "err", err)
	}
	if channelID == "" {
		core.RespondOK(s, i, "Announcement channel cleared", "Jail and release notices will only be posted in the channel the command was run in.")
		return
	}
	core.RespondOK(s, i, "Announcement channel set", fmt.Sprintf("Jail and release notices will also be posted in <#%s>.", channelID))
}

func (p *Plugin) handleListChannels(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate) {
	announce := "Announcements: the invoking channel only."
	if id := p.jailChannelConfig.JailAnnounceChannelID(i.GuildID); id != "" {
		announce = fmt.Sprintf("Announcements also go to <#%s>.", id)
	}
	baseline := "Baseline for both: @everyone (no member role configured; `/roles configure member-role`)."
	if id := p.jailChannelConfig.MemberRoleID(i.GuildID); id != "" {
		baseline = "Baseline for both: " + core.MentionRole(id) + ". A marker role is never allowed more than it has in a channel."
	}
	vacation := "Visible while on vacation: nothing configured."
	if v := p.vacationAllowlist(i.GuildID); len(v) > 0 {
		mentions := make([]string, len(v))
		for k, id := range v {
			mentions[k] = core.MentionChannel(id)
		}
		vacation = "Visible while on vacation: " + strings.Join(mentions, " ")
	}
	ids := p.jailChannelConfig.JailAllowedChannelIDs(i.GuildID)
	if len(ids) == 0 {
		core.RespondInfo(s, i, "No allowed channels", "No channels are configured to stay visible to jailed members, so jail currently hides every channel.\n\n"+announce+"\n"+vacation+"\n"+baseline)
		return
	}
	lines := make([]string, 0, len(ids)+4)
	for _, id := range ids {
		lines = append(lines, fmt.Sprintf("<#%s>", id))
	}
	lines = append(lines, "", announce, vacation, baseline)
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
		if verr := p.syncAllVacationOverwrites(i.GuildID); verr != nil && syncErr == nil {
			syncErr = verr
		}
		if syncErr != nil {
			followUpErr = core.FollowUpErr(s, i, "Sync completed with errors", syncErr)
		} else {
			followUpErr = core.FollowUpOK(s, i, "Channels synced", "Every channel's jail and vacation visibility now matches the current allowlists.")
		}
	}
	if followUpErr != nil {
		p.log.Error("roles: sync-channels follow-up failed", "guild", i.GuildID, "err", followUpErr)
	}
}

// handleScriptsSet turns a script on or off. Turning one on answers with a
// warning rather than a success: the admin has just handed merlin authority
// over their own future actions, and the response should read that way. The
// first enforcement runs here as well, so anything already defined is put
// right now rather than up to a sweep later.
func (p *Plugin) handleScriptsSet(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate) {
	opts := core.LeafArgs(i)
	name := opts["script"].StringValue()
	enabled := opts["enabled"].BoolValue()
	if !slices.Contains(pluginScripts, name) {
		core.RespondErr(s, i, "Unknown script", fmt.Errorf("%q is not a script of this plugin", name))
		return
	}
	if p.scripts == nil {
		core.RespondErr(s, i, "Scripts unavailable", fmt.Errorf("this deployment has no script store"))
		return
	}
	if err := core.DeferResponse(s, i); err != nil {
		return
	}
	if err := p.scripts.SetEnabled(ctx, i.GuildID, name, enabled); err != nil {
		_ = core.FollowUpErr(s, i, "Failed to change script", err)
		return
	}
	if !enabled {
		p.auditScript(ctx, i, "roles.script_disabled", name)
		_ = core.FollowUpOK(s, i, "Script off", fmt.Sprintf("`%s` is off. Whatever it was protecting is ordinary again; its definitions and merlin's stored copies are kept.", name))
		return
	}
	p.auditScript(ctx, i, "roles.script_enabled", name)
	switch name {
	case scriptEternalRole:
		if err := p.enforceEternalRoles(ctx, i.GuildID); err != nil {
			p.log.Error("roles: first enforcement after enabling script", "guild", i.GuildID, "script", name, "err", err)
		}
	case scriptVacation:
		// The one full sync the island gets on its own: turning the script
		// on is the moment the admin opts into deny-by-default there.
		if err := p.syncAllVacationOverwrites(i.GuildID); err != nil {
			p.log.Error("roles: vacation sync after enabling script", "guild", i.GuildID, "err", err)
		}
	}
	recs, _ := p.store.ListEternalRoles(ctx, i.GuildID)
	_ = core.FollowUpEmbed(s, i, core.NewEmbed(core.ColorWarning, "Script on: "+name,
		scripts.Warning+"\n\n"+p.scriptDescription(i.GuildID, name, recs)+
			fmt.Sprintf("\n\nTurn it off with `/roles scripts set script:%s enabled:false`.", name)))
}

func (p *Plugin) auditScript(ctx context.Context, i *discordgo.InteractionCreate, action, name string) {
	if err := p.audit.Record(ctx, i.GuildID, actorID(i), action, "", name); err != nil {
		p.log.Error("roles: audit script toggle failed", "guild", i.GuildID, "err", err)
	}
}

func (p *Plugin) handleScriptsList(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate) {
	var b strings.Builder
	for _, name := range pluginScripts {
		status := "off"
		if p.scripts != nil {
			if on, err := p.scripts.Enabled(ctx, i.GuildID, name); err != nil {
				status = "unreadable (treated as off)"
			} else if on {
				status = "on"
			}
		}
		fmt.Fprintf(&b, "- `%s`: %s", name, status)
		switch name {
		case scriptEternalRole:
			recs, err := p.store.ListEternalRoles(ctx, i.GuildID)
			if err != nil {
				b.WriteString(" (definitions unreadable)")
			} else {
				b.WriteString(": " + eternalRolesLine(recs))
			}
		case scriptVacation:
			b.WriteString(": " + p.vacationLine(i.GuildID))
		}
		b.WriteString("\n")
	}
	core.RespondInfo(s, i, "Scripts", b.String())
}

// scriptDescription is what an admin is told they just turned on.
func (p *Plugin) scriptDescription(guildID, name string, recs []EternalRoleRecord) string {
	switch name {
	case scriptVacation:
		return "**vacation**: " + p.vacationLine(guildID) + ". `/roles vacation` does what `/roles jail` does with that role " +
			"as the marker instead of the jail one: roles snapshotted and stripped, restored on the timer or with `/roles release`, " +
			"same evasion handling. merlin never creates the island's role; it hides every channel from it except " +
			"the vacation allowlist (`/roles configure vacation-allow-channel`), moving only View/Connect and leaving " +
			"the rest of each channel's overwrite as the server set it. " +
			"`/roles jail` on somebody on vacation moves them to jail with the new duration, and the reverse; " +
			"a mod swapping the two roles by hand is recognised and the sentence follows."
	case scriptEternalRole:
		return "**eternal-role**: " + eternalRolesLine(recs) + ". merlin keeps a copy of each role as it was when added. " +
			"If it is removed from them it is given back; if it is deleted it is recreated from the copy; " +
			"if it is edited, a fresh copy is created and placed above the edited one. Jail still works normally. " +
			"Only the server owner can add or remove definitions (`/roles scripts eternal-add`)."
	}
	return ""
}

// handleEternalAdd defines an eternal role. The owner-or-operator check is
// the real gate here; see canDefineEternal. Deferred because the capture
// downloads the role's icon.
func (p *Plugin) handleEternalAdd(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate) {
	if !p.requireDefiner(s, i) {
		return
	}
	opts := core.LeafArgs(i)
	userID := opts["user"].UserValue(nil).ID
	roleID := opts["role"].RoleValue(nil, "").ID
	if err := core.DeferResponse(s, i); err != nil {
		return
	}
	rec, err := p.addEternalRole(ctx, i.GuildID, userID, roleID, actorID(i))
	if err != nil {
		_ = core.FollowUpErr(s, i, "Not added", err)
		return
	}
	if err := p.enforceEternalRoles(ctx, i.GuildID); err != nil {
		p.log.Error("roles: enforce after eternal-add", "guild", i.GuildID, "err", err)
	}
	on := false
	if p.scripts != nil {
		on, _ = p.scripts.Enabled(ctx, i.GuildID, scriptEternalRole)
	}
	note := ""
	if !on {
		note = "\n\nThe script is **off** in this server, so nothing is enforced yet: `/roles scripts set script:eternal-role enabled:true`."
	}
	icon := "no icon"
	if len(rec.Icon) > 0 {
		icon = "icon captured"
	}
	_ = core.FollowUpOK(s, i, "Eternal role added",
		fmt.Sprintf("%s now keeps %s. Copied as it is right now (%s); the copy never changes from Discord's side.%s",
			core.MentionUser(userID), core.MentionRole(roleID), icon, note))
}

func (p *Plugin) handleEternalRemove(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate) {
	if !p.requireDefiner(s, i) {
		return
	}
	opts := core.LeafArgs(i)
	userID := opts["user"].UserValue(nil).ID
	roleID := opts["role"].RoleValue(nil, "").ID
	recs, err := p.store.ListEternalRoles(ctx, i.GuildID)
	if err != nil {
		core.RespondErr(s, i, "Not removed", err)
		return
	}
	idx := slices.IndexFunc(recs, func(r EternalRoleRecord) bool {
		return r.UserID == userID && (r.OriginRoleID == roleID || r.RoleID == roleID)
	})
	if idx < 0 {
		core.RespondErr(s, i, "Not removed", fmt.Errorf("%s does not keep %s", core.MentionUser(userID), core.MentionRole(roleID)))
		return
	}
	rec := recs[idx]
	if err := p.store.DeleteEternalRole(ctx, i.GuildID, rec.UserID, rec.OriginRoleID); err != nil {
		core.RespondErr(s, i, "Not removed", err)
		return
	}
	if err := p.audit.Record(ctx, i.GuildID, actorID(i), "roles.eternal_removed", core.MentionRole(rec.RoleID),
		fmt.Sprintf("no longer an eternal role of %s", core.MentionUser(userID))); err != nil {
		p.log.Error("roles: audit eternal-remove failed", "guild", i.GuildID, "err", err)
	}
	core.RespondOK(s, i, "Eternal role removed",
		fmt.Sprintf("%s no longer keeps %s. The role itself is untouched.", core.MentionUser(userID), core.MentionRole(rec.RoleID)))
}

// requireDefiner answers the interaction and returns false unless the actor
// may define eternal roles.
func (p *Plugin) requireDefiner(s *discordgo.Session, i *discordgo.InteractionCreate) bool {
	ok, err := p.canDefineEternal(i.GuildID, actorID(i))
	if err != nil {
		core.RespondErr(s, i, "Not allowed", err)
		return false
	}
	if !ok {
		core.RespondErr(s, i, "Not allowed", fmt.Errorf("only the server owner (or merlin's operator) can add or remove eternal roles; admins can only turn the script off"))
		return false
	}
	return true
}
