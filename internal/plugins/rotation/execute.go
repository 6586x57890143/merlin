package rotation

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/6586x57890143/merlin/internal/core"
	"github.com/6586x57890143/merlin/internal/settings"
)

const (
	// maxChannelsPerGuild is Discord's hard cap; channelCapHeadroom keeps a
	// safety margin so rotation fails loud with a clean error well before
	// ever hitting Discord's raw 30013-style error.
	maxChannelsPerGuild = 500
	channelCapHeadroom  = 10
)

// makeRotationJob returns the Scheduler job function for one guild's one
// configured rotation slot, identified by rotationID (settings.RotationChannel.ID
// — stable across retargets, unlike ChannelID; see rotation.go's reconcile).
// It re-fetches the slot's current settings at execution time (not at
// registration time) so edits made via /rotation configure take effect on
// the very next run without needing a job re-register — only IntervalHours
// needs that (see reconcile in rotation.go), since it drives the Scheduler's
// own due-check.
func (p *Plugin) makeRotationJob(guildID string, rotationID int64) func(ctx context.Context) error {
	return func(ctx context.Context) error {
		rc, ok := p.settings.RotationChannelByID(guildID, rotationID)
		if !ok {
			p.log.Info("rotation: job fired for a rotation slot no longer configured, skipping", "guild", guildID, "rotation_id", rotationID)
			return nil
		}
		return p.rotate(ctx, guildID, rc)
	}
}

