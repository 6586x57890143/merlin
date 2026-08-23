package rotation

import (
	"context"
	"fmt"
	"slices"
	"sort"

	"github.com/bwmarrin/discordgo"

	"github.com/6586x57890143/merlin/internal/core"
)

// Archive visibility is a property of the archive *category*, and every
// archived channel under it is kept synced to that category, which is exactly
// what Discord's own "sync permissions with category" does: it copies the
// category's overwrite list onto the channel. There is no inheritance to lean
// on instead. Discord's permission check for a channel reads only that
// channel's own permission_overwrites, never its parent's (see
// roles/jailchannels.go's own note on this); a category's overwrites are
// copied onto children only at creation time, and an archived channel is
// never created, it is moved. So the category is where the policy is
// *written*, and the sync is what makes it *true*.
//
// Doing it this way is what fixes the two things a per-channel overwrite list
// computed once at archive time could never fix: a mod role (or an archive
// viewer role) added later never reached channels already archived, and a
// stray allow added to the category by hand was invisible and permanent.
//
// Access comes in two shapes. Mods see an archive as part of moderating and
// keep whatever their own role carries inside it. Anybody else granted access
// is there to read it, so they get view and message history and are denied
// everything else outright (archiveViewerDeny), because revealing a channel to
// a role does not otherwise restrict that role: every permission it holds at
// guild level applies the moment it can see the channel.

// archiveModAllow is what a mod role gets on an archive: view, and whatever
// else their own role already carries. Mods are the people who go back through
// an archive to work out what happened, so nothing is taken off them here.
const archiveModAllow = discordgo.PermissionViewChannel

// archiveViewerAllow is what a non-mod archive viewer role gets: look at it and
// read the history back, nothing more. ReadMessageHistory is granted rather
// than assumed, since ViewChannel on its own shows an empty channel to anyone
// whose roles do not already carry it.
const archiveViewerAllow = discordgo.PermissionViewChannel | discordgo.PermissionReadMessageHistory

// archiveViewerDeny is everything else, taken away explicitly.
//
// Granting ViewChannel to a role does not restrict that role, it only reveals
// the channel; every permission the role holds at guild level then applies
// inside it. So a plain view grant hands a normal member role the ability to
// post in the archive, react to old messages, open threads on them and pull in
// webhooks. An archive is a record of what a channel used to be, and someone
// added to it to read it should not be able to write to it, which means the
// read-only part has to be spelled out rather than left to whatever the role
// happens to hold elsewhere.
//
// Voice bits are in here because a category's overwrites are copied onto
// anything created under it, and there is no reason for a viewer role to be
// able to sit in a voice channel somebody makes inside the archive category.
const archiveViewerDeny = discordgo.PermissionSendMessages |
	discordgo.PermissionSendMessagesInThreads |
	discordgo.PermissionSendTTSMessages |
	discordgo.PermissionSendVoiceMessages |
	discordgo.PermissionSendPolls |
	discordgo.PermissionAddReactions |
	discordgo.PermissionCreatePublicThreads |
	discordgo.PermissionCreatePrivateThreads |
	discordgo.PermissionManageThreads |
	discordgo.PermissionManageMessages |
	discordgo.PermissionEmbedLinks |
	discordgo.PermissionAttachFiles |
	discordgo.PermissionMentionEveryone |
	discordgo.PermissionUseExternalEmojis |
	discordgo.PermissionUseExternalStickers |
	discordgo.PermissionUseApplicationCommands |
	discordgo.PermissionUseExternalApps |
	discordgo.PermissionCreateInstantInvite |
	discordgo.PermissionManageWebhooks |
	discordgo.PermissionManageChannels |
	discordgo.PermissionManageRoles |
	discordgo.PermissionVoiceConnect |
	discordgo.PermissionVoiceSpeak |
	discordgo.PermissionVoiceStreamVideo

// archiveAccess is who may see one guild's archives, split by how much they
// get. Mods read an archive as part of moderating and keep their own
// permissions inside it; anyone else added to an archive is there to read it,
// and gets read-only.
type archiveAccess struct {
	modRoleIDs    []string
	viewerRoleIDs []string
	viewerUserIDs []string // legacy /config import whitelist, read-only like the roles
}

