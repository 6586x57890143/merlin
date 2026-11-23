package dragoman

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/disgoorg/disgo/discord"

	"github.com/6586x57890143/merlin/internal/core"
	"github.com/6586x57890143/merlin/internal/core/ui"
	"github.com/6586x57890143/merlin/internal/scripts"
)

const (
	panelSet     = "dragoman:panel:set:"
	panelScripts = "dragoman:panel:scripts:"
)

var (
	viewSpec      = core.PermSpec{Tier: core.TierMod, Action: "dragoman.read"}
	configureSpec = core.PermSpec{Tier: core.TierAdmin, Action: "dragoman.configure"}
	scriptsSpec   = core.PermSpec{Tier: core.TierAdmin, Action: "dragoman.scripts"}
	// deleteSpec is for somebody else's translation; anyone may delete theirs.
	deleteSpec = core.PermSpec{Tier: core.TierMod, Action: "dragoman.delete"}
)

func (p *Plugin) registerCommands() {
	p.commands.RegisterCommand(p.Name(), discord.SlashCommandCreate{
		Name:        "dragoman",
		Description: "Messages in other languages, put into English",
		Options: []discord.ApplicationCommandOption{
			discord.ApplicationCommandOptionSubCommand{
				Name:        "settings",
				Description: "On or off, channels left alone and the dialects script, on one panel",
			},
			discord.ApplicationCommandOptionSubCommand{
				Name:        "delete",
				Description: "Take down a translation of one of your messages",
				Options: []discord.ApplicationCommandOption{discord.ApplicationCommandOptionString{
					Name: "message", Description: "The translation's link (Copy Message Link) or ID", Required: true,
				}},
			},
			discord.ApplicationCommandOptionSubCommand{
				Name:        "opt-out",
				Description: "Have your messages left in the language you wrote them in",
				Options: []discord.ApplicationCommandOption{discord.ApplicationCommandOptionBool{
					Name: "enabled", Description: "True to opt out, false to be translated again", Required: true,
				}},
			},
		},
	})
	p.commands.HandleIx("dragoman", "settings", viewSpec, p.handleSettings)
	// Public: whose words they are decides, inside.
	p.commands.HandleIx("dragoman", "delete", core.PermSpec{Tier: core.TierPublic}, p.handleDelete)
	p.commands.HandleIx("dragoman", "opt-out", core.PermSpec{Tier: core.TierPublic}, p.handleOptOut)
	p.commands.HandleComponentIx(p.Name(), panelSet, configureSpec, p.handlePanelSet)
	p.commands.HandleComponentIx(p.Name(), panelScripts, scriptsSpec, p.handlePanelScripts)
}

func (p *Plugin) handleSettings(ctx context.Context, ix *core.Ix) {
	if err := ix.RespondPanel(p.panel(ctx, ix, "")); err != nil {
		p.log.Error("dragoman: panel response failed", "err", err)
	}
}

func (p *Plugin) redraw(ctx context.Context, ix *core.Ix, note string) {
	if err := ix.UpdatePanel(p.panel(ctx, ix, note)); err != nil {
		p.log.Error("dragoman: panel update failed", "err", err)
	}
}

