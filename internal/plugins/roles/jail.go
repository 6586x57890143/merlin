package roles

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/6586x57890143/merlin/internal/core"
)

// handleJail snapshots userID's current roles, replaces them with just the
// jail marker role (resolveJailRole) plus any role the bot structurally
// can't manage (CanManageRole, spec.MD §4 item 4), and schedules automatic
// release. The snapshot always records every role the member held, even
// ones the bot couldn't strip, so release restores the member's exact prior
// state regardless of which roles jail itself was able to touch. Channel
// visibility is handled entirely by the Jailed role's own permission
// overwrites (jailchannels.go). Jailing a member never touches channels
// directly, only which roles they hold.
// jailUserOptionNames are /roles jail's member slots, in the order they are
// read. Discord has no multi-user option type, so "several people at once"
// means several native User pickers rather than a free-text list of IDs.
// spec.MD §4a's rule that user-valued options never take a raw string is
// exactly what stops a mistyped snowflake from jailing a stranger. Five is a
// judgement call: enough for the usual "these three started it", short of the
// point where /roles jail-role is the better tool.
var jailUserOptionNames = []string{"user", "user2", "user3", "user4", "user5"}

// collectJailUserIDs reads the filled-in member slots, in order, without
// duplicates: picking the same person in two slots is a slip, and letting it
// through would report them as "already jailed" by their own first slot.
func collectJailUserIDs(opts map[string]*discordgo.ApplicationCommandInteractionDataOption) []string {
	var ids []string
	for _, name := range jailUserOptionNames {
		opt, ok := opts[name]
		if !ok {
			continue
		}
		id, _ := opt.Value.(string)
		if id == "" || slices.Contains(ids, id) {
			continue
		}
		ids = append(ids, id)
	}
	return ids
}

func (p *Plugin) handleJail(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate) {
	opts := core.LeafArgs(i)
	userIDs := collectJailUserIDs(opts)
	duration, err := core.ParseFlexibleDuration(opts["duration"].StringValue())
	if err != nil {
		core.RespondErr(s, i, "Invalid duration", err)
		return
	}
	reason := ""
	if v, ok := opts["reason"]; ok {
		reason = v.StringValue()
	}
	if len(userIDs) == 0 {
		core.RespondErr(s, i, "No members given", errors.New("pick at least one member to jail"))
		return
	}

	// Deferred because the first jail in a guild is the expensive one: it
	// creates the Jailed role and writes a deny overwrite to every channel,
	// which in a large server runs well past Discord's 3-second response
	// deadline. Several members only add to that. Every path below therefore
	// answers with FollowUp*.
	if err := core.DeferResponse(s, i); err != nil {
		p.log.Error("roles: defer jail response failed", "guild", i.GuildID, "err", err)
		return
	}
	fail := func(title string, err error) {
		if ferr := core.FollowUpErr(s, i, title, err); ferr != nil {
			p.log.Error("roles: jail follow-up failed", "guild", i.GuildID, "err", ferr)
		}
	}

	// Checked before any tracking record is written, not left to
	// discordguard's per-call refusal: applyJail deliberately records the
	// jail before stripping roles, so a refusal at the strip would leave a
	// jail record for a member who was never actually jailed.
	if p.dryRun(i.GuildID) {
		if err := core.FollowUpOK(s, i, "Dry-run", fmt.Sprintf("Dry-run is enabled for this server: %s not jailed. Turn it off with `/config dryrun enabled:false`.",
			mentionList(userIDs))); err != nil {
			p.log.Error("roles: jail dry-run follow-up failed", "guild", i.GuildID, "err", err)
		}
		return
	}

	targets, fetchFailed := p.resolveTargets(i.GuildID, userIDs)

	// Rank check before anything is created or written: jail is TierMod but
	// strips roles, so without this a mod could use it to strip an admin's
	// Administrator bit and with it their TierAdmin access to this bot.
	allowed, res := p.partitionByRank(i.GuildID, i.Member, targets)
	res.failed = append(res.failed, fetchFailed...)

	if len(allowed) > 0 {
		jailRoleID, err := p.resolveJailRole(i.GuildID)
		if err != nil {
			fail("Failed to resolve jail role", err)
			return
		}
		// Ensure the bot can manage the configured marker role before attempting
		// to jail otherwise the GuildMemberEdit will fail and leave the
		// recorded snapshot inconsistent. Report as a failure to the actor.
		if err := p.perms.CanManageRole(i.GuildID, jailRoleID); err != nil {
			fail("Cannot use configured jail role", err)
			return
		}
		res = res.merge(p.jailMany(ctx, i.GuildID, jailRoleID, allowed, duration, actorID(i), reason))
	}

	p.reportSentence(ctx, s, i, jailSentence, userIDs, duration, reason, res)
}

