package roles

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/disgoorg/disgo/rest"

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
var pluginScripts = []string{scriptEternalRole, scriptVacation, scriptJoinRole}

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
	durationOpt := func(name, desc string, required bool) *discordgo.ApplicationCommandOption {
		return &discordgo.ApplicationCommandOption{Type: discordgo.ApplicationCommandOptionString, Name: name, Description: desc, Required: required}
	}
	reasonOpt := &discordgo.ApplicationCommandOption{
		Type: discordgo.ApplicationCommandOptionString, Name: "reason", Description: "Why (recorded in the audit log)", Required: true,
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
				// so the extra member slots follow duration and reason. They are plain
				// optional User pickers rather than a free-text list of IDs.
				// See collectJailUserIDs.
				Options: []*discordgo.ApplicationCommandOption{
					userOpt("user", "The member to jail; paste an ID to sentence an account that isn't here yet"),
					durationOpt("duration", "How long before automatic release: \"3d\", \"24h\", \"90m\", or \"forever\"", true),
					reasonOpt,
					optionalUserOpt("user2", "A second member, jailed with the same duration and reason"),
					optionalUserOpt("user3", "A third member"),
					optionalUserOpt("user4", "A fourth member"),
					optionalUserOpt("user5", "A fifth member"),
				},
			},
			{
				Type:        discordgo.ApplicationCommandOptionSubCommand,
				Name:        "vacation",
				Description: "vacation script: strip up to 5 members to the island's role for a period, then restore them",
				Options: []*discordgo.ApplicationCommandOption{
					userOpt("user", "The member to send on vacation; paste an ID for an account that isn't here yet"),
					durationOpt("duration", "How long before they come back: \"3d\", \"24h\", \"90m\", or \"forever\"", true),
					reasonOpt,
					optionalUserOpt("user2", "A second member, same duration and reason"),
					optionalUserOpt("user3", "A third member"),
					optionalUserOpt("user4", "A fourth member"),
					optionalUserOpt("user5", "A fifth member"),
				},
			},
			{
				Type:        discordgo.ApplicationCommandOptionSubCommand,
				Name:        "jail-role",
				Description: "Jail everyone holding one role, for shutting down a raid",
				Options: []*discordgo.ApplicationCommandOption{
					roleOpt("role", "Every member holding this role will be jailed"),
					durationOpt("duration", "How long before automatic release: \"3d\", \"24h\", \"90m\", or \"forever\"", true),
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
					reasonOpt,
					durationOpt("duration", "How long before automatic revocation. Needs a unit: \"3d\", \"24h\", \"90m\". Omit for permanent.", false),
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
				Type:        discordgo.ApplicationCommandOptionSubCommand,
				Name:        "settings",
				Description: "Jail and vacation channels, new members' roles, and scripts, on one panel",
			},
		},
	}

	p.commands.RegisterCommand(p.Name(), cmd)
	p.commands.HandleIx("roles", "jail", core.PermSpec{Tier: core.TierMod, Action: actionJail}, p.handleJail)
	p.commands.HandleIx("roles", "vacation", core.PermSpec{Tier: core.TierMod, Action: actionVacation}, p.handleVacation)
	p.commands.HandleIx("roles", "jail-role", core.PermSpec{Tier: core.TierAdmin, Action: actionJailRole}, p.handleJailRole)
	p.commands.HandleIx("roles", "release", core.PermSpec{Tier: core.TierMod, Action: actionJail}, p.handleRelease)
	p.commands.HandleIx("roles", "grant", core.PermSpec{Tier: core.TierAdmin, Action: actionGrant}, p.handleGrant)
	p.commands.HandleIx("roles", "revoke", core.PermSpec{Tier: core.TierAdmin, Action: actionGrant}, p.handleRevoke)
	p.commands.HandleIx("roles", "list", core.PermSpec{Tier: core.TierMod, Action: actionList}, p.handleList)
	p.commands.HandleIx("roles", "settings", viewSpec, p.handleSettings)
	p.registerPanel()
}

func actorID(ix *core.Ix) string {
	if ix.Member() != nil {
		return ix.UserID
	}
	return ""
}

func (p *Plugin) handleList(ctx context.Context, ix *core.Ix) {
	userID := ix.Args()["user"].String()

	var lines []string
	if rec, ok, err := p.store.GetJail(ctx, ix.GuildID, userID); err != nil {
		ix.RespondErr("Failed to look up jail", err)
		return
	} else if ok {
		release := "indefinite"
		if rec.ReleaseAt != nil {
			release = rec.ReleaseAt.Format(time.RFC3339)
		}
		state := "Jailed"
		if p.sentenceFor(ix.GuildID, rec.JailRoleID) == vacationSentence {
			state = "On vacation"
		}
		// Whether a sentence is in *effect* is a fact about Discord, not
		// about the row, so it is read from Discord rather than stored
		// beside it: a sentence recorded against somebody who is not here
		// (bulkjail.go's absent target) becomes an ordinary one the moment
		// they join, and a column saying "pending" would still say it.
		// Anything other than a clean Unknown Member is left alone: a rate
		// limit is not evidence that somebody is absent.
		if _, err := p.ops(ix.GuildID).GetMember(core.ID(ix.GuildID), core.ID(userID)); core.HasDiscordErrorCode(err, int(rest.JSONErrorCodeUnknownMember)) {
			state += " (not in the server; applies on arrival)"
		}
		lines = append(lines, fmt.Sprintf("**%s:** released at %s", state, release))
	}

	grants, err := p.store.ListGrants(ctx, ix.GuildID, userID)
	if err != nil {
		ix.RespondErr("Failed to list grants", err)
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
		ix.RespondInfo("No active jail or grants", fmt.Sprintf("<@%s> has no active jail and no tracked role grants.", userID))
		return
	}
	ix.RespondInfo(fmt.Sprintf("Status for <@%s>", userID), strings.Join(lines, "\n"))
}
