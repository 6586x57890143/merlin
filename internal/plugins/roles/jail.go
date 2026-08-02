package roles

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

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

	// Deferred because the first jail in a guild is the expensive one: it
	// creates the Jailed role and writes a deny overwrite to every channel,
	// which in a large server runs well past Discord's 3-second response
	// deadline. Every path below therefore answers with FollowUp*.
	if err := core.DeferResponse(s, i); err != nil {
		p.log.Error("roles: defer jail response failed", "guild", i.GuildID, "err", err)
		return
	}
	fail := func(title string, err error) {
		if ferr := core.FollowUpErr(s, i, title, err); ferr != nil {
			p.log.Error("roles: jail follow-up failed", "guild", i.GuildID, "err", ferr)
		}
	}

	// Checked before the tracking record is written, not left to
	// discordguard's per-call refusal: applyJail deliberately records the
	// jail before stripping roles, so a refusal at the strip would leave a
	// jail record for a member who was never actually jailed.
	if p.dryRun(i.GuildID) {
		if err := core.FollowUpOK(s, i, "Dry-run", fmt.Sprintf("Dry-run is enabled for this server — <@%s> was not jailed. Turn it off with `/config dryrun enabled:false`.", userID)); err != nil {
			p.log.Error("roles: jail dry-run follow-up failed", "guild", i.GuildID, "err", err)
		}
		return
	}

	if _, ok, err := p.store.GetJail(ctx, i.GuildID, userID); err != nil {
		fail("Failed to check existing jail", err)
		return
	} else if ok {
		fail("Already jailed", fmt.Errorf("<@%s> is already jailed — use `/roles release` first", userID))
		return
	}

	member, err := p.ops(i.GuildID).GuildMember(i.GuildID, userID)
	if err != nil {
		fail("Failed to fetch member", err)
		return
	}

	// Rank check before anything is created or written: jail is TierMod but
	// strips roles, so without this a mod could use it to strip an admin's
	// Administrator bit and with it their TierAdmin access to this bot.
	if err := p.perms.CanModerate(i.GuildID, i.Member, userID, member.Roles); err != nil {
		fail("Cannot jail that member", err)
		return
	}

	jailRoleID, err := p.resolveJailRole(i.GuildID)
	if err != nil {
		fail("Failed to resolve jail role", err)
		return
	}

	unmanageable, err := p.applyJail(ctx, i.GuildID, userID, jailRoleID, member.Roles, duration, actorID(i), reason)
	if err != nil {
		// Losing the insert race means a concurrent jail already recorded
		// this member and stripped their roles. Nothing was changed here, and
		// the snapshot on record is the one taken before they were stripped —
		// reporting it as a plain failure would invite a retry that could
		// only overwrite that snapshot with the stripped state.
		if errors.Is(err, ErrAlreadyJailed) {
			fail("Already jailed", fmt.Errorf("<@%s> was jailed by someone else a moment ago — use `/roles release` first", userID))
			return
		}
		fail("Failed to jail member", err)
		return
	}

	if err := p.audit.Record(ctx, i.GuildID, actorID(i), "roles.jail", "", fmt.Sprintf("user=%s duration=%s reason=%q unmanageable_roles=%v", userID, core.FormatDuration(duration), reason, unmanageable)); err != nil {
		p.log.Error("roles: audit jail failed", "guild", i.GuildID, "user", userID, "err", err)
	}

	msg := fmt.Sprintf("<@%s> jailed for %s.", userID, core.FormatDuration(duration))
	if len(unmanageable) > 0 {
		msg += fmt.Sprintf(" %d role(s) could not be stripped (positioned at/above Merlin's own top role).", len(unmanageable))
	}
	if err := core.FollowUpOK(s, i, "Member jailed", msg); err != nil {
		p.log.Error("roles: jail follow-up failed", "guild", i.GuildID, "err", err)
	}
}

