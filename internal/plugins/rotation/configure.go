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

func (p *Plugin) registerCommands() {
	channelOpt := func(name, desc string) *discordgo.ApplicationCommandOption {
		return &discordgo.ApplicationCommandOption{
			Type:         discordgo.ApplicationCommandOptionChannel,
			Name:         name,
			Description:  desc,
			Required:     true,
			ChannelTypes: []discordgo.ChannelType{discordgo.ChannelTypeGuildText},
		}
	}
	// durationOpt is used for both interval and retention: a plain string
	// rather than an Integer, so one option can carry its own unit
	// (core.ParseFlexibleDuration): "24h" or "3d" for a daily rotation, "30d"
	// or "720h" for a month of retention.
	//
	// The unit is mandatory, and every description below says so where the
	// person can read it before typing rather than only in the error
	// afterwards. A bare "3" was once accepted as three hours, which cost a
	// guild its archives a day early when three *days* was meant, and
	// permanent deletion has no undo. Integer options can't express a
	// unit at all, which is exactly why this used to be interval_hours/
	// retention_days: two different granularities for what's conceptually
	// the same kind of value, and no way to ask for "3 days" without doing
	// the multiplication by hand.
	durationOpt := func(name, desc string, required bool) *discordgo.ApplicationCommandOption {
		return &discordgo.ApplicationCommandOption{
			Type:        discordgo.ApplicationCommandOptionString,
			Name:        name,
			Description: desc,
			Required:    required,
		}
	}
	visibilityOpt := func(required bool) *discordgo.ApplicationCommandOption {
		return &discordgo.ApplicationCommandOption{
			Type:        discordgo.ApplicationCommandOptionString,
			Name:        "visibility",
			Description: "Who can see the archive besides mods (default: mods only)",
			Required:    required,
			Choices: []*discordgo.ApplicationCommandOptionChoice{
				{Name: "Mods only", Value: "mod_only"},
				{Name: "Mods + whitelist", Value: "whitelist"},
			},
		}
	}

	cmd := &discordgo.ApplicationCommand{
		Name:        "rotation",
		Description: "Configure and inspect channel rotation (spec.MD §6)",
		Options: []*discordgo.ApplicationCommandOption{
			{
				Type:        discordgo.ApplicationCommandOptionSubCommand,
				Name:        "list",
				Description: "List every rotating channel configured for this server",
			},
			{
				Type:        discordgo.ApplicationCommandOptionSubCommandGroup,
				Name:        "configure",
				Description: "Add, remove, or adjust a rotating channel",
				Options: []*discordgo.ApplicationCommandOption{
					{
						Type:        discordgo.ApplicationCommandOptionSubCommand,
						Name:        "add",
						Description: "Start rotating a channel",
						Options: []*discordgo.ApplicationCommandOption{
							channelOpt("channel", "The live channel to rotate"),
							durationOpt("interval", "How often it rotates. Needs a unit: \"3d\", \"24h\", \"90m\". Minimum 1 hour.", true),
							{
								Type:         discordgo.ApplicationCommandOptionChannel,
								Name:         "archive_category",
								Description:  "Hidden category archived channels move into. Omit to auto-create/reuse one named \"Archive\"",
								ChannelTypes: []discordgo.ChannelType{discordgo.ChannelTypeGuildCategory},
							},
							durationOpt("retention", "Keep the archive this long before PERMANENT deletion. Needs a unit: \"30d\", \"72h\". Omit = forever.", false),
							noticeOpt(),
							visibilityOpt(false),
						},
					},
					{
						Type:        discordgo.ApplicationCommandOptionSubCommand,
						Name:        "remove",
						Description: "Stop rotating a channel (does not delete any existing archive)",
						Options:     []*discordgo.ApplicationCommandOption{channelOpt("channel", "The rotating channel to stop rotating")},
					},
					{
						Type:        discordgo.ApplicationCommandOptionSubCommand,
						Name:        "edit",
						Description: "Adjust an already-configured rotating channel",
						Options: []*discordgo.ApplicationCommandOption{
							channelOpt("channel", "The rotating channel to adjust"),
							durationOpt("interval", "New rotation interval. Needs a unit: \"3d\", \"24h\", \"90m\". Minimum 1 hour.", false),
							durationOpt("retention", "New retention before PERMANENT deletion. Needs a unit: \"30d\" or \"72h\"", false),
							{
								Type:        discordgo.ApplicationCommandOptionBoolean,
								Name:        "retention_forever",
								Description: "Set true to keep archives forever instead of a fixed retention",
							},
							noticeOpt(),
							visibilityOpt(false),
						},
					},
					{
						Type:        discordgo.ApplicationCommandOptionSubCommand,
						Name:        "sticky",
						Description: "Set the messages reposted and pinned each time this channel rotates",
						Options: []*discordgo.ApplicationCommandOption{
							channelOpt("channel", "The rotating channel"),
							{
								Type:        discordgo.ApplicationCommandOptionBoolean,
								Name:        "enabled",
								Description: "Whether to repost sticky messages after each rotation",
								Required:    true,
							},
							{
								Type:        discordgo.ApplicationCommandOptionString,
								Name:        "messages",
								Description: "One message per line. Replaces the current set. Omit to leave unchanged.",
							},
						},
					},
				},
			},
		},
	}

	p.commands.RegisterCommand(p.Name(), cmd)
	p.commands.Handle("rotation", "list", core.PermSpec{Tier: core.TierMod, Action: actionList}, p.handleList)
	p.commands.Handle("rotation", "configure/add", core.PermSpec{Tier: core.TierAdmin, Action: actionStructural}, p.handleAdd)
	p.commands.Handle("rotation", "configure/remove", core.PermSpec{Tier: core.TierAdmin, Action: actionStructural}, p.handleRemove)
	p.commands.Handle("rotation", "configure/edit", core.PermSpec{Tier: core.TierAdmin, Action: actionAdjust}, p.handleEdit)
	p.commands.Handle("rotation", "configure/sticky", core.PermSpec{Tier: core.TierAdmin, Action: actionAdjust}, p.handleSticky)
	p.commands.HandleComponent(p.Name(), rotationListComponentPrefix, core.PermSpec{Tier: core.TierMod, Action: actionList}, p.handleListPage)
	p.commands.HandleComponent(p.Name(), rotationListSelectPrefix, core.PermSpec{Tier: core.TierMod, Action: actionList}, p.handleListSelect)
	p.commands.HandleComponent(p.Name(), rotationListBackPrefix, core.PermSpec{Tier: core.TierMod, Action: actionList}, p.handleListBack)
}

