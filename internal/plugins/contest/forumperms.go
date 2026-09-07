package contest

import (
	"cmp"
	"fmt"
	"slices"

	"github.com/bwmarrin/discordgo"

	"github.com/6586x57890143/merlin/internal/core"
)

// Who a contest forum is for.
//
// The problem this solves: createForum used to set one overwrite -- @everyone
// denied the posting bit -- and inherit everything else from whatever category
// it landed in. On a server where accounts that have not passed the gate still
// hold @everyone and only gated members hold a role like @melted, every
// ungated account could read the contest forum, react in it, and post to it
// the moment submissions opened. Members' work, published to exactly the
// people the server has deliberately not let in.
//
// The gate is derived from a channel the guild already gates the way it wants,
// rather than stored as a list of roles, because a stored list is a second
// copy of a fact the server already states, and a second copy goes stale: a
// deleted role rots in it, a renamed one is invisible to it, and writing an
// overwrite for a role that no longer exists does not fail small -- it fails
// the whole channel create and the contest with it. Mirroring means nothing to
// prune, nothing for settings.PruneDeletedRole to learn about, and no change
// on this side when a guild reorganises its roles.

// forumViewAllow is what a role that may see the contest gets. History is
// granted rather than assumed, since ViewChannel alone shows an empty channel
// to anyone whose roles do not already carry it -- the same reason rotation's
// archiveViewerAllow spells it out.
const forumViewAllow = discordgo.PermissionViewChannel | discordgo.PermissionReadMessageHistory

// forumMediaAllow is what a media role gets on top of view. A contest forum is
// where people post drawings, so unlike an archive there is deliberately no
// read-only clamp anywhere in this file: it grants, it never restricts.
const forumMediaAllow = discordgo.PermissionAttachFiles | discordgo.PermissionEmbedLinks

// forumBotAllow is what merlin needs in a forum she made: post the
// announcements, pin the winner, and read the entries back.
const forumBotAllow = discordgo.PermissionViewChannel |
	discordgo.PermissionSendMessages |
	discordgo.PermissionSendMessagesInThreads |
	discordgo.PermissionReadMessageHistory |
	discordgo.PermissionManageThreads |
	discordgo.PermissionManageMessages

// forumAccess is the resolved answer to "who is this contest for".
type forumAccess struct {
	// everyoneMayView mirrors the reference channel. A guild whose general
	// chat is open to @everyone gets a contest forum that is open too, and
	// that is correct: the job is to match the server, not to be strict for
	// its own sake.
	everyoneMayView bool
	viewRoleIDs     []string
	mediaRoleIDs    []string
}

// resolveAccess works out who the forum is for.
//
// Roles are resolved against the guild's live role list, which is what makes
// the "nothing stored, nothing to prune" property hold: a role deleted since
// it was named is dropped here rather than written into an overwrite Discord
// would reject.
func (p *Plugin) resolveAccess(cfg Config, guildID string) (forumAccess, error) {
	ops := p.opsFor(guildID)

	roles, err := ops.GuildRoles(guildID)
	if err != nil {
		return forumAccess{}, fmt.Errorf("contest: read guild roles: %w", err)
	}
	exists := make(map[string]bool, len(roles))
	for _, r := range roles {
		exists[r.ID] = true
	}
	keep := func(ids []string) []string {
		out := make([]string, 0, len(ids))
		for _, id := range ids {
			// @everyone is never granted access as a role: whether it sees
			// the forum is everyoneMayView's single job, and an entry in both
			// places is two answers to one question.
			if id == "" || id == guildID || !exists[id] {
				continue
			}
			out = append(out, id)
		}
		slices.Sort(out)
		return slices.Compact(out)
	}

	access := forumAccess{mediaRoleIDs: keep(cfg.MediaRoleIDs)}

	// An explicit list wins over the mirror. Somebody who named roles has
	// already answered the question this function exists to ask.
	if len(cfg.AccessRoleIDs) > 0 {
		access.viewRoleIDs = keep(cfg.AccessRoleIDs)
		return access, nil
	}

	ref, err := ops.Channel(cfg.GateChannelID)
	if err != nil {
		// Fail rather than fall back to open. An unreadable reference channel
		// says nothing about who should be let in, and the wrong guess in
		// this direction publishes members' work to people the server kept
		// out. /contest new reports it and creates nothing.
		return forumAccess{}, fmt.Errorf("contest: read the channel to mirror: %w", err)
	}
	for _, ow := range ref.PermissionOverwrites {
		if ow.Type != discordgo.PermissionOverwriteTypeRole {
			// Member-level entries are deliberately not mirrored: one
			// person's exception on one channel is not a statement about who
			// the server is for, and copying it onto every future contest
			// forum spreads a decision past where it was made.
			continue
		}
		if ow.ID == guildID {
			// Only an explicit allow counts as "everyone may see this".
			// Absent means inherited, and what it inherits from is a category
			// the contest forum is not going in.
			access.everyoneMayView = ow.Allow&discordgo.PermissionViewChannel != 0
			continue
		}
		if ow.Allow&discordgo.PermissionViewChannel != 0 {
			access.viewRoleIDs = append(access.viewRoleIDs, ow.ID)
		}
	}
	access.viewRoleIDs = keep(access.viewRoleIDs)
	return access, nil
}

