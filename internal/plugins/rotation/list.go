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
// picked from the list's select menu — the compact per-page summary line
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
		core.RespondErr(s, i, "No longer configured", fmt.Errorf("that channel isn't configured to rotate anymore — the list may be stale, try `/rotation list` again"))
		return
	}
	backRow := []discordgo.MessageComponent{discordgo.ActionsRow{Components: []discordgo.MessageComponent{
		discordgo.Button{
			Label:    "◀ Back to list",
			Style:    discordgo.SecondaryButton,
			CustomID: core.PaginationCustomID(rotationListBackPrefix, page),
		},
	}}}
	if err := core.UpdateEmbedWithComponents(s, i, renderRotationDetailEmbed(rc), backRow); err != nil {
		p.log.Error("rotation: list detail update failed", "err", err)
	}
}

// handleListBack returns from a detail view to the list page the pick was
// made from — re-queried fresh, same as every other component handler in
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
// raw text inside a select), which costs one guild-channel listing — a
// single REST call for the whole page rather than one per row. Per-row
// lookups blew the interaction's 3-second response budget on a page of ten,
// and rotating channels change names on every rotation, so there's nothing
// worth caching between renders either.
func (p *Plugin) renderListPage(guildID string, channels []settings.RotationChannel, page int) (*discordgo.MessageEmbed, []discordgo.MessageComponent) {
	pageChannels, clampedPage, totalPages := core.Paginate(channels, page)

	names := make(map[string]string)
	if guildChannels, err := p.ops.GuildChannels(guildID); err == nil {
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
			Value: fmt.Sprintf("Every %s · archive <#%s> (%s) · retention %s · sticky %v",
				humanDuration(time.Duration(rc.IntervalHours)*time.Hour), rc.ArchiveCategoryID, rc.ArchiveVisibility, retention, rc.StickyEnabled),
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

func renderRotationDetailEmbed(rc settings.RotationChannel) *discordgo.MessageEmbed {
	retention := "forever"
	if rc.RetentionHours != nil {
		retention = humanDuration(time.Duration(*rc.RetentionHours) * time.Hour)
	}
	fields := []*discordgo.MessageEmbedField{
		{Name: "Interval", Value: humanDuration(time.Duration(rc.IntervalHours) * time.Hour), Inline: true},
		{Name: "Retention", Value: retention, Inline: true},
		{Name: "Archive category", Value: fmt.Sprintf("<#%s>", rc.ArchiveCategoryID), Inline: true},
		{Name: "Archive visibility", Value: rc.ArchiveVisibility, Inline: true},
		{Name: "Sticky", Value: fmt.Sprintf("%v", rc.StickyEnabled), Inline: true},
	}
	if rc.StickyEnabled && len(rc.StickyMessages) > 0 {
		fields = append(fields, &discordgo.MessageEmbedField{Name: "Sticky messages", Value: strings.Join(rc.StickyMessages, "\n")})
	}
	return core.NewEmbed(core.ColorInfo, fmt.Sprintf("<#%s>", rc.ChannelID), "", fields...)
}
