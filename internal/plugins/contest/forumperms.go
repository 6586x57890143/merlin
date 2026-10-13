package contest

import (
	"fmt"
	"slices"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/snowflake/v2"

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
const forumViewAllow = discord.PermissionViewChannel | discord.PermissionReadMessageHistory

// forumMediaAllow is what a media role gets on top of view. A contest forum is
// where people post drawings, so unlike an archive there is deliberately no
// read-only clamp anywhere in this file: it grants, it never restricts.
const forumMediaAllow = discord.PermissionAttachFiles | discord.PermissionEmbedLinks

// forumBotAllow is what merlin needs in a forum she made: post the
// announcements, pin the winner, and read the entries back.
const forumBotAllow = discord.PermissionViewChannel |
	discord.PermissionSendMessages |
	discord.PermissionSendMessagesInThreads |
	discord.PermissionReadMessageHistory |
	discord.PermissionManageThreads |
	discord.PermissionManageMessages

// forumAccess is the resolved answer to "who is this contest for".
type forumAccess struct {
	// everyoneMayView mirrors the reference channel. A guild whose general
	// chat is open to @everyone gets a contest forum that is open too, and
	// that is correct: the job is to match the server, not to be strict for
	// its own sake.
	everyoneMayView bool
	viewRoleIDs     []snowflake.ID
	mediaRoleIDs    []snowflake.ID
}

// resolveAccess works out who the forum is for.
//
// Roles are resolved against the guild's live role list, which is what makes
// the "nothing stored, nothing to prune" property hold: a role deleted since
// it was named is dropped here rather than written into an overwrite Discord
// would reject.
func (p *Plugin) resolveAccess(cfg Config, guildID string) (forumAccess, error) {
	ops := p.opsFor(guildID)
	guild := core.ID(guildID)

	roles, err := ops.GetRoles(guild)
	if err != nil {
		return forumAccess{}, fmt.Errorf("contest: read guild roles: %w", err)
	}
	exists := make(map[snowflake.ID]bool, len(roles))
	for _, r := range roles {
		exists[r.ID] = true
	}
	keep := func(ids []snowflake.ID) []snowflake.ID {
		out := make([]snowflake.ID, 0, len(ids))
		for _, id := range ids {
			// @everyone is never granted access as a role: whether it sees
			// the forum is everyoneMayView's single job, and an entry in both
			// places is two answers to one question.
			if id == 0 || id == guild || !exists[id] {
				continue
			}
			out = append(out, id)
		}
		slices.Sort(out)
		return slices.Compact(out)
	}

	access := forumAccess{mediaRoleIDs: keep(ids(cfg.MediaRoleIDs))}

	// An explicit list wins over the mirror. Somebody who named roles has
	// already answered the question this function exists to ask.
	if len(cfg.AccessRoleIDs) > 0 {
		access.viewRoleIDs = keep(ids(cfg.AccessRoleIDs))
		return access, nil
	}

	ref, err := guildChannel(ops, core.ID(cfg.GateChannelID))
	if err != nil {
		// Fail rather than fall back to open. An unreadable reference channel
		// says nothing about who should be let in, and the wrong guess in
		// this direction publishes members' work to people the server kept
		// out. /contest new reports it and creates nothing.
		return forumAccess{}, fmt.Errorf("contest: read the channel to mirror: %w", err)
	}
	// Who can see the reference channel is not the same question as which
	// roles carry an allow on it, and reading the overwrite list as though it
	// were is what made this mirror copy nothing on an ordinary server.
	//
	// A channel overwrite is an *adjustment* to a role's guild-level
	// permissions, so the roles that can see a channel are usually not
	// written on it at all: the common gate is @everyone without View
	// Channels at the guild level and a @verified role that has it, on a
	// channel carrying no overwrites whatsoever. Scraping allows found
	// exactly nothing there, and since an absent @everyone entry was also
	// read as "everyone may not view", the forum came out denied to
	// @everyone and granted to nobody, which is a contest only merlin and
	// the guild's admins can see. The reverse missed too: a genuinely open
	// reference channel, whose @everyone View comes from the guild default
	// with nothing to override it, produced a locked forum.
	//
	// So ask Discord's own question instead, with Discord's own arithmetic
	// (developers/topics/permissions: base permissions, then the @everyone
	// overwrite, then the role's). Nothing is stored either way, which is the
	// property this whole file is built on.
	overwrites := ref.PermissionOverwrites()
	var everyoneBase discord.Permissions
	for _, r := range roles {
		if r.ID == guild {
			everyoneBase = r.Permissions
			break
		}
	}
	canView := func(r discord.Role) bool {
		perms := everyoneBase | r.Permissions
		// Administrator bypasses every overwrite, including a deny.
		if perms&discord.PermissionAdministrator != 0 {
			return true
		}
		if ow, ok := overwrites.Role(guild); ok {
			perms &^= ow.Deny
			perms |= ow.Allow
		}
		if r.ID != guild {
			if ow, ok := overwrites.Role(r.ID); ok {
				perms &^= ow.Deny
				perms |= ow.Allow
			}
		}
		return perms&discord.PermissionViewChannel != 0
	}

	for _, r := range roles {
		if r.ID != guild {
			continue
		}
		access.everyoneMayView = canView(r)
		break
	}
	// Only when the forum is being closed to @everyone is there anything to
	// grant: an open forum is open, and an entry per role on top of that
	// would say nothing while spending one of the channel's overwrite slots.
	//
	// ponytail: no cap on how many roles this writes. Discord takes 100
	// overwrites per channel and a gated server has a handful of roles that
	// pass the gate; if one ever overruns it, the answer is to name the roles
	// with /contest configure access-role, which already wins over the mirror.
	if !access.everyoneMayView {
		for _, r := range roles {
			if r.ID == guild {
				continue
			}
			// An Administrator role needs no entry for the same reason it is
			// allowed above: overwrites do not apply to it.
			if (everyoneBase|r.Permissions)&discord.PermissionAdministrator != 0 {
				continue
			}
			if canView(r) {
				access.viewRoleIDs = append(access.viewRoleIDs, r.ID)
			}
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
func desiredForumOverwrites(current []discord.PermissionOverwrite, guildID, botUserID snowflake.ID, access forumAccess, open bool) []discord.PermissionOverwrite {
	start := current
	if !access.everyoneMayView {
		// Denies @everyone View and grants merlin herself access, merging
		// into the existing entries for both rather than duplicating them.
		start = core.DenyEveryone(current, guildID, botUserID, forumBotAllow)
	}
	var out []*core.Overwrite
	for _, ow := range core.Overwrites(start) {
		out = append(out, &ow)
	}
	find := func(id snowflake.ID, kind discord.PermissionOverwriteType) *core.Overwrite {
		for _, ow := range out {
			if ow.ID == id && ow.Type == kind {
				return ow
			}
		}
		return nil
	}

	grant := func(id snowflake.ID, kind discord.PermissionOverwriteType, allow discord.Permissions) {
		if ow := find(id, kind); ow != nil {
			ow.Allow |= allow
			ow.Deny &^= allow
			return
		}
		out = append(out, &core.Overwrite{ID: id, Type: kind, Allow: allow})
	}

	grant(botUserID, discord.PermissionOverwriteTypeMember, forumBotAllow)
	for _, id := range access.viewRoleIDs {
		grant(id, discord.PermissionOverwriteTypeRole, forumViewAllow)
	}
	for _, id := range access.mediaRoleIDs {
		grant(id, discord.PermissionOverwriteTypeRole, forumMediaAllow)
	}

	// The posting bit rides on @everyone whether or not @everyone can see the
	// channel, because that is the entry every member inherits from and the
	// one setForumOpen flips. Keeping it here, and view on the role entries,
	// is what lets "who is this for" and "is it taking entries" stay
	// independent switches.
	everyone := find(guildID, discord.PermissionOverwriteTypeRole)
	if everyone == nil {
		everyone = &core.Overwrite{ID: guildID, Type: discord.PermissionOverwriteTypeRole}
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
	var kept []core.Overwrite
	for _, ow := range out {
		if ow.Allow != 0 || ow.Deny != 0 {
			kept = append(kept, *ow)
		}
	}
	return core.Disgo(core.SortOverwrites(kept))
}

// ids is ss as snowflakes.
func ids(ss []string) []snowflake.ID {
	out := make([]snowflake.ID, 0, len(ss))
	for _, s := range ss {
		out = append(out, core.ID(s))
	}
	return out
}

// forumOverwritesFor is the whole resolution in one call: who the forum is
// for, and what that means as a list Discord will take.
func (p *Plugin) forumOverwritesFor(cfg Config, guildID string, current []discord.PermissionOverwrite, open bool) ([]discord.PermissionOverwrite, error) {
	access, err := p.resolveAccess(cfg, guildID)
	if err != nil {
		return nil, err
	}
	return desiredForumOverwrites(current, core.ID(guildID), p.botID, access, open), nil
}