// rotate runs one rotation cycle. It is safe to re-enter from scratch on
// retry: every step re-derives its "have I already done this" answer from
// live Discord/Postgres state rather than trusting in-memory progress, so
// the Scheduler's normal retry/backoff (internal/scheduler, unchanged) is
// sufficient to resume a partially-completed rotation.
//
// Step order deliberately deviates from spec.MD §6's literal listing: the
// new channel is created (hidden) and fully populated BEFORE the old
// channel is touched at all, then both are flipped new-first. This
// eliminates any window where the guild has zero live channel matching the
// configured name — see the Milestone 3 plan for the full rationale.
func (p *Plugin) rotate(ctx context.Context, guildID string, rc settings.RotationChannel) error {
	// 1&2. Preflight fetch + capacity-check listing, run concurrently: these
	// are two independent reads (one channel by ID, one full guild channel
	// list) with no data dependency on each other — only the processing
	// below needs both results. Fetch-channel's error takes priority if
	// both fail, matching the original sequential short-circuit order.
	var oldChannel *discordgo.Channel
	var channels []*discordgo.Channel
	var channelErr, listErr error
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		oldChannel, channelErr = p.ops.Channel(rc.ChannelID)
	}()
	go func() {
		defer wg.Done()
		channels, listErr = p.ops.GuildChannels(guildID)
	}()
	wg.Wait()

	// Confused-deputy check (spec.MD §4): confirm guild ownership rather
	// than trusting rc.ChannelID blindly.
	if channelErr != nil {
		return fmt.Errorf("rotation: fetch channel %s: %w", rc.ChannelID, channelErr)
	}
	if oldChannel.GuildID != guildID {
		return fmt.Errorf("rotation: channel %s does not belong to guild %s", rc.ChannelID, guildID)
	}
	// Capacity check — fail loud and clean, not with a raw Discord error.
	if listErr != nil {
		return fmt.Errorf("rotation: list channels for guild %s: %w", guildID, listErr)
	}
	if len(channels) >= maxChannelsPerGuild-channelCapHeadroom {
		return fmt.Errorf("rotation: guild %s at %d/%d channels, skipping rotation for %s",
			guildID, len(channels), maxChannelsPerGuild, rc.ChannelID)
	}

	tempName := stagingChannelName(rc.ChannelID)

	// 4. Idempotency check / create new (hidden). Three retry cases, each
	// recognized from live state rather than in-memory progress:
	//   - a prior run completed the whole flip and failed afterwards (while
	//     recording the archive, or retargeting the config) — the "old"
	//     channel is by now sitting in the archive category under an archive
	//     name, so its original name has to be recovered from that name
	//     before anything else can be matched against it. Without this the
	//     retry recognizes nothing, rotates a second time, and leaves the
	//     guild with a duplicate replacement channel;
	//   - a prior run already revealed the replacement but failed before
	//     archiving the old channel — found by the original name, excluding
	//     the old channel itself, since both legitimately share a name at
	//     that point;
	//   - a prior run created the hidden staging channel but failed before
	//     reveal — found by its deterministic temp name.
	// Otherwise, this is a fresh run: create it.
	originalName, alreadyArchived := archivedChannelOrigin(oldChannel, rc.ArchiveCategoryID)

	// Discord allows duplicate channel names, so matching on name alone can
	// find an unrelated channel and make this rotation archive a live
	// channel and retarget itself onto a stranger's. A genuine replacement
	// also sits outside the archive category and — until the old channel is
	// archived and moved — in the same category as the channel it replaces.
	newChannel := findChannel(channels, func(c *discordgo.Channel) bool {
		return c.Name == originalName && c.ID != oldChannel.ID &&
			c.ParentID != rc.ArchiveCategoryID &&
			(alreadyArchived || c.ParentID == oldChannel.ParentID)
	})
	alreadyRevealed := newChannel != nil
	if newChannel == nil {
		newChannel = findChannel(channels, func(c *discordgo.Channel) bool { return c.Name == tempName })
	}
	if newChannel == nil {
		// Normally the replacement belongs wherever the channel it replaces
		// lives. When resuming after the flip already happened, though, that
		// is the archive category by now — creating the replacement there
		// would bury the live channel inside the archive. The original
		// category isn't recoverable, so it lands uncategorized, where it's
		// plainly visible for a mod to move.
		parentID := oldChannel.ParentID
		if alreadyArchived {
			parentID = ""
		}
		var err error
		newChannel, err = p.createHiddenChannel(guildID, oldChannel, tempName, parentID)
		if err != nil {
			return fmt.Errorf("rotation: create staging channel: %w", err)
		}
	}

	// 3. Capture active threads — visibility only, logged/audited, no
	// gating logic (Milestone 3 decision: no per-thread "keep active"
	// exemption for v1). Only on a fresh run: if a prior attempt already
	// got past reveal (alreadyRevealed), the old channel's threads were
	// already captured and logged in that attempt, so re-fetching them
	// again on a retry is a pure wasted call.
	var threadNames []string
	if !alreadyRevealed {
		threadNames = p.captureThreadNames(rc.ChannelID)

		// 5. Populate the still-hidden channel: sticky repost + pin, then
		// the transparency notice. Any failure here leaves the OLD channel
		// completely untouched and still live — zero member-visible impact.
		if err := p.populateIfNeeded(newChannel.ID, rc); err != nil {
			return fmt.Errorf("rotation: populate staging channel: %w", err)
		}

		// 6. Flip — new first: reveal it under the final name. Past this
		// point there is a live channel matching the configured name.
		//
		// Deliberately NOT parallelized with the archive step below, even
		// though the two ChannelEditComplex calls target disjoint channel
		// IDs with no Go-level data dependency: the whole point of this
		// ordering is that there is *always* at least one live channel
		// bearing the configured name. Running both concurrently would
		// race against Discord's own response timing — if archive's PATCH
		// lands before reveal's, there'd be a window where *neither*
		// channel has the right name, which is strictly worse than today's
		// accepted "both visible" window. The realistic time saved (well
		// under a second) isn't worth trading away that guarantee.
		overwrites := oldChannel.PermissionOverwrites
		if alreadyArchived {
			// Resuming after the flip already happened, with the replacement
			// since deleted: the old channel's overwrites are the archive's
			// own by now (@everyone denied), and copying those onto the
			// replacement would hide the channel that's meant to be live.
			// The originals aren't recoverable, so fall back to guild
			// defaults — revealNewChannel's empty-overwrites path — which is
			// what a typical rotating channel had to begin with.
			overwrites = nil
		}
		if err := p.revealNewChannel(newChannel.ID, originalName, guildID, overwrites); err != nil {
			return fmt.Errorf("rotation: reveal new channel: %w", err)
		}
	}

	// 7. Flip — archive old, unless a previous attempt already did (which
	// would otherwise re-stamp the archive with a fresh timestamp in its
	// name on every retry).
	now := p.now()
	if !alreadyArchived {
		archiveName := archiveChannelName(originalName, now)
		modRoleIDs := p.settings.ModRoleIDs(guildID)
		if err := p.archiveOldChannel(oldChannel.ID, archiveName, rc.ArchiveCategoryID, guildID, modRoleIDs, rc); err != nil {
			return fmt.Errorf("rotation: archive old channel: %w", err)
		}
	}

	// 8. Record the archive for eventual sweep-based permanent deletion.
	// DeleteAfter is the deadline as of right now; RotationID is what lets the
	// sweep re-derive it from the live retention setting on every pass, so a
	// later retention change applies to this archive too (see archiveDeadline).
	var deleteAfter *time.Time
	if rc.RetentionHours != nil {
		t := now.Add(time.Duration(*rc.RetentionHours) * time.Hour)
		deleteAfter = &t
	}
	rotationID := rc.ID
	if err := p.archives.Insert(ctx, ArchiveRecord{
		ChannelID:         oldChannel.ID,
		GuildID:           guildID,
		SourceChannelID:   rc.ChannelID,
		ArchiveCategoryID: rc.ArchiveCategoryID,
		RotationID:        &rotationID,
		ArchivedAt:        now,
		DeleteAfter:       deleteAfter,
	}); err != nil {
		return fmt.Errorf("rotation: record archive: %w", err)
	}

	// 8a. Retarget the rotation config itself onto the new channel. Without
	// this, rc.ChannelID keeps pointing at what is now an archived channel —
	// the next scheduled fire would refetch it by that stale ID and try to
	// "rotate" the archive (wrong name, wrong parent category) instead of
	// the live replacement. RetargetRotationChannel publishes
	// core.EventConfigChanged, which reconcile (subscribed in Init) picks up
	// to re-key the Scheduler job from the old channel ID to the new one.
	if err := p.settings.RetargetRotationChannel(ctx, guildID, oldChannel.ID, newChannel.ID); err != nil {
		return fmt.Errorf("rotation: retarget rotation config to new channel: %w", err)
	}

	// 9. Audit + event.
	if len(threadNames) > 0 {
		p.log.Info("rotation: active threads archived with channel",
			"channel", oldChannel.ID, "threads", strings.Join(threadNames, ", "))
	}
	// The rotation itself has already fully succeeded by this point (rename,
	// archive, new channel live, archive record persisted) — an audit-embed
	// failure (e.g. #bird-audit-log not yet configured, or deleted) must not
	// mark this job as failed, or the scheduler would retry an already-done
	// rotation and eventually false-alarm #bird-status after
	// maxConsecutiveFailures, masking the fact that rotation itself is fine.
	// Matches the log-and-continue policy every other audit call site uses
	// (sweep.go, adminconfig.go, rotation/configure.go).
	if err := p.audit.Record(ctx, guildID, "system", "channel.rotated", oldChannel.ID, newChannel.ID); err != nil {
		p.log.Error("rotation: audit failed", "old_channel", oldChannel.ID, "new_channel", newChannel.ID, "err", err)
	}
	p.bus.Publish(ctx, core.Event{
		Type:    core.EventChannelRotated,
		GuildID: guildID,
		Payload: core.ChannelRotatedPayload{OldChannelID: oldChannel.ID, NewChannelID: newChannel.ID},
	})
	return nil
}