func (p *Plugin) panel(ctx context.Context, ix *core.Ix, note string) core.Panel {
	perms := p.commands.Perms()
	canSet, canScript := perms.Allows(ix, configureSpec), perms.Allows(ix, scriptsSpec)
	pn := core.Panel{Color: core.ColorInfo, Title: "Dragoman", Plugin: p.Name(), Note: note,
		Line:   "Messages in other languages, put into English",
		Footer: "Runs on AI moderation's key and budget (/aimod settings). What was really written goes to the audit log."}
	cfg, err := p.config(ctx, core.ID(ix.GuildID))
	if err != nil {
		pn.Color = core.ColorError
		pn.Parts = []discord.ContainerSubComponent{ui.Text("✗ The settings could not be read: " + err.Error())}
		return pn
	}

	rows := [][2]string{{"today", "0 translated"}}
	if n, err := p.store.CountSince(ctx, ix.GuildID, p.now().Truncate(24*time.Hour)); err == nil {
		rows[0][1] = fmt.Sprintf("%d translated", n)
	} else {
		rows[0][1] = "unreadable"
	}
	spent, limit, ok := p.engine.Budget(ctx, ix.GuildID)
	budget := "no key on /aimod settings"
	if ok {
		budget = fmt.Sprintf("$%.2f of $%.2f spent, stops at $%.2f", spent, limit, limit*reserve)
	}
	rows = append(rows, [2]string{"budget", budget})
	if !p.scanning {
		rows = append(rows, [2]string{"reading", "no: message content intent is off"})
	}
	p.mu.Lock()
	why := p.trouble[ix.GuildID]
	p.mu.Unlock()
	if why != "" {
		rows = append(rows, [2]string{"last problem", clip(why, 120)})
	}

	toggle, state := ui.Button("Turn on", panelSet+"on", discord.ButtonStyleSuccess, !canSet), "off"
	if cfg.Enabled {
		toggle, state = ui.Button("Turn off", panelSet+"off", discord.ButtonStyleDanger, !canSet), "**on**"
	}
	if cfg.Enabled && (!ok || !p.scanning || why != "") {
		pn.Color = core.ColorWarning
	}

	on, script := false, "off"
	if on, err = p.scripts.Enabled(ctx, ix.GuildID, scriptDialects); err != nil {
		on, script = false, "unreadable (treated as off)"
	}
	sb := ui.Button("Turn on", panelScripts+"on", discord.ButtonStyleSecondary, !canScript)
	if on {
		sb, script = ui.Button("Turn off", panelScripts+"off", discord.ButtonStyleDanger, !canScript), "**on**"
	}
	pn.Parts = []discord.ContainerSubComponent{
		ui.Text(core.Readout(rows)),
		ui.Switch("Translation is "+state+"\n-# A message in another language is replaced with its English, under its author's name. Harmful ones are made harmless.", toggle),
		ui.Text("**Left alone**: these channels are never translated."),
		ui.Row(ui.ChannelSelectMany(panelSet+"exempt", "Channels left alone", cfg.ExemptChannelIDs, ui.MaxSelectValues, !canSet)),
		ui.Gap(),
		ui.Text("-# " + scripts.Warning),
		ui.Switch(fmt.Sprintf("`%s` is %s\n-# %s", scriptDialects, script, dialectsAbout()), sb),
	}
	return pn
}

// dialectsAbout is what an admin is told the script does.
func dialectsAbout() string {
	var lines []string
	for base, v := range dialects {
		tag, _ := parseSource(base)
		lines = append(lines, languageName(tag)+" becomes "+v.name+".")
	}
	slices.Sort(lines)
	return strings.Join(lines, " ") + " Every other language becomes " + standard.name + "."
}

func clip(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n-3]) + "..."
	}
	return s
}

func (p *Plugin) handlePanelSet(ctx context.Context, ix *core.Ix, customID string) {
	var note string
	switch verb := strings.TrimPrefix(customID, panelSet); verb {
	case "on", "off":
		note = p.setEnabled(ctx, ix, verb == "on")
	case "exempt":
		note = p.setExempt(ctx, ix)
	default:
		note = "✗ That control is out of date; run `/dragoman settings` again."
	}
	p.redraw(ctx, ix, note)
}

func (p *Plugin) setEnabled(ctx context.Context, ix *core.Ix, on bool) string {
	if err := p.store.SetEnabled(ctx, ix.GuildID, on); err != nil {
		return fmt.Sprintf("✗ Not saved: %v", err)
	}
	p.forgetConfig(ix.GuildID)
	p.setTrouble(ix.GuildID, "")
	p.record(ctx, ix, "dragoman.enabled_set", onOff(!on), onOff(on))
	// What merlin asks servers for follows this (Perms).
	if p.bus != nil {
		p.bus.Publish(ctx, core.Event{Type: core.EventConfigChanged, GuildID: ix.GuildID})
	}
	if !on {
		return "✓ Translation off. What is already translated stays."
	}
	return "✓ Translation on. New messages in another language are put into English from now on."
}

// setExempt is the channel picker; each one added or removed is its own
// change and audit line.
func (p *Plugin) setExempt(ctx context.Context, ix *core.Ix) string {
	added, removed := ix.SelectDiff(panelSet + "exempt")
	defer p.forgetConfig(ix.GuildID)
	for _, c := range []struct {
		ids []string
		on  bool
		act string
	}{{added, true, "dragoman.channel_exempted"}, {removed, false, "dragoman.channel_included"}} {
		for _, id := range c.ids {
			if err := p.store.SetExempt(ctx, ix.GuildID, id, c.on); err != nil {
				return fmt.Sprintf("✗ Not saved: %v", err)
			}
			p.record(ctx, ix, c.act, "", core.MentionChannel(id))
		}
	}
	if len(added)+len(removed) == 0 {
		return ""
	}
	return "✓ Channels updated."
}

