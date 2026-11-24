package dragoman

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/disgoorg/disgo/discord"

	"github.com/6586x57890143/merlin/internal/core"
	"github.com/6586x57890143/merlin/internal/core/ui"
)

// The dialects page of /dragoman settings: pick a language, pick a dialect,
// and the rest looks after itself.
//
//	view:home             the settings
//	view:dialects         every dialect there is, and the language picker
//	view:lang             a language was picked: the dialect picker
//	view:dialect          a dialect was picked: its page
//	view:dialect:<lang>   the same, from a button
const (
	panelView    = "dragoman:panel:view:"
	panelDialect = "dragoman:panel:dialect:"
	formDialect  = "dragoman:form:dialect:"
	pickOther    = "other"
)

// offered languages and dialects are the likely ones. Anything else is typed.
var (
	offeredLanguages = []string{"nl", "da", "de", "fr", "es", "pt", "it", "sv", "no", "fi", "pl", "ro", "hu", "cs", "hr", "el", "tr", "ru", "uk", "ar", "hi", "ja", "ko", "zh"}
	offeredDialects  = []string{
		"African American Vernacular English", "Jamaican English", "Trinidadian English", "Scottish English", "Irish English", "Northern Irish English",
		"Yorkshire English", "Geordie English", "Scouse English", "Cockney", "Multicultural London English", "West Country English", "Welsh English",
		"Southern American English", "Appalachian English", "Boston English", "New York City English", "Newfoundland English", "Australian English",
		"New Zealand English", "South African English", "Nigerian Pidgin", "Indian English", "Singlish",
	}
)

func (p *Plugin) handlePanelView(ctx context.Context, ix *core.Ix, customID string) {
	page, arg, _ := strings.Cut(strings.TrimPrefix(customID, panelView), ":")
	switch page {
	case "dialects":
		p.show(ix, p.dialectsPage(ctx, ix, ""))
	case "lang":
		if ix.FirstValue() == pickOther {
			p.openForm(ix, formDialect+"lang", "Another language", ui.Field("language", "Language", "", "Frisian", false, true))
			return
		}
		p.show(ix, p.pickPage(ix, ix.FirstValue(), ""))
	case "dialect":
		if arg == "" {
			arg = ix.FirstValue()
		}
		p.show(ix, p.dialectPage(ctx, ix, arg, ""))
	default:
		p.redraw(ctx, ix, "")
	}
}

func (p *Plugin) show(ix *core.Ix, pn core.Panel) {
	if err := ix.UpdatePanel(pn); err != nil {
		p.log.Error("dragoman: panel update failed", "err", err)
	}
}

func (p *Plugin) openForm(ix *core.Ix, id, title string, fields ...discord.LayoutComponent) {
	if err := ix.OpenModal(id, title, fields...); err != nil {
		p.log.Error("dragoman: open form", "form", id, "err", err)
	}
}

// page is a panel under the settings.
func (p *Plugin) page(title, line, note string, parts ...discord.ContainerSubComponent) core.Panel {
	return core.Panel{Color: core.ColorInfo, Title: title, Line: line, Plugin: p.Name(), Note: note, Parts: parts}
}

func (p *Plugin) failedPage(err error) core.Panel {
	pn := p.page("Dialects", "", "", ui.Text("✗ The dialects could not be read: "+err.Error()),
		ui.Row(ui.Button("Back", panelView+"home", discord.ButtonStyleSecondary, false)))
	pn.Color = core.ColorError
	return pn
}

// dialectsLine is the dialects as the settings page has them.
func dialectsLine(m map[string]Dialect) string {
	if len(m) == 0 {
		return "Every language becomes " + standardName + "."
	}
	var lines []string
	for key, d := range m {
		name, emoji := keyName(key)
		lines = append(lines, fmt.Sprintf("%s %s becomes %s.", emoji, name, d.Name))
	}
	slices.Sort(lines)
	if extra := len(lines) - 5; extra > 0 {
		lines = append(lines[:5], fmt.Sprintf("And %d more.", extra))
	}
	return strings.Join(lines, " ") + " Every other language becomes " + standardName + "."
}

