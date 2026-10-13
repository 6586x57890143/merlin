package rotation

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/disgoorg/disgo/discord"

	"github.com/6586x57890143/merlin/internal/core"
	"github.com/6586x57890143/merlin/internal/core/ui"
	"github.com/6586x57890143/merlin/internal/settings"
)

// /rotation is one panel (see core.Panel). It replaced list and seven
// configure subcommands. Every control keeps the action its subcommand had,
// so a guild's tier overrides and allows still apply: adding, removing and
// who reads the archive are structural, everything about a channel already
// rotating is an adjustment, and looking is list.
const (
	panelView   = "rotation:panel:view:"
	panelStruct = "rotation:panel:struct:"
	panelAdjust = "rotation:panel:adjust:"
	formStruct  = "rotation:form:struct:"
	formAdjust  = "rotation:form:adjust:"
)

var (
	viewSpec   = core.PermSpec{Tier: core.TierMod, Action: actionList}
	structSpec = core.PermSpec{Tier: core.TierAdmin, Action: actionStructural}
	adjustSpec = core.PermSpec{Tier: core.TierAdmin, Action: actionAdjust}
)

func (p *Plugin) registerCommands() {
	p.commands.RegisterCommand(p.Name(), discord.SlashCommandCreate{
		Name:        "rotation",
		Description: "Rotating channels: which ones, how often, and how long archives are kept",
	})
	p.commands.HandleIx("rotation", "", viewSpec, p.handleOpen)
	p.commands.HandleComponentIx(p.Name(), panelView, viewSpec, p.handleView)
	p.commands.HandleComponentIx(p.Name(), panelStruct, structSpec, p.handleSet)
	p.commands.HandleComponentIx(p.Name(), panelAdjust, adjustSpec, p.handleSet)
	p.commands.HandleModalIx(p.Name(), formStruct, structSpec, p.handleForm)
	p.commands.HandleModalIx(p.Name(), formAdjust, adjustSpec, p.handleForm)
}

func (p *Plugin) handleOpen(ctx context.Context, ix *core.Ix) {
	if err := ix.RespondPanel(p.panel(ctx, ix, "", "")); err != nil {
		p.log.Error("rotation: panel response failed", "err", err)
	}
}

// handleView is the channel picker (rotation:panel:view:ch) and the back
// button (rotation:panel:view:home).
func (p *Plugin) handleView(ctx context.Context, ix *core.Ix, customID string) {
	channelID := ""
	if strings.TrimPrefix(customID, panelView) == "ch" {
		channelID = ix.FirstValue()
	}
	p.redraw(ctx, ix, channelID, "")
}

func (p *Plugin) redraw(ctx context.Context, ix *core.Ix, channelID, note string) {
	if err := ix.UpdatePanel(p.panel(ctx, ix, channelID, note)); err != nil {
		p.log.Error("rotation: panel update failed", "err", err)
	}
}