func (a archiveAccess) grantsTo(id string, kind discordgo.PermissionOverwriteType) (allow, deny int64, ok bool) {
	if kind == discordgo.PermissionOverwriteTypeMember {
		if slices.Contains(a.viewerUserIDs, id) {
			return archiveViewerAllow, archiveViewerDeny, true
		}
		return 0, 0, false
	}
	if slices.Contains(a.modRoleIDs, id) {
		return archiveModAllow, 0, true
	}
	if slices.Contains(a.viewerRoleIDs, id) {
		return archiveViewerAllow, archiveViewerDeny, true
	}
	return 0, 0, false
}

// desiredArchiveOverwrites is what an archive category, and every channel
// synced to it, should carry.
//
// It starts from current rather than building a fresh list, because the two
// kinds of entry are not symmetric. An *allow* of View on an archive is the
// thing being governed: only the bot, the guild's mod roles and the guild's
// configured archive viewer roles get one, and anything else allowing View has
// it taken away, whether an admin added it by hand or an older version of this
// bot wrote it. A *deny* belonging to somebody else is never touched: a jailed
// role's deny, or one member specifically shut out of the archive, is a
// decision somebody made and none of this function's business. The only denies
// written here are archiveViewerDeny, and only onto the viewer roles that are
// currently granted access. Anyone holding Discord's Administrator bit
// bypasses channel overwrites outright, so admins need no entry and never
// appear in one.
//
// The result is sorted, and applying it to its own output is a no-op, which is
// what lets the periodic drift check write (and audit) only when something
// genuinely changed.
func desiredArchiveOverwrites(current []*discordgo.PermissionOverwrite, guildID, botUserID string, access archiveAccess) []*discordgo.PermissionOverwrite {
	// Denies @everyone View and grants the bot itself View, merging into the
	// existing entries for both rather than duplicating them.
	out := core.DenyEveryoneExceptBot(current, guildID, botUserID, archiveModAllow)

	grant := func(id string, kind discordgo.PermissionOverwriteType, allow, deny int64) {
		for _, ow := range out {
			if ow.ID == id && ow.Type == kind {
				// A role promoted from read-only viewer to mod role still
				// carries the clamp this file wrote for it, and would stay
				// muted in the archive it can now moderate. Matching the whole
				// mask is what tells the two cases apart: a deny somebody set
				// by hand is never exactly archiveViewerDeny, so this lifts the
				// clamp without touching a deliberate restriction.
				if deny == 0 && ow.Deny&archiveViewerDeny == archiveViewerDeny {
					ow.Deny &^= archiveViewerDeny
				}
				ow.Allow = ow.Allow&^deny | allow
				ow.Deny = ow.Deny&^allow | deny
				return
			}
		}
		out = append(out, &discordgo.PermissionOverwrite{ID: id, Type: kind, Allow: allow, Deny: deny})
	}
	for _, roleID := range append(append([]string{}, access.modRoleIDs...), access.viewerRoleIDs...) {
		if roleID == "" || roleID == guildID { // @everyone is never granted archive access
			continue
		}
		allow, deny, _ := access.grantsTo(roleID, discordgo.PermissionOverwriteTypeRole)
		grant(roleID, discordgo.PermissionOverwriteTypeRole, allow, deny)
	}
	for _, userID := range access.viewerUserIDs {
		if userID == "" {
			continue
		}
		allow, deny, _ := access.grantsTo(userID, discordgo.PermissionOverwriteTypeMember)
		grant(userID, discordgo.PermissionOverwriteTypeMember, allow, deny)
	}

	kept := out[:0]
	for _, ow := range out {
		if _, _, ok := access.grantsTo(ow.ID, ow.Type); !ok && ow.ID != botUserID && ow.ID != guildID {
			// Not on the list: it may not reveal the archive. Its own denies,
			// including a read-only deny left behind by an earlier grant, are
			// left where they are.
			ow.Allow &^= discordgo.PermissionViewChannel
		}
		// An entry that now permits and forbids nothing says nothing. Dropping
		// it is what keeps a stripped stray from lingering as a confusing
		// empty row in Discord's permission list forever.
		if ow.Allow == 0 && ow.Deny == 0 {
			continue
		}
		kept = append(kept, ow)
	}
	sort.SliceStable(kept, func(i, j int) bool {
		if kept[i].Type != kept[j].Type {
			return kept[i].Type < kept[j].Type
		}
		return kept[i].ID < kept[j].ID
	})
	return kept
}

