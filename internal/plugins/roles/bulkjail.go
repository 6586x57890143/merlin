package roles

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/6586x57890143/merlin/internal/core"
)

// maxBulkJailTargets caps how many members one command may jail.
//
// The number is not arbitrary and it is not about Discord's rate limits.
// Jail and release both go through GuildMemberEdit, so they draw on the same
// per-guild member.edit budget in discordguard (opCaps). A batch large enough
// to drain that budget would leave the guild unable to run the releases that
// undo it: the mistake and its remedy compete for the same tokens, and the
// mistake gets there first. Capping well below the hourly allowance keeps a
// mass jail reversible within the same hour, which matters more than being
// able to jail an arbitrarily large group in one go.
//
// Over the cap the command refuses outright rather than jailing an arbitrary
// subset: "50 of the 300 people you meant, chosen by pagination order" is not
// a moderation action anybody asked for, and working out which 50 to undo is
// worse than not having started.
const maxBulkJailTargets = 50

// maxMemberScanPages bounds the member enumeration behind /roles jail-role.
// Discord has no "list members with role X" endpoint, so finding them means
// paging the whole member list. At memberPageSize per page this covers a
// guild of 20,000; past that the command reports that it could not enumerate
// reliably rather than acting on a partial scan, since a partial scan silently
// under-reports how many people a role actually covers.
const (
	maxMemberScanPages = 20
	memberPageSize     = 1000
)

// jailTarget is a member about to be considered for jailing, carried with the
// roles they held at enumeration time so the bulk path doesn't re-fetch every
// member it already has in hand.
type jailTarget struct {
	userID string
	roles  []string
}

// bulkJailResult is what a batch actually did, per outcome. Everything is
// reported: a bulk moderation action that quietly dropped some of its targets
// would leave a mod believing people are jailed who are not.
type bulkJailResult struct {
	jailed       []string
	redated      []string // already jailed; sentence moved to the one just given
	protected    []string // CanModerate refused, target outranks the actor
	failed       []string // "userID: reason"
	unmanageable int      // members who kept at least one role the bot can't touch
}

func (r bulkJailResult) attempted() int {
	return len(r.jailed) + len(r.redated) + len(r.protected) + len(r.failed)
}

// merge folds another result into this one, so the outcomes collected before
// the mutation (rank refusals, unresolvable members) and the ones from the
// mutation itself end up in a single report.
func (r bulkJailResult) merge(other bulkJailResult) bulkJailResult {
	r.jailed = append(r.jailed, other.jailed...)
	r.redated = append(r.redated, other.redated...)
	r.protected = append(r.protected, other.protected...)
	r.failed = append(r.failed, other.failed...)
	r.unmanageable += other.unmanageable
	return r
}

// jailMany applies the same jail to every target, independently.
//
// Deliberately not transactional. Each member's jail is its own record plus
// its own Discord mutation, and there is no way to atomically un-jail the
// first thirty if the thirty-first fails, since attempting a rollback would mean
// doing the exact thing that failed, thirty more times, on a guild that is
// evidently already unhappy. Instead every target is attempted and every
// outcome is reported, so a mod can see precisely who ended up jailed and
// retry the rest by hand.
//
// Ordering within a target is unchanged from the single-member path: applyJail
// still records before it strips, so a crash mid-batch leaves tracked jails
// rather than untracked stripped members.
func (p *Plugin) jailMany(ctx context.Context, guildID, jailRoleID string,
	targets []jailTarget, duration time.Duration, actorID, reason string,
) bulkJailResult {
	var res bulkJailResult
	for _, t := range targets {
		unmanageable, err := p.applyJail(ctx, guildID, t.userID, jailRoleID, t.roles, duration, actorID, reason)
		switch {
		case errors.Is(err, ErrAlreadyJailed):
			// Already serving a sentence, so re-jailing moves the end of it
			// to the one just given (shortening it too, if that is what the
			// mod asked for: how long someone is jailed is their call). The
			// alternative a refusal left was releasing first and jailing
			// again, which hands every stripped role back in between.
			//
			// Only release_at moves. The member is already stripped, and the
			// snapshot on record is the only copy of what they held before
			// that: re-recording it now would capture the marker role alone.
			releaseAt := p.now().Add(duration)
			if serr := p.store.SetJailRelease(ctx, guildID, t.userID, &releaseAt); serr != nil {
				res.failed = append(res.failed, fmt.Sprintf("%s: %v", t.userID, serr))
				continue
			}
			res.redated = append(res.redated, t.userID)
		case err != nil:
			res.failed = append(res.failed, fmt.Sprintf("%s: %v", t.userID, err))
		default:
			res.jailed = append(res.jailed, t.userID)
			if len(unmanageable) > 0 {
				res.unmanageable++
			}
		}
	}
	return res
}