func (p *Plugin) captureThreadNames(channelID string) []string {
	list, err := p.ops.ThreadsActive(channelID)
	if err != nil || list == nil {
		return nil
	}
	names := make([]string, 0, len(list.Threads))
	for _, t := range list.Threads {
		names = append(names, t.Name)
	}
	return names
}

// stagingChannelBotAllow is what the bot needs on the hidden staging
// channel: enough to read/post while checking/populating it. Deliberately
// does NOT include PermissionManageMessages (which pinning would need) —
// the bot's own Discord role only holds Manage Channels + Manage Roles
// (least privilege, spec.MD §4), and core.DenyEveryoneExceptBot's overwrite
// grant fails the entire channel-creation request (403 Missing Permissions)
// if it tries to grant a bit the bot doesn't actually hold. Pinning is
// attempted best-effort in populateIfNeeded instead of required here.
const stagingChannelBotAllow = discordgo.PermissionViewChannel | discordgo.PermissionSendMessages

func (p *Plugin) createHiddenChannel(guildID string, oldChannel *discordgo.Channel, tempName, parentID string) (*discordgo.Channel, error) {
	botUserID, err := p.getBotUserID()
	if err != nil {
		return nil, err
	}
	return p.ops.GuildChannelCreateComplex(guildID, discordgo.GuildChannelCreateData{
		Name:                 tempName,
		Type:                 oldChannel.Type,
		Topic:                oldChannel.Topic,
		RateLimitPerUser:     oldChannel.RateLimitPerUser,
		Position:             oldChannel.Position,
		PermissionOverwrites: core.DenyEveryoneExceptBot(oldChannel.PermissionOverwrites, guildID, botUserID, stagingChannelBotAllow),
		ParentID:             parentID,
		NSFW:                 oldChannel.NSFW,
	})
}

