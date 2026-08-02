package roles

import "github.com/bwmarrin/discordgo"

// DiscordMemberOps is the narrow slice of *discordgo.Session this plugin
// needs — mirrors rotation.DiscordChannelOps's role: a real *discordgo.
// Session satisfies this structurally with zero glue code, while tests use
// an in-memory fake instead of live Discord.
type DiscordMemberOps interface {
	// GuildMember is a live REST fetch, deliberately not session.State's
	// cached view — every confused-deputy re-check in this plugin (sweep.go,
	// handleRelease, handleRevoke) needs the member's actual current roles,
	// not a possibly-stale gateway cache snapshot.
	GuildMember(guildID, userID string, options ...discordgo.RequestOption) (*discordgo.Member, error)
	// GuildMembers pages the guild's member list, which is the only way to
	// find everyone holding a given role — Discord has no "members with role
	// X" endpoint. It needs the privileged GUILD_MEMBERS intent to return
	// anything, which is why /roles jail-role reports that plainly rather
	// than silently finding nobody. See membersWithRole.
	GuildMembers(guildID string, after string, limit int, options ...discordgo.RequestOption) ([]*discordgo.Member, error)
	GuildMemberEdit(guildID, userID string, data *discordgo.GuildMemberParams, options ...discordgo.RequestOption) (*discordgo.Member, error)
	GuildRoles(guildID string, options ...discordgo.RequestOption) ([]*discordgo.Role, error)
	GuildRoleCreate(guildID string, data *discordgo.RoleParams, options ...discordgo.RequestOption) (*discordgo.Role, error)
	// GuildMemberRoleAdd/Remove are used for grant/revoke — a single
	// additive role change, unlike jail/release's full Roles-list replace,
	// so a grant never disturbs any other role the member independently
	// holds.
	GuildMemberRoleAdd(guildID, userID, roleID string, options ...discordgo.RequestOption) error
	GuildMemberRoleRemove(guildID, userID, roleID string, options ...discordgo.RequestOption) error

	// Channel/GuildChannels/ChannelPermissionSet/ChannelPermissionDelete back
	// jailchannels.go's sync of the Jailed role's per-channel overwrites —
	// deny-by-default, allow only the guild's configured exceptions.
	Channel(channelID string, options ...discordgo.RequestOption) (*discordgo.Channel, error)
	GuildChannels(guildID string, options ...discordgo.RequestOption) ([]*discordgo.Channel, error)
	ChannelPermissionSet(channelID, targetID string, targetType discordgo.PermissionOverwriteType, allow, deny int64, options ...discordgo.RequestOption) error
	ChannelPermissionDelete(channelID, targetID string, options ...discordgo.RequestOption) error
}

// RoleManager is the narrow view of *core.Permissions this plugin depends
// on — jail/grant both need the role-hierarchy safeguard (spec.MD §4 item
// 4), never previously wired up to any handler until this plugin, and jail
// additionally needs the actor-vs-target rank check that stops a TierMod
// command from being used to strip an admin's authority.
type RoleManager interface {
	CanManageRole(guildID, targetRoleID string) error
	CanModerate(guildID string, actor *discordgo.Member, targetUserID string, targetRoleIDs []string) error
}
