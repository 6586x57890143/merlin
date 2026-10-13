package rotation

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/disgoorg/disgo/discord"

	"github.com/6586x57890143/merlin/internal/core"
	"github.com/6586x57890143/merlin/internal/settings"
)

// Action namespaces for whitelist grants (core.PermSpec.Action). Adding or
// removing a rotating channel is structural and stays Admin-tier; adjusting
// an already-configured channel's interval/retention/visibility/sticky
// content is a separate action (not folded into "rotation.configure_structural")
// so an admin can independently loosen just adjustment (via
// /config permissions set-tier rotation.configure mod) without also handing
// out the ability to add or remove rotating channels entirely. Both default
// to Admin-only: mods get no configure access out of the box, only via an
// explicit per-guild tier override or whitelist grant.
const (
	actionStructural = "rotation.configure_structural"
	actionAdjust     = "rotation.configure"
	actionList       = "rotation.list"
)

// minRotationInterval is the floor on how often a channel may rotate.
//
// Interval is stored in minutes and accepted at minute precision, so "every
// 90 minutes" and "every 2h30m" are both expressible, but not "every 5
// minutes". Each rotation creates a channel, populates it, edits two
// channels, and eventually deletes one, and a guild's channel-create budget
// in discordguard is 20/hour: a sub-hourly cadence on even a couple of
// channels would spend it on rotation alone and starve everything else,
// including the sweep that deletes the archives it just produced. It is also
// the point past which rotation stops being a privacy measure and becomes
// unusable for members, who lose the channel mid-conversation.
//
// A floor, not a granularity: anything at or above it is allowed to the
// minute.
const (
	minRotationInterval        = time.Hour
	minRotationIntervalMinutes = int(minRotationInterval / time.Minute)
)

// defaultArchiveCategoryName is what /rotation configure add creates (or
// reuses, if one already exists under this name) when archive_category is
// omitted, mirroring /config setup's "auto-create whatever's missing"
// pattern (adminconfig.handleSetup) instead of forcing a mod to go create a
// category by hand in Discord's UI before they can configure rotation at
// all.
const defaultArchiveCategoryName = "Archive"

// genericDisclosureNote explains the one place the disclosure mode reaches
// beyond the post-rotation notice, and is empty for every other mode.
//
// The heads-up and the disclosure mode are deliberately separate switches:
// notice lead controls whether the channel is warned, disclosure controls how
// much it is told. Generic is the one combination where they interact, since
// a countdown *is* the rotation schedule. An admin who set generic to stop
// publishing that schedule has not obviously realised the warning still
// posts, so the response says both what changed and how to go further.
func genericDisclosureNote(rc settings.RotationChannel) string {
	if rc.Disclosure.Resolve() != settings.DisclosureGeneric || rc.NoticeLeadMinutes <= 0 {
		return ""
	}
	return "\n\nOn generic disclosure the pre-rotation heads-up still posts, but without a countdown. " +
		"Set the heads-up to off to silence it entirely."
}

// resolveArchiveCategory returns the archive_category option's channel ID
// if the mod supplied one, otherwise finds (by name) or creates a category
// called defaultArchiveCategoryName, so archive_category being optional
// doesn't just shift the "go create a category first" chore onto a
// separate manual step; the bird does it.
func (p *Plugin) resolveArchiveCategory(guildID, categoryID string) (string, error) {
	if categoryID != "" {
		p.hideArchiveCategory(guildID, categoryID)
		return categoryID, nil
	}
	channels, err := p.ops(guildID).GetGuildChannels(core.ID(guildID))
	if err != nil {
		return "", fmt.Errorf("list channels: %w", err)
	}
	for _, ch := range channels {
		if ch.Type() == discord.ChannelTypeGuildCategory && ch.Name() == defaultArchiveCategoryName {
			return ch.ID().String(), nil
		}
	}
	created, err := p.ops(guildID).CreateGuildChannel(core.ID(guildID), discord.GuildCategoryChannelCreate{
		Name: defaultArchiveCategoryName,
	})
	if err != nil {
		return "", fmt.Errorf("create %q category: %w", defaultArchiveCategoryName, err)
	}
	p.hideArchiveCategory(guildID, created.ID().String())
	return created.ID().String(), nil
}

// hideArchiveCategory applies the archive permission policy to categoryID at
// configure time, so a category described to the admin as "hidden" actually is
// one from the moment it is picked or created. Before this, the category was
// created with no overwrites at all and only the channels moved into it were
// ever restricted.
//
// Deliberately log-and-continue: rotate() reconciles the category again (and
// fails loudly there) before it archives anything into it, so a blip here
// costs nothing but a temporarily visible empty category. Existing channels
// under the category aren't resynced from here; the caller responds inside
// Discord's 3-second window, and the hourly sweep covers them.
func (p *Plugin) hideArchiveCategory(guildID, categoryID string) {
	if _, _, err := p.reconcileArchiveCategory(guildID, core.ID(categoryID)); err != nil {
		p.log.Error("rotation: could not apply archive permissions to category", "guild", guildID, "category", categoryID, "err", err)
	}
}