// populateIfNeeded posts sticky messages (pinned if the bot happens to hold
// Manage Messages, best-effort otherwise) and the transparency notice into
// the staging channel, unless it already has messages — which only happens
// on a retry after a prior run got this far, and reposting would duplicate
// content in what will shortly become a very-visible channel.
func (p *Plugin) populateIfNeeded(channelID string, rc settings.RotationChannel) error {
	existing, err := p.ops.ChannelMessages(channelID, 1, "", "", "")
	if err != nil {
		return fmt.Errorf("check existing messages: %w", err)
	}
	if len(existing) > 0 {
		return nil
	}

	for _, msg := range resolveSticky(rc) {
		sent, err := p.ops.ChannelMessageSend(channelID, msg)
		if err != nil {
			return fmt.Errorf("post sticky message: %w", err)
		}
		// Pinning needs Manage Messages, which the bot's invite scope
		// doesn't currently request — treat it as a nice-to-have, not a
		// reason to fail the whole rotation. The sticky message itself
		// still posted successfully either way.
		if err := p.ops.ChannelMessagePin(channelID, sent.ID); err != nil {
			p.log.Warn("rotation: pin sticky message failed, continuing without pin", "channel", channelID, "err", err)
		}
	}

	if _, err := p.ops.ChannelMessageSendEmbed(channelID, &discordgo.MessageEmbed{
		Description: retentionNotice(rc),
	}); err != nil {
		return fmt.Errorf("post retention notice: %w", err)
	}
	return nil
}

// revealNewChannel restores the old channel's original permission overwrites
// onto the new one — the "copy permissions exactly, swap only the visibility"
// contract this whole feature promises.
//
// If the old channel had zero explicit overwrites (the common case for a
// fully public channel like general chat: @everyone sees it purely via
// guild-level role permissions, no channel-specific entry needed), that's an
// empty slice here — but discordgo's ChannelEdit.PermissionOverwrites is
// `json:"...,omitempty"`, so an empty/nil slice is dropped from the outgoing
// PATCH entirely rather than sent as `[]`. Discord then leaves the channel's
// existing overwrites untouched, which at this point are still
// createHiddenChannel's staging ones (@everyone denied, only the bot
// allowed) — permanently locking everyone but the bot out of what's meant to
// become the fully public replacement. An explicit no-op @everyone overwrite
// (zero Allow/Deny) is functionally identical to no overwrite at all, but as
// a non-empty slice it actually reaches the API and replaces the staging
// overwrites instead of being silently skipped.
func (p *Plugin) revealNewChannel(channelID, finalName, guildID string, originalOverwrites []*discordgo.PermissionOverwrite) error {
	overwrites := originalOverwrites
	if len(overwrites) == 0 {
		overwrites = []*discordgo.PermissionOverwrite{{
			ID:   guildID,
			Type: discordgo.PermissionOverwriteTypeRole,
		}}
	}
	_, err := p.ops.ChannelEditComplex(channelID, &discordgo.ChannelEdit{
		Name:                 finalName,
		PermissionOverwrites: overwrites,
	})
	return err
}