// panel is the list of rotating channels, or with channelID, that one
// channel's settings.
func (p *Plugin) panel(ctx context.Context, ix *core.Ix, channelID, note string) core.Panel {
	perms := p.commands.Perms()
	canStruct, canAdjust := perms.AllowsIx(ix, structSpec), perms.AllowsIx(ix, adjustSpec)
	pn := core.Panel{Color: core.ColorInfo, Title: "Rotation", Plugin: p.Name(), Note: note,
		Line:   "Channels that are replaced on a schedule, their old copy archived",
		Footer: "Every change is written to the audit log. Deleting an archive has no undo."}

	if channelID == "" {
		channels := p.settings.RotationChannels(ix.GuildID)
		list := "Nothing rotates here yet. Pick a channel below to start."
		if len(channels) > 0 {
			var b strings.Builder
			for _, rc := range channels {
				fmt.Fprintf(&b, "<#%s>\n-# every %s · kept %s · next %s\n", rc.ChannelID,
					humanDuration(time.Duration(rc.IntervalMinutes)*time.Minute), retentionText(rc.RetentionHours), p.nextRotationText(ctx, rc))
			}
			list = strings.TrimSuffix(b.String(), "\n")
		}
		pn.Parts = []discord.ContainerSubComponent{
			ui.Text(list),
			ui.Row(ui.ChannelSelect(panelView+"ch", "Pick a channel to rotate or edit", "", false, discord.ChannelTypeGuildText)),
			ui.Gap(),
			ui.Text("**Archive readers** can read every archived channel, on top of mods. Read-only: no posting, reactions or threads."),
			ui.Row(ui.RoleSelect(panelStruct+"readers", "Roles that can read archives", p.settings.ArchiveViewerRoleIDs(ix.GuildID), ui.MaxSelectValues, !canStruct)),
		}
		return pn
	}

	back := ui.Row(ui.Button("◀ All channels", panelView+"home", discord.ButtonStyleSecondary, false))
	rc, rotating := p.settings.RotationChannel(ix.GuildID, channelID)
	if !rotating {
		pn.Parts = []discord.ContainerSubComponent{
			ui.Text(fmt.Sprintf("<#%s> doesn't rotate. Starting it asks how often, how long archives are kept, and how long before each rotation the channel is warned.", channelID)),
			ui.Row(ui.Button("Start rotating", panelStruct+"add:"+channelID, discord.ButtonStylePrimary, !canStruct)),
			back,
		}
		return pn
	}

	headsUp := "off"
	if rc.NoticeLeadMinutes > 0 {
		headsUp = humanDuration(time.Duration(rc.NoticeLeadMinutes)*time.Minute) + " before"
	}
	sticky := "off"
	if rc.StickyEnabled {
		sticky = fmt.Sprintf("%d message(s)", len(rc.StickyMessages))
	}
	grid := core.Readout([][2]string{
		{"every", humanDuration(time.Duration(rc.IntervalMinutes) * time.Minute)},
		{"next", p.nextRotationText(ctx, rc)},
		{"heads-up", headsUp},
		{"kept", retentionText(rc.RetentionHours)},
		{"tells", disclosureLabel(rc.Disclosure)},
		{"sticky", sticky},
	})
	stickyBtn := ui.Button("Sticky on", panelAdjust+"stickyon:"+channelID, discord.ButtonStyleSecondary, !canAdjust || len(rc.StickyMessages) == 0)
	if rc.StickyEnabled {
		stickyBtn = ui.Button("Sticky off", panelAdjust+"stickyoff:"+channelID, discord.ButtonStyleSecondary, !canAdjust)
	}
	var disclosure []discord.StringSelectMenuOption
	for _, d := range []settings.Disclosure{settings.DisclosureFull, settings.DisclosureCadence, settings.DisclosureRetention, settings.DisclosureGeneric} {
		disclosure = append(disclosure, discord.StringSelectMenuOption{Label: "Tells " + disclosureLabel(d), Value: string(d), Default: d == rc.Disclosure.Resolve()})
	}
	pn.Parts = []discord.ContainerSubComponent{
		ui.Text(fmt.Sprintf("<#%s>\n%s\n-# Archived into <#%s> (%s).", channelID, grid, rc.ArchiveCategoryID, visibilityLabel(rc.ArchiveVisibility))),
		ui.Row(
			ui.Button("Edit timings", panelAdjust+"timing:"+channelID, discord.ButtonStylePrimary, !canAdjust),
			ui.Button("Sticky messages", panelAdjust+"sticky:"+channelID, discord.ButtonStyleSecondary, !canAdjust),
			stickyBtn,
			ui.Button("Stop rotating", panelStruct+"remove:"+channelID, discord.ButtonStyleDanger, !canStruct),
		),
		ui.Row(ui.StringSelect(panelAdjust+"disclosure:"+channelID, "What the channel is told", disclosure, false, !canAdjust)),
		ui.Row(ui.ChannelSelect(panelAdjust+"category:"+channelID, "Archive category", rc.ArchiveCategoryID, !canAdjust, discord.ChannelTypeGuildCategory)),
		back,
	}
	if note := genericDisclosureNote(rc); note != "" {
		pn.Parts = append([]discord.ContainerSubComponent{ui.Text("-# " + strings.TrimSpace(note))}, pn.Parts...)
	}
	return pn
}

func retentionText(hours *int) string {
	if hours == nil {
		return "forever"
	}
	return humanDuration(time.Duration(*hours) * time.Hour)
}

