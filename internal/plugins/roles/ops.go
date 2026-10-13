package roles

import (
	"fmt"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/rest"
	"github.com/disgoorg/snowflake/v2"

	"github.com/6586x57890143/merlin/internal/core"
)

// DiscordMemberOps is what roles does in Discord, on disgo's types.
// discordguard.GuildOps satisfies it, so every write is gated; tests use an
// in-memory fake instead of live Discord.
type DiscordMemberOps interface {
	// GetMember is a live REST fetch, deliberately not the gateway
	// cache's view: every confused-deputy re-check in this plugin (release.go,
	// sweep.go, handleRelease, handleRevoke) needs the member's actual current roles,
	// not a possibly-stale gateway cache snapshot.
	GetMember(guildID, userID snowflake.ID) (*discord.Member, error)
	// GetMembers pages the guild's member list, which is the only way to
	// find everyone holding a given role, since Discord has no "members with role
	// X" endpoint. It needs the privileged GUILD_MEMBERS intent to return
	// anything, which is why /roles jail-role reports that plainly rather
	// than silently finding nobody. See membersWithRole.
	GetMembers(guildID snowflake.ID, limit int, after snowflake.ID) ([]discord.Member, error)
	// GetUser is the guild-free account lookup, and it is what makes a
	// sentence against somebody who is not here possible at all: a member
	// fetch answers Unknown Member for a stranger and for a typo alike, so
	// it cannot tell "not in this server" from "not a Discord account".
	// This one can, which is the double-check resolveTargets runs before it
	// records a sentence nobody is standing in front of.
	GetUser(userID snowflake.ID) (*discord.User, error)
	// UpdateMember replaces a member's roles (jail and release).
	UpdateMember(guildID, userID snowflake.ID, u discord.MemberUpdate) (*discord.Member, error)
	// DisconnectMember drops a member from voice; see disconnectFromVoice.
	DisconnectMember(guildID, userID snowflake.ID) error
	GetRoles(guildID snowflake.ID) ([]discord.Role, error)
	CreateRole(guildID snowflake.ID, r discord.RoleCreate) (*discord.Role, error)
	// UpdateRolePositions is used only by the eternal-role script, to put a
	// recreated role directly above the one it replaces. Discord accepts a
	// partial list, so the call names just the role being moved.
	UpdateRolePositions(guildID snowflake.ID, u []discord.RolePositionUpdate) ([]discord.Role, error)

	// GetGuild, CreateDMChannel and CreateMessage exist only to tell a member
	// what happened to them. A jail that arrives as silently vanishing access
	// is worse for the person on the receiving end and worse for the mods,
	// who then field the question in modmail. GetGuild is there because a DM
	// has to name the server: most members are in many, and "you have been
	// jailed" on its own is not actionable.
	GetGuild(guildID snowflake.ID) (*discord.RestGuild, error)
	CreateDMChannel(userID snowflake.ID) (*discord.DMChannel, error)
	CreateMessage(channelID snowflake.ID, m discord.MessageCreate) (*discord.Message, error)
	// AddMemberRole/RemoveMemberRole are used for grant/revoke: a single
	// additive role change, unlike jail/release's full role-list replace,
	// so a grant never disturbs any other role the member independently
	// holds.
	AddMemberRole(guildID, userID, roleID snowflake.ID, opts ...rest.RequestOpt) error
	RemoveMemberRole(guildID, userID, roleID snowflake.ID) error

	// GetChannel/GetGuildChannels/UpdatePermissionOverwrite/
	// DeletePermissionOverwrite back jailchannels.go's sync of the Jailed
	// role's per-channel overwrites: deny-by-default, allow only the guild's
	// configured exceptions.
	GetChannel(channelID snowflake.ID) (discord.Channel, error)
	GetGuildChannels(guildID snowflake.ID) ([]discord.GuildChannel, error)
	UpdatePermissionOverwrite(channelID, targetID snowflake.ID, u discord.PermissionOverwriteUpdate) error
	DeletePermissionOverwrite(channelID, targetID snowflake.ID) error
}

// RoleManager is the narrow view of *core.Permissions this plugin depends
// on. jail/grant both need the role-hierarchy safeguard (spec.MD §4 item
// 4), never previously wired up to any handler until this plugin, and jail
// additionally needs the actor-vs-target rank check that stops a TierMod
// command from being used to strip an admin's authority.
type RoleManager interface {
	CanManageRole(guildID, targetRoleID string) error
	CanModerate(guildID string, actor *core.Actor, targetUserID string, targetRoleIDs []string) error
	// IsBootstrapAdmin is only consulted by JailAutomatic, and only on the
	// path where a target has consented to being sanctioned despite their
	// rank. The bootstrap identity is the operator's guaranteed way back into
	// every guild, so it stays untargetable even then.
	IsBootstrapAdmin(userID string) bool
}

// setOverwrite writes target's whole overwrite on channelID.
func setOverwrite(ops DiscordMemberOps, channelID, targetID snowflake.ID, kind discord.PermissionOverwriteType, allow, deny discord.Permissions) error {
	if kind == discord.PermissionOverwriteTypeMember {
		return ops.UpdatePermissionOverwrite(channelID, targetID, discord.MemberPermissionOverwriteUpdate{Allow: &allow, Deny: &deny})
	}
	return ops.UpdatePermissionOverwrite(channelID, targetID, discord.RolePermissionOverwriteUpdate{Allow: &allow, Deny: &deny})
}

// guildChannel is channelID read as a guild channel.
func guildChannel(ops DiscordMemberOps, channelID snowflake.ID) (discord.GuildChannel, error) {
	c, err := ops.GetChannel(channelID)
	if err != nil {
		return nil, err
	}
	gc, ok := c.(discord.GuildChannel)
	if !ok {
		return nil, fmt.Errorf("channel %s is not in a server", channelID)
	}
	return gc, nil
}
