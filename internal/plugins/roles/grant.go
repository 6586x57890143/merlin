package roles

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/disgoorg/disgo/rest"

	"github.com/6586x57890143/merlin/internal/core"
)

func (p *Plugin) handleGrant(ctx context.Context, ix *core.Ix) {
	opts := ix.Args()
	userID := opts["user"].String()
	roleID := opts["role"].String()
	reason := ""
	if v, ok := opts["reason"]; ok {
		reason = v.String()
	}

	if err := p.perms.CanManageRole(ix.GuildID, roleID); err != nil {
		ix.RespondErr("Cannot manage that role", err)
		return
	}

	var expiresAt *time.Time
	durationLabel := "permanently"
	if v, ok := opts["duration"]; ok {
		duration, err := core.ParseFlexibleDuration(v.String())
		if err != nil {
			ix.RespondErr("Invalid duration", err)
			return
		}
		at := p.now().Add(duration)
		expiresAt = &at
		durationLabel = "for " + core.FormatDuration(duration)
	}

	if err := p.ops(ix.GuildID).AddMemberRole(core.ID(ix.GuildID), core.ID(userID), core.ID(roleID)); err != nil {
		ix.RespondErr("Failed to grant role", err)
		return
	}

	if err := p.store.InsertGrant(ctx, GrantRecord{
		GuildID:   ix.GuildID,
		UserID:    userID,
		RoleID:    roleID,
		GrantedAt: p.now(),
		ExpiresAt: expiresAt,
		GrantedBy: actorID(ix),
		Reason:    reason,
	}); err != nil {
		ix.RespondErr("Failed to save grant record", err)
		return
	}
	if expiresAt != nil {
		p.armGrantRevoke(ix.GuildID, userID, roleID, *expiresAt)
	}

	if err := p.audit.Record(ctx, ix.GuildID, actorID(ix), "roles.grant", "", fmt.Sprintf("user=%s role=%s reason=%q", core.MentionUser(userID), core.MentionRole(roleID), reason)); err != nil {
		p.log.Error("roles: audit grant failed", "guild", ix.GuildID, "user", userID, "err", err)
	}

	ix.RespondOK("Role granted", fmt.Sprintf("<@&%s> granted to <@%s> %s.", roleID, userID, durationLabel))
}

func (p *Plugin) handleRevoke(ctx context.Context, ix *core.Ix) {
	opts := ix.Args()
	userID := opts["user"].String()
	roleID := opts["role"].String()

	if _, ok, err := p.store.GetGrant(ctx, ix.GuildID, userID, roleID); err != nil {
		ix.RespondErr("Failed to look up grant", err)
		return
	} else if !ok {
		ix.RespondErr("Not a tracked grant", fmt.Errorf("merlin isn't tracking a grant of <@&%s> to <@%s>. Revoke it directly in Discord if needed", roleID, userID))
		return
	}

	if err := p.revokeGrant(ctx, ix.GuildID, userID, roleID, actorID(ix)); err != nil {
		if errors.Is(err, errReleaseInProgress) {
			ix.RespondErr("Revoke in progress", fmt.Errorf("<@&%s> is being revoked from <@%s> right now", roleID, userID))
			return
		}
		ix.RespondErr("Failed to revoke", err)
		return
	}
	ix.RespondOK("Role revoked", fmt.Sprintf("<@&%s> revoked from <@%s>.", roleID, userID))
}

// revokeGrant removes roleID from userID and stops tracking the grant.
// Shared by the expiry timer (release.go), the sweep job (its backstop) and
// handleRevoke (a mod revoking early) for the same confused-deputy reason
// releaseJail is: if a
// mod already manually removed the role, there's nothing left to revoke,
// just stop tracking it.
func (p *Plugin) revokeGrant(ctx context.Context, guildID, userID, roleID, actor string) error {
	key := grantKey(guildID, userID, roleID)
	if !p.claim(key) {
		return errReleaseInProgress
	}
	defer p.unclaim(key)

	member, err := p.ops(guildID).GetMember(core.ID(guildID), core.ID(userID))
	if err != nil {
		// Unknown Member only; see releaseJail.
		if core.HasDiscordErrorCode(err, int(rest.JSONErrorCodeUnknownMember)) {
			// Member left the guild, so the grant left with them.
			return p.store.DeleteGrant(ctx, guildID, userID, roleID)
		}
		// Transient: untracking here would leave a timed grant in place
		// forever, silently turning "for 24 hours" into permanent.
		return fmt.Errorf("roles: fetch member %s for revoke: %w", userID, err)
	}

	if slices.Contains(core.IDStrings(member.RoleIDs), roleID) {
		if err := p.ops(guildID).RemoveMemberRole(core.ID(guildID), core.ID(userID), core.ID(roleID)); err != nil {
			return fmt.Errorf("roles: remove granted role %s from %s: %w", roleID, userID, err)
		}
	}

	if err := p.audit.Record(ctx, guildID, actor, "roles.grant", fmt.Sprintf("user=%s role=%s", core.MentionUser(userID), core.MentionRole(roleID)), ""); err != nil {
		p.log.Error("roles: audit revoke failed", "guild", guildID, "user", userID, "role", roleID, "err", err)
	}

	return p.store.DeleteGrant(ctx, guildID, userID, roleID)
}