func (p *Plugin) archiveOldChannel(channelID, archiveName, archiveCategoryID, guildID string, modRoleIDs []string, rc settings.RotationChannel) error {
	botUserID, err := p.getBotUserID()
	if err != nil {
		return err
	}
	_, err = p.ops.ChannelEditComplex(channelID, &discordgo.ChannelEdit{
		Name:                 archiveName,
		ParentID:             archiveCategoryID,
		PermissionOverwrites: archiveOverwrites(guildID, botUserID, modRoleIDs, rc),
	})
	return err
}

// archiveOverwrites builds a permission-overwrite set denying @everyone,
// keeping the bot itself able to read it (needed later by sweep.go's
// rescue-hatch check and eventual delete) via core.DenyEveryoneExceptBot,
// always allowing the guild's configured mod roles, and — when
// rc.ArchiveVisibility is "whitelist" — additionally allowing
// rc.ArchiveWhitelistRoleIDs/ArchiveWhitelistUserIDs (spec.MD §6's
// "archive_visibility: mod_only | whitelist").
func archiveOverwrites(guildID, botUserID string, modRoleIDs []string, rc settings.RotationChannel) []*discordgo.PermissionOverwrite {
	out := core.DenyEveryoneExceptBot(nil, guildID, botUserID, discordgo.PermissionViewChannel)
	for _, roleID := range modRoleIDs {
		out = append(out, &discordgo.PermissionOverwrite{
			ID:    roleID,
			Type:  discordgo.PermissionOverwriteTypeRole,
			Allow: discordgo.PermissionViewChannel,
		})
	}
	if rc.ArchiveVisibility != "whitelist" {
		return out
	}
	for _, roleID := range rc.ArchiveWhitelistRoleIDs {
		out = append(out, &discordgo.PermissionOverwrite{
			ID:    roleID,
			Type:  discordgo.PermissionOverwriteTypeRole,
			Allow: discordgo.PermissionViewChannel,
		})
	}
	for _, userID := range rc.ArchiveWhitelistUserIDs {
		out = append(out, &discordgo.PermissionOverwrite{
			ID:    userID,
			Type:  discordgo.PermissionOverwriteTypeMember,
			Allow: discordgo.PermissionViewChannel,
		})
	}
	return out
}

func findChannel(channels []*discordgo.Channel, match func(*discordgo.Channel) bool) *discordgo.Channel {
	for _, c := range channels {
		if match(c) {
			return c
		}
	}
	return nil
}

// stagingChannelName is the deterministic name a rotation's not-yet-revealed
// replacement carries. Derived from the channel being replaced so a retry
// can find the staging channel a previous attempt left behind, and so two
// rotating channels can never collide on it.
func stagingChannelName(channelID string) string { return "rotating-" + channelID }

const archiveNameTimeLayout = "2006-01-02-1504"

func archiveChannelName(originalName string, at time.Time) string {
	return fmt.Sprintf("%s-archive-%s", originalName, at.Format(archiveNameTimeLayout))
}

// archiveNamePattern matches what archiveChannelName produces, capturing the
// name the channel had before it was archived. Greedy on purpose: a channel
// legitimately named "foo-archive-bar" that gets archived yields
// "foo-archive-bar-archive-2026-01-01-0000", and the longest prefix is the
// right answer.
var archiveNamePattern = regexp.MustCompile(`^(.+)-archive-\d{4}-\d{2}-\d{2}-\d{4}$`)

// archivedChannelOrigin reports whether ch has already been archived by an
// earlier attempt at this rotation — it carries an archive name *and* sits
// in the configured archive category, neither alone being conclusive — and
// returns the name it had before that. For a channel that hasn't been
// archived, its current name is its original name.
func archivedChannelOrigin(ch *discordgo.Channel, archiveCategoryID string) (originalName string, archived bool) {
	if archiveCategoryID == "" || ch.ParentID != archiveCategoryID {
		return ch.Name, false
	}
	m := archiveNamePattern.FindStringSubmatch(ch.Name)
	if m == nil {
		return ch.Name, false
	}
	return m[1], true
}
