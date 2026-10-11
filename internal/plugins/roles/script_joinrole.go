package roles

import (
	"context"
	"slices"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/6586x57890143/merlin/internal/core"
)

// The join-role script: everyone who joins gets one role.
//
// The role is the guild's member role (/roles configure member-role), the
// same one memberBaseline caps the jail and vacation markers against: a
// second setting naming "the role ordinary members hold" would be two facts
// that have to agree. No member role configured means the script does
// nothing. It hooks HandleMemberJoin and nothing else: no sweep, no
// backfill, so it covers joins from the moment it is on.
//
// It waits joinRoleSettle before looking, then re-fetches the member and
// adds the role only if they still lack it. The wait is for other bots that
// persist roles across a rejoin: if one restores the role, merlin does
// nothing. It yields to jail and vacation (any role_jails row), since a role
// carrying channel allows would beat the marker's denies, and the switch is
// read after the wait, so turning it off stops joins already pending.
//
// No audit entry per join: during a raid that is one embed per member
// against the guild's message budget. The log line and Discord's own audit
// log reason are the trail.

const scriptJoinRole = "join-role"

// joinRoleSettle is how long a new member is left alone first. A var so
// tests need not wait.
var joinRoleSettle = 15 * time.Second

const joinRoleTimeout = 15 * time.Second

// scheduleJoinRole is the script's hook into HandleMemberJoin. The handler's
// context ends with the event, so the delayed check gets its own.
func (p *Plugin) scheduleJoinRole(guildID, userID string) {
	if p.jailChannelConfig.MemberRoleID(guildID) == "" {
		return
	}
	time.AfterFunc(joinRoleSettle, func() {
		ctx, cancel := context.WithTimeout(context.Background(), joinRoleTimeout)
		defer cancel()
		p.giveJoinRole(ctx, guildID, userID)
	})
}

func (p *Plugin) giveJoinRole(ctx context.Context, guildID, userID string) {
	roleID := p.jailChannelConfig.MemberRoleID(guildID)
	if roleID == "" || p.dryRun(guildID) || !p.scriptOn(ctx, guildID, scriptJoinRole) {
		return
	}
	if _, jailed, err := p.store.GetJail(ctx, guildID, userID); err != nil || jailed {
		if err != nil {
			p.log.Error("roles: join-role: look up jail, skipping", "guild", guildID, "user", userID, "err", err)
		}
		return
	}
	ops := p.ops(guildID)
	member, err := ops.GuildMember(guildID, userID)
	if err != nil {
		if !core.HasDiscordErrorCode(err, discordgo.ErrCodeUnknownMember) {
			p.log.Error("roles: join-role: fetch member", "guild", guildID, "user", userID, "err", err)
		}
		return
	}
	if (member.User != nil && member.User.Bot) || slices.Contains(member.Roles, roleID) {
		return
	}
	if err := ops.GuildMemberRoleAdd(guildID, userID, roleID, discordgo.WithAuditLogReason("join-role script")); err != nil {
		p.log.Error("roles: join-role: add role", "guild", guildID, "user", userID, "role", roleID, "err", err)
		return
	}
	p.log.Info("roles: join-role: role given", "guild", guildID, "user", userID, "role", roleID)
}

// joinRoleLine renders the script's definition for guildID.
func (p *Plugin) joinRoleLine(guildID string) string {
	id := p.jailChannelConfig.MemberRoleID(guildID)
	if id == "" {
		return "no member role configured, so it does nothing (`/roles configure member-role`)"
	}
	return "new members get " + core.MentionRole(id)
}
