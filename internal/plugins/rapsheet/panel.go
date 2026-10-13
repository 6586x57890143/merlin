package rapsheet

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/disgoorg/disgo/discord"

	"github.com/6586x57890143/merlin/internal/core"
	"github.com/6586x57890143/merlin/internal/core/ui"
)

// /rapsheet settings is one panel (core.Panel) for what used to be the eight
// /rapsheet configure subcommands. Everything that records or reads an entry
// stays a command, and so does /rapsheet status, the health report. Every
// control is rapsheet.configure, as every configure leaf was.
const (
	panelView = "rapsheet:panel:view:"
	panelSet  = "rapsheet:panel:set:"
	formSet   = "rapsheet:form:set:"
)

var (
	viewSpec      = core.PermSpec{Tier: core.TierMod, Action: actionView}
	configureSpec = core.PermSpec{Tier: core.TierAdmin, Action: actionConfigure}
)

func (p *Plugin) registerPanel() {
	p.commands.HandleComponentIx(p.Name(), panelView, viewSpec, p.handlePanelView)
	p.commands.HandleComponentIx(p.Name(), panelSet, configureSpec, p.handlePanelSet)
	p.commands.HandleModalIx(p.Name(), formSet, configureSpec, p.handleForm)
}

func (p *Plugin) handleSettings(ctx context.Context, ix *core.Ix) {
	if err := ix.RespondPanel(p.panel(ctx, ix, "", "")); err != nil {
		p.log.Error("rapsheet: panel response failed", "err", err)
	}
}

// handlePanelView is the category picker, which shows that category's
// points and the button that changes them.
func (p *Plugin) handlePanelView(ctx context.Context, ix *core.Ix, _ string) {
	p.redraw(ctx, ix, ix.FirstValue(), "")
}

func (p *Plugin) redraw(ctx context.Context, ix *core.Ix, category, note string) {
	if err := ix.UpdatePanel(p.panel(ctx, ix, Category(category), note)); err != nil {
		p.log.Error("rapsheet: panel update failed", "err", err)
	}
}

func (p *Plugin) panel(ctx context.Context, ix *core.Ix, category Category, note string) core.Panel {
	can := p.commands.Perms().AllowsIx(ix, configureSpec)
	cfg := p.config(ctx, ix.GuildID)
	tuned := "none"
	if len(cfg.CategoryPoints) > 0 {
		var parts []string
		for _, c := range categories {
			if v, ok := cfg.CategoryPoints[c]; ok {
				parts = append(parts, fmt.Sprintf("%s=%d", c, v))
			}
		}
		tuned = strings.Join(parts, ", ")
	}
	bands := "defaults"
	if len(cfg.Bands) > 0 {
		bands = "custom"
	}
	modes := []discord.StringSelectMenuOption{
		{Label: "Ladder off", Value: string(ModeOff), Description: "Record and score; suggest and apply nothing"},
		{Label: "Ladder suggests", Value: string(ModeSuggest), Description: "Post to the mod channel with an Apply button"},
		{Label: "Ladder acts", Value: string(ModeAuto), Description: "Apply on automatic entries; never on staff"},
	}
	for n := range modes {
		modes[n].Default = Mode(modes[n].Value) == cfg.EscalationMode
	}
	alts := ui.Button("Turn off", panelSet+"alts:off", discord.ButtonStyleSecondary, !can)
	if !cfg.AltHints {
		alts = ui.Button("Turn on", panelSet+"alts:on", discord.ButtonStyleSecondary, !can)
	}
	var catOpts []discord.StringSelectMenuOption
	for _, c := range categories {
		catOpts = append(catOpts, discord.StringSelectMenuOption{Label: categoryLabel(c), Value: string(c), Default: c == category})
	}

	pn := core.Panel{Color: core.ColorInfo, Title: "Rapsheets", Plugin: p.Name(), Note: note,
		Line:   "Every warning, jail, timeout, kick, ban and AI removal, scored",
		Footer: "Recording stays commands: /rapsheet warn, /timeout, /kick, /ban. /rapsheet status checks the setup. Every change is audited."}
	pn.Parts = []discord.ContainerSubComponent{
		ui.Text(core.Readout([][2]string{
			{"ladder", string(cfg.EscalationMode)},
			{"half-life", core.FormatDuration(cfg.HalfLife)},
			{"bands", bands},
			{"tuned", tuned},
		})),
		ui.Row(ui.StringSelect(panelSet+"mode", "What the ladder does", modes, false, !can)),
		ui.Row(
			ui.Button("Half-life", panelSet+"halflife", discord.ButtonStyleSecondary, !can),
			ui.Button("Ladder bands", panelSet+"bands", discord.ButtonStyleSecondary, !can),
		),
		ui.Text("**Mod channel**: ladder suggestions and alt hints. Staff only: a suggestion names the member.\n-# Now: " + orUnset(core.MentionChannel(cfg.ModChannelID))),
		ui.Row(ui.ChannelSelect(panelSet+"modch", "The mod channel", cfg.ModChannelID, !can, discord.ChannelTypeGuildText)),
		ui.Text("**Case files**: a forum post per member on record.\n-# Now: " + orUnset(core.MentionChannel(cfg.ForumChannelID))),
		ui.Row(ui.ChannelSelect(panelSet+"forum", "An existing forum (permissions left as they are)", cfg.ForumChannelID, !can, discord.ChannelTypeGuildForum)),
		ui.Row(ui.Button("Create #"+caseFileForumName+" for me", panelSet+"forumnew", discord.ButtonStyleSecondary, !can)),
		ui.Switch(fmt.Sprintf("**Alt hints** are %s: joins that resemble a member on record are noted, never linked without a moderator.", onOff(cfg.AltHints)), alts),
		ui.Row(ui.StringSelect(panelView+"cat", "Points for a category", catOpts, false, false)),
	}
	if validCategory(category) {
		pn.Parts = append(pn.Parts, ui.Switch(
			fmt.Sprintf("**%s** is worth **%d** points here (default %d).", categoryLabel(category), pointsFor(cfg, KindWarn, category, "", 0), defaultPoints[category]),
			ui.Button("Change", panelSet+"points:"+string(category), discord.ButtonStyleSecondary, !can)))
	}
	return pn
}