func (p *Plugin) handleAdd(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate) {
	opts := core.LeafArgs(i)

	channelID, ok := opts["channel"]
	if !ok {
		core.RespondErr(s, i, "Missing channel", fmt.Errorf("the channel option is required"))
		return
	}
	interval, err := core.ParseFlexibleDuration(opts["interval"].StringValue())
	if err != nil {
		core.RespondErr(s, i, "Invalid interval", err)
		return
	}
	archiveCategoryID, err := p.resolveArchiveCategory(i.GuildID, opts)
	if err != nil {
		core.RespondErr(s, i, "Failed to resolve archive category", err)
		return
	}

	rc := settings.RotationChannel{
		GuildID:           i.GuildID,
		ChannelID:         channelID.Value.(string),
		ArchiveCategoryID: archiveCategoryID,
		IntervalMinutes:   int(interval / time.Minute),
		ArchiveVisibility: "mod_only",
	}
	if v, ok := opts["visibility"]; ok {
		rc.ArchiveVisibility = v.StringValue()
	}
	if v, ok := opts["retention"]; ok {
		retention, err := core.ParseFlexibleDuration(v.StringValue())
		if err != nil {
			core.RespondErr(s, i, "Invalid retention", err)
			return
		}
		hours := int(retention / time.Hour)
		rc.RetentionHours = &hours
	}
	if v, ok := opts["notice"]; ok {
		minutes, err := parseNoticeLead(v.StringValue())
		if err != nil {
			core.RespondErr(s, i, "Invalid notice lead", err)
			return
		}
		rc.NoticeLeadMinutes = minutes
	} else {
		rc.NoticeLeadMinutes = defaultNoticeLeadMinutes
	}

	if err := validateRotationChannel(rc); err != nil {
		core.RespondErr(s, i, "Invalid configuration", err)
		return
	}
	if _, exists := p.settings.RotationChannel(i.GuildID, rc.ChannelID); exists {
		core.RespondErr(s, i, "Already rotating", fmt.Errorf("that channel is already configured to rotate. Use `/rotation configure edit` instead"))
		return
	}

	if err := p.settings.UpsertRotationChannel(ctx, rc); err != nil {
		core.RespondErr(s, i, "Failed to save", err)
		return
	}
	// No explicit SyncGuild call needed: UpsertRotationChannel already
	// published core.EventConfigChanged synchronously, which p.reconcile is
	// subscribed to (rotation.go's Init), so calling SyncGuild here too would
	// just re-run the identical, already-current reconcile a second time.
	// The reconcile it just triggered registered this channel's job with no
	// run history, which the Scheduler treats as immediately due, so defer
	// that first fire by one interval since this channel was *just* added.
	p.deferFirstRotation(ctx, i.GuildID, rc.ChannelID)
	p.auditConfigChange(ctx, i, "rotation.add", "", fmt.Sprintf("channel=<#%s> %s", rc.ChannelID, rotationSummary(rc)))
	core.RespondOK(s, i, "Rotation configured", fmt.Sprintf("<#%s> will now rotate every %s.", rc.ChannelID, humanDuration(interval)))
}

