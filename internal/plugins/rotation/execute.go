package rotation

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/snowflake/v2"

	"github.com/6586x57890143/merlin/internal/core"
	"github.com/6586x57890143/merlin/internal/discordguard"
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
// configured rotation slot, identified by rotationID (settings.RotationChannel.ID,
// stable across retargets, unlike ChannelID; see rotation.go's reconcile).
// It re-fetches the slot's current settings at execution time (not at
// registration time) so edits made via /rotation configure take effect on
// the very next run without needing a job re-register. Only IntervalMinutes
// needs that (see reconcile in rotation.go), since it drives the Scheduler's
// own due-check.
func (p *Plugin) makeRotationJob(guildID string, rotationID int64) func(ctx context.Context) error {
	return func(ctx context.Context) error {
		rc, ok := p.settings.RotationChannelByID(guildID, rotationID)
		if !ok {
			p.log.Info("rotation: job fired for a rotation slot no longer configured, skipping", "guild", guildID, "rotation_id", rotationID)
			return nil
		}
		// A paused guild is an operator's deliberate choice, not a fault.
		// Reported as failure it would burn down this job's
		// consecutive-failure budget and alert #bird-status about the very
		// state the operator just asked for. The job stays due, so it runs
		// for real on the first tick after the pause is lifted.
		if err := p.rotate(ctx, guildID, rc); err != nil {
			if discordguard.Skipped(err) {
				p.log.Info("rotation: skipped, writes paused", "guild", guildID, "channel", rc.ChannelID)
				return nil
			}
			return err
		}
		return nil
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
// configured name. See the Milestone 3 plan for the full rationale.
func (p *Plugin) rotate(ctx context.Context, guildID string, rc settings.RotationChannel) error {
	// Dry-run is checked here rather than left to discordguard's per-call
	// refusal because this is a multi-step flow: each step below re-derives
	// its state from the previous step's real effects, so refusing calls
	// one at a time would produce an incoherent half-rehearsal rather than
	// an honest "here is what a rotation would do".
	if p.dryRun(guildID) {
		p.log.Info("rotation: dry-run, skipping rotation", "guild", guildID, "channel", rc.ChannelID)
		if err := p.audit.Record(ctx, guildID, core.ActorSystem, "rotation.dryrun", core.MentionChannel(rc.ChannelID), "would have rotated now"); err != nil {
			p.log.Error("rotation: audit dry-run", "guild", guildID, "err", err)
		}
		return nil
	}

	// 1&2. Preflight fetch + capacity-check listing, run concurrently: these
	// are two independent reads (one channel by ID, one full guild channel
	// list) with no data dependency on each other. Only the processing
	// below needs both results. Fetch-channel's error takes priority if
	// both fail, matching the original sequential short-circuit order.
	gid, cat := core.ID(guildID), core.ID(rc.ArchiveCategoryID)
	var oldChannel discord.GuildChannel
	var channels []discord.GuildChannel
	var channelErr, listErr error
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		oldChannel, channelErr = guildChannel(p.ops(guildID), core.ID(rc.ChannelID))
	}()
	go func() {
		defer wg.Done()
		channels, listErr = p.ops(guildID).GetGuildChannels(gid)
	}()
	wg.Wait()

	// Confused-deputy check (spec.MD §4): confirm guild ownership rather
	// than trusting rc.ChannelID blindly.
	if channelErr != nil {
		return fmt.Errorf("rotation: fetch channel %s: %w", rc.ChannelID, channelErr)
	}
	if oldChannel.GuildID() != gid {
		return fmt.Errorf("rotation: channel %s does not belong to guild %s", rc.ChannelID, guildID)
	}
	// Capacity check: fail loud and clean, not with a raw Discord error.
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
	//     recording the archive, or retargeting the config): the "old"
	//     channel is by now sitting in the archive category under an archive
	//     name, so its original name has to be recovered from that name
	//     before anything else can be matched against it. Without this the
	//     retry recognizes nothing, rotates a second time, and leaves the
	//     guild with a duplicate replacement channel;
	//   - a prior run already revealed the replacement but failed before
	//     archiving the old channel, found by the original name, excluding
	//     the old channel itself, since both legitimately share a name at
	//     that point;
	//   - a prior run created the hidden staging channel but failed before
	//     reveal, found by its deterministic temp name.
	// Otherwise, this is a fresh run: create it.
	originalName, alreadyArchived := archivedChannelOrigin(oldChannel, cat)

	// Discord allows duplicate channel names, so matching on name alone can
	// find an unrelated channel and make this rotation archive a live
	// channel and retarget itself onto a stranger's. A genuine replacement
	// also sits outside the archive category and, until the old channel is
	// archived and moved, in the same category as the channel it replaces.
	newChannel := findChannel(channels, func(c discord.GuildChannel) bool {
		return c.Name() == originalName && c.ID() != oldChannel.ID() &&
			parent(c) != cat &&
			(alreadyArchived || parent(c) == parent(oldChannel))
	})
	alreadyRevealed := newChannel != nil
	if newChannel == nil {
		newChannel = findChannel(channels, func(c discord.GuildChannel) bool { return c.Name() == tempName })
	}
	if newChannel == nil {
		// Normally the replacement belongs wherever the channel it replaces
		// lives. When resuming after the flip already happened, though, that
		// is the archive category by now. Creating the replacement there
		// would bury the live channel inside the archive. The original
		// category isn't recoverable, so it lands uncategorized, where it's
		// plainly visible for a mod to move.
		parentID := parent(oldChannel)
		if alreadyArchived {
			parentID = 0
		}
		var err error
		newChannel, err = p.createHiddenChannel(guildID, oldChannel, tempName, parentID)
		if err != nil {
			return fmt.Errorf("rotation: create staging channel: %w", err)
		}
	}

	// 3. Capture active threads: visibility only, logged/audited, no
	// gating logic (Milestone 3 decision: no per-thread "keep active"
	// exemption for v1). Only on a fresh run: if a prior attempt already
	// got past reveal (alreadyRevealed), the old channel's threads were
	// already captured and logged in that attempt, so re-fetching them
	// again on a retry is a pure wasted call.
	var threadNames []string
	if !alreadyRevealed {
		threadNames = p.captureThreadNames(guildID, oldChannel.ID())

		// 5. Populate the still-hidden channel: sticky repost + pin, then
		// the transparency notice. Any failure here leaves the OLD channel
		// completely untouched and still live, with zero member-visible impact.
		if err := p.populateIfNeeded(ctx, newChannel.ID(), rc); err != nil {
			return fmt.Errorf("rotation: populate staging channel: %w", err)
		}

		// 6. Flip (new first): reveal it under the final name. Past this
		// point there is a live channel matching the configured name.
		//
		// Deliberately NOT parallelized with the archive step below, even
		// though the two ChannelEditComplex calls target disjoint channel
		// IDs with no Go-level data dependency: the whole point of this
		// ordering is that there is *always* at least one live channel
		// bearing the configured name. Running both concurrently would
		// race against Discord's own response timing: if archive's PATCH
		// lands before reveal's, there'd be a window where *neither*
		// channel has the right name, which is strictly worse than today's
		// accepted "both visible" window. The realistic time saved (well
		// under a second) isn't worth trading away that guarantee.
		overwrites := []discord.PermissionOverwrite(oldChannel.PermissionOverwrites())
		if alreadyArchived {
			// Resuming after the flip already happened, with the replacement
			// since deleted: the old channel's overwrites are the archive's
			// own by now (@everyone denied), and copying those onto the
			// replacement would hide the channel that's meant to be live.
			// The originals aren't recoverable, so fall back to guild
			// defaults (revealNewChannel's empty-overwrites path), which is
			// what a typical rotating channel had to begin with.
			overwrites = nil
		}
		if err := p.revealNewChannel(newChannel.ID(), originalName, guildID, overwrites); err != nil {
			return fmt.Errorf("rotation: reveal new channel: %w", err)
		}
	}

	// 7. Flip: archive old, unless a previous attempt already did (which
	// would otherwise re-stamp the archive with a fresh timestamp in its
	// name on every retry).
	now := p.now()
	if !alreadyArchived {
		archiveName := archiveChannelName(originalName, now)
		// Bring the archive category's own overwrites in line first, and
		// archive the channel *with* that exact list, so it is born correctly
		// restricted rather than sitting under its old, public overwrites
		// until some later pass notices. This is the moment the check matters
		// most: a rotation is precisely when a channel people could read
		// becomes one only mods and the guild's archive viewer roles may.
		desired, _, err := p.reconcileArchiveCategory(guildID, cat)
		if err != nil {
			return fmt.Errorf("rotation: reconcile archive category: %w", err)
		}
		if err := p.archiveOldChannel(oldChannel.ID(), archiveName, cat, guildID, desired); err != nil {
			return fmt.Errorf("rotation: archive old channel: %w", err)
		}

		// 7b. Put the replacement exactly where the original sat in the
		// sidebar. Only after the archive: until the old channel leaves the
		// category, both occupy it, and Discord resolves the collision by
		// pushing one of them down, so a position set at create time is
		// re-flowed out from under us the moment the old channel moves. Doing
		// it here, with the slot genuinely free, is the only ordering that
		// lands the channel where members expect to find it.
		p.restorePosition(guildID, newChannel.ID(), oldChannel.Position())
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
	oldID, newID := oldChannel.ID().String(), newChannel.ID().String()
	if err := p.archives.Insert(ctx, ArchiveRecord{
		ChannelID:         oldID,
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
	// this, rc.ChannelID keeps pointing at what is now an archived channel.
	// The next scheduled fire would refetch it by that stale ID and try to
	// "rotate" the archive (wrong name, wrong parent category) instead of
	// the live replacement. RetargetRotationChannel publishes
	// core.EventConfigChanged, which reconcile (subscribed in Init) picks up
	// to re-key the Scheduler job from the old channel ID to the new one.
	if err := p.settings.RetargetRotationChannel(ctx, guildID, oldID, newID); err != nil {
		return fmt.Errorf("rotation: retarget rotation config to new channel: %w", err)
	}

	// 9. Audit + event.
	if len(threadNames) > 0 {
		p.log.Info("rotation: active threads archived with channel",
			"channel", oldID, "threads", strings.Join(threadNames, ", "))
	}
	// The rotation itself has already fully succeeded by this point (rename,
	// archive, new channel live, archive record persisted). An audit-embed
	// failure (e.g. #bird-audit-log not yet configured, or deleted) must not
	// mark this job as failed, or the scheduler would retry an already-done
	// rotation and eventually false-alarm #bird-status after
	// maxConsecutiveFailures, masking the fact that rotation itself is fine.
	// Matches the log-and-continue policy every other audit call site uses
	// (sweep.go, adminconfig.go, rotation/configure.go).
	if err := p.audit.Record(ctx, guildID, core.ActorSystem, "channel.rotated", core.MentionChannel(oldID), core.MentionChannel(newID)); err != nil {
		p.log.Error("rotation: audit failed", "old_channel", oldID, "new_channel", newID, "err", err)
	}
	p.bus.Publish(ctx, core.Event{
		Type:    core.EventChannelRotated,
		GuildID: guildID,
		Payload: core.ChannelRotatedPayload{OldChannelID: oldID, NewChannelID: newID},
	})
	return nil
}

// captureThreadNames reads the guild's active threads and keeps the ones
// hanging off this channel. Guild-scoped because Discord removed the
// channel-scoped endpoint, so the parent filter below is the only thing
// keeping one channel's rotation from logging the thread names of every other
// channel in the server.
func (p *Plugin) captureThreadNames(guildID string, channelID snowflake.ID) []string {
	list, err := p.ops(guildID).GetActiveGuildThreads(core.ID(guildID))
	if err != nil || list == nil {
		return nil
	}
	var names []string
	for _, t := range list.Threads {
		if parent(t) == channelID {
			names = append(names, t.Name())
		}
	}
	return names
}

// stagingChannelBotAllow is what the bot needs on the hidden staging
// channel: enough to read/post while checking/populating it. Deliberately
// does NOT include PermissionManageMessages (which pinning would need).
// The bot's own Discord role only holds Manage Channels + Manage Roles
// (least privilege, spec.MD §4), and core.DenyEveryone's overwrite
// grant fails the entire channel-creation request (403 Missing Permissions)
// if it tries to grant a bit the bot doesn't actually hold. Pinning is
// attempted best-effort in populateIfNeeded instead of required here.
const stagingChannelBotAllow = discord.PermissionViewChannel | discord.PermissionSendMessages

// createHiddenChannel makes the replacement as a copy of oldChannel that only
// the bird can see. Rotation only accepts text channels, but one may have been
// made an announcement channel since, and the copy keeps that.
func (p *Plugin) createHiddenChannel(guildID string, oldChannel discord.GuildChannel, tempName string, parentID snowflake.ID) (discord.GuildChannel, error) {
	gid := core.ID(guildID)
	c := discord.GuildTextChannelCreate{
		Name:                 tempName,
		Position:             oldChannel.Position(),
		PermissionOverwrites: core.DenyEveryone(oldChannel.PermissionOverwrites(), gid, p.botID, stagingChannelBotAllow),
		ParentID:             parentID,
	}
	if m, ok := oldChannel.(discord.GuildMessageChannel); ok {
		if t := m.Topic(); t != nil {
			c.Topic = *t
		}
		c.RateLimitPerUser = m.RateLimitPerUser()
		c.NSFW = m.NSFW()
	}
	if oldChannel.Type() == discord.ChannelTypeGuildNews {
		return p.ops(guildID).CreateGuildChannel(gid, discord.GuildNewsChannelCreate(c))
	}
	return p.ops(guildID).CreateGuildChannel(gid, c)
}

// populateIfNeeded posts sticky messages (pinned if the bot happens to hold
// Manage Messages, best-effort otherwise) and the transparency notice into
// the staging channel, unless it already has messages, which only happens
// on a retry after a prior run got this far, and reposting would duplicate
// content in what will shortly become a very-visible channel.
func (p *Plugin) populateIfNeeded(ctx context.Context, channelID snowflake.ID, rc settings.RotationChannel) error {
	existing, err := p.ops(rc.GuildID).GetMessages(channelID, 0, 1)
	if err != nil {
		return fmt.Errorf("check existing messages: %w", err)
	}
	if len(existing) > 0 {
		return nil
	}

	for _, msg := range resolveSticky(rc) {
		sent, err := p.ops(rc.GuildID).CreateMessage(channelID, discord.MessageCreate{Content: msg})
		if err != nil {
			return fmt.Errorf("post sticky message: %w", err)
		}
		// Pinning needs Manage Messages, which the bot's invite scope
		// doesn't currently request, so treat it as a nice-to-have, not a
		// reason to fail the whole rotation. The sticky message itself
		// still posted successfully either way.
		if err := p.ops(rc.GuildID).PinMessage(channelID, sent.ID); err != nil {
			p.log.Warn("rotation: pin sticky message failed, continuing without pin", "channel", channelID, "err", err)
		}
	}

	// Sent through core.Embed like every other embed this bot produces,
	// rather than as a bare literal. This one is the most-read message
	// merlin sends: it lands in the busiest channel in the server on every
	// single rotation, and until now it was the only embed with no colour,
	// no footer and no timestamp, which made the server's own retention
	// notice look less like the bot than the bot's error messages did.
	//
	// The footer icon is an attachment:// reference, so the file has to
	// travel with it or the icon renders broken.
	notice := core.Embed(core.ColorPrimary, "", p.retentionNotice(ctx, rc))
	if _, err := p.ops(rc.GuildID).CreateMessage(channelID, discord.MessageCreate{
		Embeds: []discord.Embed{core.Bulleted(notice)},
		Files:  core.EmbedUploads(notice),
	}); err != nil {
		return fmt.Errorf("post retention notice: %w", err)
	}
	return nil
}

// revealNewChannel restores the old channel's original permission overwrites
// onto the new one, the "copy permissions exactly, swap only the visibility"
// contract this whole feature promises.
//
// If the old channel had zero explicit overwrites (the common case for a
// fully public channel like general chat), the list sent is an empty one,
// never a missing one: a PATCH without the field leaves the staging
// overwrites in place (@everyone denied, only the bird allowed), locking
// everyone out of what's meant to become the public replacement.
func (p *Plugin) revealNewChannel(channelID snowflake.ID, finalName, guildID string, originalOverwrites []discord.PermissionOverwrite) error {
	overwrites := append([]discord.PermissionOverwrite{}, originalOverwrites...)
	_, err := p.ops(guildID).UpdateChannel(channelID, discord.GuildTextChannelUpdate{
		Name:                 &finalName,
		PermissionOverwrites: &overwrites,
	})
	return err
}

// restorePosition moves the freshly revealed channel into the slot its
// predecessor occupied.
//
// Best-effort by design, and the one step here that must never fail a
// rotation. Everything that makes a rotation *correct* has already happened by
// the time this runs: the replacement is live under the right name and the old
// channel is archived. Returning an error would hand the Scheduler a failed
// job, and its retry re-enters rotate(), which, finding the configured
// channel already rotated, would rotate again and create a second replacement.
// Trading a channel in the wrong sidebar position for a duplicate channel every
// retry is a bad trade, so this logs and moves on, exactly like the
// audit-failure policy every other call site follows.
//
// Deliberately not attempted on the resumed-after-archive path: the old
// channel's position by then is its position inside the archive category, and
// applying that to the live channel would be worse than leaving it alone.
func (p *Plugin) restorePosition(guildID string, channelID snowflake.ID, position int) {
	if _, err := p.ops(guildID).UpdateChannel(channelID, discord.GuildTextChannelUpdate{Position: &position}); err != nil {
		p.log.Error("rotation: could not restore channel position; rotation itself succeeded",
			"guild", guildID, "channel", channelID, "position", position, "err", err)
	}
}

// archiveOldChannel renames the outgoing channel, moves it under the archive
// category, and replaces its overwrites with the category's own in one PATCH:
// the same thing Discord's "sync permissions with category" does, and the
// reason overwrites is passed in rather than computed here (see
// archiveperms.go for why the category owns this).
func (p *Plugin) archiveOldChannel(channelID snowflake.ID, archiveName string, archiveCategoryID snowflake.ID, guildID string, overwrites []discord.PermissionOverwrite) error {
	_, err := p.ops(guildID).UpdateChannel(channelID, discord.GuildTextChannelUpdate{
		Name:                 &archiveName,
		ParentID:             &archiveCategoryID,
		PermissionOverwrites: &overwrites,
	})
	return err
}

func findChannel(channels []discord.GuildChannel, match func(discord.GuildChannel) bool) discord.GuildChannel {
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
// earlier attempt at this rotation (it carries an archive name *and* sits
// in the configured archive category, neither alone being conclusive) and
// returns the name it had before that. For a channel that hasn't been
// archived, its current name is its original name.
func archivedChannelOrigin(ch discord.GuildChannel, archiveCategoryID snowflake.ID) (originalName string, archived bool) {
	if archiveCategoryID == 0 || parent(ch) != archiveCategoryID {
		return ch.Name(), false
	}
	m := archiveNamePattern.FindStringSubmatch(ch.Name())
	if m == nil {
		return ch.Name(), false
	}
	return m[1], true
}