// reportSentence is everything after the mutation, shared by /roles jail
// and /roles vacation so the two report identically: the public
// announcement, the audit entry, the member's DM, and the ephemeral
// follow-up to the actor. sn is which sentence was handed out.
func (p *Plugin) reportSentence(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate, sn sentence, userIDs []string, duration time.Duration, reason string, res bulkJailResult) {
	// Public spectacle, not the ephemeral command response: DeferResponse
	// sets the ephemeral flag on the placeholder it replaces, so
	// FollowUpOK/FollowUpErr below are only ever visible to the actor. This
	// is the one place a jail is actually announced to the channel it was
	// run in, covering both the single and bulk case the same way.
	p.announceJail(ctx, i.GuildID, i.ChannelID, res.jailed, duration, reason, sn)
	// A transfer is announced as one, whichever command did it: the people
	// in the channel were told where this member went last time.
	p.announceMoved(ctx, i.GuildID, i.ChannelID, res.transferred, ptrTime(p.now().Add(duration)), sn)

	// One member keeps the precise, actionable wording it always had, and its
	// original roles.jail audit action, so existing audit history stays one
	// queryable series rather than splitting the day this landed. Several get
	// the batch summary and a single roles.jail_bulk entry. Same execution
	// path either way; only the reporting differs, so the two can't drift in
	// behaviour.
	if len(userIDs) == 1 {
		if len(res.jailed) == 1 {
			if err := p.audit.Record(ctx, i.GuildID, actorID(i), sn.audit, "",
				fmt.Sprintf("user=%s duration=%s reason=%q", core.MentionUser(userIDs[0]), core.FormatDuration(duration), reason)); err != nil {
				p.log.Error("roles: audit jail failed", "guild", i.GuildID, "user", userIDs[0], "err", err)
			}
			// Only the individually-targeted path notifies. A jail is
			// otherwise experienced as access silently vanishing, which is
			// worse for the member and worse for the mods who then answer
			// the question in modmail.
			//
			// Deliberately not done for bulk jails or /roles jail-role.
			// Those exist to shut down a raid, and DMing fifty accounts
			// would spend the guild's hourly message budget on notifying
			// the raid, at the exact moment the releases that undo a
			// mistake need that budget more. Same reasoning as the cap on
			// batch size: reversibility beats completeness.
			p.notifyJailed(ctx, i.GuildID, userIDs[0], p.now().Add(duration), reason, sn)
		}
		// Sentenced in absentia: its own action, for the same reason a
		// re-sentence is. Nobody was stripped of anything, and a reader
		// counting jails in the log would otherwise count a sentence that
		// has not started and may never start. No DM and no channel
		// announcement either: there is nobody in this server to tell, and
		// announcing it would be telling the room about somebody who cannot
		// read it and has not arrived.
		if len(res.pending) == 1 {
			if err := p.audit.Record(ctx, i.GuildID, actorID(i), sn.auditPending, "",
				fmt.Sprintf("user=%s duration=%s reason=%q not in the server; applied on arrival", core.MentionUser(userIDs[0]), core.FormatDuration(duration), reason)); err != nil {
				p.log.Error("roles: audit pending jail failed", "guild", i.GuildID, "user", userIDs[0], "err", err)
			}
		}
		// A moved sentence is its own audit action rather than a second
		// roles.jail: nobody was jailed here, and a reader counting jails in
		// the log would otherwise count this member twice. The DM is the same
		// one, because the thing it tells them (when they get out) is exactly
		// what just changed.
		if len(res.redated) == 1 {
			if err := p.audit.Record(ctx, i.GuildID, actorID(i), sn.auditMoved, "",
				fmt.Sprintf("user=%s duration=%s reason=%q", core.MentionUser(userIDs[0]), core.FormatDuration(duration), reason)); err != nil {
				p.log.Error("roles: audit jail re-sentence failed", "guild", i.GuildID, "user", userIDs[0], "err", err)
			}
			p.notifyJailed(ctx, i.GuildID, userIDs[0], p.now().Add(duration), reason, sn)
		}
		// A transfer between the nest and the island is its own action too,
		// and its own DM: the member is told where they are now, not merely
		// when it ends.
		if len(res.transferred) == 1 {
			if err := p.audit.Record(ctx, i.GuildID, actorID(i), "roles.transferred", "",
				fmt.Sprintf("user=%s to=%s duration=%s reason=%q", core.MentionUser(userIDs[0]), sn.name, core.FormatDuration(duration), reason)); err != nil {
				p.log.Error("roles: audit transfer failed", "guild", i.GuildID, "user", userIDs[0], "err", err)
			}
			p.notifyMoved(ctx, i.GuildID, userIDs[0], ptrTime(p.now().Add(duration)), sn, reason)
		}
		p.respondSingleJail(s, i, userIDs[0], duration, res, sn)
		return
	}

	p.recordBulkAudit(ctx, i.GuildID, actorID(i), fmt.Sprintf("users=%d", len(userIDs)), duration, reason, res, sn)
	title := fmt.Sprintf("%s %d of %d member(s)", capitalize(sn.verb), len(res.jailed), len(userIDs))
	if err := core.FollowUpOK(s, i, title, summarizeBulkJail(res, duration, sn)); err != nil {
		p.log.Error("roles: jail follow-up failed", "guild", i.GuildID, "err", err)
	}
}