func (p *Plugin) dialectsPage(ctx context.Context, ix *core.Ix, note string) core.Panel {
	m, err := p.dialects(ctx, ix.GuildID)
	if err != nil {
		return p.failedPage(err)
	}
	can := p.commands.Perms().Allows(ix, dialectsSpec)
	pn := p.page("Dialects", "Which English each language becomes", note)
	pn.Footer = "I write the guide to a dialect myself, and keep improving it. What members write never goes into one."
	var rows [][2]string
	var opts []discord.StringSelectMenuOption
	for key, d := range m {
		name, emoji := keyName(key)
		state := p.stateOf(d)
		rows = append(rows, [2]string{name, d.Name})
		o := discord.StringSelectMenuOption{Label: clip(name+" becomes "+d.Name, 100), Value: key, Description: clip(state, 100)}
		if utf8.RuneCountInString(emoji) == 2 {
			o.Emoji = &discord.ComponentEmoji{Name: emoji}
		}
		opts = append(opts, o)
		if d.Guide == nil && d.Problem != "" {
			pn.Color = core.ColorWarning
		}
	}
	slices.SortFunc(rows, func(a, b [2]string) int { return strings.Compare(a[0], b[0]) })
	slices.SortFunc(opts, func(a, b discord.StringSelectMenuOption) int { return strings.Compare(a.Label, b.Label) })
	rows = append(rows, [2]string{"all others", standardName})
	pn.Parts = append(pn.Parts, ui.Text(core.Readout(rows)))
	if len(opts) > 0 {
		pn.Parts = append(pn.Parts, ui.Row(ui.StringSelect(panelView+"dialect", "Look at one, try it or change it", opts, false, false)))
	}
	var langs []discord.StringSelectMenuOption
	for _, key := range offeredLanguages {
		name, emoji := keyName(key)
		langs = append(langs, discord.StringSelectMenuOption{Label: name, Value: key, Emoji: &discord.ComponentEmoji{Name: emoji}})
	}
	langs = append(langs, discord.StringSelectMenuOption{Label: "Another language...", Value: pickOther})
	pn.Parts = append(pn.Parts,
		ui.Text("**Give a language a dialect**"),
		ui.Row(ui.StringSelect(panelView+"lang", "Language", langs, false, !can)),
		ui.Row(ui.Button("Back", panelView+"home", discord.ButtonStyleSecondary, false)))
	return pn
}

// stateOf is how a dialect is doing, in a few words.
func (p *Plugin) stateOf(d Dialect) string {
	busy := p.busy(d.GuildID, d.Language)
	switch {
	case d.Guide == nil && busy:
		return "writing the guide, a minute or two"
	case d.Guide == nil && d.Problem != "":
		return "no guide: " + d.Problem
	case d.Guide == nil:
		return "waiting for its guide"
	case busy:
		return fmt.Sprintf("on guide %d, trying a better one", d.Version)
	}
	return fmt.Sprintf("on guide %d, %d messages with it", d.Version, d.Uses)
}

// pickPage is the dialect picker for a language.
func (p *Plugin) pickPage(ix *core.Ix, key, note string) core.Panel {
	can := p.commands.Perms().Allows(ix, dialectsSpec)
	name, emoji := keyName(key)
	var opts []discord.StringSelectMenuOption
	for _, d := range offeredDialects {
		opts = append(opts, discord.StringSelectMenuOption{Label: d, Value: d})
	}
	opts = append(opts, discord.StringSelectMenuOption{Label: "Another dialect...", Value: pickOther, Description: "Any English you can name"})
	return p.page(name+" becomes...", "Any English there is", note,
		ui.Text(emoji+" Pick the English that messages in **"+name+"** are put into."),
		ui.Row(ui.StringSelect(panelDialect+"pick:"+key, "Dialect", opts, false, !can)),
		ui.Row(ui.Button("Back", panelView+"dialects", discord.ButtonStyleSecondary, false)))
}