// desiredForumOverwrites is what a contest forum should carry.
//
// It starts from the channel's current entries rather than building a fresh
// list, exactly as rotation's desiredArchiveOverwrites does and for the same
// reason: an allow of View is the thing being governed here, but a deny
// belonging to somebody else -- a jailed role's, or one member specifically
// shut out of this forum -- is a decision somebody made and none of this
// function's business.
//
// open flips the posting bit and nothing else. That is the whole safety
// property: a phase change can start or stop taking entries, and it can never
// change who is able to see the channel.
//
// The result is sorted, and applying it to its own output is a no-op, which is
// what lets sync-forum write only when something genuinely changed.
func desiredForumOverwrites(current []*discordgo.PermissionOverwrite, guildID, botUserID string, access forumAccess, open bool) []*discordgo.PermissionOverwrite {
	var out []*discordgo.PermissionOverwrite
	if access.everyoneMayView {
		out = make([]*discordgo.PermissionOverwrite, 0, len(current)+2)
		for _, ow := range current {
			clone := *ow
			out = append(out, &clone)
		}
	} else {
		// Denies @everyone View and grants merlin herself access, merging
		// into the existing entries for both rather than duplicating them.
		out = core.DenyEveryoneExceptBot(current, guildID, botUserID, forumBotAllow)
	}

	grant := func(id string, kind discordgo.PermissionOverwriteType, allow int64) {
		for _, ow := range out {
			if ow.ID == id && ow.Type == kind {
				ow.Allow |= allow
				ow.Deny &^= allow
				return
			}
		}
		out = append(out, &discordgo.PermissionOverwrite{ID: id, Type: kind, Allow: allow})
	}

	grant(botUserID, discordgo.PermissionOverwriteTypeMember, forumBotAllow)
	for _, id := range access.viewRoleIDs {
		grant(id, discordgo.PermissionOverwriteTypeRole, forumViewAllow)
	}
	for _, id := range access.mediaRoleIDs {
		grant(id, discordgo.PermissionOverwriteTypeRole, forumMediaAllow)
	}

	// The posting bit rides on @everyone whether or not @everyone can see the
	// channel, because that is the entry every member inherits from and the
	// one setForumOpen flips. Keeping it here, and view on the role entries,
	// is what lets "who is this for" and "is it taking entries" stay
	// independent switches.
	everyone := findIn(out, guildID, discordgo.PermissionOverwriteTypeRole)
	if everyone == nil {
		everyone = &discordgo.PermissionOverwrite{ID: guildID, Type: discordgo.PermissionOverwriteTypeRole}
		out = append(out, everyone)
	}
	if open {
		everyone.Deny &^= postingPerms
	} else {
		everyone.Deny |= postingPerms
		everyone.Allow &^= postingPerms
	}

	// Drop entries that ended up saying nothing, then sort, so this is
	// comparable with its own previous output.
	out = slices.DeleteFunc(out, func(ow *discordgo.PermissionOverwrite) bool {
		return ow.Allow == 0 && ow.Deny == 0
	})
	slices.SortFunc(out, func(a, b *discordgo.PermissionOverwrite) int {
		if c := cmp.Compare(a.Type, b.Type); c != 0 {
			return c
		}
		return cmp.Compare(a.ID, b.ID)
	})
	return out
}

func findIn(list []*discordgo.PermissionOverwrite, id string, kind discordgo.PermissionOverwriteType) *discordgo.PermissionOverwrite {
	for _, ow := range list {
		if ow.ID == id && ow.Type == kind {
			return ow
		}
	}
	return nil
}

// overwritesEqual compares two lists regardless of order, so a resync that
// would change nothing writes nothing. Every permission write lands in the
// guild's own Discord audit log, and a no-op write buries the entries a
// moderator is looking for.
func overwritesEqual(a, b []*discordgo.PermissionOverwrite) bool {
	if len(a) != len(b) {
		return false
	}
	key := func(ow *discordgo.PermissionOverwrite) string {
		return fmt.Sprintf("%d:%s:%d:%d", ow.Type, ow.ID, ow.Allow, ow.Deny)
	}
	seen := make(map[string]int, len(a))
	for _, ow := range a {
		seen[key(ow)]++
	}
	for _, ow := range b {
		k := key(ow)
		if seen[k] == 0 {
			return false
		}
		seen[k]--
	}
	return true
}

// botUserID caches merlin's own ID for the life of the process, the way
// rotation does. Every overwrite list needs it and it never changes.
func (p *Plugin) botUserID(guildID string) (string, error) {
	p.mu.Lock()
	id := p.botID
	p.mu.Unlock()
	if id != "" {
		return id, nil
	}
	u, err := p.opsFor(guildID).User("@me")
	if err != nil {
		return "", fmt.Errorf("contest: resolve bot user: %w", err)
	}
	p.mu.Lock()
	p.botID = u.ID
	p.mu.Unlock()
	return u.ID, nil
}

// forumOverwritesFor is the whole resolution in one call: who the forum is
// for, and what that means as a list Discord will take.
func (p *Plugin) forumOverwritesFor(cfg Config, guildID string, current []*discordgo.PermissionOverwrite, open bool) ([]*discordgo.PermissionOverwrite, error) {
	access, err := p.resolveAccess(cfg, guildID)
	if err != nil {
		return nil, err
	}
	botID, err := p.botUserID(guildID)
	if err != nil {
		return nil, err
	}
	return desiredForumOverwrites(current, guildID, botID, access, open), nil
}