// applyJail is the jail mutation itself: record the jail, then strip the
// member down to the marker role. Split out of handleJail so the ordering
// below is testable without a Discord session (same reason jailRoles is a
// free function), since ordering is the entire correctness argument here.
//
// The tracking record is written *before* the roles are stripped, and that
// order matters in exactly one direction. Stripping first and then failing to
// record left a member holding nothing but the marker with nothing anywhere
// tracking them: no sweep would ever release them, no /roles release would
// find a record, and from the outside it looks like an ordinary jail that
// simply never ends — the same "work silently abandoned, invisible from the
// outside" failure the untrack-on-gone rule exists to prevent.
//
// Recording first can only fail the other way: a record for a member whose
// roles were never touched. That self-heals on the next sweep a minute later
// through the confused-deputy check release already applies — the marker
// isn't on them, so it counts as already handled, gets untracked, and no
// roles are restored. A spurious row that cleans itself up beats a member
// jailed indefinitely. The rollback below just makes that immediate instead
// of eventual.
func (p *Plugin) applyJail(ctx context.Context, guildID, userID, jailRoleID string, currentRoles []string, duration time.Duration, actor, reason string) ([]string, error) {
	newRoles, unmanageable := jailRoles(p.perms, guildID, jailRoleID, currentRoles)

	now := p.now()
	releaseAt := now.Add(duration)
	if err := p.store.InsertJail(ctx, JailRecord{
		GuildID:         guildID,
		UserID:          userID,
		SnapshotRoleIDs: currentRoles,
		JailRoleID:      jailRoleID,
		JailedAt:        now,
		ReleaseAt:       &releaseAt,
		JailedBy:        actor,
		Reason:          reason,
	}); err != nil {
		return nil, fmt.Errorf("roles: save jail record for %s: %w", userID, err)
	}

	if _, err := p.ops(guildID).GuildMemberEdit(guildID, userID, &discordgo.GuildMemberParams{Roles: &newRoles}); err != nil {
		if core.HasDiscordErrorCode(err, discordgo.ErrCodeUnknownRole) {
			// The cached Jailed role was deleted in Discord. Forget it so the
			// next attempt resolves or recreates one instead of retrying
			// against a dead ID until the process restarts. Deliberately not
			// any "unknown resource": this same call reports an unknown
			// *member* when the target left the guild, which says nothing
			// about the role and would throw away a good cache entry.
			p.forgetJailRole(guildID)
		}
		if delErr := p.store.DeleteJail(ctx, guildID, userID); delErr != nil {
			p.log.Error("roles: roll back jail record after failed role update",
				"guild", guildID, "user", userID, "err", delErr)
		}
		return nil, fmt.Errorf("roles: strip roles for %s: %w", userID, err)
	}
	return unmanageable, nil
}

// jailRoles decides what a jailed member ends up holding: the marker role,
// plus every role the bot structurally can't manage (positioned at or above
// its own top role — spec.MD §4 item 4), which stay put and are returned
// separately so the mod is told the jail was partial rather than left to
// discover it. A pure function of the member's current roles, so the rule
// stays testable without a Discord session.
func jailRoles(perms RoleManager, guildID, jailRoleID string, current []string) (newRoles, unmanageable []string) {
	newRoles = []string{jailRoleID}
	for _, r := range current {
		if err := perms.CanManageRole(guildID, r); err != nil {
			unmanageable = append(unmanageable, r)
			newRoles = append(newRoles, r)
		}
	}
	return newRoles, unmanageable
}

// rejoinGrace is how much later than JailedAt a member's JoinedAt must be
// before it counts as a rejoin. Discord stamps JoinedAt; this bot stamps
// JailedAt from its own clock, so a member jailed moments after joining can
// have the two within a second or two of each other under ordinary clock
// skew. Without the grace, that member plus a mod stripping the marker by
// hand in the same window would look like an evasion.
const rejoinGrace = 30 * time.Second

// rejoinedSinceJail reports whether member left the guild and came back after
// rec's jail began.
//
// This is what makes jail survive a leave-and-rejoin without the privileged
// GUILD_MEMBERS intent. Discord does not preserve roles across a rejoin, so a
// jailed member who left and returned comes back with nothing — including no
// marker role — and every channel the Jailed role was denying becomes visible
// again. The bot's own record still says "jailed", but nothing was checking.
//
// JoinedAt is the discriminator, and it comes free with the REST member fetch
// the sweep already makes. A member who never left has a JoinedAt from before
// their jail; one who left and returned has a JoinedAt after it. That
// distinction is exactly what separates an evasion from the case the
// confused-deputy rule protects — a mod deliberately stripping the marker to
// let someone out early — so honoring one doesn't cost the other.
func rejoinedSinceJail(member *discordgo.Member, rec JailRecord) bool {
	if member.JoinedAt.IsZero() {
		// No timestamp to reason from. Fail toward the existing behavior
		// (treat a missing marker as a manual release) rather than re-jailing
		// someone on a guess.
		return false
	}
	return member.JoinedAt.After(rec.JailedAt.Add(rejoinGrace))
}