// partitionByRank splits targets into those the actor may moderate and those
// they may not, without mutating anything.
//
// Separate from jailMany, and run before it, because resolving the jail role
// can *create* it (and write an overwrite to every channel). A batch where the
// actor outranks nobody must not leave that behind as a side effect, the same
// "a refused jail creates no guild roles" property the single-member path has
// always had.
//
// Per target rather than once for the batch: the tier check answered "may you
// run this command", never "against this person". Skipping the ones the actor
// may not touch, instead of failing the whole batch, stops a role that happens
// to contain one admin from being a way to find that out, and keeps the
// legitimate part of the action working.
func (p *Plugin) partitionByRank(guildID string, actor *discordgo.Member, targets []jailTarget) (allowed []jailTarget, res bulkJailResult) {
	for _, t := range targets {
		err := p.perms.CanModerate(guildID, actor, t.userID, t.roles)
		var forbidden core.ErrForbidden
		switch {
		case err == nil:
			allowed = append(allowed, t)
		case errors.As(err, &forbidden):
			res.protected = append(res.protected, t.userID)
		default:
			// Couldn't resolve the guild's state at all. Fail closed for this
			// member rather than assuming they're ordinary.
			res.failed = append(res.failed, fmt.Sprintf("%s: %v", t.userID, err))
		}
	}
	return allowed, res
}

// resolveTargets turns user IDs into jailTargets, fetching each member's
// current roles. Used by the multi-user path, where the command gives us IDs
// and nothing else; the role path already holds full members and skips this.
func (p *Plugin) resolveTargets(guildID string, userIDs []string) (targets []jailTarget, failed []string) {
	for _, id := range userIDs {
		member, err := p.ops(guildID).GuildMember(guildID, id)
		if err != nil {
			failed = append(failed, fmt.Sprintf("%s: %v", id, err))
			continue
		}
		targets = append(targets, jailTarget{userID: id, roles: member.Roles})
	}
	return targets, failed
}

// membersWithRole finds everyone currently holding roleID.
//
// Discord offers no endpoint for this, so it pages the full member list and
// filters. Enumeration stops early once the match count passes maxBulkJailTargets
// (the caller only needs to know the batch is too large, not exactly how
// large) and gives up after maxMemberScanPages rather than paging a very
// large guild indefinitely.
//
// complete reports whether the scan actually finished. A caller must not treat
// an incomplete scan as "this is everyone", because acting on a partial answer
// jails some of a role's members and silently leaves the rest.
func (p *Plugin) membersWithRole(guildID, roleID string) (matches []jailTarget, complete bool, err error) {
	after := ""
	for range maxMemberScanPages {
		page, err := p.ops(guildID).GuildMembers(guildID, after, memberPageSize)
		if err != nil {
			return nil, false, fmt.Errorf("roles: list guild members: %w", err)
		}
		for _, m := range page {
			if m.User == nil || !slices.Contains(m.Roles, roleID) {
				continue
			}
			matches = append(matches, jailTarget{userID: m.User.ID, roles: m.Roles})
			if len(matches) > maxBulkJailTargets {
				// Already more than we would act on. The exact total doesn't
				// change the answer, and scanning on would only cost calls.
				return matches, true, nil
			}
		}
		if len(page) < memberPageSize {
			return matches, true, nil // Last page.
		}
		last := page[len(page)-1].User
		if last == nil {
			// No cursor to page from. Report incomplete rather than
			// re-requesting the same page or claiming this is everyone.
			return matches, false, nil
		}
		after = last.ID
	}
	return matches, false, nil
}

// validateJailRoleTarget rejects roles that must never be mass-jailed, before
// anything is enumerated or written. Split out so both refusals are testable
// without a live interaction, since they are the two that stop a single
// mistyped command from taking the server down.
func (p *Plugin) validateJailRoleTarget(guildID, roleID string) error {
	// @everyone's role ID is the guild's own ID. Jailing it means jailing the
	// entire server, including whoever ran the command and every admin who
	// could undo it. No moderation need is served by letting that through the
	// same path as an ordinary role.
	if roleID == guildID {
		return errors.New("that's @everyone, and jailing it would jail the entire server, including you and everyone who could undo it")
	}

	// A role positioned at or above Merlin's own survives jailRoles by design
	// (spec.MD §4 item 4), so its members would keep it and the jail would not
	// accomplish the one thing it was asked to do. Refusing beats silently
	// half-working: the mod would see "jailed 40 members" and still have 40
	// members holding the role they were trying to neutralise.
	if err := p.perms.CanManageRole(guildID, roleID); err != nil {
		return fmt.Errorf("<@&%s> sits at or above Merlin's own top role, so she can't strip it. "+
			"Its members would keep it even while jailed: %w", roleID, err)
	}
	return nil
}

