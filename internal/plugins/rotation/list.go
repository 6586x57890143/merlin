package rotation

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/6586x57890143/merlin/internal/core"
	"github.com/6586x57890143/merlin/internal/scheduler"
	"github.com/6586x57890143/merlin/internal/settings"
)

// Component CustomID prefixes for /rotation list's pagination, its
// channel-picker select menu, and the "back to list" button on the detail
// view it drills into (core.CommandRouter.HandleComponent, spec.MD §4a).
// The select/back prefixes carry the originating page number after them
// (core.PaginationCustomID/ParsePaginationPage) purely so "back" returns to
// the same page the pick was made from, not always page 0.
const (
	rotationListComponentPrefix = "rotation:list:page:"
	rotationListSelectPrefix    = "rotation:list:select:"
	rotationListBackPrefix      = "rotation:list:back:"
)

func (p *Plugin) handleList(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate) {
	channels := p.settings.RotationChannels(i.GuildID)
	if len(channels) == 0 {
		core.RespondInfo(s, i, "Rotating channels", "No channels are configured to rotate in this server. Use `/rotation configure add` to start.")
		return
	}
	embed, components := p.renderListPage(i.GuildID, channels, 0)
	if err := core.RespondEmbedWithComponents(s, i, embed, components); err != nil {
		p.log.Error("rotation: list response failed", "err", err)
	}
}

// handleListPage re-renders handleList's embed for the page encoded in a
// Prev/Next button's CustomID and edits the message in place.
func (p *Plugin) handleListPage(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate, customID string) {
	page, err := core.ParsePaginationPage(customID, rotationListComponentPrefix)
	if err != nil {
		p.log.Error("rotation: parse pagination page", "custom_id", customID, "err", err)
		page = 0
	}
	embed, components := p.renderListPage(i.GuildID, p.settings.RotationChannels(i.GuildID), page)
	if err := core.UpdateEmbedWithComponents(s, i, embed, components); err != nil {
		p.log.Error("rotation: list page update failed", "err", err)
	}
}

// handleListSelect drills into full detail for whichever channel the mod
// picked from the list's select menu. The compact per-page summary line
// doesn't have room for sticky message content or a spelled-out visibility
// policy, so this is a genuinely useful expansion rather than pagination's
// "same data, more of it."
func (p *Plugin) handleListSelect(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate, customID string) {
	page, err := core.ParsePaginationPage(customID, rotationListSelectPrefix)
	if err != nil {
		page = 0
	}
	values := i.MessageComponentData().Values
	if len(values) == 0 {
		return
	}
	rc, exists := p.settings.RotationChannel(i.GuildID, values[0])
	if !exists {
		core.RespondErr(s, i, "No longer configured", fmt.Errorf("that channel isn't configured to rotate anymore. The list may be stale, try `/rotation list` again"))
		return
	}
	backRow := []discordgo.MessageComponent{discordgo.ActionsRow{Components: []discordgo.MessageComponent{
		discordgo.Button{
			Label:    "◀ Back to list",
			Style:    discordgo.SecondaryButton,
			CustomID: core.PaginationCustomID(rotationListBackPrefix, page),
		},
	}}}
	if err := core.UpdateEmbedWithComponents(s, i, p.renderRotationDetailEmbed(ctx, rc), backRow); err != nil {
		p.log.Error("rotation: list detail update failed", "err", err)
	}
}

// handleListBack returns from a detail view to the list page the pick was
// made from, re-queried fresh, same as every other component handler in
// this bot, since nothing survives in memory between interactions.
func (p *Plugin) handleListBack(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate, customID string) {
	page, err := core.ParsePaginationPage(customID, rotationListBackPrefix)
	if err != nil {
		page = 0
	}
	embed, components := p.renderListPage(i.GuildID, p.settings.RotationChannels(i.GuildID), page)
	if err := core.UpdateEmbedWithComponents(s, i, embed, components); err != nil {
		p.log.Error("rotation: list back update failed", "err", err)
	}
}

// renderListPage builds one page of the summary list plus its controls: a
// select menu offering exactly this page's channels for drill-down, and
// Prev/Next buttons if more than one page exists at all.
//
// The select menu's options need real channel names (a mention renders as
// raw text inside a select), which costs one guild-channel listing: a
// single REST call for the whole page rather than one per row. Per-row
// lookups blew the interaction's 3-second response budget on a page of ten,
// and rotating channels change names on every rotation, so there's nothing
// worth caching between renders either.
func (p *Plugin) renderListPage(guildID string, channels []settings.RotationChannel, page int) (*discordgo.MessageEmbed, []discordgo.MessageComponent) {
	pageChannels, clampedPage, totalPages := core.Paginate(channels, page)

	names := make(map[string]string)
	if guildChannels, err := p.ops(guildID).GuildChannels(guildID); err == nil {
		for _, ch := range guildChannels {
			names[ch.ID] = ch.Name
		}
	} else {
		p.log.Error("rotation: list channel names unavailable, falling back to IDs", "guild", guildID, "err", err)
	}

	fields := make([]*discordgo.MessageEmbedField, 0, len(pageChannels))
	options := make([]discordgo.SelectMenuOption, 0, len(pageChannels))
	for _, rc := range pageChannels {
		retention := "forever"
		if rc.RetentionHours != nil {
			retention = humanDuration(time.Duration(*rc.RetentionHours) * time.Hour)
		}
		fields = append(fields, &discordgo.MessageEmbedField{
			Name: fmt.Sprintf("<#%s>", rc.ChannelID),
			Value: fmt.Sprintf("Every %s · archive <#%s> (%s) · retention %s · sticky %v · discloses %s",
				humanDuration(time.Duration(rc.IntervalMinutes)*time.Minute), rc.ArchiveCategoryID,
				visibilityLabel(rc.ArchiveVisibility), retention, rc.StickyEnabled, disclosureLabel(rc.Disclosure)),
		})

		label := rc.ChannelID
		if name, ok := names[rc.ChannelID]; ok {
			label = name
		}
		options = append(options, discordgo.SelectMenuOption{Label: "#" + label, Value: rc.ChannelID})
	}

	var components []discordgo.MessageComponent
	if len(options) > 0 {
		components = append(components, discordgo.ActionsRow{Components: []discordgo.MessageComponent{
			discordgo.SelectMenu{
				MenuType:    discordgo.StringSelectMenu,
				CustomID:    core.PaginationCustomID(rotationListSelectPrefix, clampedPage),
				Placeholder: "View details for a rotating channel...",
				Options:     options,
			},
		}})
	}
	if row := core.PaginationRow(rotationListComponentPrefix, clampedPage, totalPages); row != nil {
		components = append(components, row...)
	}

	return core.NewEmbed(core.ColorInfo, "Rotating channels", "", fields...), components
}