func (p *Plugin) handlePanelScripts(ctx context.Context, ix *core.Ix, customID string) {
	verb := strings.TrimPrefix(customID, panelScripts)
	if verb != "on" && verb != "off" {
		p.redraw(ctx, ix, "✗ That control is out of date; run `/dragoman settings` again.")
		return
	}
	if err := p.scripts.SetEnabled(ctx, ix.GuildID, scriptDialects, verb == "on"); err != nil {
		p.redraw(ctx, ix, fmt.Sprintf("✗ Not saved: %v", err))
		return
	}
	if verb == "off" {
		p.record(ctx, ix, "dragoman.script_disabled", "", scriptDialects)
		p.redraw(ctx, ix, "✓ Script off: every language becomes "+standard.name+".")
		return
	}
	p.record(ctx, ix, "dragoman.script_enabled", "", scriptDialects)
	p.redraw(ctx, ix, "! Script on: "+scriptDialects+".\n-# "+dialectsAbout())
}

// messageID is the last ID in a message link, or the ID itself.
var messageID = regexp.MustCompile(`(\d{15,20})\s*$`)

// handleDelete takes a translation down for its author or a moderator: it is
// a webhook's message, so Discord gives its author no way to.
func (p *Plugin) handleDelete(ctx context.Context, ix *core.Ix) {
	m := messageID.FindStringSubmatch(ix.Args()["message"].String())
	if m == nil {
		ix.RespondErr("Not a message", errors.New("give the translation's link or its ID"))
		return
	}
	t, err := p.store.Find(ctx, ix.GuildID, m[1])
	if errors.Is(err, errNotFound) {
		ix.RespondInfo("Not a translation", "I have no record of translating that message.")
		return
	}
	if err != nil {
		ix.RespondErr("Could not look it up", err)
		return
	}
	if ix.UserID != t.AuthorID && !p.commands.Perms().Allows(ix, deleteSpec) {
		ix.RespondWarn("Not yours", "Only whoever wrote the message, or a moderator, can take its translation down.")
		return
	}
	if err := p.ops(ix.GuildID).DeleteMessage(core.ID(t.ChannelID), core.ID(t.RepostID)); err != nil && !core.IsUnknownResource(err) {
		ix.RespondErr("Could not delete it", err)
		return
	}
	p.record(ctx, ix, "dragoman.deleted", "", fmt.Sprintf("user=%s channel=%s", core.MentionUser(t.AuthorID), core.MentionChannel(t.ChannelID)))
	ix.RespondOK("Deleted", "That translation is gone.")
}

// handleOptOut sets the caller's own choice and nobody else's.
func (p *Plugin) handleOptOut(ctx context.Context, ix *core.Ix) {
	on := ix.Args()["enabled"].Bool()
	if err := p.store.SetOptOut(ctx, ix.GuildID, ix.UserID, on); err != nil {
		ix.RespondErr("Could not save that", err)
		return
	}
	p.forgetConfig(ix.GuildID)
	p.record(ctx, ix, "dragoman.optout_changed", "", fmt.Sprintf("user=%s translated=%s", core.MentionUser(ix.UserID), onOff(!on)))
	if on {
		ix.RespondOK("Opted out", "I'll leave your messages in the language you write them in.")
		return
	}
	ix.RespondOK("Back in", "Your messages in other languages are put into English again.")
}

func (p *Plugin) record(ctx context.Context, ix *core.Ix, action, was, now string) {
	if err := p.audit.Record(ctx, ix.GuildID, ix.UserID, action, was, now); err != nil {
		p.log.Error("dragoman: audit failed", "guild", ix.GuildID, "action", action, "err", err)
	}
}

func onOff(on bool) string {
	if on {
		return "on"
	}
	return "off"
}

// Help is this plugin's page in /help.
func (p *Plugin) Help() core.Help {
	return core.Help{
		Title: "Dragoman",
		Line:  "puts messages in other languages into english",
		About: "when somebody writes in another language i read it in context, put it into english and repost it under their name, with the flag of the language it came from. anything harmful in it comes out harmless. `/dragoman delete` takes down a translation of yours, and `/dragoman opt-out` has me leave your messages as you wrote them. admins turn it on at `/dragoman settings`; it runs on ai moderation's key and budget.",
	}
}