// handlePanelSet applies one control. Creating the forum is REST work and
// defers; everything else answers at once, and the three that need typing
// open a form.
func (p *Plugin) handlePanelSet(ctx context.Context, ix *core.Ix, customID string) {
	verb, arg, _ := strings.Cut(strings.TrimPrefix(customID, panelSet), ":")
	cfg := p.config(ctx, ix.GuildID)
	switch verb {
	case "halflife":
		p.openForm(ix, formSet+"halflife", "Half-life",
			ui.Field("half_life", "Time for an entry to count half (1d to 365d)", core.FormatDuration(cfg.HalfLife), "30d", false, true))
		return
	case "bands":
		p.openForm(ix, formSet+"bands", "Ladder bands",
			ui.Field("ladder", "Which score owes what; never a permanent ban", bandsWords(cfg.Bands), "25 notice, 50 jail 2h, 100 jail 1d, 200 ban 7d, or default", true, true))
		return
	case "points":
		current := ""
		if v, ok := cfg.CategoryPoints[Category(arg)]; ok {
			current = strconv.Itoa(v)
		}
		p.openForm(ix, formSet+"points:"+arg, "Points: "+categoryLabel(Category(arg)),
			ui.Field("points", fmt.Sprintf("1-%d; empty goes back to the default", maxPoints), current, strconv.Itoa(defaultPoints[Category(arg)]), false, false))
		return
	case "forumnew", "forum":
		if err := ix.DeferPanelUpdate(); err != nil {
			p.log.Error("rapsheet: defer forum", "err", err)
			return
		}
		note := p.setForum(ctx, ix, ix.FirstValue())
		if err := ix.FollowUpPanel(p.panel(ctx, ix, "", note)); err != nil {
			p.log.Error("rapsheet: panel follow-up failed", "err", err)
		}
		return
	}
	var note string
	switch verb {
	case "mode":
		note = p.setMode(ctx, ix, Mode(ix.FirstValue()))
	case "modch":
		note = p.setModChannel(ctx, ix, ix.FirstValue())
	case "alts":
		note = p.setAltHints(ctx, ix, arg == "on")
	default:
		note = "✗ That control is out of date. Run `/rapsheet settings` again."
	}
	p.redraw(ctx, ix, "", note)
}

func (p *Plugin) openForm(ix *core.Ix, customID, title string, fields ...discord.LayoutComponent) {
	if err := ix.OpenModal(customID, title, fields...); err != nil {
		p.log.Error("rapsheet: open form failed", "err", err)
	}
}

func (p *Plugin) handleForm(ctx context.Context, ix *core.Ix, customID string) {
	verb, arg, _ := strings.Cut(strings.TrimPrefix(customID, formSet), ":")
	v := ix.ModalValues()
	var note string
	switch verb {
	case "halflife":
		note = p.setHalfLife(ctx, ix, v["half_life"])
	case "bands":
		note = p.setBands(ctx, ix, v["ladder"])
	case "points":
		note = p.setPoints(ctx, ix, Category(arg), v["points"])
	default:
		note = "✗ That form is out of date. Run `/rapsheet settings` again."
	}
	p.redraw(ctx, ix, arg, note)
}