// handleSet is every button and select that changes something. The channel
// it acts on rides in the CustomID after the verb.
func (p *Plugin) handleSet(ctx context.Context, ix *core.Ix, customID string) {
	rest := strings.TrimPrefix(strings.TrimPrefix(customID, panelStruct), panelAdjust)
	verb, channelID, _ := strings.Cut(rest, ":")
	switch verb {
	case "readers":
		p.applyReaders(ctx, ix)
	case "add":
		p.openForm(ix, formStruct+"add:"+channelID, "Start rotating", settings.RotationChannel{IntervalMinutes: 24 * 60, NoticeLeadMinutes: defaultNoticeLeadMinutes}, false)
	case "timing":
		if rc, ok := p.settings.RotationChannel(ix.GuildID, channelID); ok {
			p.openForm(ix, formAdjust+"timing:"+channelID, "Timings", rc, false)
			return
		}
		p.redraw(ctx, ix, channelID, "✗ That channel isn't configured to rotate anymore.")
	case "sticky":
		if rc, ok := p.settings.RotationChannel(ix.GuildID, channelID); ok {
			p.openForm(ix, formAdjust+"sticky:"+channelID, "Sticky messages", rc, true)
			return
		}
		p.redraw(ctx, ix, channelID, "✗ That channel isn't configured to rotate anymore.")
	case "stickyon", "stickyoff":
		p.redraw(ctx, ix, channelID, p.editChannel(ctx, ix, channelID, "rotation.sticky", func(rc *settings.RotationChannel) error {
			rc.StickyEnabled = verb == "stickyon"
			if rc.StickyEnabled && len(rc.StickyMessages) == 0 {
				return fmt.Errorf("there are no sticky messages yet; set some first")
			}
			return nil
		}))
	case "disclosure":
		p.redraw(ctx, ix, channelID, p.editChannel(ctx, ix, channelID, "rotation.edit", func(rc *settings.RotationChannel) error {
			rc.Disclosure = settings.Disclosure(ix.FirstValue())
			return nil
		}))
	case "category":
		p.redraw(ctx, ix, channelID, p.editChannel(ctx, ix, channelID, "rotation.edit", func(rc *settings.RotationChannel) error {
			// Editable rather than add-only, because the alternative was
			// remove and re-add: that mints a fresh row ID, and the sweep
			// re-derives each archive's deadline through rotation_id
			// (migration 0013), so every existing archive would fall back to
			// its frozen delete_after.
			rc.ArchiveCategoryID = ix.FirstValue()
			p.hideArchiveCategory(ix.GuildID, rc.ArchiveCategoryID)
			return nil
		}))
	case "remove":
		// Two presses: stopping rotation leaves the archives, but undoing it
		// mints a new row, which detaches every existing archive from the
		// channel's live retention setting (see "category" above).
		pn := p.panel(ctx, ix, channelID, fmt.Sprintf("! Stop rotating <#%s>? Its archives stay where they are.", channelID))
		pn.Parts = append([]discord.ContainerSubComponent{ui.Row(ui.Button("Yes, stop rotating", panelStruct+"removeyes:"+channelID, discord.ButtonStyleDanger, false))}, pn.Parts...)
		if err := ix.UpdatePanel(pn); err != nil {
			p.log.Error("rotation: panel update failed", "err", err)
		}
	case "removeyes":
		p.redraw(ctx, ix, channelID, p.remove(ctx, ix, channelID))
	default:
		p.redraw(ctx, ix, "", "✗ That control is out of date. Run `/rotation` again.")
	}
}

// openForm asks for a channel's timings, or its sticky messages, prefilled
// with what it has now. A blank retention means forever, which is also how
// it reads when the form opens on a channel that keeps archives forever.
func (p *Plugin) openForm(ix *core.Ix, customID, title string, rc settings.RotationChannel, sticky bool) {
	fields := []discord.LayoutComponent{
		ui.Field("interval", "How often it rotates (at least 1h)", core.FormatDuration(time.Duration(rc.IntervalMinutes)*time.Minute), "3d, 24h or 90m", false, true),
		ui.Field("retention", "Keep archives this long, then DELETE them", retentionInput(rc.RetentionHours), "30d or 72h; leave blank to keep forever", false, false),
		ui.Field("notice", "Warn the channel this long before", formatNoticeLead(rc.NoticeLeadMinutes), "10m or 1h; off for no warning", false, true),
	}
	if sticky {
		fields = []discord.LayoutComponent{
			ui.Field("messages", "Reposted and pinned after each rotation", strings.Join(rc.StickyMessages, "\n"), "One message per line", true, false),
		}
	}
	if err := ix.OpenModal(customID, title, fields...); err != nil {
		p.log.Error("rotation: open form failed", "err", err)
	}
}

