package roles

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"time"

	"github.com/disgoorg/disgo/rest"

	"github.com/6586x57890143/merlin/internal/core"
	"github.com/6586x57890143/merlin/internal/discordguard"
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

	if err := ix.Defer(); err != nil {
		return
	}
	if p.dryRun(ix.GuildID) {
		_ = ix.FollowUpOK("Dry-run", "The role and its grant record were left unchanged.")
		return
	}
	key := jailKey(ix.GuildID, userID)
	if !p.claim(key) {
		_ = ix.FollowUpErr("Member update in progress", errReleaseInProgress)
		return
	}
	defer p.unclaim(key)
	if _, jailed, err := p.store.GetJail(ctx, ix.GuildID, userID); err != nil || jailed {
		if err == nil {
			err = errors.New("release this member from jail or vacation before granting roles")
		}
		_ = ix.FollowUpErr("Cannot grant role", err)
		return
	}

	// Persist first. A failed or ambiguous Discord response must leave the
	// expiry recoverable by the sweep, even if the role was actually added.
	previous, tracked, err := p.store.GetGrant(ctx, ix.GuildID, userID, roleID)
	if err != nil {
		_ = ix.FollowUpErr("Failed to read grant record", err)
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
		_ = ix.FollowUpErr("Failed to save grant record", err)
		return
	}
	if expiresAt != nil {
		p.armGrantRevoke(ix.GuildID, userID, roleID, *expiresAt)
	}
	if err := p.ops(ix.GuildID).AddMemberRole(core.ID(ix.GuildID), core.ID(userID), core.ID(roleID)); err != nil {
		_ = ix.FollowUpErr("Role grant not confirmed", p.failedGrant(ctx, ix.GuildID, userID, roleID, previous, tracked, err))
		return
	}

	if err := p.audit.Record(ctx, ix.GuildID, actorID(ix), "roles.grant", "", fmt.Sprintf("user=%s role=%s reason=%q", core.MentionUser(userID), core.MentionRole(roleID), reason)); err != nil {
		p.log.Error("roles: audit grant failed", "guild", ix.GuildID, "user", userID, "err", err)
	}

	_ = ix.FollowUpOK("Role granted", fmt.Sprintf("<@&%s> granted to <@%s> %s.", roleID, userID, durationLabel))
}

// The member claim must be held by the caller. Guard refusals and explicit
// Discord rejections leave roles unchanged, unlike ambiguous transport failures.
// Restore prior tracking so a refused grant cannot time out an already-held role.
func (p *Plugin) failedGrant(ctx context.Context, guildID, userID, roleID string, previous GrantRecord, tracked bool, cause error) error {
	if !definitiveWriteRefusal(cause) {
		return fmt.Errorf("tracking retained for cleanup: %w", cause)
	}
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), stateWriteTimeout)
	defer cancel()
	var err error
	if tracked {
		err = p.store.InsertGrant(writeCtx, previous)
	} else {
		err = p.store.DeleteGrant(writeCtx, guildID, userID, roleID)
	}
	if err != nil {
		return fmt.Errorf("grant refused (%v), but restoring prior tracking failed: %w", cause, err)
	}
	key := grantKey(guildID, userID, roleID)
	p.timerMu.Lock()
	if timer := p.timers[key]; timer != nil {
		timer.Stop()
		delete(p.timers, key)
	}
	p.timerMu.Unlock()
	if tracked && previous.ExpiresAt != nil {
		p.armGrantRevoke(guildID, userID, roleID, *previous.ExpiresAt)
	}
	return cause
}

func definitiveWriteRefusal(err error) bool {
	if discordguard.Skipped(err) || errors.Is(err, discordguard.ErrRateLimited) || errors.Is(err, discordguard.ErrCircuitOpen) || core.IsUnknownResource(err) {
		return true
	}
	if failure, ok := errors.AsType[*rest.Error](err); ok && failure.Response != nil {
		status := failure.Response.StatusCode
		return status >= 400 && status < 500 && status != http.StatusRequestTimeout
	}
	return false
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

	// Same reason as handleRelease: a member fetch, a role edit and an
	// audit card can outrun the router's 2.5s response window.
	if err := ix.Defer(); err != nil {
		p.log.Error("roles: defer revoke response failed", "guild", ix.GuildID, "err", err)
		return
	}
	fail := func(title string, err error) {
		if ferr := ix.FollowUpErr(title, err); ferr != nil {
			p.log.Error("roles: revoke follow-up failed", "guild", ix.GuildID, "err", ferr)
		}
	}
	if err := p.revokeGrant(ctx, ix.GuildID, userID, roleID, actorID(ix)); err != nil {
		if errors.Is(err, errReleaseInProgress) {
			fail("Revoke in progress", fmt.Errorf("<@&%s> is being revoked from <@%s> right now", roleID, userID))
			return
		}
		fail("Failed to revoke", err)
		return
	}
	if err := ix.FollowUpOK("Role revoked", fmt.Sprintf("<@&%s> revoked from <@%s>.", roleID, userID)); err != nil {
		p.log.Error("roles: revoke follow-up failed", "guild", ix.GuildID, "err", err)
	}
}

// revokeGrant removes roleID from userID and stops tracking the grant.
// Shared by the expiry timer (release.go), the sweep job (its backstop) and
// handleRevoke (a mod revoking early) for the same confused-deputy reason
// releaseJail is: if a
// mod already manually removed the role, there's nothing left to revoke,
// just stop tracking it.
func (p *Plugin) revokeGrant(ctx context.Context, guildID, userID, roleID, actor string) error {
	return p.revokeGrantChecked(ctx, guildID, userID, roleID, actor, false)
}

func (p *Plugin) revokeGrantChecked(ctx context.Context, guildID, userID, roleID, actor string, dueOnly bool) error {
	memberKey := jailKey(guildID, userID)
	if !p.claim(memberKey) {
		return errReleaseInProgress
	}
	defer p.unclaim(memberKey)
	key := grantKey(guildID, userID, roleID)
	if !p.claim(key) {
		return errReleaseInProgress
	}
	defer p.unclaim(key)
	if dueOnly {
		rec, ok, err := p.store.GetGrant(ctx, guildID, userID, roleID)
		if err != nil {
			return err
		}
		if !ok || rec.ExpiresAt == nil || rec.ExpiresAt.After(p.now()) {
			return nil
		}
	}

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

	// Untracked before the audit card and on a context nothing can cancel,
	// for releaseJail's reason. The role is gone either way; if this write
	// fails, the sweep (or, for a permanent grant, the next /roles revoke)
	// finds it gone and drops the row.
	delCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), stateWriteTimeout)
	defer cancel()
	if err := p.store.DeleteGrant(delCtx, guildID, userID, roleID); err != nil {
		p.log.Error("roles: untrack revoked grant", "guild", guildID, "user", userID, "role", roleID, "err", err)
		return fmt.Errorf("roles: role removed but grant and jail snapshot cleanup failed; retry revoke: %w", err)
	}

	if err := p.audit.Record(ctx, guildID, actor, "roles.grant", fmt.Sprintf("user=%s role=%s", core.MentionUser(userID), core.MentionRole(roleID)), ""); err != nil {
		p.log.Error("roles: audit revoke failed", "guild", guildID, "user", userID, "role", roleID, "err", err)
	}
	return nil
}