// resolveArchiveCategory returns the archive_category option's channel ID
// if the mod supplied one, otherwise finds (by name) or creates a category
// called defaultArchiveCategoryName, so archive_category being optional
// doesn't just shift the "go create a category first" chore onto a
// separate manual step; the bird does it.
func (p *Plugin) resolveArchiveCategory(guildID string, opts map[string]*discordgo.ApplicationCommandInteractionDataOption) (string, error) {
	if v, ok := opts["archive_category"]; ok {
		return v.Value.(string), nil
	}
	channels, err := p.ops(guildID).GuildChannels(guildID)
	if err != nil {
		return "", fmt.Errorf("list channels: %w", err)
	}
	for _, ch := range channels {
		if ch.Type == discordgo.ChannelTypeGuildCategory && ch.Name == defaultArchiveCategoryName {
			return ch.ID, nil
		}
	}
	created, err := p.ops(guildID).GuildChannelCreateComplex(guildID, discordgo.GuildChannelCreateData{
		Name: defaultArchiveCategoryName,
		Type: discordgo.ChannelTypeGuildCategory,
	})
	if err != nil {
		return "", fmt.Errorf("create %q category: %w", defaultArchiveCategoryName, err)
	}
	return created.ID, nil
}

func (p *Plugin) handleRemove(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate) {
	opts := core.LeafArgs(i)
	channelID := opts["channel"].Value.(string)

	if _, exists := p.settings.RotationChannel(i.GuildID, channelID); !exists {
		core.RespondErr(s, i, "Not rotating", fmt.Errorf("that channel isn't configured to rotate"))
		return
	}
	if err := p.settings.RemoveRotationChannel(ctx, i.GuildID, channelID); err != nil {
		core.RespondErr(s, i, "Failed to remove", err)
		return
	}
	// RemoveRotationChannel already triggered reconcile via
	// core.EventConfigChanged, see the comment in handleAdd above.
	p.auditConfigChange(ctx, i, "rotation.remove", fmt.Sprintf("channel=<#%s>", channelID), "")
	core.RespondOK(s, i, "Rotation removed", fmt.Sprintf("<#%s> will no longer rotate. Any existing archive is untouched.", channelID))
}

func (p *Plugin) handleEdit(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate) {
	opts := core.LeafArgs(i)
	channelID := opts["channel"].Value.(string)

	rc, exists := p.settings.RotationChannel(i.GuildID, channelID)
	if !exists {
		core.RespondErr(s, i, "Not rotating", fmt.Errorf("that channel isn't configured to rotate. Use `/rotation configure add` first"))
		return
	}
	before := rotationSummary(rc)

	if v, ok := opts["interval"]; ok {
		interval, err := core.ParseFlexibleDuration(v.StringValue())
		if err != nil {
			core.RespondErr(s, i, "Invalid interval", err)
			return
		}
		rc.IntervalMinutes = int(interval / time.Minute)
	}
	if v, ok := opts["retention_forever"]; ok && v.BoolValue() {
		rc.RetentionHours = nil
	} else if v, ok := opts["retention"]; ok {
		retention, err := core.ParseFlexibleDuration(v.StringValue())
		if err != nil {
			core.RespondErr(s, i, "Invalid retention", err)
			return
		}
		hours := int(retention / time.Hour)
		rc.RetentionHours = &hours
	}
	if v, ok := opts["notice"]; ok {
		minutes, err := parseNoticeLead(v.StringValue())
		if err != nil {
			core.RespondErr(s, i, "Invalid notice lead", err)
			return
		}
		rc.NoticeLeadMinutes = minutes
	}
	if v, ok := opts["visibility"]; ok {
		rc.ArchiveVisibility = v.StringValue()
	}

	if err := validateRotationChannel(rc); err != nil {
		core.RespondErr(s, i, "Invalid configuration", err)
		return
	}
	if err := p.settings.UpsertRotationChannel(ctx, rc); err != nil {
		core.RespondErr(s, i, "Failed to save", err)
		return
	}
	// UpsertRotationChannel already triggered reconcile via
	// core.EventConfigChanged, see the comment in handleAdd above.
	after := rotationSummary(rc)
	p.auditConfigChange(ctx, i, "rotation.edit", before, after)
	core.RespondOK(s, i, "Rotation updated", fmt.Sprintf("Updated <#%s>.", channelID))
}