// rotationSummary renders the settings that decide how long content survives,
// for the audit trail. Retention gets its own formatting because it is a
// *int: the audit line used to interpolate it with %v, which prints the
// pointer address ("retention=0x55c4509432e8") rather than the value, so the
// audit record for the one irreversible setting in this plugin was
// unreadable, in the exact place someone would look after a channel was
// permanently deleted.
func rotationSummary(rc settings.RotationChannel) string {
	return fmt.Sprintf("interval=%s retention=%s visibility=%s notice=%s disclosure=%s",
		core.FormatDuration(time.Duration(rc.IntervalMinutes)*time.Minute),
		formatRetention(rc.RetentionHours),
		rc.ArchiveVisibility,
		formatNoticeLead(rc.NoticeLeadMinutes),
		rc.Disclosure.Resolve())
}

// formatNoticeLead renders the heads-up lead, distinguishing "off" from any
// real value. Same reasoning as formatRetention: an audit record showing
// "notice=0" would leave a reader guessing whether that means disabled or
// unset, and this is a record somebody reads when working out why a channel
// did or did not warn people.
func formatNoticeLead(minutes int) string {
	if minutes <= 0 {
		return "off"
	}
	return core.FormatDuration(time.Duration(minutes) * time.Minute)
}

// formatRetention renders a retention window, distinguishing "kept forever"
// (nil) from any finite window. Never prints a pointer.
func formatRetention(hours *int) string {
	if hours == nil {
		return "forever"
	}
	return core.FormatDuration(time.Duration(*hours) * time.Hour)
}

func (p *Plugin) auditConfigChange(ctx context.Context, ix *core.Ix, action, oldValue, newValue string) {
	if err := p.audit.Record(ctx, ix.GuildID, ix.UserID, action, oldValue, newValue); err != nil {
		p.log.Error("rotation: audit record failed", "action", action, "err", err)
	}
}

// ValidateChannel mirrors the checks the old YAML-loader-time
// validation used to perform (internal/config/rotation_validate.go, removed
// with the move to DB-backed settings), now enforced at the point of
// mutation instead of at config-file load time.
func ValidateChannel(rc settings.RotationChannel) error {
	if rc.ChannelID == rc.ArchiveCategoryID {
		return fmt.Errorf("a channel can't be its own archive category")
	}
	// "whitelist" is still accepted, even though visibilityOpt no longer
	// offers it: archiveperms.go still folds it into the allowed set, and
	// legacy guilds carry it from /config import. Rejecting it here would
	// break /rotation configure
	// edit for exactly those guilds, since edit is a read-modify-write of the
	// whole struct and would fail validation on a field the admin never
	// touched.
	switch rc.ArchiveVisibility {
	case "mod_only", "whitelist":
	default:
		return fmt.Errorf("visibility must be mod_only or whitelist, got %q", rc.ArchiveVisibility)
	}
	if !rc.Disclosure.Resolve().Valid() {
		return fmt.Errorf("disclosure must be one of full, cadence, retention, or generic, got %q", rc.Disclosure)
	}
	if rc.RetentionHours != nil && *rc.RetentionHours < 1 {
		return fmt.Errorf("retention must be at least 1 hour if set (omit it entirely for forever)")
	}
	if rc.IntervalMinutes < minRotationIntervalMinutes {
		return fmt.Errorf("interval must be at least %s (got %s)",
			core.FormatDuration(minRotationInterval),
			core.FormatDuration(time.Duration(rc.IntervalMinutes)*time.Minute))
	}
	// A lead at or beyond the interval would mean the warning for the next
	// rotation is already due the instant the previous one finishes, so the
	// channel would carry a permanent "about to wipe" notice and the words
	// would stop meaning anything.
	if rc.NoticeLeadMinutes < 0 {
		return fmt.Errorf("notice lead can't be negative (use 0 to turn the heads-up off)")
	}
	if rc.NoticeLeadMinutes >= rc.IntervalMinutes {
		return fmt.Errorf("notice lead must be shorter than the rotation interval (lead %s, interval %s)",
			core.FormatDuration(time.Duration(rc.NoticeLeadMinutes)*time.Minute),
			core.FormatDuration(time.Duration(rc.IntervalMinutes)*time.Minute))
	}
	return nil
}

// defaultNoticeLeadMinutes is how long before a rotation a channel is
// warned when nobody said otherwise. Long enough to finish a thought,
// short enough that people who were not around when it posted are not
// reading a stale countdown.
const defaultNoticeLeadMinutes = 10

// parseNoticeLead accepts a duration or the word "off", which is the
// spelling somebody reaches for when they want the message gone. Requiring
// a literal 0 for that would be a small puzzle in a command people run once.
func parseNoticeLead(v string) (int, error) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "off", "none", "0":
		return 0, nil
	}
	d, err := core.ParseFlexibleDuration(v)
	if err != nil {
		return 0, err
	}
	return int(d / time.Minute), nil
}
