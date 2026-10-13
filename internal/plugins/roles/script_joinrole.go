package roles

import (
	"context"
	"fmt"
	"math/bits"
	"slices"
	"strings"
	"time"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/rest"

	"github.com/6586x57890143/merlin/internal/core"
)

// The join-role script: everyone who joins gets the guild's join roles.
//
// Those are the member role (/roles configure member-role), the same one
// memberBaseline caps the jail and vacation markers against, plus any extra
// roles added with /roles configure join-role-add (a media-permissions role,
// say). The member role is not duplicated into the list: a second setting
// naming "the role ordinary members hold" would be two facts that have to
// agree. No join roles at all means the script does nothing. It hooks
// HandleMemberJoin and nothing else: no sweep, no backfill, so it covers
// joins from the moment it is on.
//
// It waits joinRoleSettle before looking, then re-fetches the member and
// adds only the roles they still lack. The wait is for other bots that
// persist roles across a rejoin: whatever one restores, merlin leaves alone.
// It yields to jail and vacation (any role_jails row), since a role carrying
// channel allows would beat the marker's denies, and the switch is read
// after the wait, so turning it off stops joins already pending.
//
// No audit entry per join: during a raid that is one embed per member
// against the guild's message budget. The log line and Discord's own audit
// log reason are the trail.
//
// A guild that configured join roles depends on them existing and doing
// what they did, so an edit or deletion of one in Discord is reported to the
// audit log whether or not the script is on (joinRoleUpdated,
// joinRoleDeleted). Position is the one change ignored, since Discord sends
// an update for every role a reorder shifts, except where a move takes the
// role out of merlin's reach.

const scriptJoinRole = "join-role"

// joinRoleSettle is how long a new member is left alone first. A var so
// tests need not wait.
var joinRoleSettle = 15 * time.Second

const joinRoleTimeout = 15 * time.Second

// joinRoles is every role a new member of guildID is given, member role
// first.
func (p *Plugin) joinRoles(guildID string) []string {
	var out []string
	if id := p.jailChannelConfig.MemberRoleID(guildID); id != "" {
		out = append(out, id)
	}
	for _, id := range p.jailChannelConfig.JoinRoleIDs(guildID) {
		if id != "" && !slices.Contains(out, id) {
			out = append(out, id)
		}
	}
	return out
}

// scheduleJoinRole is the script's hook into HandleMemberJoin. The handler's
// context ends with the event, so the delayed check gets its own.
func (p *Plugin) scheduleJoinRole(guildID, userID string) {
	if len(p.joinRoles(guildID)) == 0 {
		return
	}
	time.AfterFunc(joinRoleSettle, func() {
		ctx, cancel := context.WithTimeout(context.Background(), joinRoleTimeout)
		defer cancel()
		p.giveJoinRole(ctx, guildID, userID)
	})
}

func (p *Plugin) giveJoinRole(ctx context.Context, guildID, userID string) {
	roleIDs := p.joinRoles(guildID)
	if len(roleIDs) == 0 || p.dryRun(guildID) || !p.scriptOn(ctx, guildID, scriptJoinRole) {
		return
	}
	if _, jailed, err := p.store.GetJail(ctx, guildID, userID); err != nil || jailed {
		if err != nil {
			p.log.Error("roles: join-role: look up jail, skipping", "guild", guildID, "user", userID, "err", err)
		}
		return
	}
	ops := p.ops(guildID)
	member, err := ops.GetMember(core.ID(guildID), core.ID(userID))
	if err != nil {
		if !core.HasDiscordErrorCode(err, int(rest.JSONErrorCodeUnknownMember)) {
			p.log.Error("roles: join-role: fetch member", "guild", guildID, "user", userID, "err", err)
		}
		return
	}
	if member.User.Bot {
		return
	}
	// One additive call per role rather than one GuildMemberEdit with the
	// whole list: an edit replaces roles, and would race whatever another
	// bot is restoring at the same moment. One failing does not stop the rest.
	for _, roleID := range roleIDs {
		if slices.Contains(core.IDStrings(member.RoleIDs), roleID) {
			continue
		}
		if err := ops.AddMemberRole(core.ID(guildID), core.ID(userID), core.ID(roleID), rest.WithReason("join-role script")); err != nil {
			p.log.Error("roles: join-role: add role", "guild", guildID, "user", userID, "role", roleID, "err", err)
			continue
		}
		p.log.Info("roles: join-role: role given", "guild", guildID, "user", userID, "role", roleID)
	}
}

