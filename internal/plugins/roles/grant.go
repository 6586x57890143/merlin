package roles

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/6586x57890143/merlin/internal/core"
)

func (p *Plugin) handleGrant(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate) {
	opts := core.LeafArgs(i)
	userID := opts["user"].Value.(string)
	roleID := opts["role"].Value.(string)
	reason := ""
	if v, ok := opts["reason"]; ok {
		reason = v.StringValue()
	}

	if err := p.perms.CanManageRole(i.GuildID, roleID); err != nil {
		core.RespondErr(s, i, "Cannot manage that role", err)
		return
	}

	var expiresAt *time.Time
	durationLabel := "permanently"
	if v, ok := opts["duration"]; ok {
		duration, err := core.ParseFlexibleDuration(v.StringValue())
		if err != nil {
			core.RespondErr(s, i, "Invalid duration", err)
			return
		}
		at := p.now().Add(duration)
		expiresAt = &at
		durationLabel = "for " + core.FormatDuration(duration)
	}

	if err := p.ops(i.GuildID).GuildMemberRoleAdd(i.GuildID, userID, roleID); err != nil {
		core.RespondErr(s, i, "Failed to grant role", err)
		return
	}

	if err := p.store.InsertGrant(ctx, GrantRecord{
		GuildID:   i.GuildID,
		UserID:    userID,
		RoleID:    roleID,
		GrantedAt: p.now(),
		ExpiresAt: expiresAt,
		GrantedBy: actorID(i),
		Reason:    reason,
	}); err != nil {
		core.RespondErr(s, i, "Failed to save grant record", err)
		return
	}

	if err := p.audit.Record(ctx, i.GuildID, actorID(i), "roles.grant", "", fmt.Sprintf("user=%s role=%s reason=%q", userID, roleID, reason)); err != nil {
		p.log.Error("roles: audit grant failed", "guild", i.GuildID, "user", userID, "err", err)
	}

	core.RespondOK(s, i, "Role granted", fmt.Sprintf("<@&%s> granted to <@%s> %s.", roleID, userID, durationLabel))
}

func (p *Plugin) handleRevoke(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate) {
	opts := core.LeafArgs(i)
	userID := opts["user"].Value.(string)
	roleID := opts["role"].Value.(string)

	if _, ok, err := p.store.GetGrant(ctx, i.GuildID, userID, roleID); err != nil {
		core.RespondErr(s, i, "Failed to look up grant", err)
		return
	} else if !ok {
		core.RespondErr(s, i, "Not a tracked grant", fmt.Errorf("merlin isn't tracking a grant of <@&%s> to <@%s> — revoke it directly in Discord if needed", roleID, userID))
		return
	}

	if err := p.revokeGrant(ctx, i.GuildID, userID, roleID, actorID(i)); err != nil {
		core.RespondErr(s, i, "Failed to revoke", err)
		return
	}
	core.RespondOK(s, i, "Role revoked", fmt.Sprintf("<@&%s> revoked from <@%s>.", roleID, userID))
}

// revokeGrant removes roleID from userID and stops tracking the grant.
// Shared by the sweep job (automatic expiry) and handleRevoke (a mod
// revoking early) for the same confused-deputy reason releaseJail is: if a
// mod already manually removed the role, there's nothing left to revoke —
// just stop tracking it.
func (p *Plugin) revokeGrant(ctx context.Context, guildID, userID, roleID, actor string) error {
	member, err := p.ops(guildID).GuildMember(guildID, userID)
	if err != nil {
		if core.IsUnknownResource(err) {
			// Member left the guild — the grant left with them.
			return p.store.DeleteGrant(ctx, guildID, userID, roleID)
		}
		// Transient: untracking here would leave a timed grant in place
		// forever, silently turning "for 24 hours" into permanent.
		return fmt.Errorf("roles: fetch member %s for revoke: %w", userID, err)
	}

	if slices.Contains(member.Roles, roleID) {
		if err := p.ops(guildID).GuildMemberRoleRemove(guildID, userID, roleID); err != nil {
			return fmt.Errorf("roles: remove granted role %s from %s: %w", roleID, userID, err)
		}
	}

	if err := p.audit.Record(ctx, guildID, actor, "roles.grant", fmt.Sprintf("user=%s role=%s", userID, roleID), ""); err != nil {
		p.log.Error("roles: audit revoke failed", "guild", guildID, "user", userID, "role", roleID, "err", err)
	}

	return p.store.DeleteGrant(ctx, guildID, userID, roleID)
}