func ptrTime(t time.Time) *time.Time { return &t }

func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// respondSingleJail renders a one-member outcome, preserving the wording (and
// the audit action name) from before /roles jail could take several members.
func (p *Plugin) respondSingleJail(s *discordgo.Session, i *discordgo.InteractionCreate, userID string, duration time.Duration, res bulkJailResult, sn sentence) {
	fail := func(title string, err error) {
		if ferr := core.FollowUpErr(s, i, title, err); ferr != nil {
			p.log.Error("roles: jail follow-up failed", "guild", i.GuildID, "err", ferr)
		}
	}

	switch {
	case len(res.protected) > 0:
		fail("Cannot "+sn.name+" that member", fmt.Errorf("<@%s> outranks you", userID))
		return
	case len(res.failed) > 0:
		fail("Failed to "+sn.name+" member", errors.New(res.failed[0]))
		return
	}

	// Moved between the nest and the island. The snapshot restored at
	// release is still the original one; only the marker and the end moved.
	if len(res.transferred) > 0 {
		if err := core.FollowUpOK(s, i, "Member moved",
			fmt.Sprintf("<@%s> has been moved to %s. Their sentence now ends %s from now.", userID, sn.name, core.FormatDuration(duration))); err != nil {
			p.log.Error("roles: jail follow-up failed", "guild", i.GuildID, "err", err)
		}
		return
	}

	// Already serving a sentence, so this call moved the end of it rather
	// than jailing anyone. Reported as its own outcome, not folded into the
	// "Member jailed" wording below: the roles were stripped by the earlier
	// jail, and the snapshot restored at release is still that one's.
	if len(res.redated) > 0 {
		if err := core.FollowUpOK(s, i, "Sentence updated",
			fmt.Sprintf("<@%s> was already %s. Their sentence now ends %s from now.", userID, sn.verb, core.FormatDuration(duration))); err != nil {
			p.log.Error("roles: jail follow-up failed", "guild", i.GuildID, "err", err)
		}
		return
	}

	// Not in the server. Said plainly rather than as a success, because the
	// two look identical from the command's side and only one of them has
	// actually taken anybody's roles away.
	if len(res.pending) > 0 {
		if err := core.FollowUpOK(s, i, "Sentence recorded", fmt.Sprintf(
			"<@%s> isn't in this server. merlin has checked the account exists and recorded the sentence: they'll be %s for %s the moment they join, and nothing happens if they never do.\n"+
				"Cancel it with `/roles release`.", userID, sn.verb, core.FormatDuration(duration))); err != nil {
			p.log.Error("roles: jail follow-up failed", "guild", i.GuildID, "err", err)
		}
		return
	}

	msg := fmt.Sprintf("<@%s> %s for %s.", userID, sn.verb, core.FormatDuration(duration))
	if res.unmanageable > 0 {
		msg += " Some role(s) could not be stripped (positioned at/above merlin's own top role, or managed by an integration)."
	}
	if err := core.FollowUpOK(s, i, sn.title, msg); err != nil {
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
// simply never ends, the same "work silently abandoned, invisible from the
// outside" failure the untrack-on-gone rule exists to prevent.
//
// Recording first can only fail the other way: a record for a member whose
// roles were never touched. That self-heals when the jail comes due, through
// the confused-deputy check release already applies: the marker
// isn't on them, so it counts as already handled, gets untracked, and no
// roles are restored. A spurious row that cleans itself up beats a member
// jailed indefinitely. The rollback below just makes that immediate instead
// of eventual.
//
// A target who is not in the guild (t.absent) is recorded and nothing else.
// There is no member to strip, and the record on its own is already the
// whole mechanism: reapplyIfEvaded applies the marker the moment they
// arrive, by the same JoinedAt-after-JailedAt test that catches an evader
// coming back, so the sentence starts on arrival rather than being missed.
// The snapshot is empty because they held nothing here, which is exactly
// what release should hand back.
func (p *Plugin) applyJail(ctx context.Context, guildID, jailRoleID string, t jailTarget, duration time.Duration, actor, reason string) ([]string, error) {
	userID := t.userID
	newRoles, unmanageable := jailRoles(p.perms, guildID, jailRoleID, t.roles)

	now := p.now()
	releaseAt := now.Add(duration)
	if err := p.store.InsertJail(ctx, JailRecord{
		GuildID:         guildID,
		UserID:          userID,
		SnapshotRoleIDs: t.roles,
		JailRoleID:      jailRoleID,
		JailedAt:        now,
		ReleaseAt:       &releaseAt,
		JailedBy:        actor,
		Reason:          reason,
	}); err != nil {
		return nil, fmt.Errorf("roles: save jail record for %s: %w", userID, err)
	}

	if !t.absent {
		if _, err := p.stripToJailRoles(guildID, userID, newRoles); err != nil {
			if delErr := p.store.DeleteJail(ctx, guildID, userID); delErr != nil {
				p.log.Error("roles: roll back jail record after failed role update",
					"guild", guildID, "user", userID, "err", delErr)
			}
			return nil, fmt.Errorf("roles: strip roles for %s: %w", userID, err)
		}
	}
	p.armJailRelease(guildID, userID, releaseAt)
	if p.sentenceFor(guildID, jailRoleID) == vacationSentence {
		p.publishVacation(ctx, guildID, userID, actor, reason, duration)
	} else {
		p.publishJailed(ctx, guildID, userID, actor, reason, duration, releaseAt)
	}
	return unmanageable, nil
}

// stripToJailRoles replaces userID's roles with newRoles (as computed by
// jailRoles) and, on success, force-disconnects them from voice. The single
// chokepoint every jail-(re)application path funnels through: applyJail,
// reapplyIfEvaded, and HandleMemberUpdate's onboarding-regrant reassertion
// all call this rather than each hand-rolling the same GuildMemberEdit, so
// none of them can forget the voice-kick, including whatever future call
// site needs to reassert a jail next.
//
// Deliberately does not touch member-level channel overwrites itself
// (contrast syncMemberJailOverwrites): that hardening is O(at-risk
// channels) per call, and an ordinary jail that nobody ever tries to evade
// should stay the O(1) role edit it always was. Only reapplyIfEvaded and
// HandleMemberUpdate call syncMemberJailOverwrites themselves, after this
// returns, because those two are specifically what fire when live state has
// drifted from the jail: a rejoin, or a role a guild's Onboarding/Membership
// Screening flow regranted. A member-level deny is what actually stops that
// regranted role from beating the Jailed role's own channel-level deny (see
// its doc comment), and paying that cost is worth it exactly there, not on
// the common case of a jail nobody is fighting.
func (p *Plugin) stripToJailRoles(guildID, userID string, newRoles []string) (*discordgo.Member, error) {
	m, err := p.ops(guildID).GuildMemberEdit(guildID, userID, &discordgo.GuildMemberParams{Roles: &newRoles})
	if err != nil {
		if core.HasDiscordErrorCode(err, discordgo.ErrCodeUnknownRole) {
			// The cached Jailed role was deleted in Discord. Forget it so the
			// next attempt resolves or recreates one instead of retrying
			// against a dead ID until the process restarts. Deliberately not
			// any "unknown resource": this same call reports an unknown
			// *member* when the target left the guild, which says nothing
			// about the role and would throw away a good cache entry. The
			// vacation cache goes too: this same edit applies the island's
			// marker, and Discord does not say which role it did not know.
			p.forgetJailRole(guildID)
			p.forgetVacationRole(guildID)
		}
		return nil, err
	}
	p.disconnectFromVoice(guildID, userID)
	return m, nil
}

// disconnectFromVoice force-kicks userID from any voice channel they're
// currently connected to in guildID. Role and permission-overwrite changes
// don't propagate to an already-established voice session: Discord only
// evaluates Connect at connection time, so a jailed member who was mid-call
// stays connected, audible, and (if streaming) visible until they leave on
// their own unless explicitly disconnected here.
//
// A separate GuildMemberEdit call, deliberately not merged into the role
// edit above: Discord's Modify Guild Member endpoint permission-checks the
// whole request per field it touches, and channel_id needs Move Members
// where roles only needs Manage Roles. Combining them would mean a guild
// that has never re-authorized the bot with Move Members sees jail's core
// role-strip start failing outright too, over a permission the strip itself
// never needed. Best-effort and non-fatal for the same reason an
// audit-post failure never fails the operation that triggered it: a jailed
// member with intact voice access is a strictly better outcome than no jail
// at all.
func (p *Plugin) disconnectFromVoice(guildID, userID string) {
	if channelID, ok := p.voiceChannelOf(guildID, userID); ok {
		p.log.Info("roles: disconnecting jailed member from voice", "guild", guildID, "user", userID, "channel", channelID)
	}
	empty := ""
	if _, err := p.ops(guildID).GuildMemberEdit(guildID, userID, &discordgo.GuildMemberParams{ChannelID: &empty}); err != nil {
		p.log.Warn("roles: failed to disconnect jailed member from voice", "guild", guildID, "user", userID, "err", err)
	}
}

// sameRoleSet reports whether a and b hold the same roles, ignoring order.
// Used to make every jail-reassertion path idempotent: if live roles already
// equal what jailRoles would produce, there's nothing to do. This is what
// stops HandleMemberUpdate from looping on its own edit (that edit fires its
// own GUILD_MEMBER_UPDATE, re-entering the handler; the second pass sees
// roles already matching and no-ops).
func sameRoleSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	seen := make(map[string]bool, len(a))
	for _, r := range a {
		seen[r] = true
	}
	for _, r := range b {
		if !seen[r] {
			return false
		}
	}
	return true
}

// jailRoles decides what a jailed member ends up holding: the marker role,
// plus every role the bot structurally can't manage (positioned at or above
// its own top role, spec.MD §4 item 4), which stay put and are returned
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
// jailed member who left and returned comes back with nothing, including no
// marker role, and every channel the Jailed role was denying becomes visible
// again. The bot's own record still says "jailed", but nothing was checking.
//
// JoinedAt is the discriminator, and it comes free with the REST member fetch
// the sweep already makes. A member who never left has a JoinedAt from before
// their jail; one who left and returned has a JoinedAt after it. That
// distinction is exactly what separates an evasion from the case the
// confused-deputy rule protects (a mod deliberately stripping the marker to
// let someone out early), so honoring one doesn't cost the other.
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
// Runs on the one-minute sweep, so the window an evader gets is bounded by
// that tick rather than by how long nobody notices.
// The GUILD_MEMBERS intent, requested by default, closes that window to
// near-instant by also reacting to the rejoin event itself, see
// HandleMemberJoin. This remains the backstop, and the sole mechanism for a
// deployment that has turned the intent off.
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

// reapplyIfEvaded re-jails userID against rec if live Discord state has
// drifted from what the jail should look like: either they rejoined without
// the marker (evasion), or the marker is still on them but they now hold
// roles jailRoles wouldn't have left them with (most commonly a guild's
// Onboarding or Membership Screening flow regranting roles once they
// complete it, entirely outside this bot's control). This is the sweep's
// backstop for both cases: HandleMemberJoin calls it for the rejoin case
// near-instantly when the GUILD_MEMBERS intent allows it, and
// HandleMemberUpdate does the same for the regrant case; this function is
// what still catches both within one sweep tick when that intent is off and
// neither event ever arrives.
//
// It deliberately does not touch the stored record. The snapshot holds the
// member's real pre-jail roles and is the only copy of them. Overwriting it
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

	expected, unmanageable := jailRoles(p.perms, guildID, rec.JailRoleID, member.Roles)
	rejoined := false
	if slices.Contains(member.Roles, rec.JailRoleID) {
		if sameRoleSet(expected, member.Roles) {
			return nil // Still exactly as jailed as they should be.
		}
		// Marker present but roles drifted beyond it: something (typically
		// onboarding/screening) regranted roles after the strip. Falls
		// through to the same reassertion below as the rejoin case.
	} else if p.detectManualTransfer(ctx, guildID, rec, member.Roles) {
		// A mod moved them between the nest and the island by hand. The row
		// followed the marker (script_vacation.go); nothing to re-apply.
		return nil
	} else if rejoinedSinceJail(member, rec) {
		rejoined = true
	} else {
		// Marker gone without a rejoin: a mod released them by hand. That is
		// the confused-deputy rescue hatch, and it stays honored: the
		// existing sweep untracks them when the jail comes due.
		return nil
	}

	if _, err := p.stripToJailRoles(guildID, rec.UserID, expected); err != nil {
		return fmt.Errorf("roles: re-apply jail to %s: %w", rec.UserID, err)
	}
	// Only reached once live state has actually drifted from the jail (a
	// rejoin, or a regranted role): the member-level hardening isn't paid on
	// an ordinary jail nobody is fighting, only here, where it's what
	// actually stops a role a guild's Onboarding/Membership Screening flow
	// hands back from beating the Jailed role's own channel-level deny.
	// Never for a vacation: the island's permissions are the guild's own,
	// and a member-level deny would lock the beach as tight as the nest.
	if p.sentenceFor(guildID, rec.JailRoleID) == jailSentence {
		if err := p.syncMemberJailOverwrites(guildID, rec.UserID); err != nil {
			p.log.Warn("roles: failed to set member-level jail overwrites", "guild", guildID, "user", rec.UserID, "err", err)
		}
	}

	// "joined" rather than "rejoined": the same JoinedAt-after-JailedAt test
	// catches an evader coming back and an account sentenced before it ever
	// arrived (resolveTargets' absent target), and nothing in the row tells
	// the two apart. Wording that covered only the first would read as a
	// plain falsehood in the second, to a mod deciding whether merlin got
	// this right.
	action, reason := "roles.jail_reasserted", "roles were regranted while jailed (server onboarding/screening)"
	if rejoined {
		action, reason = "roles.jail_reapplied", "joined while a sentence was in force"
	}
	p.log.Warn("roles: re-applied jail", "guild", guildID, "user", rec.UserID, "reason", reason,
		"jailed_at", rec.JailedAt, "joined_at", member.JoinedAt)
	if err := p.audit.Record(ctx, guildID, core.ActorSystem, action, core.MentionUser(rec.UserID),
		fmt.Sprintf("%s; jail re-applied until %s unmanageable_roles=%v", reason, releaseAtText(rec), unmanageable)); err != nil {
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
// rather than waiting for the next sweep. Only ever called while the
// GUILD_MEMBERS intent is in effect (on by default,
// MERLIN_DISABLE_GUILD_MEMBERS_INTENT opts out). Without it Discord never
// sends the event, and the sweep above remains the sole mechanism.
func (p *Plugin) HandleMemberJoin(ctx context.Context, guildID, userID string) {
	defer p.memberChanged(ctx, guildID, userID)
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

// HandleMemberUpdate re-strips userID back to their jail role set if
// Discord's own GUILD_MEMBER_UPDATE shows roles were regranted while they
// were jailed, most commonly a guild's Onboarding or Membership Screening
// flow, which grants its configured roles the moment a member completes it,
// entirely independent of and unseen by anything this bot does. Only ever
// called while the GUILD_MEMBERS intent is in effect, same as
// HandleMemberJoin; reapplyIfEvaded's sweep is this fix's backstop when the
// intent is off, bounded to one minute instead of instant.
//
// roles is trusted directly from the event rather than re-fetched: unlike
// reapplyIfEvaded's deliberate live-REST-not-cache policy (reading a local
// cache that can go stale), a GUILD_MEMBER_UPDATE payload is Discord's own
// authoritative push of the member's current state, not something read back
// out of this bot's cache.
func (p *Plugin) HandleMemberUpdate(ctx context.Context, guildID, userID string, roles []string) {
	defer p.memberChanged(ctx, guildID, userID)
	rec, ok, err := p.store.GetJail(ctx, guildID, userID)
	if err != nil {
		p.log.Error("roles: look up jail on member update", "guild", guildID, "user", userID, "err", err)
		return
	}
	if !ok {
		return
	}
	if !slices.Contains(roles, rec.JailRoleID) {
		// The marker itself is gone. A mod swapping it for the other marker
		// by hand is a transfer and the row follows it (script_vacation.go);
		// anything else is a manual release or the rejoin path, both already
		// owned by reapplyIfEvaded's confused-deputy rule. Don't fight it.
		if rec.ReleaseAt == nil || rec.ReleaseAt.After(p.now()) {
			p.detectManualTransfer(ctx, guildID, rec, roles)
		}
		return
	}
	if rec.ReleaseAt != nil && !rec.ReleaseAt.After(p.now()) {
		// Sentence already served; the record just hasn't been swept out
		// yet. GetJail doesn't filter by expiry (unlike ActiveJails, which
		// is what reapplyEvadedJails' sweep uses), so without this check an
		// update landing in that window would re-strip someone whose jail
		// has already ended, mirroring the same guard HandleMemberJoin has.
		return
	}

	expected, unmanageable := jailRoles(p.perms, guildID, rec.JailRoleID, roles)
	if sameRoleSet(expected, roles) {
		return // Nothing drifted; also what stops this handler looping on its own edit below.
	}

	if _, err := p.stripToJailRoles(guildID, userID, expected); err != nil {
		p.log.Error("roles: re-strip roles regranted after jail", "guild", guildID, "user", userID, "err", err)
		return
	}
	// This is the evasion routine, not an ordinary jail: onboarding/screening
	// just proved it can hand a channel-unlocking role back, so the
	// member-level deny that actually stops that role from beating the
	// Jailed role's own channel-level deny is worth its per-channel cost
	// here specifically. Never for a vacation (see reapplyIfEvaded).
	if p.sentenceFor(guildID, rec.JailRoleID) == jailSentence {
		if err := p.syncMemberJailOverwrites(guildID, userID); err != nil {
			p.log.Warn("roles: failed to set member-level jail overwrites", "guild", guildID, "user", userID, "err", err)
		}
	}

	p.log.Warn("roles: roles regranted to a jailed member were stripped again", "guild", guildID, "user", userID,
		"unmanageable_roles", unmanageable)
	if err := p.audit.Record(ctx, guildID, core.ActorSystem, "roles.jail_reasserted", core.MentionUser(userID),
		fmt.Sprintf("roles were regranted while jailed (server onboarding/screening); stripped again until %s unmanageable_roles=%v",
			releaseAtText(rec), unmanageable)); err != nil {
		p.log.Error("roles: audit jail reassert failed", "guild", guildID, "user", userID, "err", err)
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
		core.RespondErr(s, i, "Not jailed", fmt.Errorf("<@%s> isn't currently jailed or on vacation", userID))
		return
	}
	sn := p.sentenceFor(i.GuildID, rec.JailRoleID)

	if err := p.releaseJail(ctx, i.GuildID, userID, rec, actorID(i)); err != nil {
		if errors.Is(err, errReleaseInProgress) {
			core.RespondErr(s, i, "Release in progress", fmt.Errorf("<@%s> is being released right now; check `/roles list` in a moment", userID))
			return
		}
		core.RespondErr(s, i, "Failed to release", err)
		return
	}
	// Only the manual command path announces publicly: it has an invoking
	// channel to post into. Automatic release from the sweep does not, and
	// stays DM-only (notifyReleased, inside releaseJail).
	p.announceRelease(ctx, i.GuildID, i.ChannelID, []string{userID}, sn)
	core.RespondOK(s, i, "Member released", fmt.Sprintf("<@%s> has been released from %s and their prior roles restored.", userID, sn.name))
}

// releaseJail restores rec's snapshotted roles to userID and stops tracking
// the jail. Shared by the release timer (release.go, automatic release at
// the due instant), the sweep job (its backstop) and handleRelease (a mod
// releasing early) so every path applies the exact same
// confused-deputy safeguard: re-fetch the member fresh and only restore if
// they still hold the jail marker role. If a mod already manually changed
// the member's roles (marker gone), that's treated as an implicit "already
// handled": stop tracking, don't fight the manual override, matching
// rotation.sweepOne's rescue-hatch precedent.
//
// actor is who is releasing: the invoking mod from handleRelease, or
// core.ActorSystem from the timer and the sweep. It used to be hardcoded to
// the system sentinel, so a mod releasing somebody early was audited as an
// automatic release and the decision had no owner.
func (p *Plugin) releaseJail(ctx context.Context, guildID, userID string, rec JailRecord, actor string) error {
	// The timer and the sweep can both arrive at a row that has just come
	// due; the second one finds it claimed and leaves it to the first.
	key := jailKey(guildID, userID)
	if !p.claim(key) {
		return errReleaseInProgress
	}
	defer p.unclaim(key)

	member, err := p.ops(guildID).GuildMember(guildID, userID)
	if err != nil {
		// Unknown Member specifically, not any unknown resource: the same
		// call answers Unknown Guild once the bot has been removed, and
		// reading that as "the member left" would drop the row ForgetGuild
		// keeps for a re-invite.
		if core.HasDiscordErrorCode(err, discordgo.ErrCodeUnknownMember) {
			// Member left the guild, nothing left to restore.
			return p.store.DeleteJail(ctx, guildID, userID)
		}
		// Any other failure is transient. Dropping the row here would strand
		// the member in jail permanently with nothing left tracking them,
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
	// Release removes the marker and puts the snapshot back; it never
	// removes anything else. GuildMemberEdit replaces the whole role set,
	// and Discord refuses the edit if any role in the diff is one the bot
	// can't manage. Writing the snapshot alone would strip whatever the
	// member gained while jailed, and a booster role or a role above the
	// bot's own made that a 403 on every retry, with the member stuck in
	// jail past their release. Same guard on the roles being put back:
	// one moved above the bot since jailing is skipped and logged rather
	// than failing the release forever.
	restore := make([]string, 0, len(member.Roles)+len(rec.SnapshotRoleIDs))
	for _, id := range member.Roles {
		if id != rec.JailRoleID {
			restore = append(restore, id)
		}
	}
	for _, id := range rec.SnapshotRoleIDs {
		if !valid[id] || slices.Contains(restore, id) {
			continue
		}
		if err := p.perms.CanManageRole(guildID, id); err != nil {
			p.log.Warn("roles: not restoring role the bot can no longer manage", "guild", guildID, "user", userID, "role", id, "err", err)
			continue
		}
		restore = append(restore, id)
	}

	if _, err := p.ops(guildID).GuildMemberEdit(guildID, userID, &discordgo.GuildMemberParams{Roles: &restore}); err != nil {
		return fmt.Errorf("roles: restore roles for %s: %w", userID, err)
	}
	if err := p.clearMemberJailOverwrites(guildID, userID); err != nil {
		p.log.Warn("roles: failed to clear member-level jail overwrites", "guild", guildID, "user", userID, "err", err)
	}

	sn := p.sentenceFor(guildID, rec.JailRoleID)
	if err := p.audit.Record(ctx, guildID, actor, "roles.release", "", fmt.Sprintf("user=%s from=%s restored=%v", core.MentionUser(userID), sn.name, restore)); err != nil {
		p.log.Error("roles: audit release failed", "guild", guildID, "user", userID, "err", err)
	}
	p.publishReleased(ctx, guildID, userID, actor)

	// Every release notifies, including the automatic ones, and unlike jail
	// this does not skip batches. Releases arrive spread over time as each
	// jail comes due rather than fifty at once, and the message is good
	// news: somebody who was told they were jailed should be told when that
	// has ended, or the only way to find out is to keep trying doors.
	p.notifyReleased(ctx, guildID, userID, sn)

	return p.store.DeleteJail(ctx, guildID, userID)
}
