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
	// "whitelist" is deliberately not offered. archiveperms.go still honours
	// it, but nothing except /config import's legacy YAML path has ever
	// populated archive_whitelist_role_ids/user_ids, so choosing it from
	// here produced a mode that behaved exactly like mod_only. Guilds that
	// carry it from an import keep it, and validateRotationChannel still
	// accepts it; see the comment there. spec.MD §6 lists it as a mode, so
	// this is a deliberate narrowing of the command surface rather than a
	// removal of the feature. Granting a non-mod role archive access is what
	// /rotation configure allow-archive-role does instead, guild-wide, since
	// archive permissions live on the shared archive category.
	visibilityOpt := func(required bool) *discordgo.ApplicationCommandOption {
		return &discordgo.ApplicationCommandOption{
			Type:        discordgo.ApplicationCommandOptionString,
			Name:        "visibility",
			Description: "Who can see the archive besides mods (default: mods only)",
			Required:    required,
			Choices: []*discordgo.ApplicationCommandOptionChoice{
				{Name: "Mods only", Value: "mod_only"},
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
							disclosureOpt(),
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
							disclosureOpt(),
							visibilityOpt(false),
							{
								Type:         discordgo.ApplicationCommandOptionChannel,
								Name:         "archive_category",
								Description:  "Move future archives into a different hidden category",
								ChannelTypes: []discordgo.ChannelType{discordgo.ChannelTypeGuildCategory},
							},
						},
					},
					{
						Type:        discordgo.ApplicationCommandOptionSubCommand,
						Name:        "allow-archive-role",
						Description: "Let a role see this server's archived channels, on top of the mod roles",
						Options: []*discordgo.ApplicationCommandOption{{
							Type:        discordgo.ApplicationCommandOptionRole,
							Name:        "role",
							Description: "The role to grant archive access",
							Required:    true,
						}},
					},
					{
						Type:        discordgo.ApplicationCommandOptionSubCommand,
						Name:        "disallow-archive-role",
						Description: "Stop a role seeing this server's archived channels",
						Options: []*discordgo.ApplicationCommandOption{{
							Type:        discordgo.ApplicationCommandOptionRole,
							Name:        "role",
							Description: "The role to remove archive access from",
							Required:    true,
						}},
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
	// Structural, not adjust: who can read an archive is a visibility
	// decision with the same blast radius as adding or removing a rotating
	// channel, and a guild that lowered rotation.configure to mod shouldn't
	// have handed out the archive along with it.
	p.commands.Handle("rotation", "configure/allow-archive-role", core.PermSpec{Tier: core.TierAdmin, Action: actionStructural}, p.handleAllowArchiveRole)
	p.commands.Handle("rotation", "configure/disallow-archive-role", core.PermSpec{Tier: core.TierAdmin, Action: actionStructural}, p.handleDisallowArchiveRole)
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
	if v, ok := opts["disclosure"]; ok {
		rc.Disclosure = settings.Disclosure(v.StringValue())
	} else {
		rc.Disclosure = settings.DisclosureFull
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
	core.RespondOK(s, i, "Rotation configured",
		fmt.Sprintf("<#%s> will now rotate every %s.%s", rc.ChannelID, humanDuration(interval), genericDisclosureNote(rc)))
}

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
		"Set `notice:off` to silence it entirely."
}

// resolveArchiveCategory returns the archive_category option's channel ID
// if the mod supplied one, otherwise finds (by name) or creates a category
// called defaultArchiveCategoryName, so archive_category being optional
// doesn't just shift the "go create a category first" chore onto a
// separate manual step; the bird does it.
func (p *Plugin) resolveArchiveCategory(guildID string, opts map[string]*discordgo.ApplicationCommandInteractionDataOption) (string, error) {
	if v, ok := opts["archive_category"]; ok {
		id := v.Value.(string)
		p.hideArchiveCategory(guildID, id)
		return id, nil
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
	p.hideArchiveCategory(guildID, created.ID)
	return created.ID, nil
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
	if _, _, err := p.reconcileArchiveCategory(guildID, categoryID); err != nil {
		p.log.Error("rotation: could not apply archive permissions to category", "guild", guildID, "category", categoryID, "err", err)
	}
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
	if v, ok := opts["disclosure"]; ok {
		rc.Disclosure = settings.Disclosure(v.StringValue())
	}
	// Editable rather than add-only, because the alternative was remove and
	// re-add: that mints a fresh settings_rotation_channels.id, and the
	// sweep re-derives each archive's deadline through rotation_id
	// (migration 0013), so every existing archive would silently fall back
	// to its frozen delete_after and stop tracking the guild's live
	// retention setting.
	if v, ok := opts["archive_category"]; ok {
		rc.ArchiveCategoryID = v.Value.(string)
		p.hideArchiveCategory(i.GuildID, rc.ArchiveCategoryID)
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
	core.RespondOK(s, i, "Rotation updated",
		fmt.Sprintf("Updated <#%s>.%s", channelID, genericDisclosureNote(rc)))
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

// handleAllowArchiveRole and handleDisallowArchiveRole edit the guild's
// archive viewer roles, then immediately reconcile every archive category and
// resync its channels, so the grant is visible the moment the command
// returns rather than at the next rotation. That resync is O(channels) and
// writes to Discord, so both defer first (spec.MD §4a: a handler has 3
// seconds to respond at all).
//
// Same mutate-then-sync shape /roles configure allow-channel already uses for
// jail's own allowlist.
func (p *Plugin) handleAllowArchiveRole(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate) {
	p.changeArchiveViewerRole(ctx, s, i, true)
}

func (p *Plugin) handleDisallowArchiveRole(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate) {
	p.changeArchiveViewerRole(ctx, s, i, false)
}

func (p *Plugin) changeArchiveViewerRole(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate, allow bool) {
	roleID := core.LeafArgs(i)["role"].Value.(string)
	if err := core.DeferResponse(s, i); err != nil {
		p.log.Error("rotation: defer archive-role response failed", "guild", i.GuildID, "err", err)
		return
	}

	var (
		err    error
		action = "rotation.archive_role_removed"
		verb   = "can no longer see"
	)
	if allow {
		err = p.settings.AddArchiveViewerRole(ctx, i.GuildID, roleID)
		action, verb = "rotation.archive_role_allowed", "can now see"
	} else {
		err = p.settings.RemoveArchiveViewerRole(ctx, i.GuildID, roleID)
	}
	if err != nil {
		if followUpErr := core.FollowUpErr(s, i, "Failed to save", err); followUpErr != nil {
			p.log.Error("rotation: archive-role follow-up failed", "guild", i.GuildID, "err", followUpErr)
		}
		return
	}
	p.auditConfigChange(ctx, i, action, "", "role="+core.MentionRole(roleID))

	var followUpErr error
	if syncErr := p.syncAllArchivePermissions(ctx, i.GuildID); syncErr != nil {
		// The setting is saved either way, and the hourly sweep retries the
		// sync, so this reports a partial result rather than a failure.
		followUpErr = core.FollowUpErr(s, i, "Saved, but the resync hit errors", syncErr)
	} else {
		followUpErr = core.FollowUpOK(s, i, "Archive access updated",
			fmt.Sprintf("<@&%s> %s this server's archived channels.%s", roleID, verb, archiveViewerList(p.settings.ArchiveViewerRoleIDs(i.GuildID))))
	}
	if followUpErr != nil {
		p.log.Error("rotation: archive-role follow-up failed", "guild", i.GuildID, "err", followUpErr)
	}
}

// archiveViewerList renders the guild's current extra archive viewer roles as
// a trailing line, empty when there are none. Mod roles and Discord
// administrators are deliberately not listed: they always have access, and
// repeating that on every edit buries the part that actually changed.
func archiveViewerList(roleIDs []string) string {
	if len(roleIDs) == 0 {
		return "\n\nNo extra roles configured: mods and administrators only."
	}
	mentions := make([]string, len(roleIDs))
	for idx, id := range roleIDs {
		mentions[idx] = core.MentionRole(id)
	}
	return "\n\nExtra roles with archive access: " + strings.Join(mentions, ", ")
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

// disclosureOpt is the shared "how much does the channel get told" option.
//
// Fixed Choices rather than autocomplete: spec.MD §4a's autocomplete rule is
// about options whose valid values come from bot state (job keys, action
// names), which cannot be enumerated at compile time. This is a closed
// four-value enum, so Discord can render the whole set as a picker and there
// is nothing to discover.
func disclosureOpt() *discordgo.ApplicationCommandOption {
	return &discordgo.ApplicationCommandOption{
		Type:        discordgo.ApplicationCommandOptionString,
		Name:        "disclosure",
		Description: "How much the channel is told about its own rotation (default: everything)",
		Choices: []*discordgo.ApplicationCommandOptionChoice{
			{Name: "Everything (rotation schedule + archive window)", Value: string(settings.DisclosureFull)},
			{Name: "Rotation schedule only", Value: string(settings.DisclosureCadence)},
			{Name: "Archive window only", Value: string(settings.DisclosureRetention)},
			{Name: "Just that it rotated", Value: string(settings.DisclosureGeneric)},
		},
	}
}

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
