package rotation

import (
	"context"
	"fmt"
	"strings"

	"github.com/bwmarrin/discordgo"

	"github.com/6586x57890143/merlin/internal/core"
	"github.com/6586x57890143/merlin/internal/settings"
)

// Action namespaces for whitelist grants (core.PermSpec.Action). Adding or
// removing a rotating channel is structural and stays Admin-tier; adjusting
// an already-configured channel's interval/retention/visibility/sticky
// content is Mod-tier — kept as two separate actions (not one shared
// "rotation.configure") so an admin can whitelist a trusted non-mod for
// day-to-day adjustments without also handing them the ability to add or
// remove rotating channels entirely.
const (
	actionStructural = "rotation.configure_structural"
	actionAdjust     = "rotation.configure"
	actionList       = "rotation.list"
)

func (p *Plugin) registerCommands() {
	channelOpt := func(name, desc string) *discordgo.ApplicationCommandOption {
		return &discordgo.ApplicationCommandOption{
			Type:        discordgo.ApplicationCommandOptionChannel,
			Name:        name,
			Description: desc,
			Required:    true,
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
							{
								Type:         discordgo.ApplicationCommandOptionChannel,
								Name:         "archive_category",
								Description:  "Hidden category archived channels move into",
								Required:     true,
								ChannelTypes: []discordgo.ChannelType{discordgo.ChannelTypeGuildCategory},
							},
							{
								Type:        discordgo.ApplicationCommandOptionInteger,
								Name:        "interval_hours",
								Description: "How often it rotates, in hours",
								Required:    true,
								MinValue:    float64Ptr(1),
							},
							{
								Type:        discordgo.ApplicationCommandOptionInteger,
								Name:        "retention_days",
								Description: "Days to keep the archive before permanent deletion. Omit to keep forever.",
								MinValue:    float64Ptr(1),
							},
							{
								Type:        discordgo.ApplicationCommandOptionString,
								Name:        "visibility",
								Description: "Who can see the archive besides mods (default: mods only)",
								Choices: []*discordgo.ApplicationCommandOptionChoice{
									{Name: "Mods only", Value: "mod_only"},
									{Name: "Mods + whitelist", Value: "whitelist"},
								},
							},
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
							{
								Type:        discordgo.ApplicationCommandOptionInteger,
								Name:        "interval_hours",
								Description: "New rotation interval, in hours",
								MinValue:    float64Ptr(1),
							},
							{
								Type:        discordgo.ApplicationCommandOptionInteger,
								Name:        "retention_days",
								Description: "New minimum retention, in days",
								MinValue:    float64Ptr(1),
							},
							{
								Type:        discordgo.ApplicationCommandOptionBoolean,
								Name:        "retention_forever",
								Description: "Set true to keep archives forever instead of a fixed retention",
							},
							{
								Type:        discordgo.ApplicationCommandOptionString,
								Name:        "visibility",
								Description: "Who can see the archive besides mods",
								Choices: []*discordgo.ApplicationCommandOptionChoice{
									{Name: "Mods only", Value: "mod_only"},
									{Name: "Mods + whitelist", Value: "whitelist"},
								},
							},
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
								Description: "One message per line — replaces the current set. Omit to leave unchanged.",
							},
						},
					},
				},
			},
		},
	}

	p.commands.RegisterCommand(cmd)
	p.commands.Handle("rotation", "list", core.PermSpec{Tier: core.TierMod, Action: actionList}, p.handleList)
	p.commands.Handle("rotation", "configure/add", core.PermSpec{Tier: core.TierAdmin, Action: actionStructural}, p.handleAdd)
	p.commands.Handle("rotation", "configure/remove", core.PermSpec{Tier: core.TierAdmin, Action: actionStructural}, p.handleRemove)
	p.commands.Handle("rotation", "configure/edit", core.PermSpec{Tier: core.TierMod, Action: actionAdjust}, p.handleEdit)
	p.commands.Handle("rotation", "configure/sticky", core.PermSpec{Tier: core.TierMod, Action: actionAdjust}, p.handleSticky)
}

func (p *Plugin) handleList(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate) {
	channels := p.settings.RotationChannels(i.GuildID)
	if len(channels) == 0 {
		respond(s, i, "No channels are configured to rotate in this server. Use `/rotation configure add` to start.")
		return
	}
	var b strings.Builder
	b.WriteString("**Rotating channels**\n")
	for _, rc := range channels {
		retention := "forever"
		if rc.RetentionDays != nil {
			retention = fmt.Sprintf("%d days", *rc.RetentionDays)
		}
		fmt.Fprintf(&b, "- <#%s> — every %dh, archive: <#%s> (%s), retention: %s, sticky: %v\n",
			rc.ChannelID, rc.IntervalHours, rc.ArchiveCategoryID, rc.ArchiveVisibility, retention, rc.StickyEnabled)
	}
	respond(s, i, b.String())
}

