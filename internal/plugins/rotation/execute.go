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
// configured rotating channel. It re-fetches the channel's current settings
// at execution time (not at registration time) so edits made via
// /rotation configure take effect on the very next run without needing a
// job re-register — only IntervalHours needs that (see reconcile in
// rotation.go), since it drives the Scheduler's own due-check.
func (p *Plugin) makeRotationJob(guildID, channelID string) func(ctx context.Context) error {
	return func(ctx context.Context) error {
		rc, ok := p.settings.RotationChannel(guildID, channelID)
		if !ok {
			p.log.Info("rotation: job fired for a channel no longer configured, skipping", "channel", channelID)
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
		if err := p.revealNewChannel(newChannel.ID, oldChannel.Name, oldChannel.PermissionOverwrites); err != nil {
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
	if rc.RetentionDays != nil {
		t := now.AddDate(0, 0, *rc.RetentionDays)
		deleteAfter = &t
	}
	if err := p.archives.Insert(ctx, ArchiveRecord{
		ChannelID:       oldChannel.ID,
		GuildID:         guildID,
		SourceChannelID: rc.ChannelID,
		ArchivedAt:      now,
		DeleteAfter:     deleteAfter,
	}); err != nil {
		return fmt.Errorf("rotation: record archive: %w", err)
	}

	// 9. Audit + event.
	if len(threadNames) > 0 {
		p.log.Info("rotation: active threads archived with channel",
			"channel", oldChannel.ID, "threads", strings.Join(threadNames, ", "))
	}
	if err := p.audit.Record(ctx, guildID, "system", "channel.rotated", oldChannel.ID, newChannel.ID); err != nil {
		return fmt.Errorf("rotation: audit: %w", err)
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

func (p *Plugin) createHiddenChannel(guildID string, oldChannel *discordgo.Channel, tempName string) (*discordgo.Channel, error) {
	return p.ops.GuildChannelCreateComplex(guildID, discordgo.GuildChannelCreateData{
		Name:                 tempName,
		Type:                 oldChannel.Type,
		Topic:                oldChannel.Topic,
		RateLimitPerUser:     oldChannel.RateLimitPerUser,
		Position:             oldChannel.Position,
		PermissionOverwrites: denyEveryone(oldChannel.PermissionOverwrites, guildID),
		ParentID:             oldChannel.ParentID,
		NSFW:                 oldChannel.NSFW,
	})
}

// populateIfNeeded posts sticky messages (pinned) and the transparency
// notice into the staging channel, unless it already has messages — which
// only happens on a retry after a prior run got this far, and reposting
// would duplicate content in what will shortly become a very-visible
// channel.
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
		if err := p.ops.ChannelMessagePin(channelID, sent.ID); err != nil {
			return fmt.Errorf("pin sticky message: %w", err)
		}
	}

	if _, err := p.ops.ChannelMessageSendEmbed(channelID, &discordgo.MessageEmbed{
		Description: retentionNotice(rc.IntervalHours),
	}); err != nil {
		return fmt.Errorf("post retention notice: %w", err)
	}
	return nil
}

func (p *Plugin) revealNewChannel(channelID, finalName string, originalOverwrites []*discordgo.PermissionOverwrite) error {
	_, err := p.ops.ChannelEditComplex(channelID, &discordgo.ChannelEdit{
		Name:                 finalName,
		PermissionOverwrites: originalOverwrites,
	})
	return err
}

func (p *Plugin) archiveOldChannel(channelID, archiveName, archiveCategoryID, guildID string, modRoleIDs []string, rc settings.RotationChannel) error {
	_, err := p.ops.ChannelEditComplex(channelID, &discordgo.ChannelEdit{
		Name:                 archiveName,
		ParentID:             archiveCategoryID,
		PermissionOverwrites: archiveOverwrites(guildID, modRoleIDs, rc),
	})
	return err
}

// denyEveryone clones src, ensuring @everyone (whose overwrite ID is always
// the guild ID) has VIEW_CHANNEL explicitly denied — Discord's permission
// resolution means a channel-level deny wins over any category-level
// access, reliably hiding the staging channel from ordinary members.
func denyEveryone(src []*discordgo.PermissionOverwrite, guildID string) []*discordgo.PermissionOverwrite {
	out := make([]*discordgo.PermissionOverwrite, 0, len(src)+1)
	found := false
	for _, ow := range src {
		clone := *ow
		if ow.Type == discordgo.PermissionOverwriteTypeRole && ow.ID == guildID {
			clone.Deny |= discordgo.PermissionViewChannel
			clone.Allow &^= discordgo.PermissionViewChannel
			found = true
		}
		out = append(out, &clone)
	}
	if !found {
		out = append(out, &discordgo.PermissionOverwrite{
			ID:   guildID,
			Type: discordgo.PermissionOverwriteTypeRole,
			Deny: discordgo.PermissionViewChannel,
		})
	}
	return out
}

// archiveOverwrites builds a permission-overwrite set denying @everyone,
// always allowing the guild's configured mod roles, and — when
// rc.ArchiveVisibility is "whitelist" — additionally allowing
// rc.ArchiveWhitelistRoleIDs/ArchiveWhitelistUserIDs (spec.MD §6's
// "archive_visibility: mod_only | whitelist").
func archiveOverwrites(guildID string, modRoleIDs []string, rc settings.RotationChannel) []*discordgo.PermissionOverwrite {
	out := []*discordgo.PermissionOverwrite{
		{ID: guildID, Type: discordgo.PermissionOverwriteTypeRole, Deny: discordgo.PermissionViewChannel},
	}
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
