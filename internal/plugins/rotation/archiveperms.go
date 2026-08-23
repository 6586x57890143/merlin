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

// archiveViewAllow is the single permission an archive grants. Deliberately
// read-only: an archive is a record, and core.DenyEveryoneExceptBot's doc
// comment warns that a grant is only meaningful for bits the bot's own guild
// role actually holds.
const archiveViewAllow = discordgo.PermissionViewChannel

// desiredArchiveOverwrites is what an archive category, and every channel
// synced to it, should carry.
//
// It starts from current rather than building a fresh list, because the two
// kinds of entry are not symmetric. An *allow* of View on an archive is the
// thing being governed: only the bot, the guild's mod roles, and the guild's
// configured archive viewer roles get one, and anything else allowing View
// has it taken away, whether an admin added it by hand or an older version of
// this bot wrote it. A *deny* is never touched: a jailed role's deny, or one
// member specifically shut out of the archive, is somebody's deliberate
// decision and none of this function's business. Anyone holding Discord's
// Administrator bit bypasses channel overwrites outright, so admins need no
// entry here and never appear in one.
//
// The result is sorted, and applying it to its own output is a no-op, which is
// what lets the periodic drift check write (and audit) only when something
// genuinely changed.
func desiredArchiveOverwrites(current []*discordgo.PermissionOverwrite, guildID, botUserID string, viewRoleIDs, viewUserIDs []string) []*discordgo.PermissionOverwrite {
	// Denies @everyone View and grants the bot itself View, merging into the
	// existing entries for both rather than duplicating them.
	out := core.DenyEveryoneExceptBot(current, guildID, botUserID, archiveViewAllow)

	grant := func(id string, kind discordgo.PermissionOverwriteType) {
		for _, ow := range out {
			if ow.ID == id && ow.Type == kind {
				ow.Allow |= archiveViewAllow
				ow.Deny &^= archiveViewAllow
				return
			}
		}
		out = append(out, &discordgo.PermissionOverwrite{ID: id, Type: kind, Allow: archiveViewAllow})
	}
	for _, roleID := range viewRoleIDs {
		if roleID == "" || roleID == guildID { // @everyone is never a viewer role
			continue
		}
		grant(roleID, discordgo.PermissionOverwriteTypeRole)
	}
	for _, userID := range viewUserIDs {
		if userID == "" {
			continue
		}
		grant(userID, discordgo.PermissionOverwriteTypeMember)
	}

	allowed := func(ow *discordgo.PermissionOverwrite) bool {
		if ow.Type == discordgo.PermissionOverwriteTypeMember {
			return ow.ID == botUserID || slices.Contains(viewUserIDs, ow.ID)
		}
		return slices.Contains(viewRoleIDs, ow.ID)
	}

	kept := out[:0]
	for _, ow := range out {
		if !allowed(ow) {
			ow.Allow &^= archiveViewAllow
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

// archiveViewers is who may see guildID's archives: the configured mod roles,
// the guild's archive viewer roles, and, for guilds carrying an imported
// archive_visibility of "whitelist", that slot's whitelist too.
//
// The legacy whitelist is folded in rather than dropped because nothing but
// /config import ever populated it, and a guild that has it is relying on it
// today. It is unioned across every slot pointing at categoryID, since the
// category is shared and there is no per-channel answer to give.
func (p *Plugin) archiveViewers(guildID, categoryID string) (roleIDs, userIDs []string) {
	roleIDs = append(roleIDs, p.settings.ModRoleIDs(guildID)...)
	roleIDs = append(roleIDs, p.settings.ArchiveViewerRoleIDs(guildID)...)
	for _, rc := range p.settings.RotationChannels(guildID) {
		if rc.ArchiveCategoryID != categoryID || rc.ArchiveVisibility != "whitelist" {
			continue
		}
		roleIDs = append(roleIDs, rc.ArchiveWhitelistRoleIDs...)
		userIDs = append(userIDs, rc.ArchiveWhitelistUserIDs...)
	}
	slices.Sort(roleIDs)
	slices.Sort(userIDs)
	return slices.Compact(roleIDs), slices.Compact(userIDs)
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
	roleIDs, userIDs := p.archiveViewers(guildID, categoryID)
	desired := desiredArchiveOverwrites(cat.PermissionOverwrites, guildID, botUserID, roleIDs, userIDs)
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