func (p *Plugin) handleAdd(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate) {
	opts := core.LeafArgs(i)

	channelID, ok := opts["channel"]
	if !ok {
		respond(s, i, "Missing channel.")
		return
	}
	rc := settings.RotationChannel{
		GuildID:           i.GuildID,
		ChannelID:         channelID.Value.(string),
		ArchiveCategoryID: opts["archive_category"].Value.(string),
		IntervalHours:     int(opts["interval_hours"].IntValue()),
		ArchiveVisibility: "mod_only",
	}
	if v, ok := opts["visibility"]; ok {
		rc.ArchiveVisibility = v.StringValue()
	}
	if v, ok := opts["retention_days"]; ok {
		days := int(v.IntValue())
		rc.RetentionDays = &days
	}

	if err := validateRotationChannel(rc); err != nil {
		respond(s, i, "Invalid configuration: "+err.Error())
		return
	}
	if _, exists := p.settings.RotationChannel(i.GuildID, rc.ChannelID); exists {
		respond(s, i, "That channel is already configured to rotate — use `/rotation configure edit` instead.")
		return
	}

	if err := p.settings.UpsertRotationChannel(ctx, rc); err != nil {
		respond(s, i, fmt.Sprintf("Failed to save: %v", err))
		return
	}
	p.SyncGuild(i.GuildID)
	p.auditConfigChange(ctx, i, "rotation.add", "", fmt.Sprintf("channel=<#%s> interval=%dh", rc.ChannelID, rc.IntervalHours))
	respond(s, i, fmt.Sprintf("<#%s> will now rotate every %d hours.", rc.ChannelID, rc.IntervalHours))
}

func (p *Plugin) handleRemove(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate) {
	opts := core.LeafArgs(i)
	channelID := opts["channel"].Value.(string)

	if _, exists := p.settings.RotationChannel(i.GuildID, channelID); !exists {
		respond(s, i, "That channel isn't configured to rotate.")
		return
	}
	if err := p.settings.RemoveRotationChannel(ctx, i.GuildID, channelID); err != nil {
		respond(s, i, fmt.Sprintf("Failed to remove: %v", err))
		return
	}
	p.SyncGuild(i.GuildID)
	p.auditConfigChange(ctx, i, "rotation.remove", fmt.Sprintf("channel=<#%s>", channelID), "")
	respond(s, i, fmt.Sprintf("<#%s> will no longer rotate. Any existing archive is untouched.", channelID))
}

func (p *Plugin) handleEdit(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate) {
	opts := core.LeafArgs(i)
	channelID := opts["channel"].Value.(string)

	rc, exists := p.settings.RotationChannel(i.GuildID, channelID)
	if !exists {
		respond(s, i, "That channel isn't configured to rotate — use `/rotation configure add` first.")
		return
	}
	before := fmt.Sprintf("interval=%dh retention=%v visibility=%s", rc.IntervalHours, rc.RetentionDays, rc.ArchiveVisibility)

	if v, ok := opts["interval_hours"]; ok {
		rc.IntervalHours = int(v.IntValue())
	}
	if v, ok := opts["retention_forever"]; ok && v.BoolValue() {
		rc.RetentionDays = nil
	} else if v, ok := opts["retention_days"]; ok {
		days := int(v.IntValue())
		rc.RetentionDays = &days
	}
	if v, ok := opts["visibility"]; ok {
		rc.ArchiveVisibility = v.StringValue()
	}

	if err := validateRotationChannel(rc); err != nil {
		respond(s, i, "Invalid configuration: "+err.Error())
		return
	}
	if err := p.settings.UpsertRotationChannel(ctx, rc); err != nil {
		respond(s, i, fmt.Sprintf("Failed to save: %v", err))
		return
	}
	p.SyncGuild(i.GuildID)
	after := fmt.Sprintf("interval=%dh retention=%v visibility=%s", rc.IntervalHours, rc.RetentionDays, rc.ArchiveVisibility)
	p.auditConfigChange(ctx, i, "rotation.edit", before, after)
	respond(s, i, fmt.Sprintf("Updated <#%s>.", channelID))
}

func (p *Plugin) handleSticky(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate) {
	opts := core.LeafArgs(i)
	channelID := opts["channel"].Value.(string)

	rc, exists := p.settings.RotationChannel(i.GuildID, channelID)
	if !exists {
		respond(s, i, "That channel isn't configured to rotate — use `/rotation configure add` first.")
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
		respond(s, i, "Sticky is enabled but there are no messages set — pass `messages` at least once.")
		return
	}

	if err := p.settings.UpsertRotationChannel(ctx, rc); err != nil {
		respond(s, i, fmt.Sprintf("Failed to save: %v", err))
		return
	}
	p.auditConfigChange(ctx, i, "rotation.sticky", "", fmt.Sprintf("channel=<#%s> enabled=%v messages=%d", channelID, rc.StickyEnabled, len(rc.StickyMessages)))
	respond(s, i, fmt.Sprintf("Sticky settings for <#%s> updated.", channelID))
}

func (p *Plugin) auditConfigChange(ctx context.Context, i *discordgo.InteractionCreate, action, oldValue, newValue string) {
	if err := p.audit.Record(ctx, i.GuildID, i.Member.User.ID, action, oldValue, newValue); err != nil {
		p.log.Error("rotation: audit record failed", "action", action, "err", err)
	}
}

// validateRotationChannel mirrors the checks the old YAML-loader-time
// validation used to perform (internal/config/rotation_validate.go, removed
// with the move to DB-backed settings) — now enforced at the point of
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
	if rc.RetentionDays != nil && *rc.RetentionDays < 1 {
		return fmt.Errorf("retention_days must be at least 1 if set (omit it entirely for forever)")
	}
	if rc.IntervalHours < 1 {
		return fmt.Errorf("interval_hours must be at least 1")
	}
	return nil
}

func float64Ptr(f float64) *float64 { return &f }

func respond(s *discordgo.Session, i *discordgo.InteractionCreate, msg string) {
	_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Content: msg,
			Flags:   discordgo.MessageFlagsEphemeral,
		},
	})
}