// saveConfig writes cfg and audits the change as one line.
func (p *Plugin) saveConfig(ctx context.Context, ix *core.Ix, cfg Config, what, old, now string) error {
	if err := p.store.SetConfig(ctx, cfg); err != nil {
		return err
	}
	if err := p.audit.Record(ctx, ix.GuildID, actorID(ix), "rapsheet.configured", old, what+"="+now); err != nil {
		p.log.Error("rapsheet: audit configure", "guild", ix.GuildID, "err", err)
	}
	return nil
}

func (p *Plugin) setMode(ctx context.Context, ix *core.Ix, mode Mode) string {
	switch mode {
	case ModeOff, ModeSuggest, ModeAuto:
	default:
		return fmt.Sprintf("✗ Unknown mode %q.", mode)
	}
	cfg := p.config(ctx, ix.GuildID)
	old := cfg.EscalationMode
	cfg.EscalationMode = mode
	if err := p.saveConfig(ctx, ix, cfg, "mode", string(old), string(mode)); err != nil {
		return "✗ " + err.Error()
	}
	p.mu.Lock()
	p.reconcileReviewJob(ctx, ix.GuildID)
	p.mu.Unlock()
	msg := map[Mode]string{
		ModeOff:     "The ladder is off. Entries are recorded and scored; nothing is suggested or applied.",
		ModeSuggest: "When a record crosses a band, merlin posts the recommendation to the mod channel with an Apply button.",
		ModeAuto:    "When a record crosses a band on an automatic entry (aimod, or a ban done through Discord), merlin applies the consequence herself. A moderator's own command still gets a suggestion, never an automatic action on top of it. Staff are never actioned, and the ladder never bans permanently.",
	}[mode]
	if mode != ModeOff && cfg.ModChannelID == "" {
		return "! **Ladder: " + string(mode) + "** " + msg + "\nNo mod channel is set, so suggestions have nowhere to go: pick one below."
	}
	return "✓ **Ladder: " + string(mode) + "** " + msg
}

func (p *Plugin) setModChannel(ctx context.Context, ix *core.Ix, channelID string) string {
	if channelID == "" {
		return ""
	}
	cfg := p.config(ctx, ix.GuildID)
	old := cfg.ModChannelID
	cfg.ModChannelID = channelID
	if err := p.saveConfig(ctx, ix, cfg, "mod_channel", core.MentionChannel(old), core.MentionChannel(channelID)); err != nil {
		return "✗ " + err.Error()
	}
	p.mu.Lock()
	p.reconcileReviewJob(ctx, ix.GuildID)
	p.mu.Unlock()
	return fmt.Sprintf("✓ Ladder suggestions and alt hints go to %s. Make sure only staff can see it: a suggestion names the member and what they are up for.", core.MentionChannel(channelID))
}

const (
	minHalfLife = 24 * time.Hour
	maxHalfLife = 365 * 24 * time.Hour
)

func (p *Plugin) setHalfLife(ctx context.Context, ix *core.Ix, raw string) string {
	d, err := core.ParseFlexibleDuration(raw)
	if err != nil {
		return "✗ Half-life: " + err.Error()
	}
	if d < minHalfLife || d > maxHalfLife {
		return fmt.Sprintf("✗ The half-life must be between %s and %s.", core.FormatDuration(minHalfLife), core.FormatDuration(maxHalfLife))
	}
	cfg := p.config(ctx, ix.GuildID)
	old := cfg.HalfLife
	cfg.HalfLife = d
	if err := p.saveConfig(ctx, ix, cfg, "half_life", core.FormatDuration(old), core.FormatDuration(d)); err != nil {
		return "✗ " + err.Error()
	}
	return fmt.Sprintf("✓ Points now count half after %s and a quarter after %s. Every entry's current value is worked out from its own date on the new curve; the points each was given do not change.",
		core.FormatDuration(d), core.FormatDuration(2*d))
}

// setPoints tunes one category, or with an empty value puts it back on its
// default. Entries already on file keep the points they were given.
func (p *Plugin) setPoints(ctx context.Context, ix *core.Ix, category Category, raw string) string {
	if !validCategory(category) {
		return fmt.Sprintf("✗ Unknown category %q.", category)
	}
	cfg := p.config(ctx, ix.GuildID)
	old := pointsFor(cfg, KindWarn, category, actorID(ix), 0)
	var msg string
	if raw = strings.TrimSpace(raw); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > maxPoints {
			return fmt.Sprintf("✗ Points are a whole number from 1 to %d.", maxPoints)
		}
		cfg.CategoryPoints[category] = n
		msg = fmt.Sprintf("✓ One offence in **%s** is now worth **%d** points (was %d). Entries already on file keep the points they were given.", categoryLabel(category), n, old)
	} else {
		delete(cfg.CategoryPoints, category)
		msg = fmt.Sprintf("✓ **%s** is back on its default of **%d** points (was %d).", categoryLabel(category), defaultPoints[category], old)
	}
	if err := p.saveConfig(ctx, ix, cfg, "points."+string(category), fmt.Sprint(old), fmt.Sprint(pointsFor(cfg, KindWarn, category, actorID(ix), 0))); err != nil {
		return "✗ " + err.Error()
	}
	return msg
}