func (p *Plugin) handleSticky(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate) {
	opts := core.LeafArgs(i)
	channelID := opts["channel"].Value.(string)

	rc, exists := p.settings.RotationChannel(i.GuildID, channelID)
	if !exists {
		core.RespondErr(s, i, "Not rotating", fmt.Errorf("that channel isn't configured to rotate. Use `/rotation configure add` first"))
		return
	}

	rc.StickyEnabled = opts["enabled"].BoolValue()
	if v, ok := opts["messages"]; ok {
		var msgs []string
		for _, line := range strings.Split(v.StringValue(), "\n") {
			if trimmed := strings.TrimSpace(line); trimmed != "" {
				msgs = append(msgs, trimmed)
			}
		}
		rc.StickyMessages = msgs
	}
	if rc.StickyEnabled && len(rc.StickyMessages) == 0 {
		core.RespondErr(s, i, "Nothing to stick", fmt.Errorf("sticky is enabled but there are no messages set. Pass `messages` at least once"))
		return
	}

	if err := p.settings.UpsertRotationChannel(ctx, rc); err != nil {
		core.RespondErr(s, i, "Failed to save", err)
		return
	}
	p.auditConfigChange(ctx, i, "rotation.sticky", "", fmt.Sprintf("channel=<#%s> enabled=%v messages=%d", channelID, rc.StickyEnabled, len(rc.StickyMessages)))
	core.RespondOK(s, i, "Sticky updated", fmt.Sprintf("Sticky settings for <#%s> updated.", channelID))
}

// rotationSummary renders the settings that decide how long content survives,
// for the audit trail. Retention gets its own formatting because it is a
// *int: the audit line used to interpolate it with %v, which prints the
// pointer address ("retention=0x55c4509432e8") rather than the value, so the
// audit record for the one irreversible setting in this plugin was
// unreadable, in the exact place someone would look after a channel was
// permanently deleted.
func rotationSummary(rc settings.RotationChannel) string {
	return fmt.Sprintf("interval=%s retention=%s visibility=%s notice=%s",
		core.FormatDuration(time.Duration(rc.IntervalMinutes)*time.Minute),
		formatRetention(rc.RetentionHours),
		rc.ArchiveVisibility,
		formatNoticeLead(rc.NoticeLeadMinutes))
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

func (p *Plugin) auditConfigChange(ctx context.Context, i *discordgo.InteractionCreate, action, oldValue, newValue string) {
	if err := p.audit.Record(ctx, i.GuildID, i.Member.User.ID, action, oldValue, newValue); err != nil {
		p.log.Error("rotation: audit record failed", "action", action, "err", err)
	}
}

// validateRotationChannel mirrors the checks the old YAML-loader-time
// validation used to perform (internal/config/rotation_validate.go, removed
// with the move to DB-backed settings), now enforced at the point of
// mutation instead of at config-file load time.
func validateRotationChannel(rc settings.RotationChannel) error {
	if rc.ChannelID == rc.ArchiveCategoryID {
		return fmt.Errorf("a channel can't be its own archive category")
	}
	switch rc.ArchiveVisibility {
	case "mod_only", "whitelist":
	default:
		return fmt.Errorf("visibility must be mod_only or whitelist, got %q", rc.ArchiveVisibility)
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

// noticeOpt is the shared "how long before the wipe to warn" option.
func noticeOpt() *discordgo.ApplicationCommandOption {
	return &discordgo.ApplicationCommandOption{
		Type:        discordgo.ApplicationCommandOptionString,
		Name:        "notice",
		Description: "Warn the channel this long before it rotates. Needs a unit: \"10m\", \"1h\". \"off\" to disable.",
	}
}

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