// dialectPage is one dialect: how it is doing, what it sounds like, and what
// can be done about it.
func (p *Plugin) dialectPage(ctx context.Context, ix *core.Ix, key, note string) core.Panel {
	d, err := p.store.Dialect(ctx, ix.GuildID, key)
	if errors.Is(err, errStale) {
		return p.dialectsPage(ctx, ix, cmpOr(note, "That language has no dialect any more."))
	}
	if err != nil {
		return p.failedPage(err)
	}
	can := p.commands.Perms().Allows(ix, dialectsSpec)
	name, _ := keyName(key)
	pn := p.page(d.Name, "What "+name+" becomes here", note)
	rows := [][2]string{{"state", clip(p.stateOf(d), 120)}}
	if d.Guide != nil && d.Uses > 0 {
		rows = append(rows, [2]string{"first try", fmt.Sprintf("%d of %d were right", d.Uses-d.Missed, d.Uses)})
	}
	pn.Parts = append(pn.Parts, ui.Text(core.Readout(rows)))
	if d.Guide == nil {
		pn.Color = core.ColorWarning
		pn.Parts = append(pn.Parts, ui.Text("Until it is written, "+name+" becomes "+standardName+"."))
	} else {
		var b strings.Builder
		b.WriteString("**What it sounds like**\n")
		for _, e := range d.Guide.Examples[:min(4, len(d.Guide.Examples))] {
			fmt.Fprintf(&b, "-# %s\n> %s\n", e.Standard, e.Dialect)
		}
		pn.Parts = append(pn.Parts, ui.Text(b.String()))
	}
	if n := len(d.Notes); n > 0 {
		said := "**What you've told me**"
		for _, s := range d.Notes[max(0, n-3):] {
			said += "\n- " + clip(s, 120)
		}
		pn.Parts = append(pn.Parts, ui.Text(said))
	}
	again := ui.Button("Try again", panelDialect+"improve:"+key, discord.ButtonStylePrimary, !can || p.busy(d.GuildID, key))
	if d.Guide != nil {
		again.Label = "Improve now"
	}
	pn.Parts = append(pn.Parts,
		ui.Row(
			ui.Button("Try a sentence", panelDialect+"try:"+key, discord.ButtonStyleSecondary, !can || d.Guide == nil),
			ui.Button("Tell me what's off", panelDialect+"note:"+key, discord.ButtonStyleSecondary, !can),
			again,
			ui.Button("Undo last change", panelDialect+"undo:"+key, discord.ButtonStyleSecondary, !can || d.Previous == nil),
			ui.Button("Remove", panelDialect+"remove:"+key, discord.ButtonStyleDanger, !can),
		),
		ui.Row(
			ui.Button("Back", panelView+"dialects", discord.ButtonStyleSecondary, false),
			ui.Button("Refresh", panelView+"dialect:"+key, discord.ButtonStyleSecondary, false),
		))
	return pn
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func (p *Plugin) handlePanelDialect(ctx context.Context, ix *core.Ix, customID string) {
	verb, key, _ := strings.Cut(strings.TrimPrefix(customID, panelDialect), ":")
	var note string
	switch verb {
	case "pick":
		if ix.FirstValue() == pickOther {
			p.openForm(ix, formDialect+"name:"+key, "Another dialect", ui.Field("name", "Dialect", "", "Hawaiian Pidgin", false, true))
			return
		}
		note = p.setDialect(ctx, ix, key, ix.FirstValue())
	case "try":
		p.openForm(ix, formDialect+"try:"+key, "Try a sentence", ui.Field("text", "Something in Standard English", "", "I don't think he is coming tonight.", true, true))
		return
	case "note":
		p.openForm(ix, formDialect+"note:"+key, "What's off about it?", ui.Field("note", "In your words", "", "too heavy, nobody says wah gwaan that often", true, true))
		return
	case "improve":
		note = p.improveNow(ctx, ix, key)
	case "undo":
		note = p.undoDialect(ctx, ix, key)
	case "remove":
		note = p.removeDialect(ctx, ix, key)
	default:
		note = "✗ That control is out of date."
	}
	p.show(ix, p.dialectPage(ctx, ix, key, note))
}

func (p *Plugin) handleDialectForm(ctx context.Context, ix *core.Ix, customID string) {
	verb, key, _ := strings.Cut(strings.TrimPrefix(customID, formDialect), ":")
	v := ix.ModalValues()
	var note string
	switch verb {
	case "lang":
		tag, ok := parseLanguage(v["language"])
		if !ok {
			p.show(ix, p.dialectsPage(ctx, ix, "✗ I don't know that as a language other than English. Its English name or its code works, like `Frisian` or `fy`."))
			return
		}
		p.show(ix, p.pickPage(ix, languageKey(tag), ""))
		return
	case "name":
		note = p.setDialect(ctx, ix, key, v["name"])
	case "note":
		note = p.noteDialect(ctx, ix, key, v["note"])
	case "try":
		note = p.tryDialect(ctx, ix, key, v["text"])
	default:
		note = "✗ That form is out of date."
	}
	p.show(ix, p.dialectPage(ctx, ix, key, note))
}

// setDialect gives a language a dialect and has its guide written.
func (p *Plugin) setDialect(ctx context.Context, ix *core.Ix, key, raw string) string {
	if tag, ok := parseLanguage(key); !ok || languageKey(tag) != key {
		return "✗ That language is out of date; pick it again."
	}
	d, err := cleanName(raw)
	if err != nil {
		return "✗ Not saved: " + err.Error() + "."
	}
	if !p.engine.Publishable(ctx, ix.GuildID, d) {
		return "✗ Not saved: that name isn't one I can post under."
	}
	m, err := p.dialects(ctx, ix.GuildID)
	if err != nil {
		return fmt.Sprintf("✗ Not saved: %v", err)
	}
	was, had := m[key]
	if had && strings.EqualFold(was.Name, d) {
		return "That's already what it becomes."
	}
	if !had && len(m) >= maxDialects {
		return fmt.Sprintf("✗ Not saved: %d languages have dialects already, which is all I keep.", maxDialects)
	}
	if err := p.store.SetDialect(ctx, ix.GuildID, key, d); err != nil {
		return fmt.Sprintf("✗ Not saved: %v", err)
	}
	p.forgetConfig(ix.GuildID)
	p.setTrouble(ix.GuildID, "")
	name, _ := keyName(key)
	p.record(ctx, ix, "dragoman.dialect_set", was.Name, fmt.Sprintf("language=%q english=%q", name, d))
	p.tend(ix.GuildID, key, true)
	if !p.busy(ix.GuildID, key) {
		return "✓ " + name + " becomes " + d + ". Today's work on guides is used up, so I'll write this one tomorrow."
	}
	return "✓ " + name + " becomes " + d + ". I'm writing the guide; press Refresh in a minute or two."
}

func (p *Plugin) removeDialect(ctx context.Context, ix *core.Ix, key string) string {
	d, err := p.store.Dialect(ctx, ix.GuildID, key)
	if err == nil {
		err = p.store.RemoveDialect(ctx, ix.GuildID, key)
	}
	if errors.Is(err, errStale) {
		return ""
	}
	if err != nil {
		return fmt.Sprintf("✗ Not removed: %v", err)
	}
	p.forgetConfig(ix.GuildID)
	p.setTrouble(ix.GuildID, "")
	name, _ := keyName(key)
	p.record(ctx, ix, "dragoman.dialect_removed", d.Name, fmt.Sprintf("language=%q", name))
	return "✓ " + name + " becomes " + standardName + " again."
}

// improveNow is a round an admin asked for, or the first guide once more.
func (p *Plugin) improveNow(ctx context.Context, ix *core.Ix, key string) string {
	if _, err := p.store.Dialect(ctx, ix.GuildID, key); err != nil {
		return ""
	}
	p.tend(ix.GuildID, key, true)
	if !p.busy(ix.GuildID, key) {
		return fmt.Sprintf("✗ That's %d rounds today, which is all a day gets.", tendMax)
	}
	return "✓ Working on it. Whatever is better takes over by itself; press Refresh in a minute or two."
}

// noteDialect keeps what an admin says about a dialect and acts on it.
func (p *Plugin) noteDialect(ctx context.Context, ix *core.Ix, key, raw string) string {
	note := clip(strings.Join(strings.Fields(raw), " "), maxNoteRunes)
	d, err := p.store.Dialect(ctx, ix.GuildID, key)
	if err != nil {
		return ""
	}
	if note == "" || !p.engine.Publishable(ctx, ix.GuildID, note) {
		return "✗ Not saved: say it without slurs or links."
	}
	notes := append(slices.Clone(d.Notes), note)
	notes = notes[max(0, len(notes)-maxNotes):]
	if err := p.store.SetDialectNotes(ctx, d, notes); err != nil {
		return fmt.Sprintf("✗ Not saved: %v", err)
	}
	p.forgetConfig(ix.GuildID)
	name, _ := keyName(key)
	p.record(ctx, ix, "dragoman.dialect_note", "", fmt.Sprintf("language=%q english=%q reason=%q", name, d.Name, note))
	p.tend(ix.GuildID, key, true)
	return "✓ Noted, and I'm working it into the guide."
}

func (p *Plugin) undoDialect(ctx context.Context, ix *core.Ix, key string) string {
	d, err := p.store.Dialect(ctx, ix.GuildID, key)
	if err == nil && d.Previous == nil {
		return "There is nothing to go back to."
	}
	if err == nil {
		err = p.store.UndoGuide(ctx, d, 4*firstReview)
	}
	if err != nil {
		return fmt.Sprintf("✗ Not undone: %v", err)
	}
	p.forgetConfig(ix.GuildID)
	name, _ := keyName(key)
	p.record(ctx, ix, "dragoman.dialect_undone", "", fmt.Sprintf("language=%q english=%q", name, d.Name))
	return "✓ Back on the guide before."
}

// tryDialect is one sentence through the guide, shown to the admin alone.
func (p *Plugin) tryDialect(ctx context.Context, ix *core.Ix, key, raw string) string {
	d, err := p.store.Dialect(ctx, ix.GuildID, key)
	if err != nil || d.Guide == nil {
		return "✗ It has no guide yet."
	}
	masked, parts, ok := mask(clip(strings.TrimSpace(raw), 500))
	if !ok || masked == "" {
		return "✗ I need some words to try."
	}
	t, _, err := p.restyle(ctx, job{guildID: ix.GuildID}, translation{masked: masked}, d, "", parts)
	if err != nil {
		return "✗ That didn't work: " + clip(err.Error(), 150)
	}
	if !p.engine.Publishable(ctx, ix.GuildID, t.masked) {
		return "✗ It came out as something I wouldn't post."
	}
	return "✓ " + strings.Join(strings.Fields(t.text), " ")
}