func retentionInput(hours *int) string {
	if hours == nil {
		return ""
	}
	return core.FormatDuration(time.Duration(*hours) * time.Hour)
}

// handleForm is the timings form, for a new channel or an existing one, and
// the sticky messages form.
func (p *Plugin) handleForm(ctx context.Context, ix *core.Ix, customID string) {
	rest := strings.TrimPrefix(strings.TrimPrefix(customID, formStruct), formAdjust)
	verb, channelID, _ := strings.Cut(rest, ":")
	v := ix.ModalValues()
	switch verb {
	case "add":
		// Finding or creating the archive category is REST work that can
		// outrun Discord's 3 seconds.
		if err := ix.DeferPanelUpdate(); err != nil {
			p.log.Error("rotation: defer failed", "err", err)
			return
		}
		note := p.add(ctx, ix, channelID, v)
		if err := ix.FollowUpPanel(p.panel(ctx, ix, channelID, note)); err != nil {
			p.log.Error("rotation: panel follow-up failed", "err", err)
		}
	case "timing":
		p.redraw(ctx, ix, channelID, p.editChannel(ctx, ix, channelID, "rotation.edit", func(rc *settings.RotationChannel) error {
			return applyTimings(rc, v)
		}))
	case "sticky":
		p.redraw(ctx, ix, channelID, p.editChannel(ctx, ix, channelID, "rotation.sticky", func(rc *settings.RotationChannel) error {
			var msgs []string
			for line := range strings.SplitSeq(v["messages"], "\n") {
				if t := strings.TrimSpace(line); t != "" {
					msgs = append(msgs, t)
				}
			}
			rc.StickyMessages = msgs
			rc.StickyEnabled = len(msgs) > 0
			return nil
		}))
	default:
		p.redraw(ctx, ix, "", "✗ That form is out of date. Run `/rotation` again.")
	}
}

// applyTimings reads the timings form onto rc. Every duration needs its
// unit: a bare "3" was once read as hours when days were meant, which cost a
// guild its archives early, and deletion has no undo.
func applyTimings(rc *settings.RotationChannel, v map[string]string) error {
	interval, err := core.ParseFlexibleDuration(v["interval"])
	if err != nil {
		return fmt.Errorf("interval: %w", err)
	}
	rc.IntervalMinutes = int(interval / time.Minute)
	rc.RetentionHours = nil
	if r := strings.TrimSpace(v["retention"]); r != "" {
		retention, err := core.ParseFlexibleDuration(r)
		if err != nil {
			return fmt.Errorf("retention: %w", err)
		}
		hours := int(retention / time.Hour)
		rc.RetentionHours = &hours
	}
	if rc.NoticeLeadMinutes, err = parseNoticeLead(v["notice"]); err != nil {
		return fmt.Errorf("heads-up: %w", err)
	}
	return nil
}

func (p *Plugin) add(ctx context.Context, ix *core.Ix, channelID string, v map[string]string) string {
	if _, exists := p.settings.RotationChannel(ix.GuildID, channelID); exists {
		return "✗ That channel already rotates."
	}
	rc := settings.RotationChannel{GuildID: ix.GuildID, ChannelID: channelID, ArchiveVisibility: "mod_only", Disclosure: settings.DisclosureFull}
	if err := applyTimings(&rc, v); err != nil {
		return "✗ " + err.Error()
	}
	if err := ValidateChannel(rc); err != nil {
		return "✗ " + err.Error()
	}
	category, err := p.resolveArchiveCategory(ix.GuildID, "")
	if err != nil {
		return fmt.Sprintf("✗ Couldn't find or create the archive category: %v", err)
	}
	rc.ArchiveCategoryID = category
	if err := p.settings.UpsertRotationChannel(ctx, rc); err != nil {
		return fmt.Sprintf("✗ Couldn't save: %v", err)
	}
	// Saving already published EventConfigChanged, which reconciled and
	// registered the job with no run history, so it would fire on the next
	// tick. A channel somebody just added waits a full interval instead.
	p.deferFirstRotation(ctx, ix.GuildID, channelID)
	p.auditConfigChange(ctx, ix, "rotation.add", "", fmt.Sprintf("channel=<#%s> %s", channelID, rotationSummary(rc)))
	return fmt.Sprintf("✓ <#%s> rotates every %s.", channelID, humanDuration(time.Duration(rc.IntervalMinutes)*time.Minute))
}

