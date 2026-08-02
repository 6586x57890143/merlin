package roles

import (
	"context"
	"fmt"
	"slices"

	"github.com/bwmarrin/discordgo"

	"github.com/6586x57890143/merlin/internal/core"
)

// handleJail snapshots userID's current roles, replaces them with just the
// jail marker role (resolveJailRole) plus any role the bot structurally
// can't manage (CanManageRole — spec.MD §4 item 4), and schedules automatic
// release. The snapshot always records every role the member held, even
// ones the bot couldn't strip, so release restores the member's exact prior
// state regardless of which roles jail itself was able to touch. Channel
// visibility is handled entirely by the Jailed role's own permission
// overwrites (jailchannels.go) — jailing a member never touches channels
// directly, only which roles they hold.
func (p *Plugin) handleJail(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate) {
	opts := core.LeafArgs(i)
	userID := opts["user"].Value.(string)
	duration, err := core.ParseFlexibleDuration(opts["duration"].StringValue())
	if err != nil {
		core.RespondErr(s, i, "Invalid duration", err)
		return
	}
	reason := ""
	if v, ok := opts["reason"]; ok {
		reason = v.StringValue()
	}

	if _, ok, err := p.store.GetJail(ctx, i.GuildID, userID); err != nil {
		core.RespondErr(s, i, "Failed to check existing jail", err)
		return
	} else if ok {
		core.RespondErr(s, i, "Already jailed", fmt.Errorf("<@%s> is already jailed — use `/roles release` first", userID))
		return
	}

	member, err := p.ops.GuildMember(i.GuildID, userID)
	if err != nil {
		core.RespondErr(s, i, "Failed to fetch member", err)
		return
	}
	jailRoleID, err := p.resolveJailRole(i.GuildID)
	if err != nil {
		core.RespondErr(s, i, "Failed to resolve jail role", err)
		return
	}

	var unmanageable []string
	newRoles := []string{jailRoleID}
	for _, r := range member.Roles {
		if err := p.perms.CanManageRole(i.GuildID, r); err != nil {
			unmanageable = append(unmanageable, r)
			newRoles = append(newRoles, r)
		}
	}

	if _, err := p.ops.GuildMemberEdit(i.GuildID, userID, &discordgo.GuildMemberParams{Roles: &newRoles}); err != nil {
		core.RespondErr(s, i, "Failed to update roles", err)
		return
	}

	now := p.now()
	releaseAt := now.Add(duration)
	if err := p.store.InsertJail(ctx, JailRecord{
		GuildID:         i.GuildID,
		UserID:          userID,
		SnapshotRoleIDs: member.Roles,
		JailRoleID:      jailRoleID,
		JailedAt:        now,
		ReleaseAt:       &releaseAt,
		JailedBy:        actorID(i),
		Reason:          reason,
	}); err != nil {
		core.RespondErr(s, i, "Failed to save jail record", err)
		return
	}

	if err := p.audit.Record(ctx, i.GuildID, actorID(i), "roles.jail", "", fmt.Sprintf("user=%s duration=%s reason=%q unmanageable_roles=%v", userID, core.FormatDuration(duration), reason, unmanageable)); err != nil {
		p.log.Error("roles: audit jail failed", "guild", i.GuildID, "user", userID, "err", err)
	}

	msg := fmt.Sprintf("<@%s> jailed for %s.", userID, core.FormatDuration(duration))
	if len(unmanageable) > 0 {
		msg += fmt.Sprintf(" %d role(s) could not be stripped (positioned at/above Merlin's own top role).", len(unmanageable))
	}
	core.RespondOK(s, i, "Member jailed", msg)
}

func (p *Plugin) handleRelease(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate) {
	userID := core.LeafArgs(i)["user"].Value.(string)

	rec, ok, err := p.store.GetJail(ctx, i.GuildID, userID)
	if err != nil {
		core.RespondErr(s, i, "Failed to look up jail", err)
		return
	}
	if !ok {
		core.RespondErr(s, i, "Not jailed", fmt.Errorf("<@%s> isn't currently jailed", userID))
		return
	}

	if err := p.releaseJail(ctx, i.GuildID, userID, rec); err != nil {
		core.RespondErr(s, i, "Failed to release", err)
		return
	}
	core.RespondOK(s, i, "Member released", fmt.Sprintf("<@%s> has been released and their prior roles restored.", userID))
}

// releaseJail restores rec's snapshotted roles to userID and stops tracking
// the jail. Shared by the sweep job (automatic release once due) and
// handleRelease (a mod releasing early) so both paths apply the exact same
// confused-deputy safeguard: re-fetch the member fresh and only restore if
// they still hold the jail marker role. If a mod already manually changed
// the member's roles (marker gone), that's treated as an implicit "already
// handled" — stop tracking, don't fight the manual override, matching
// rotation.sweepOne's rescue-hatch precedent.
func (p *Plugin) releaseJail(ctx context.Context, guildID, userID string, rec JailRecord) error {
	member, err := p.ops.GuildMember(guildID, userID)
	if err != nil {
		// Member left the guild (or another fetch error) — nothing left to
		// restore. Drop the tracking row rather than error forever.
		return p.store.DeleteJail(ctx, guildID, userID)
	}

	if !slices.Contains(member.Roles, rec.JailRoleID) {
		p.log.Info("roles: jail marker already gone, treating as already released", "guild", guildID, "user", userID)
		return p.store.DeleteJail(ctx, guildID, userID)
	}

	guildRoles, err := p.ops.GuildRoles(guildID)
	if err != nil {
		return fmt.Errorf("roles: list guild roles for release: %w", err)
	}
	valid := make(map[string]bool, len(guildRoles))
	for _, r := range guildRoles {
		valid[r.ID] = true
	}
	restore := make([]string, 0, len(rec.SnapshotRoleIDs))
	for _, id := range rec.SnapshotRoleIDs {
		if valid[id] {
			restore = append(restore, id)
		}
	}

	if _, err := p.ops.GuildMemberEdit(guildID, userID, &discordgo.GuildMemberParams{Roles: &restore}); err != nil {
		return fmt.Errorf("roles: restore roles for %s: %w", userID, err)
	}

	if err := p.audit.Record(ctx, guildID, "system", "roles.release", "", fmt.Sprintf("user=%s restored=%v", userID, restore)); err != nil {
		p.log.Error("roles: audit release failed", "guild", guildID, "user", userID, "err", err)
	}

	return p.store.DeleteJail(ctx, guildID, userID)
}