// joinRoleLine renders the script's definition for guildID.
func (p *Plugin) joinRoleLine(guildID string) string {
	ids := p.joinRoles(guildID)
	if len(ids) == 0 {
		return "no join roles configured, so it does nothing (set them on `/roles settings`)"
	}
	mentions := make([]string, len(ids))
	for i, id := range ids {
		mentions[i] = core.MentionRole(id)
	}
	return "new members get " + strings.Join(mentions, ", ")
}

// joinRoleShape is what of a join role is worth reporting a change to.
type joinRoleShape struct {
	name        string
	permissions discord.Permissions
	// manageable is whether merlin can give the role out at all.
	manageable bool
}

func (p *Plugin) shapeOf(guildID string, r discord.Role) joinRoleShape {
	return joinRoleShape{name: r.Name, permissions: r.Permissions, manageable: p.perms.CanManageRole(guildID, r.ID.String()) == nil}
}

// watchJoinRoles records the current shape of any join role in guildID not
// already known, so its first edit has something to compare against. Run
// from SyncGuild and whenever a join role is configured. Known entries are
// kept, so an edit made while merlin was disconnected is still reported, at
// the next edit, against what was there before.
//
// ponytail: an edit made while disconnected and never followed by another
// goes unreported. Compare here on GuildCreate if that ever matters.
func (p *Plugin) watchJoinRoles(guildID string) {
	ids := p.joinRoles(guildID)
	if len(ids) == 0 {
		return
	}
	roles, err := p.ops(guildID).GetRoles(core.ID(guildID))
	if err != nil {
		p.log.Error("roles: join-role: read roles to watch", "guild", guildID, "err", err)
		return
	}
	p.joinRoleMu.Lock()
	defer p.joinRoleMu.Unlock()
	for _, r := range roles {
		key := guildID + ":" + r.ID.String()
		if _, seen := p.joinRoleSeen[key]; !seen && slices.Contains(ids, r.ID.String()) {
			p.joinRoleSeen[key] = p.shapeOf(guildID, r)
		}
	}
}

// joinRoleUpdated reports an edit to one of guildID's join roles.
func (p *Plugin) joinRoleUpdated(ctx context.Context, guildID string, role discord.Role) {
	if !slices.Contains(p.joinRoles(guildID), role.ID.String()) {
		return
	}
	now := p.shapeOf(guildID, role)
	key := guildID + ":" + role.ID.String()
	p.joinRoleMu.Lock()
	before, known := p.joinRoleSeen[key]
	p.joinRoleSeen[key] = now
	p.joinRoleMu.Unlock()
	if known && before == now {
		return
	}
	var lines []string
	if !known {
		// Watching failed earlier. Saying so beats saying nothing about an
		// edit to a role the guild depends on.
		lines = append(lines, "Edited; what it was before is unknown.")
	} else {
		if before.name != now.name {
			lines = append(lines, "Renamed from **"+before.name+"** to **"+now.name+"**.")
		}
		if lost := permNames(before.permissions &^ now.permissions); lost != "" {
			lines = append(lines, "Lost: "+lost+".")
		}
		if gained := permNames(now.permissions &^ before.permissions); gained != "" {
			lines = append(lines, "Gained: "+gained+".")
		}
		if before.manageable && !now.manageable {
			lines = append(lines, "merlin can no longer give it out: it now sits at or above merlin's own top role.")
		}
		if !before.manageable && now.manageable {
			lines = append(lines, "merlin can give it out again.")
		}
	}
	p.auditJoinRole(ctx, guildID, "roles.join_role_changed",
		core.MentionRole(role.ID.String())+" is given to every new member.\n"+strings.Join(lines, "\n"))
}