func (p *Plugin) setBands(ctx context.Context, ix *core.Ix, spec string) string {
	spec = strings.TrimSpace(spec)
	cfg := p.config(ctx, ix.GuildID)
	var bands []Band
	if !strings.EqualFold(spec, "default") {
		var err error
		if bands, err = parseBands(spec); err != nil {
			return "✗ Bands: " + err.Error()
		}
	}
	old := bandsWords(cfg.Bands)
	cfg.Bands = bands
	if err := p.saveConfig(ctx, ix, cfg, "bands", old, bandsWords(bands)); err != nil {
		return "✗ " + err.Error()
	}
	var b strings.Builder
	b.WriteString("✓ **Ladder**")
	if len(bands) == 0 {
		b.WriteString(" (the defaults)")
	}
	b.WriteString("\n")
	for _, band := range effectiveBands(bands) {
		b.WriteString(band.String() + "\n")
	}
	b.WriteString("-# A member already past a band is not re-suggested for it; the next crossing is.")
	return b.String()
}

func (p *Plugin) setAltHints(ctx context.Context, ix *core.Ix, on bool) string {
	cfg := p.config(ctx, ix.GuildID)
	old := cfg.AltHints
	cfg.AltHints = on
	if err := p.saveConfig(ctx, ix, cfg, "alt_hints", fmt.Sprint(old), fmt.Sprint(on)); err != nil {
		return "✗ " + err.Error()
	}
	if on {
		return "✓ Alt hints on: when somebody joins who resembles a member on record, merlin notes it on both sheets and, if it looks strong, posts it to the mod channel. Nothing is linked without a moderator."
	}
	return "✓ Alt hints off: joins are no longer compared with members on record. Existing hints and links are kept."
}

// setForum points case files at a forum, creating one when none is given.
// Choosing an existing forum changes nothing about its permissions, exactly
// as /config's pickers do: if it is visible to @everyone, the sheet is
// public, and the note says so.
func (p *Plugin) setForum(ctx context.Context, ix *core.Ix, channelID string) string {
	cfg := p.config(ctx, ix.GuildID)
	var (
		forum *discordgo.Channel
		err   error
	)
	if channelID != "" {
		forum, err = p.ops(ix.GuildID).Channel(channelID)
		if err == nil && forum.Type != discordgo.ChannelTypeGuildForum {
			err = fmt.Errorf("%s is not a forum channel", core.MentionChannel(forum.ID))
		}
	} else {
		forum, err = p.createForum(ix.GuildID)
	}
	if err != nil {
		return "✗ Case files: " + err.Error()
	}
	old := cfg.ForumChannelID
	cfg.ForumChannelID = forum.ID
	if err := p.store.SetConfig(ctx, cfg); err != nil {
		return "✗ Case files: " + err.Error()
	}
	if err := p.audit.Record(ctx, ix.GuildID, actorID(ix), "rapsheet.configured",
		core.MentionChannel(old), "forum="+core.MentionChannel(forum.ID)); err != nil {
		p.log.Error("rapsheet: audit configure forum", "guild", ix.GuildID, "err", err)
	}
	msg := fmt.Sprintf("✓ Case files will be kept in %s. Each member gets a post the first time they are on record.", core.MentionChannel(forum.ID))
	if channelID == "" {
		msg += " It is hidden from @everyone and readable by the mod roles."
	} else {
		msg += " Its permissions were left as they are: make sure only staff can see it."
	}
	if n, err := p.store.CountUnmirrored(ctx, ix.GuildID); err == nil && n > 0 {
		msg += fmt.Sprintf("\n%d existing entries are not mirrored; they will be, the next time each is edited or voided.", n)
	}
	return msg
}

// Help is this plugin's page in /help.
func (p *Plugin) Help() core.Help {
	return core.Help{
		Title: "Rapsheets",
		Line:  "a scored record of warnings, jails, timeouts, kicks and bans",
		About: "every warning, jail, timeout, kick, ban and ai removal goes on the member's rapsheet with a few points that fade over time. when the score crosses a band i can suggest the next step to the mods, or take it myself if the server says so; never a permanent ban. `/rapsheet view` reads one, `/rapsheet me` shows you your own, and `/ban`, `/kick` and `/timeout` record as they act. admins set the ladder on `/rapsheet settings`.",
	}
}