// overwritesEqual compares two overwrite lists ignoring order. Both sides come
// out of desiredArchiveOverwrites sorted, but Discord returns a channel's own
// list in whatever order it likes, so the live side has to be sorted here.
func overwritesEqual(a, b []*discordgo.PermissionOverwrite) bool {
	if len(a) != len(b) {
		return false
	}
	key := func(ow *discordgo.PermissionOverwrite) string {
		return fmt.Sprintf("%d:%s:%d:%d", ow.Type, ow.ID, ow.Allow, ow.Deny)
	}
	ka := make([]string, 0, len(a))
	kb := make([]string, 0, len(b))
	for i := range a {
		ka = append(ka, key(a[i]))
		kb = append(kb, key(b[i]))
	}
	slices.Sort(ka)
	slices.Sort(kb)
	return slices.Equal(ka, kb)
}

// archiveViewers is who may see guildID's archives, split into the mod roles
// (who keep their own permissions inside an archive) and the read-only viewer
// roles the guild has added with /rotation configure allow-archive-role.
//
// A guild carrying an imported archive_visibility of "whitelist" has its
// whitelist folded into the read-only side. Nothing but /config import ever
// populated it, and a guild that has one is relying on it today, so dropping
// it would quietly take access away. It is unioned across every slot pointing
// at categoryID, since the category is shared and there is no per-channel
// answer to give.
//
// A role that is both a mod role and an archive viewer role counts as a mod:
// the mod list is subtracted from the viewer list rather than the other way
// round, so adding a mod role here can never end up restricting it.
func (p *Plugin) archiveViewers(guildID, categoryID string) archiveAccess {
	// Copied, not aliased: settings.Store hands back the slice it caches, and
	// the sort below would otherwise reorder the store's own copy underneath
	// every other reader of it.
	access := archiveAccess{modRoleIDs: slices.Clone(p.settings.ModRoleIDs(guildID))}
	access.viewerRoleIDs = append(access.viewerRoleIDs, p.settings.ArchiveViewerRoleIDs(guildID)...)
	for _, rc := range p.settings.RotationChannels(guildID) {
		if rc.ArchiveCategoryID != categoryID || rc.ArchiveVisibility != "whitelist" {
			continue
		}
		access.viewerRoleIDs = append(access.viewerRoleIDs, rc.ArchiveWhitelistRoleIDs...)
		access.viewerUserIDs = append(access.viewerUserIDs, rc.ArchiveWhitelistUserIDs...)
	}
	access.viewerRoleIDs = slices.DeleteFunc(access.viewerRoleIDs, func(id string) bool {
		return slices.Contains(access.modRoleIDs, id)
	})
	for _, list := range []*[]string{&access.modRoleIDs, &access.viewerRoleIDs, &access.viewerUserIDs} {
		slices.Sort(*list)
		*list = slices.Compact(*list)
	}
	return access
}