// joinRoleDeleted reports the deletion of one of guildID's join roles. Runs
// before settings prunes the role (cmd/bot/main.go), which is what lets it
// still recognise the role as one.
func (p *Plugin) joinRoleDeleted(ctx context.Context, guildID, roleID string) {
	if !slices.Contains(p.joinRoles(guildID), roleID) {
		return
	}
	key := guildID + ":" + roleID
	p.joinRoleMu.Lock()
	name := p.joinRoleSeen[key].name
	delete(p.joinRoleSeen, key)
	p.joinRoleMu.Unlock()
	if name == "" {
		name = roleID
	}
	p.auditJoinRole(ctx, guildID, "roles.join_role_deleted",
		"**"+name+"** was deleted in Discord. It was given to every new member, and new members no longer get it. "+
			"Recreate it, then add it back on `/roles settings`.")
}

func (p *Plugin) auditJoinRole(ctx context.Context, guildID, action, detail string) {
	if err := p.audit.Record(ctx, guildID, core.ActorSystem, action, "", detail); err != nil {
		p.log.Error("roles: join-role: audit failed", "guild", guildID, "action", action, "err", err)
	}
}

// permBits names the permissions a join role plausibly carries. Anything
// else is still reported, as a count.
var permBits = []struct {
	bit  discord.Permissions
	name string
}{
	{discord.PermissionAdministrator, "Administrator"},
	{discord.PermissionViewChannel, "View Channels"},
	{discord.PermissionSendMessages, "Send Messages"},
	{discord.PermissionSendMessagesInThreads, "Send Messages in Threads"},
	{discord.PermissionCreatePublicThreads, "Create Public Threads"},
	{discord.PermissionEmbedLinks, "Embed Links"},
	{discord.PermissionAttachFiles, "Attach Files"},
	{discord.PermissionAddReactions, "Add Reactions"},
	{discord.PermissionUseExternalEmojis, "Use External Emoji"},
	{discord.PermissionUseExternalStickers, "Use External Stickers"},
	{discord.PermissionReadMessageHistory, "Read Message History"},
	{discord.PermissionSendVoiceMessages, "Send Voice Messages"},
	{discord.PermissionSendPolls, "Create Polls"},
	{discord.PermissionUseApplicationCommands, "Use Application Commands"},
	{discord.PermissionUseExternalApps, "Use External Apps"},
	{discord.PermissionConnect, "Connect"},
	{discord.PermissionSpeak, "Speak"},
	{discord.PermissionStream, "Video"},
	{discord.PermissionUseVAD, "Use Voice Activity"},
	{discord.PermissionUseSoundboard, "Use Soundboard"},
	{discord.PermissionUseEmbeddedActivities, "Use Activities"},
	{discord.PermissionChangeNickname, "Change Nickname"},
	{discord.PermissionCreateInstantInvite, "Create Invite"},
	{discord.PermissionMentionEveryone, "Mention @everyone"},
	{discord.PermissionManageMessages, "Manage Messages"},
}

func permNames(set discord.Permissions) string {
	var names []string
	for _, b := range permBits {
		if set&b.bit != 0 {
			names = append(names, b.name)
			set &^= b.bit
		}
	}
	switch n := bits.OnesCount64(uint64(set)); {
	case n == 1:
		names = append(names, "1 other permission")
	case n > 1:
		names = append(names, fmt.Sprintf("%d other permissions", n))
	}
	return strings.Join(names, ", ")
}