// reapplyEvadedJails re-jails anyone who left and rejoined to escape a jail
// that is still in force.
//
// Runs on the same one-minute sweep as automatic release, so the window an
// evader gets is bounded by that tick rather than by how long nobody notices.
// Setting MERLIN_ENABLE_GUILD_MEMBERS_INTENT closes it to near-instant by
// also reacting to the rejoin event itself — see HandleMemberJoin.
func (p *Plugin) reapplyEvadedJails(ctx context.Context, guildID string) error {
	active, err := p.store.ActiveJails(ctx, guildID, p.now())
	if err != nil {
		return fmt.Errorf("roles sweep: query active jails: %w", err)
	}
	var firstErr error
	for _, rec := range active {
		if err := p.reapplyIfEvaded(ctx, guildID, rec); err != nil {
			p.log.Error("roles sweep: re-apply evaded jail failed", "guild", guildID, "user", rec.UserID, "err", err)
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

// reapplyIfEvaded re-applies rec's marker role if userID is back in the guild
// without it, having rejoined since being jailed.
//
// It deliberately does not touch the stored record. The snapshot holds the
// member's real pre-jail roles and is the only copy of them — overwriting it
// with what they hold now (nothing, having just rejoined) would mean their
// eventual release restores nothing, the same way the concurrent-jail race
// used to destroy it. ReleaseAt is left alone too: how long someone is jailed
// is a moderator's decision, and leaving the server neither serves the
// sentence nor extends it.
func (p *Plugin) reapplyIfEvaded(ctx context.Context, guildID string, rec JailRecord) error {
	member, err := p.ops(guildID).GuildMember(guildID, rec.UserID)
	if err != nil {
		if core.IsUnknownResource(err) {
			// Still gone. Their jail record stays: if they come back before
			// it expires, this same check re-applies it then.
			return nil
		}
		return fmt.Errorf("roles: fetch member %s for evasion check: %w", rec.UserID, err)
	}

	if slices.Contains(member.Roles, rec.JailRoleID) {
		return nil // Still jailed, nothing to do.
	}
	if !rejoinedSinceJail(member, rec) {
		// Marker gone without a rejoin: a mod released them by hand. That is
		// the confused-deputy rescue hatch, and it stays honored — the
		// existing sweep untracks them when the jail comes due.
		return nil
	}

	newRoles, unmanageable := jailRoles(p.perms, guildID, rec.JailRoleID, member.Roles)
	if _, err := p.ops(guildID).GuildMemberEdit(guildID, rec.UserID, &discordgo.GuildMemberParams{Roles: &newRoles}); err != nil {
		if core.HasDiscordErrorCode(err, discordgo.ErrCodeUnknownRole) {
			// The Jailed role itself was deleted. Drop the cached ID so the
			// next jail recreates it; this record can't be re-applied until
			// then, which /config status surfaces as a missing role.
			p.forgetJailRole(guildID)
		}
		return fmt.Errorf("roles: re-apply jail to %s: %w", rec.UserID, err)
	}

	p.log.Warn("roles: re-applied jail after rejoin", "guild", guildID, "user", rec.UserID,
		"jailed_at", rec.JailedAt, "joined_at", member.JoinedAt)
	if err := p.audit.Record(ctx, guildID, "system", "roles.jail_reapplied", rec.UserID,
		fmt.Sprintf("left and rejoined while jailed; jail re-applied until %s unmanageable_roles=%v", releaseAtText(rec), unmanageable)); err != nil {
		p.log.Error("roles: audit jail re-apply failed", "guild", guildID, "user", rec.UserID, "err", err)
	}
	return nil
}

func releaseAtText(rec JailRecord) string {
	if rec.ReleaseAt == nil {
		return "indefinitely"
	}
	return rec.ReleaseAt.Format(time.RFC3339)
}

// HandleMemberJoin re-applies a still-active jail the moment a member rejoins,
// rather than waiting for the next sweep. Only ever called when the operator
// has opted into the GUILD_MEMBERS intent (MERLIN_ENABLE_GUILD_MEMBERS_INTENT)
// — without it Discord never sends the event, and the sweep above remains the
// sole mechanism.
func (p *Plugin) HandleMemberJoin(ctx context.Context, guildID, userID string) {
	rec, ok, err := p.store.GetJail(ctx, guildID, userID)
	if err != nil {
		p.log.Error("roles: look up jail on member join", "guild", guildID, "user", userID, "err", err)
		return
	}
	if !ok {
		return
	}
	if rec.ReleaseAt != nil && !rec.ReleaseAt.After(p.now()) {
		// Sentence already served while they were away; let the ordinary
		// sweep close the record out rather than re-jailing them here.
		return
	}
	if err := p.reapplyIfEvaded(ctx, guildID, rec); err != nil {
		p.log.Error("roles: re-apply jail on member join", "guild", guildID, "user", userID, "err", err)
	}
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
	member, err := p.ops(guildID).GuildMember(guildID, userID)
	if err != nil {
		if core.IsUnknownResource(err) {
			// Member left the guild — nothing left to restore.
			return p.store.DeleteJail(ctx, guildID, userID)
		}
		// Any other failure is transient. Dropping the row here would strand
		// the member in jail permanently with nothing left tracking them —
		// the worst outcome this plugin can produce, and invisible until
		// somebody complains. Fail so the next sweep (a minute later) retries.
		return fmt.Errorf("roles: fetch member %s for release: %w", userID, err)
	}

	if !slices.Contains(member.Roles, rec.JailRoleID) {
		p.log.Info("roles: jail marker already gone, treating as already released", "guild", guildID, "user", userID)
		return p.store.DeleteJail(ctx, guildID, userID)
	}

	guildRoles, err := p.ops(guildID).GuildRoles(guildID)
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

	if _, err := p.ops(guildID).GuildMemberEdit(guildID, userID, &discordgo.GuildMemberParams{Roles: &restore}); err != nil {
		return fmt.Errorf("roles: restore roles for %s: %w", userID, err)
	}

	if err := p.audit.Record(ctx, guildID, "system", "roles.release", "", fmt.Sprintf("user=%s restored=%v", userID, restore)); err != nil {
		p.log.Error("roles: audit release failed", "guild", guildID, "user", userID, "err", err)
	}

	return p.store.DeleteJail(ctx, guildID, userID)
}