// reconcileArchiveCategory computes what categoryID should carry, writes it
// back if it has drifted, and returns it either way so the caller can apply
// the same list to a channel it is about to archive. The bool reports whether
// anything actually had to change.
func (p *Plugin) reconcileArchiveCategory(guildID, categoryID string) ([]*discordgo.PermissionOverwrite, bool, error) {
	if categoryID == "" {
		return nil, false, nil
	}
	botUserID, err := p.getBotUserID(guildID)
	if err != nil {
		return nil, false, err
	}
	cat, err := p.ops(guildID).Channel(categoryID)
	if err != nil {
		return nil, false, fmt.Errorf("fetch archive category %s: %w", categoryID, err)
	}
	desired := desiredArchiveOverwrites(cat.PermissionOverwrites, guildID, botUserID, p.archiveViewers(guildID, categoryID))
	if overwritesEqual(cat.PermissionOverwrites, desired) {
		return desired, false, nil
	}
	// A whole-list PATCH, not per-target ChannelPermissionSet calls: removing
	// a stray entry outright is not expressible as a set.
	if _, err := p.ops(guildID).ChannelEditComplex(categoryID, &discordgo.ChannelEdit{PermissionOverwrites: desired}); err != nil {
		return desired, false, fmt.Errorf("set archive category %s permissions: %w", categoryID, err)
	}
	return desired, true, nil
}

// syncArchiveChannels brings every channel sitting under categoryID in line
// with desired, and reports how many actually needed it. A single channel's
// failure is logged and does not stop the rest, matching sweep's own policy.
//
// ponytail: serial, one PATCH at a time. An archive category holds a handful
// of channels and only drifted ones are written at all; borrow
// roles.forEachChannelConcurrent if one ever holds hundreds.
func (p *Plugin) syncArchiveChannels(guildID, categoryID string, desired []*discordgo.PermissionOverwrite) (int, error) {
	if categoryID == "" || len(desired) == 0 {
		return 0, nil
	}
	channels, err := p.ops(guildID).GuildChannels(guildID)
	if err != nil {
		return 0, fmt.Errorf("list channels for guild %s: %w", guildID, err)
	}
	fixed := 0
	var firstErr error
	for _, ch := range channels {
		if ch.ParentID != categoryID || ch.Type == discordgo.ChannelTypeGuildCategory {
			continue
		}
		if overwritesEqual(ch.PermissionOverwrites, desired) {
			continue
		}
		if _, err := p.ops(guildID).ChannelEditComplex(ch.ID, &discordgo.ChannelEdit{PermissionOverwrites: desired}); err != nil {
			p.log.Error("rotation: sync archive channel permissions failed", "guild", guildID, "channel", ch.ID, "err", err)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		fixed++
	}
	return fixed, firstErr
}

// syncArchivePermissions is the whole thing for one category: reconcile it,
// then bring its channels in line. It audits only when something actually
// changed, so a guild whose archives are already correct never touches the
// audit log, which is what makes it safe to run on every hourly sweep.
func (p *Plugin) syncArchivePermissions(ctx context.Context, guildID, categoryID string) error {
	desired, catFixed, err := p.reconcileArchiveCategory(guildID, categoryID)
	if err != nil {
		return err
	}
	fixed, err := p.syncArchiveChannels(guildID, categoryID, desired)
	if catFixed || fixed > 0 {
		detail := fmt.Sprintf("%d channel(s) resynced", fixed)
		if catFixed {
			detail = "category overwrites corrected, " + detail
		}
		if auditErr := p.audit.Record(ctx, guildID, core.ActorSystem, "rotation.archive_perms_fixed", core.MentionChannel(categoryID), detail); auditErr != nil {
			p.log.Error("rotation: audit archive permission fix", "guild", guildID, "err", auditErr)
		}
	}
	return err
}

// syncAllArchivePermissions runs syncArchivePermissions once per distinct
// archive category configured in guildID. Deliberately per *category* rather
// than per rotating channel: several slots can share one, and syncing it twice
// would double every write and every audit line.
func (p *Plugin) syncAllArchivePermissions(ctx context.Context, guildID string) error {
	seen := make(map[string]bool)
	var firstErr error
	for _, rc := range p.settings.RotationChannels(guildID) {
		if rc.ArchiveCategoryID == "" || seen[rc.ArchiveCategoryID] {
			continue
		}
		seen[rc.ArchiveCategoryID] = true
		if err := p.syncArchivePermissions(ctx, guildID, rc.ArchiveCategoryID); err != nil {
			p.log.Error("rotation: sync archive permissions failed", "guild", guildID, "category", rc.ArchiveCategoryID, "err", err)
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}