// renderRotationDetailEmbed is the one place an admin can see when a channel
// actually rotates next, and whether its heads-up is armed.
//
// Both were missing, and their absence was the whole reason a heads-up that
// posted nothing had no explanation available anywhere in Discord: the notice
// job stays quiet whenever the rotation is further out than the lead or the
// lead is off, which are the two most common states a channel is ever in, and
// neither was visible. The next-due read costs one scheduler-state query for a
// single channel, well inside the interaction budget, and it is deliberately
// read from the Scheduler rather than recomputed from the interval so this
// screen and the rotation itself cannot disagree.
func (p *Plugin) renderRotationDetailEmbed(ctx context.Context, rc settings.RotationChannel) *discordgo.MessageEmbed {
	retention := "forever"
	if rc.RetentionHours != nil {
		retention = humanDuration(time.Duration(*rc.RetentionHours) * time.Hour)
	}

	headsUp := "off"
	if rc.NoticeLeadMinutes > 0 {
		headsUp = humanDuration(time.Duration(rc.NoticeLeadMinutes)*time.Minute) + " before"
	}

	fields := []*discordgo.MessageEmbedField{
		{Name: "Interval", Value: humanDuration(time.Duration(rc.IntervalMinutes) * time.Minute), Inline: true},
		{Name: "Next rotation", Value: p.nextRotationText(ctx, rc), Inline: true},
		{Name: "Heads-up", Value: headsUp, Inline: true},
		{Name: "Retention", Value: retention, Inline: true},
		{Name: "Archive category", Value: fmt.Sprintf("<#%s>", rc.ArchiveCategoryID), Inline: true},
		{Name: "Archive visibility", Value: visibilityLabel(rc.ArchiveVisibility), Inline: true},
		{Name: "Discloses", Value: disclosureLabel(rc.Disclosure), Inline: true},
		{Name: "Sticky", Value: fmt.Sprintf("%v", rc.StickyEnabled), Inline: true},
	}
	if rc.StickyEnabled && len(rc.StickyMessages) > 0 {
		fields = append(fields, &discordgo.MessageEmbedField{
			Name: "Sticky messages", Value: core.TruncateEmbedField(strings.Join(rc.StickyMessages, "\n")),
		})
	}
	return core.NewEmbed(core.ColorInfo, fmt.Sprintf("<#%s>", rc.ChannelID), "", fields...)
}

// disclosureLabel renders a disclosure mode the way an admin reading a list
// thinks about it: what the channel gets told, not the stored enum.
func disclosureLabel(d settings.Disclosure) string {
	switch d.Resolve() {
	case settings.DisclosureCadence:
		return "rotation schedule only"
	case settings.DisclosureRetention:
		return "archive window only"
	case settings.DisclosureGeneric:
		return "just that it rotated"
	default:
		return "schedule + archive window"
	}
}

// visibilityLabel marks the legacy whitelist mode as legacy. It is no longer
// offered by /rotation configure, so an admin who finds one on a slot
// imported from pre-Milestone-4 YAML has no other way to learn where it came
// from or why they cannot select it.
func visibilityLabel(v string) string {
	if v == "whitelist" {
		return "mods + whitelist (legacy)"
	}
	return v
}

// nextRotationText renders the countdown to rc's next rotation, spelling out
// the two cases where there is no countdown to give rather than leaving the
// field blank.
//
// "due now" covers a job that has never run as well as one running late after
// a failure. They are worth saying out loud because they are also exactly when
// the heads-up deliberately stays silent: an overdue rotation fires on the next
// tick, and counting down to it would be wrong in the one direction that
// matters.
func (p *Plugin) nextRotationText(ctx context.Context, rc settings.RotationChannel) string {
	due, ok, err := p.sched.NextDue(ctx, scheduler.JobKey(rc.GuildID, rotationJobName(rc.ID)))
	if err != nil {
		p.log.Error("rotation: read next due for detail view", "guild", rc.GuildID, "channel", rc.ChannelID, "err", err)
		return "unknown"
	}
	if !ok {
		return "due now"
	}
	remaining := due.Sub(p.now())
	if remaining <= 0 {
		return "due now"
	}
	return "in " + humanDuration(remaining.Round(time.Minute))
}