// handleJailRole jails every member holding one role.
//
// Its own subcommand rather than an option on /roles jail, for two reasons.
// Discord requires required options ahead of optional ones, so folding a role
// in would have forced "duration" to the front of the far more common
// single-member path. And a separate leaf gets its own PermSpec: mass jail has
// a materially bigger blast radius than jailing one person, which is the same
// reasoning that already puts configure_jail_channels on its own Admin-only
// action. A guild that wants its mods to have this can lower it deliberately
// with /config permissions set-tier roles.jail_role.
func (p *Plugin) handleJailRole(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate) {
	opts := core.LeafArgs(i)
	roleID := opts["role"].Value.(string)
	duration, err := core.ParseFlexibleDuration(opts["duration"].StringValue())
	if err != nil {
		core.RespondErr(s, i, "Invalid duration", err)
		return
	}
	reason := ""
	if v, ok := opts["reason"]; ok {
		reason = v.StringValue()
	}

	// Enumerating a large guild plus up to maxBulkJailTargets member edits is
	// far past Discord's 3-second deadline. Every path below answers with a
	// follow-up.
	if err := core.DeferResponse(s, i); err != nil {
		p.log.Error("roles: defer jail-role response failed", "guild", i.GuildID, "err", err)
		return
	}
	fail := func(title string, err error) {
		if ferr := core.FollowUpErr(s, i, title, err); ferr != nil {
			p.log.Error("roles: jail-role follow-up failed", "guild", i.GuildID, "err", ferr)
		}
	}

	if err := p.validateJailRoleTarget(i.GuildID, roleID); err != nil {
		fail("Cannot jail that role", err)
		return
	}

	if p.dryRun(i.GuildID) {
		targets, complete, err := p.membersWithRole(i.GuildID, roleID)
		if err != nil {
			fail("Dry-run: failed to enumerate members", err)
			return
		}
		msg := fmt.Sprintf("Dry-run is enabled, so nobody was jailed. <@&%s> currently has %s member(s) who would be considered.",
			roleID, describeCount(len(targets), complete))
		if ferr := core.FollowUpOK(s, i, "Dry-run", msg); ferr != nil {
			p.log.Error("roles: jail-role dry-run follow-up failed", "guild", i.GuildID, "err", ferr)
		}
		return
	}

	targets, complete, err := p.membersWithRole(i.GuildID, roleID)
	if err != nil {
		fail("Failed to list members", fmt.Errorf("%w (note this needs the privileged GUILD_MEMBERS intent)", err))
		return
	}
	if !complete {
		fail("Refused", fmt.Errorf("couldn't enumerate this guild's members within %d pages, "+
			"so there is no way to be sure who holds <@&%s>; jail them individually instead", maxMemberScanPages, roleID))
		return
	}

	// Cap-check on the raw match count, *before* dropping the actor and the
	// bot. membersWithRole stops scanning once it is past the cap, so the
	// number in hand is a floor, not a total: trimming one or two names off
	// it first could bring 200 real holders down to an apparent 50 and jail an
	// arbitrary slice of them while reporting complete success.
	if len(targets) > maxBulkJailTargets {
		fail("Too many members", fmt.Errorf("<@&%s> has %s members, over the %d-per-command limit. "+
			"That limit exists so a mass jail can't exhaust the same rate budget the releases undoing it would need",
			roleID, describeCount(len(targets), true), maxBulkJailTargets))
		return
	}

	targets = p.excludeSelfAndBot(targets, actorID(i), s)
	if len(targets) == 0 {
		if ferr := core.FollowUpOK(s, i, "Nobody to jail", fmt.Sprintf("No other members hold <@&%s>.", roleID)); ferr != nil {
			p.log.Error("roles: jail-role follow-up failed", "guild", i.GuildID, "err", ferr)
		}
		return
	}

	// Rank-check before resolving the jail role, so a batch the actor may not
	// touch at all doesn't create the role and rewrite every channel's
	// overwrites on its way to refusing.
	allowed, res := p.partitionByRank(i.GuildID, i.Member, targets)
	if len(allowed) == 0 {
		if ferr := core.FollowUpOK(s, i, "Nobody was jailed", summarizeBulkJail(res, duration)); ferr != nil {
			p.log.Error("roles: jail-role follow-up failed", "guild", i.GuildID, "err", ferr)
		}
		return
	}

	jailRoleID, err := p.resolveJailRole(i.GuildID)
	if err != nil {
		fail("Failed to resolve jail role", err)
		return
	}

	res = res.merge(p.jailMany(ctx, i.GuildID, jailRoleID, allowed, duration, actorID(i), reason))
	p.announceJail(ctx, i.GuildID, i.ChannelID, res.jailed, duration, reason)
	p.recordBulkAudit(ctx, i.GuildID, actorID(i), fmt.Sprintf("role=%s", roleID), duration, reason, res)

	title := fmt.Sprintf("Jailed %d member(s) from role", len(res.jailed))
	if ferr := core.FollowUpOK(s, i, title, summarizeBulkJail(res, duration)); ferr != nil {
		p.log.Error("roles: jail-role follow-up failed", "guild", i.GuildID, "err", ferr)
	}
}

