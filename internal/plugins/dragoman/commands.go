package dragoman

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/disgoorg/disgo/discord"

	"github.com/6586x57890143/merlin/internal/core"
	"github.com/6586x57890143/merlin/internal/core/ui"
)

const (
	panelSet     = "dragoman:panel:set:"
	formModel    = "dragoman:form:model"
	modelDefault = "default"
	modelOther   = "other"
)

var (
	viewSpec      = core.PermSpec{Tier: core.TierMod, Action: "dragoman.read"}
	configureSpec = core.PermSpec{Tier: core.TierAdmin, Action: "dragoman.configure"}
	dialectsSpec  = core.PermSpec{Tier: core.TierAdmin, Action: "dragoman.dialects"}
	// deleteSpec is for anyone's translation. Authors may delete their own
	// where the member controls are on.
	deleteSpec = core.PermSpec{Tier: core.TierMod, Action: "dragoman.delete"}
)

func (p *Plugin) registerCommands() {
	p.commands.RegisterCommand(p.Name(), discord.SlashCommandCreate{
		Name:        "dragoman",
		Description: "Messages in other languages, put into English",
		Options: []discord.ApplicationCommandOption{
			discord.ApplicationCommandOptionSubCommand{
				Name:        "settings",
				Description: "On or off, channels left alone, model and dialects, on one panel",
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
	p.commands.HandleComponentIx(p.Name(), panelView, viewSpec, p.handlePanelView)
	p.commands.HandleComponentIx(p.Name(), panelDialect, dialectsSpec, p.handlePanelDialect)
	p.commands.HandleModalIx(p.Name(), formDialect, dialectsSpec, p.handleDialectForm)
	p.commands.HandleModalIx(p.Name(), formModel, configureSpec, p.handleModelForm)
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
	canSet := perms.Allows(ix, configureSpec)
	pn := core.Panel{Color: core.ColorInfo, Title: "Dragoman", Plugin: p.Name(), Note: note,
		Line:   "Messages in other languages, put into English",
		Footer: "Runs on AI moderation's key and budget (/aimod settings): \"all ai\" is both together. What was really written goes to the audit log."}
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
	rows = append(rows, p.costRows(ctx, ix.GuildID)...)
	spent, limit, ok := p.engine.Budget(ctx, ix.GuildID)
	budget := "no key on /aimod settings"
	if ok {
		budget = fmt.Sprintf("$%.2f of $%.2f", spent, limit)
	}
	rows = append(rows, [2]string{"all ai today", budget})
	if ok {
		rows = append(rows, [2]string{"pauses at", fmt.Sprintf("$%.2f all ai", limit*reserve)})
	}
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
	members, offered := ui.Button("Turn on", panelSet+"members:on", discord.ButtonStyleSecondary, !canSet), "off"
	if cfg.MemberControls {
		members, offered = ui.Button("Turn off", panelSet+"members:off", discord.ButtonStyleDanger, !canSet), "**on**"
	}
	named := p.engine.HasOpenRouter(ctx, ix.GuildID)
	if cfg.Enabled && (!ok || !p.scanning || why != "") {
		pn.Color = core.ColorWarning
	}

	spoken := "The dialects could not be read."
	if m, err := p.dialects(ctx, ix.GuildID); err == nil {
		spoken = dialectsLine(m)
	}
	pn.Parts = []discord.ContainerSubComponent{
		ui.Text(core.Readout(rows)),
		ui.Switch("Translation is "+state+"\n-# A message in another language is replaced with its English, under its author's name. Harmful ones are made harmless.", toggle),
		ui.Text("**Left alone**: these channels are never translated."),
		ui.Row(ui.ChannelSelectMany(panelSet+"exempt", "Channels left alone", cfg.ExemptChannelIDs, ui.MaxSelectValues, !canSet)),
		ui.Text("**Model**: an answer that fails a check is asked for again, with more thinking."),
		ui.Row(p.modelPicker(ctx, ix.GuildID, cfg, !canSet || !named)),
		ui.Switch("Member controls are "+offered+"\n-# Members can opt out and take down translations of their own messages. Mods can always take one down.", members),
		ui.Switch("**Dialects**\n-# "+spoken, ui.Button("Dialects", panelView+"dialects", discord.ButtonStyleSecondary, false)),
	}
	return pn
}

// costRows is what translation itself has cost today.
func (p *Plugin) costRows(ctx context.Context, guildID string) [][2]string {
	used, err := p.store.Usage(ctx, guildID, p.now().Truncate(24*time.Hour))
	if err != nil {
		return [][2]string{{"cost today", "unreadable"}}
	}
	var calls int
	var cost float64
	for name, u := range used {
		if name != rungSkipped {
			calls, cost = calls+u.Calls, cost+u.Cost
		}
	}
	rows := [][2]string{{"cost today", fmt.Sprintf("$%.4f", cost)}, {"calls", fmt.Sprintf("%d, %d asked again", calls, used[rungCareful].Calls)}}
	if u := used[rungTend]; u.Calls > 0 {
		rows = append(rows, [2]string{"of it dialects", fmt.Sprintf("$%.4f on their guides", u.Cost)})
	}
	if n := used[rungSkipped].Calls; n > 0 {
		rows = append(rows, [2]string{"pre-filter", fmt.Sprintf("%d calls saved", n)})
	}
	return rows
}

// models is what the picker offers: cheap, good at more languages than
// English, and kept nowhere. What it costs and whether it can still be
// reached without retention is read live (Engine.Prices).
var models = []struct{ id, note string }{
	{"z-ai/glm-5.3-flash", "always thinks a little"},
	{"google/gemma-4-31b-it", "made for many languages"},
	{"google/gemma-4-26b-a4b-it", "smaller and quicker Gemma"},
	{"deepseek/deepseek-v4.1-flash-1017", "strong for its price"},
	{"qwen/qwen3-235b-a22b-2507", "large, strong at Asian languages"},
	{"mistralai/mistral-small-2603", "strong at European languages"},
	{"google/gemini-3.1-flash-lite", "dearest here"},
}

// perThousand is what a thousand translations cost on a model at pr: about
// 1500 tokens read and 150 written each, before any thinking.
func perThousand(pr core.ModelPrice) string {
	return fmt.Sprintf("about $%.2f per 1,000", (1500*pr.Prompt+150*pr.Completion)/1000)
}

func (p *Plugin) modelPicker(ctx context.Context, guildID string, cfg Config, disabled bool) discord.StringSelectMenuComponent {
	// Prices are a nicety here, and not worth the panel being late for.
	ctx, cancel := context.WithTimeout(ctx, 1500*time.Millisecond)
	defer cancel()
	prices, err := p.engine.Prices(ctx, guildID)
	opts := []discord.StringSelectMenuOption{{Label: "AI moderation's deep pass", Value: modelDefault,
		Description: "Whatever /aimod settings has", Default: cfg.Model == ""}}
	listed := cfg.Model == ""
	add := func(id, note string) {
		about := note
		if pr, ok := prices[id]; ok {
			about = perThousand(pr) + ", " + note
		} else if err == nil {
			about = "not reachable without retention right now"
		}
		opts = append(opts, discord.StringSelectMenuOption{Label: clip(id, 100), Value: id, Description: clip(about, 100), Default: id == cfg.Model})
		listed = listed || id == cfg.Model
	}
	for _, m := range models {
		add(m.id, m.note)
	}
	if !listed {
		add(cfg.Model, "picked by ID")
	}
	opts = append(opts, discord.StringSelectMenuOption{Label: "Another model...", Value: modelOther, Description: "Any OpenRouter model ID with zero data retention"})
	return ui.StringSelect(panelSet+"model", "Model for translations", opts, false, disabled)
}

// handleModelForm is the "another model" form.
func (p *Plugin) handleModelForm(ctx context.Context, ix *core.Ix, _ string) {
	p.redraw(ctx, ix, p.setModel(ctx, ix, strings.TrimSpace(ix.ModalValues()["model"])))
}

// setModel takes only a model that can be reached without its provider
// keeping anything, since every request demands that and any other would
// never be answered.
func (p *Plugin) setModel(ctx context.Context, ix *core.Ix, id string) string {
	cfg, err := p.config(ctx, core.ID(ix.GuildID))
	if err != nil {
		return fmt.Sprintf("✗ The settings could not be read: %v", err)
	}
	note := "✓ Translations use AI moderation's deep pass."
	if id == modelDefault {
		id = ""
	} else {
		prices, err := p.engine.Prices(ctx, ix.GuildID)
		if err != nil {
			return fmt.Sprintf("✗ Not saved, the models could not be checked: %v", err)
		}
		pr, ok := prices[id]
		if !ok {
			return "✗ Not saved: no zero data retention endpoint on OpenRouter runs `" + clip(strings.ReplaceAll(id, "`", ""), 80) + "` with structured answers."
		}
		note = "✓ Translations use `" + id + "`, " + perThousand(pr) + " before thinking."
	}
	if err := p.store.SetModel(ctx, ix.GuildID, id); err != nil {
		return fmt.Sprintf("✗ Not saved: %v", err)
	}
	p.forgetConfig(ix.GuildID)
	p.setTrouble(ix.GuildID, "")
	p.record(ctx, ix, "dragoman.model_set", modelName(cfg.Model), modelName(id))
	return note
}

func modelName(id string) string {
	if id == "" {
		return "AI moderation's deep pass"
	}
	return id
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
	case "members:on", "members:off":
		note = p.setMemberControls(ctx, ix, verb == "members:on")
	case "model":
		if ix.FirstValue() == modelOther {
			if err := ix.OpenModal(formModel, "Model for translations", ui.Field("model", "OpenRouter model ID", "", "z-ai/glm-5.3-flash", false, true)); err != nil {
				p.log.Error("dragoman: open model form", "err", err)
			}
			return
		}
		note = p.setModel(ctx, ix, ix.FirstValue())
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

func (p *Plugin) setMemberControls(ctx context.Context, ix *core.Ix, on bool) string {
	if err := p.store.SetMemberControls(ctx, ix.GuildID, on); err != nil {
		return fmt.Sprintf("✗ Not saved: %v", err)
	}
	p.forgetConfig(ix.GuildID)
	p.record(ctx, ix, "dragoman.member_controls_set", onOff(!on), onOff(on))
	if !on {
		return "✓ Member controls off. Anyone who opted out is translated again; their choice is kept."
	}
	return "✓ Member controls on: `/dragoman delete` and `/dragoman opt-out` work for everyone, on their own messages."
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
	if !p.commands.Perms().Allows(ix, deleteSpec) {
		if !p.offered(ctx, ix) {
			return
		}
		if ix.UserID != t.AuthorID {
			ix.RespondWarn("Not yours", "Only whoever wrote the message, or a moderator, can take its translation down.")
			return
		}
	}
	if err := p.ops(ix.GuildID).DeleteMessage(core.ID(t.ChannelID), core.ID(t.RepostID)); err != nil && !core.IsUnknownResource(err) {
		ix.RespondErr("Could not delete it", err)
		return
	}
	p.record(ctx, ix, "dragoman.deleted", "", fmt.Sprintf("user=%s channel=%s", core.MentionUser(t.AuthorID), core.MentionChannel(t.ChannelID)))
	ix.RespondOK("Deleted", "That translation is gone.")
}

// offered answers ix itself and reports false where this server has not
// turned the member controls on, or where that can't be read.
func (p *Plugin) offered(ctx context.Context, ix *core.Ix) bool {
	cfg, err := p.config(ctx, core.ID(ix.GuildID))
	if err != nil {
		ix.RespondErr("Could not read the settings", err)
		return false
	}
	if !cfg.MemberControls {
		ix.RespondWarn("This server doesn't offer that", "An admin can turn the member controls on at `/dragoman settings`.")
	}
	return cfg.MemberControls
}

// handleOptOut sets the caller's own choice and nobody else's. Refused where
// it isn't offered rather than kept for later, so nobody believes they are
// out when they aren't; coming back in always works.
func (p *Plugin) handleOptOut(ctx context.Context, ix *core.Ix) {
	on := ix.Args()["enabled"].Bool()
	if on && !p.offered(ctx, ix) {
		return
	}
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
		About: "when somebody writes in another language i read it in context, put it into english and repost it under their name, with the flag of the language it came from. anything harmful in it comes out harmless. where this server allows it, `/dragoman delete` takes down a translation of yours and `/dragoman opt-out` has me leave your messages as you wrote them. admins turn it on at `/dragoman settings`; it runs on ai moderation's key and budget.",
	}
}
