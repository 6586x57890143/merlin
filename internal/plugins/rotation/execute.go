package rotation

import (
	"context"
	"fmt"
	"strings"
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
	// 1. Preflight: re-fetch old channel, confirm guild ownership
	// (confused-deputy check, spec.MD §4) rather than trusting rc.ChannelID
	// blindly.
	oldChannel, err := p.ops.Channel(rc.ChannelID)
	if err != nil {
		return fmt.Errorf("rotation: fetch channel %s: %w", rc.ChannelID, err)
	}
	if oldChannel.GuildID != guildID {
		return fmt.Errorf("rotation: channel %s does not belong to guild %s", rc.ChannelID, guildID)
	}

	// 2. Capacity check — fail loud and clean, not with a raw Discord error.
	channels, err := p.ops.GuildChannels(guildID)
	if err != nil {
		return fmt.Errorf("rotation: list channels for guild %s: %w", guildID, err)
	}
	if len(channels) >= maxChannelsPerGuild-channelCapHeadroom {
		return fmt.Errorf("rotation: guild %s at %d/%d channels, skipping rotation for %s",
			guildID, len(channels), maxChannelsPerGuild, rc.ChannelID)
	}

	// 3. Capture active threads — visibility only, logged/audited, no
	// gating logic (Milestone 3 decision: no per-thread "keep active"
	// exemption for v1).
	threadNames := p.captureThreadNames(rc.ChannelID)

	tempName := "rotating-" + rc.ChannelID

	// 4. Idempotency check / create new (hidden). Two retry cases to
	// recognize from live state, not in-memory progress:
	//   - a prior run already revealed the replacement (renamed away from
	//     tempName to oldChannel.Name) but crashed/failed before archiving
	//     the old channel — found by name, excluding the old channel
	//     itself, since both can legitimately share a name at this point;
	//   - a prior run created the hidden staging channel but crashed/failed
	//     before reveal — found by its deterministic temp name.
	// Otherwise, this is a fresh run: create it.
	newChannel := findOtherChannelByName(channels, oldChannel.Name, oldChannel.ID)
	alreadyRevealed := newChannel != nil
	if newChannel == nil {
		newChannel = findChannelByName(channels, tempName)
	}
	if newChannel == nil {
		newChannel, err = p.createHiddenChannel(guildID, oldChannel, tempName)
		if err != nil {
			return fmt.Errorf("rotation: create staging channel: %w", err)
		}
	}

	if !alreadyRevealed {
		// 5. Populate the still-hidden channel: sticky repost + pin, then
		// the transparency notice. Any failure here leaves the OLD channel
		// completely untouched and still live — zero member-visible impact.
		if err := p.populateIfNeeded(newChannel.ID, rc); err != nil {
			return fmt.Errorf("rotation: populate staging channel: %w", err)
		}

		// 6. Flip — new first: reveal it under the final name. Past this
		// point there is a live channel matching the configured name.
		if err := p.revealNewChannel(newChannel.ID, oldChannel.Name, guildID, oldChannel.PermissionOverwrites); err != nil {
			return fmt.Errorf("rotation: reveal new channel: %w", err)
		}
	}

	// 7. Flip — archive old. ChannelEditComplex is naturally idempotent
	// (re-applying the same name/parent/overwrites on a retry is a no-op),
	// so no separate "already archived" check is needed.
	now := p.now()
	archiveName := archiveChannelName(oldChannel.Name, now)
	modRoleIDs := p.settings.ModRoleIDs(guildID)
	if err := p.archiveOldChannel(oldChannel.ID, archiveName, rc.ArchiveCategoryID, guildID, modRoleIDs, rc); err != nil {
		return fmt.Errorf("rotation: archive old channel: %w", err)
	}

	// 8. Record the archive for eventual sweep-based permanent deletion.
	var deleteAfter *time.Time
	if rc.RetentionHours != nil {
		t := now.Add(time.Duration(*rc.RetentionHours) * time.Hour)
		deleteAfter = &t
	}
	if err := p.archives.Insert(ctx, ArchiveRecord{
		ChannelID:         oldChannel.ID,
		GuildID:           guildID,
		SourceChannelID:   rc.ChannelID,
		ArchiveCategoryID: rc.ArchiveCategoryID,
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

func (p *Plugin) createHiddenChannel(guildID string, oldChannel *discordgo.Channel, tempName string) (*discordgo.Channel, error) {
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
		ParentID:             oldChannel.ParentID,
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
		Description: retentionNotice(rc.IntervalHours),
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

func findChannelByName(channels []*discordgo.Channel, name string) *discordgo.Channel {
	for _, c := range channels {
		if c.Name == name {
			return c
		}
	}
	return nil
}

// findOtherChannelByName finds a channel matching name, excluding excludeID
// — used to detect an already-revealed replacement channel (which shares
// oldChannel.Name) without matching the old channel itself.
func findOtherChannelByName(channels []*discordgo.Channel, name, excludeID string) *discordgo.Channel {
	for _, c := range channels {
		if c.Name == name && c.ID != excludeID {
			return c
		}
	}
	return nil
}

func archiveChannelName(originalName string, at time.Time) string {
	return fmt.Sprintf("%s-archive-%s", originalName, at.Format("2006-01-02-1504"))
}