// excludeSelfAndBot drops the actor and Merlin herself from a batch.
//
// Both are accidents waiting to happen rather than real intentions: a mod
// jailing a role they happen to hold would strip their own roles mid-command,
// and jailing the bot would strip the very permissions it needs to undo any of
// this. Jailing yourself deliberately is still possible one target at a time
// via /roles jail.
func (p *Plugin) excludeSelfAndBot(targets []jailTarget, actorID string, s *discordgo.Session) []jailTarget {
	botID := ""
	if s != nil && s.State != nil && s.State.User != nil {
		botID = s.State.User.ID
	}
	return slices.DeleteFunc(targets, func(t jailTarget) bool {
		return t.userID == actorID || (botID != "" && t.userID == botID)
	})
}

// recordBulkAudit writes one audit entry for the whole batch rather than one
// per member.
//
// Per-member entries would be more granular, but audit.Writer posts an embed
// to the audit channel for every record, so a 50-member jail would be 50
// embeds, both unreadable and enough message.send calls to eat a large share
// of the guild's hourly budget. One entry per command matches what actually
// happened: a mod ran one action. Per-member state stays queryable in
// role_jails via /roles list.
func (p *Plugin) recordBulkAudit(ctx context.Context, guildID, actor, scope string, duration time.Duration, reason string, res bulkJailResult) {
	// Mentions rather than bare snowflakes: this is the single record of who
	// a bulk jail hit, and it is read by somebody working out whether the
	// right people were caught. A list of raw IDs makes that a lookup
	// exercise per member. The count is capped by maxBulkJailTargets and the
	// field is truncated at 1024 bytes regardless, so this cannot run away.
	mentions := make([]string, 0, len(res.jailed))
	for _, id := range res.jailed {
		mentions = append(mentions, core.MentionUser(id))
	}
	detail := fmt.Sprintf("%s duration=%s reason=%q jailed=%d already_jailed=%d protected=%d failed=%d users=%s",
		scope, core.FormatDuration(duration), reason,
		len(res.jailed), len(res.redated), len(res.protected), len(res.failed),
		strings.Join(mentions, " "))
	if err := p.audit.Record(ctx, guildID, actor, "roles.jail_bulk", "", detail); err != nil {
		p.log.Error("roles: audit bulk jail failed", "guild", guildID, "err", err)
	}
}

// summarizeBulkJail renders the outcome. Every non-empty category is shown,
// including the ones a mod would rather not read: silently omitting the people
// who were not jailed is how somebody ends up believing a raid was contained.
func summarizeBulkJail(res bulkJailResult, duration time.Duration) string {
	var b strings.Builder
	fmt.Fprintf(&b, "**Jailed %d** of %d considered, for %s.\n", len(res.jailed), res.attempted(), core.FormatDuration(duration))
	if len(res.jailed) > 0 {
		fmt.Fprintf(&b, "\n%s\n", mentionList(res.jailed))
	}
	if res.unmanageable > 0 {
		fmt.Fprintf(&b, "\n⚠️ %d of them kept at least one role Merlin can't strip (positioned at/above her own top role).\n", res.unmanageable)
	}
	if len(res.redated) > 0 {
		fmt.Fprintf(&b, "\n**Already jailed, sentence moved to this one (%d):** %s\n", len(res.redated), mentionList(res.redated))
	}
	if len(res.protected) > 0 {
		fmt.Fprintf(&b, "\n**Skipped, outranks you (%d):** %s\n", len(res.protected), mentionList(res.protected))
	}
	if len(res.failed) > 0 {
		fmt.Fprintf(&b, "\n**Failed (%d):** %s\n", len(res.failed), strings.Join(res.failed, "; "))
	}
	// A field over 1024 bytes fails the whole message rather than being
	// trimmed, and a big batch reaches that easily.
	return core.TruncateEmbedField(b.String())
}

func mentionList(userIDs []string) string {
	sorted := append([]string(nil), userIDs...)
	sort.Strings(sorted)
	parts := make([]string, len(sorted))
	for i, id := range sorted {
		parts[i] = fmt.Sprintf("<@%s>", id)
	}
	return strings.Join(parts, " ")
}

// describeCount renders a count that may have stopped short of the true total,
// so "50" and "at least 50" never read the same.
func describeCount(n int, complete bool) string {
	if !complete || n > maxBulkJailTargets {
		return fmt.Sprintf("at least %d", n)
	}
	return fmt.Sprintf("%d", n)
}