// editChannel is a read-modify-write of one channel's settings, validated as
// a whole, audited with the before and after the subcommands wrote.
func (p *Plugin) editChannel(ctx context.Context, ix *core.Ix, channelID, action string, change func(*settings.RotationChannel) error) string {
	rc, exists := p.settings.RotationChannel(ix.GuildID, channelID)
	if !exists {
		return "✗ That channel isn't configured to rotate anymore."
	}
	before := rotationSummary(rc)
	if err := change(&rc); err != nil {
		return "✗ " + err.Error()
	}
	if err := ValidateChannel(rc); err != nil {
		return "✗ " + err.Error()
	}
	if err := p.settings.UpsertRotationChannel(ctx, rc); err != nil {
		return fmt.Sprintf("✗ Couldn't save: %v", err)
	}
	if action == "rotation.sticky" {
		p.auditConfigChange(ctx, ix, action, "", fmt.Sprintf("channel=<#%s> enabled=%v messages=%d", channelID, rc.StickyEnabled, len(rc.StickyMessages)))
	} else {
		p.auditConfigChange(ctx, ix, action, before, rotationSummary(rc))
	}
	return "✓ Saved." + strings.ReplaceAll(genericDisclosureNote(rc), "\n\n", " ")
}

func (p *Plugin) remove(ctx context.Context, ix *core.Ix, channelID string) string {
	if _, exists := p.settings.RotationChannel(ix.GuildID, channelID); !exists {
		return "✗ That channel isn't configured to rotate."
	}
	if err := p.settings.RemoveRotationChannel(ctx, ix.GuildID, channelID); err != nil {
		return fmt.Sprintf("✗ Couldn't remove: %v", err)
	}
	p.auditConfigChange(ctx, ix, "rotation.remove", fmt.Sprintf("channel=<#%s>", channelID), "")
	return fmt.Sprintf("✓ <#%s> no longer rotates. Its archives are untouched.", channelID)
}

// applyReaders saves who may read archives, then resyncs every archive
// category so the change shows at once rather than at the next rotation.
// The resync walks the guild's channels, so the click is deferred first.
func (p *Plugin) applyReaders(ctx context.Context, ix *core.Ix) {
	added, removed := ix.SelectDiff(panelStruct + "readers")
	if err := ix.DeferPanelUpdate(); err != nil {
		p.log.Error("rotation: defer failed", "err", err)
		return
	}
	note := ""
	for _, id := range added {
		if err := p.settings.AddArchiveViewerRole(ctx, ix.GuildID, id); err != nil {
			note = fmt.Sprintf("✗ Couldn't save: %v", err)
			break
		}
		p.auditConfigChange(ctx, ix, "rotation.archive_role_allowed", "", "role="+core.MentionRole(id))
	}
	for _, id := range removed {
		if note != "" {
			break
		}
		if err := p.settings.RemoveArchiveViewerRole(ctx, ix.GuildID, id); err != nil {
			note = fmt.Sprintf("✗ Couldn't save: %v", err)
			break
		}
		p.auditConfigChange(ctx, ix, "rotation.archive_role_removed", "", "role="+core.MentionRole(id))
	}
	if note == "" && len(added)+len(removed) > 0 {
		note = "✓ Archive readers updated."
		if err := p.syncAllArchivePermissions(ctx, ix.GuildID); err != nil {
			// Saved either way, and the hourly sweep retries the sync.
			note = fmt.Sprintf("! Saved, but the resync hit errors; the hourly sweep will retry: %v", err)
		}
	}
	if err := ix.FollowUpPanel(p.panel(ctx, ix, "", note)); err != nil {
		p.log.Error("rotation: panel follow-up failed", "err", err)
	}
}

// Help is this plugin's page in /help.
func (p *Plugin) Help() core.Help {
	return core.Help{
		Title: "Rotation",
		Line:  "channels that start fresh on a schedule",
		About: "a rotating channel is replaced by a fresh copy every so often, and the old one is archived where only mods can read it, then deleted once its retention runs out. the channel is told when it rotates and how long the archive lasts, and gets a heads-up before it happens. admins set it all up on `/rotation`: pick a channel, start it, and change its timings, sticky messages and archive whenever.",
	}
}
