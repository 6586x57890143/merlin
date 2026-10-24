package aimod

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/disgoorg/disgo/discord"

	"github.com/6586x57890143/merlin/internal/core"
	"github.com/6586x57890143/merlin/internal/core/ui"
	"github.com/6586x57890143/merlin/internal/secret"
)

// /aimod settings is one panel (core.Panel) for what used to be the
// configure, policy, words and calibrate groups and the two model setters:
// everything an admin sets. The reports stay commands (status, models show
// and compare, calibrate show), as do the member-facing ones (why, undo,
// opt-out, moderate-me, the tip jar).
//
// Every control keeps the action its subcommand had: configure for the key,
// mode, budget, evidence, exemptions and models; policy for what each area
// does, sanctions, triage, the member opt-out, the word list and the
// weekly review. The owner-or-operator gate on the member opt-out is still
// checked in the handler, on top.
const (
	panelView = "aimod:panel:view:"
	panelCfg  = "aimod:panel:cfg:"
	panelPol  = "aimod:panel:pol:"
	formCfg   = "aimod:form:cfg:"
	formPol   = "aimod:form:pol:"
)

var (
	viewSpec   = core.PermSpec{Tier: core.TierMod, Action: actionRead}
	cfgSpec    = core.PermSpec{Tier: core.TierAdmin, Action: actionConfigure}
	policySpec = core.PermSpec{Tier: core.TierAdmin, Action: actionPolicy}
)

var aimodPages = []struct{ key, label, about string }{
	{"scanning", "Scanning", "Mode, key, budget, exemptions, sanctions"},
	{"policy", "Policy", "What happens in each of Discord's guideline areas"},
	{"models", "Models", "Which models each pass uses"},
	{"words", "Word list", "This server's own banned words"},
	{"calibration", "Calibration", "The weekly review that tunes the filter"},
}

// outcome is what one change did, shown as the panel's note.
type outcome struct {
	color       int
	title, body string
}

func failed(title string, err error) outcome { return outcome{core.ColorError, title, err.Error()} }

// note renders o under the panel's rule: its mark, its title, its detail.
func (o outcome) note() string {
	if o.title == "" {
		return ""
	}
	mark := map[int]string{core.ColorSuccess: "✓ ", core.ColorError: "✗ ", core.ColorWarning: "! "}[o.color]
	text := mark + "**" + o.title + "**"
	if o.body != "" {
		text += "\n" + o.body
	}
	// A note interpolates provider and database errors. Bounded so one long
	// error cannot take the whole panel over Discord's text limit.
	return core.TruncateEmbedField(text)
}

func (p *Plugin) registerPanel() {
	p.commands.HandleComponentIx(p.Name(), panelView, viewSpec, p.handlePanelView)
	p.commands.HandleComponentIx(p.Name(), panelCfg, cfgSpec, p.handlePanelSet)
	p.commands.HandleComponentIx(p.Name(), panelPol, policySpec, p.handlePanelSet)
	p.commands.HandleModalIx(p.Name(), formCfg, cfgSpec, p.handleForm)
	p.commands.HandleModalIx(p.Name(), formPol, policySpec, p.handleForm)
}

func (p *Plugin) handleSettings(ctx context.Context, ix *core.Ix) {
	if err := ix.RespondPanel(p.panel(ctx, ix, "scanning", "", "")); err != nil {
		p.log.Error("aimod: panel response failed", "err", err)
	}
}

// handlePanelView turns pages (view:page) and picks a policy area
// (view:bucket).
func (p *Plugin) handlePanelView(ctx context.Context, ix *core.Ix, customID string) {
	page, arg := ix.FirstValue(), ""
	if strings.TrimPrefix(customID, panelView) == "bucket" {
		page, arg = "policy", ix.FirstValue()
	}
	p.redraw(ctx, ix, page, arg, "")
}

func (p *Plugin) redraw(ctx context.Context, ix *core.Ix, page, arg, note string) {
	if err := ix.UpdatePanel(p.panel(ctx, ix, page, arg, note)); err != nil {
		p.log.Error("aimod: panel update failed", "err", err)
	}
}

func (p *Plugin) panel(ctx context.Context, ix *core.Ix, page, arg, note string) core.Panel {
	perms := p.commands.Perms()
	canCfg, canPol := perms.Allows(ix, cfgSpec), perms.Allows(ix, policySpec)
	pn := core.Panel{Color: core.ColorInfo, Title: "AI moderation", Plugin: p.Name(), Note: note,
		Line:   "Checks messages against Discord's Community Guidelines",
		Footer: "Reports stay commands: /aimod status, /aimod models show, /aimod calibrate show. Every change is audited."}
	cfg, err := p.store.Config(ctx, ix.GuildID)
	if err != nil {
		pn.Color = core.ColorError
		pn.Parts = []discord.ContainerSubComponent{ui.Text("✗ The configuration could not be read: " + err.Error())}
		return pn
	}
	spec, _ := route(cfg)

	switch page {
	case "policy":
		pn.Parts = p.policyBody(cfg, Bucket(arg), canPol)

	case "models":
		pn.Parts = []discord.ContainerSubComponent{
			ui.Text("The **cheap pass** reads every message past the free checks; the **deep pass** re-reads only what it flags, and is the only one that can delete anything.\n" +
				"**Cheap:** " + strings.Join(modelsOr(cfg.FastModels, spec.fastModels), ", ") + stackNote(cfg.FastModels) +
				"\n**Deep:** " + strings.Join(modelsOr(cfg.DeepModels, spec.deepModels), ", ") + stackNote(cfg.DeepModels)),
			ui.Row(ui.Button("Change models", panelCfg+"models", discord.ButtonStylePrimary, !canCfg)),
		}
		pn.Footer = "/aimod models set-fast and set-deep search the catalogue as you type; show and compare price them."

	case "words":
		list := "Nothing listed."
		var opts []discord.StringSelectMenuOption
		if len(cfg.WordList) > 0 {
			lines := make([]string, 0, len(cfg.WordList))
			for _, w := range cfg.WordList {
				lines = append(lines, "- "+describeWord(w))
				opts = append(opts, discord.StringSelectMenuOption{Label: w.Word, Value: w.Word})
			}
			list = core.TruncateEmbedField(strings.Join(lines, "\n"))
		}
		pn.Parts = []discord.ContainerSubComponent{
			ui.Text("Rewritten or removed with no model involved. A listed word also catches w.o.r.k, w0rk and working.\n" + list),
			ui.Row(ui.Button("Add a word", panelPol+"word", discord.ButtonStylePrimary, !canPol)),
		}
		if len(opts) > 0 {
			pn.Parts = append(pn.Parts, ui.Row(ui.StringSelect(panelPol+"unword", "Take a word off the list", opts, false, !canPol)))
		}

	case "calibration":
		ran := "never"
		if !cfg.CalibrationRanAt.IsZero() {
			ran = core.FormatDuration(p.now().Sub(cfg.CalibrationRanAt).Round(time.Minute)) + " ago"
		}
		var modes []discord.StringSelectMenuOption
		for _, m := range CalibrationModes {
			modes = append(modes, discord.StringSelectMenuOption{Label: "Weekly review: " + string(m), Value: string(m), Default: m == cfg.CalibrationMode})
		}
		pn.Parts = []discord.ContainerSubComponent{
			ui.Text("Each week the filter reviews what it did here and proposes, or applies, examples that tune it to how this server talks.\n" +
				core.Readout([][2]string{
					{"review", string(cfg.CalibrationMode)},
					{"last run", ran},
					{"in force", strconv.Itoa(len(cfg.Calibration)) + " example(s)"},
					{"proposed", strconv.Itoa(len(cfg.CalibrationPending)) + " example(s)"},
				})),
			ui.Row(ui.StringSelect(panelPol+"calmode", "Weekly review", modes, false, !canPol)),
			ui.Row(
				ui.Button("Review now", panelPol+"calnow", discord.ButtonStylePrimary, !canPol || cfg.Mode == ModeOff),
				ui.Button("Apply proposal", panelPol+"calapply", discord.ButtonStyleSuccess, !canPol || len(cfg.CalibrationPending) == 0),
				ui.Button("Clear", panelPol+"calclear", discord.ButtonStyleDanger, !canPol || len(cfg.Calibration)+len(cfg.CalibrationPending) == 0),
			),
		}
		pn.Footer = "/aimod calibrate show reads what is in force and what is proposed."

	default:
		page = "scanning"
		key := "not set"
		if len(cfg.APIKeySealed) > 0 {
			key = spec.label + " key set"
			if plain, err := p.sealer.Open(cfg.APIKeySealed); err == nil {
				key = secret.Mask(plain)
			}
		}
		owner, _ := p.ownerOrOperator(ix.GuildID, actorOf(ix))
		optBtn := ui.Button("Offer it", panelPol+"optout:on", discord.ButtonStyleSecondary, !canPol || !owner)
		if cfg.MemberOptOut {
			optBtn = ui.Button("Stop offering", panelPol+"optout:off", discord.ButtonStyleSecondary, !canPol || !owner)
		}
		modeOpts := []discord.StringSelectMenuOption{
			{Label: "Off", Value: string(ModeOff), Description: "Nothing is scanned"},
			{Label: "Flag", Value: string(ModeFlag), Description: "Watch what it would do; touch nothing"},
			{Label: "Enforce", Value: string(ModeEnforce), Description: "Remove or rewrite per the policy page"},
		}
		for n := range modeOpts {
			modeOpts[n].Default = Mode(modeOpts[n].Value) == cfg.Mode
		}
		sanctionOpts := []discord.StringSelectMenuOption{
			{Label: "No sanctions", Value: string(SanctionOff), Description: "The message is dealt with, nothing else"},
			{Label: "Report sanctions", Value: string(SanctionFlag), Description: "To the audit log for a human to decide"},
			{Label: "Jail on confirmed violations", Value: string(SanctionJail), Description: "Escalating with each prior sanction"},
		}
		for n := range sanctionOpts {
			sanctionOpts[n].Default = SanctionAction(sanctionOpts[n].Value) == cfg.SanctionAction
		}
		triageOpts := []discord.StringSelectMenuOption{
			{Label: "Pre-filter off", Value: string(TriageOff), Description: "Send everything to a model"},
			{Label: "Pre-filter in shadow", Value: string(TriageShadow), Description: "Learn and keep score, change nothing"},
			{Label: "Pre-filter on", Value: string(TriageOn), Description: "Skip the model call when confident"},
		}
		for n := range triageOpts {
			triageOpts[n].Default = TriageMode(triageOpts[n].Value) == cfg.TriageMode
		}
		pn.Parts = []discord.ContainerSubComponent{
			ui.Text(core.Readout([][2]string{
				{"mode", string(cfg.Mode)},
				{"key", key},
				{"budget", formatUSD(cfg.DailyBudgetUSD) + " a day"},
				{"evidence", evidenceWord(cfg.EvidenceHours)},
			})),
			ui.Row(ui.StringSelect(panelCfg+"mode", "Mode", modeOpts, false, !canCfg)),
			ui.Row(
				ui.Button("API key", panelCfg+"key", discord.ButtonStyleSecondary, !canCfg),
				ui.Button("Budget and evidence", panelCfg+"limits", discord.ButtonStyleSecondary, !canCfg),
			),
			ui.Text("**Not scanned**: these channels, and members holding these roles."),
			ui.Row(ui.ChannelSelectMany(panelCfg+"exch", "Exempt channels", cfg.ExemptChannelIDs, ui.MaxSelectValues, !canCfg)),
			ui.Row(ui.RoleSelect(panelCfg+"exrole", "Exempt roles", cfg.ExemptRoleIDs, ui.MaxSelectValues, !canCfg)),
			ui.Row(ui.StringSelect(panelPol+"sanction", "Sanctions", sanctionOpts, false, !canPol)),
			ui.Row(ui.StringSelect(panelPol+"triage", "Local pre-filter", triageOpts, false, !canPol)),
			ui.Switch(fmt.Sprintf("**Member opt-out** is %s (%d opted out). Server owner only: it is the one setting that makes the filter cover less.", onOff(cfg.MemberOptOut), len(cfg.OptOutUserIDs)), optBtn),
		}
	}

	opts := make([]discord.StringSelectMenuOption, len(aimodPages))
	for n, pg := range aimodPages {
		opts[n] = discord.StringSelectMenuOption{Label: pg.label, Value: pg.key, Description: pg.about, Default: pg.key == page}
	}
	pn.Parts = append(pn.Parts, ui.Gap(), ui.Row(ui.StringSelect(panelView+"page", "Go to...", opts, false, false)))
	return pn
}

func stackNote(pinned []string) string {
	if len(pinned) == 0 {
		return " (merlin's defaults)"
	}
	return ""
}

func actorOf(ix *core.Ix) string {
	if ix.Member() != nil {
		return ix.UserID
	}
	return ""
}

// policyBody is every area and what it does here, and with an area picked,
// exactly what it covers and does not, and the four things it can do. The
// "not a violation" half gets equal billing: it is the half people argue
// about, and the half that decides whether the filter is trustworthy on a
// server that likes to argue.
func (p *Plugin) policyBody(cfg Config, bucket Bucket, can bool) []discord.ContainerSubComponent {
	var rows [][2]string
	var opts []discord.StringSelectMenuOption
	for _, b := range AllBuckets {
		rows = append(rows, [2]string{string(b), string(EffectiveAction(cfg.BucketActions, b))})
		opts = append(opts, discord.StringSelectMenuOption{Label: string(b), Value: string(b), Description: truncate(p.policies[b].Short, 100), Default: b == bucket})
	}
	body := []discord.ContainerSubComponent{
		ui.Text("What this server does in each of Discord's guideline areas.\n" + core.Readout(rows)),
		ui.Row(ui.StringSelect(panelView+"bucket", "Pick an area to read or change", opts, false, false)),
	}
	pol, ok := p.policies[bucket]
	if !ok {
		return body
	}
	current := EffectiveAction(cfg.BucketActions, bucket)
	var btns []discord.InteractiveComponent
	for _, a := range Actions {
		style := discord.ButtonStyleSecondary
		if a == current {
			style = discord.ButtonStylePrimary
		}
		btns = append(btns, ui.Button(string(a), panelPol+"bucket:"+string(bucket)+":"+string(a), style, !can || a == current || bucket == BucketChildSafety))
	}
	text := fmt.Sprintf("**%s** · `%s`\n%s\n**Is a violation**\n%s\n**Is not**\n%s", bucket, current, pol.Short,
		bullets(pol.Violations), bullets(pol.NotViolations))
	if bucket == BucketChildSafety {
		text += "\n-# Always removes. A termination offence for the server and a reporting obligation for Discord, so nothing turns it off."
	}
	return append(body, ui.Text(core.TruncateEmbedDescription(text)), ui.Row(btns...))
}

func truncate(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n-3]) + "..."
	}
	return s
}

// pageOf is where each control redraws.
var pageOf = map[string]string{
	"mode": "scanning", "key": "scanning", "limits": "scanning", "exch": "scanning", "exrole": "scanning",
	"sanction": "scanning", "triage": "scanning", "optout": "scanning",
	"bucket": "policy", "models": "models", "word": "words", "unword": "words",
	"calmode": "calibration", "calnow": "calibration", "calapply": "calibration", "calclear": "calibration",
}

func (p *Plugin) handlePanelSet(ctx context.Context, ix *core.Ix, customID string) {
	verb, arg, _ := strings.Cut(strings.TrimPrefix(strings.TrimPrefix(customID, panelCfg), panelPol), ":")
	var o outcome
	switch verb {
	case "key":
		p.openForm(ix, formCfg+"key", "Model provider key",
			ui.Field("key", "API key (stored encrypted, never shown again)", "", "sk-orca-... or sk-or-...", false, true),
			ui.Field("provider", "Provider, only if the key's prefix isn't known", "", "orcarouter or openrouter", false, false))
		return
	case "limits":
		if cfg, err := p.store.Config(ctx, ix.GuildID); err == nil {
			p.openForm(ix, formCfg+"limits", "Budget and evidence",
				ui.Field("usd", "Most to spend per UTC day, in USD", strconv.FormatFloat(cfg.DailyBudgetUSD, 'f', -1, 64), "5", false, true),
				ui.Field("hours", "Hours to keep a moderated message's text", strconv.Itoa(cfg.EvidenceHours), "0 keeps nothing and makes undo impossible", false, true))
			return
		}
		o = outcome{core.ColorError, "Failed to read the configuration", ""}
	case "models":
		if cfg, err := p.store.Config(ctx, ix.GuildID); err == nil {
			p.openForm(ix, formCfg+"models", "Models",
				ui.Field("fast", "Cheap pass: model IDs, cheapest first", stackInput(cfg.FastModels), "comma separated, or default", false, true),
				ui.Field("deep", "Deep pass: model IDs, best first", stackInput(cfg.DeepModels), "comma separated, or default", false, true))
			return
		}
		o = outcome{core.ColorError, "Failed to read the configuration", ""}
	case "word":
		p.openForm(ix, formPol+"word", "Add a word",
			ui.Field("word", "Word (letters, digits and spaces)", "", "work", false, true),
			ui.Field("replacement", "What it becomes; empty removes the message", "", "the mines", false, false),
			ui.Field("noun", "Bare noun only? yes or no", "no", "yes: \"at work\" matches, \"this works\" passes", false, false))
		return
	case "mode":
		o = p.setMode(ctx, ix, Mode(ix.FirstValue()))
	case "exch", "exrole":
		o = p.setExempt(ctx, ix, verb == "exch")
	case "sanction":
		o = p.setSanction(ctx, ix, SanctionAction(ix.FirstValue()))
	case "triage":
		o = p.setTriage(ctx, ix, TriageMode(ix.FirstValue()))
	case "optout":
		o = p.setMemberOptOut(ctx, ix, arg == "on")
	case "bucket":
		bucket, action, _ := strings.Cut(arg, ":")
		o = p.setPolicy(ctx, ix, Bucket(bucket), Action(action))
		arg = bucket
	case "unword":
		o = p.removeWord(ctx, ix, ix.FirstValue())
	case "calmode":
		o = p.setCalibrationMode(ctx, ix, CalibrationMode(ix.FirstValue()))
	case "calapply":
		o = p.calibrateApply(ctx, ix)
	case "calclear":
		o = p.calibrateClear(ctx, ix)
	case "calnow":
		// A transcript fetch plus the largest model call this plugin makes,
		// nowhere near Discord's 3 second window.
		if err := ix.DeferPanelUpdate(); err != nil {
			p.log.Error("aimod: defer calibrate", "err", err)
			return
		}
		// Deferring buys Discord's 15 minutes, not the router's 30 seconds,
		// and a review cut off there never stores its proposal.
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), manualCalibrationTimeout)
		defer cancel()
		o = p.calibrateNow(ctx, ix)
		if err := ix.FollowUpPanel(p.panel(ctx, ix, "calibration", "", o.note())); err != nil {
			p.log.Error("aimod: panel follow-up failed", "err", err)
		}
		return
	default:
		o = outcome{core.ColorError, "That control is out of date", "Run `/aimod settings` again."}
	}
	p.redraw(ctx, ix, pageOf[verb], arg, o.note())
}

func (p *Plugin) openForm(ix *core.Ix, customID, title string, fields ...discord.LayoutComponent) {
	if err := ix.OpenModal(customID, title, fields...); err != nil {
		p.log.Error("aimod: open form failed", "err", err)
	}
}

func stackInput(pinned []string) string {
	if len(pinned) == 0 {
		return "default"
	}
	return strings.Join(pinned, ", ")
}

// handleForm is the key, limits, models and word forms.
func (p *Plugin) handleForm(ctx context.Context, ix *core.Ix, customID string) {
	verb := strings.TrimPrefix(strings.TrimPrefix(customID, formCfg), formPol)
	v := ix.ModalValues()
	var o outcome
	switch verb {
	case "key":
		o = p.setKey(ctx, ix, v["key"], v["provider"])
	case "limits":
		usd, err := strconv.ParseFloat(strings.TrimPrefix(strings.TrimSpace(v["usd"]), "$"), 64)
		hours, herr := strconv.Atoi(strings.TrimSpace(v["hours"]))
		switch {
		case err != nil:
			o = outcome{core.ColorError, "That budget isn't a number", "Give dollars a day, like 5 or 2.50."}
		case herr != nil:
			o = outcome{core.ColorError, "That evidence window isn't a number", "Give whole hours, like 48."}
		default:
			if o = p.setBudget(ctx, ix, usd); o.color != core.ColorError {
				o = p.setEvidence(ctx, ix, hours)
			}
		}
	case "models":
		if o = p.setStack(ctx, ix, v["fast"], true); o.color != core.ColorError {
			fastWarned := o.color == core.ColorWarning
			if o = p.setStack(ctx, ix, v["deep"], false); o.color != core.ColorError {
				if fastWarned || o.color == core.ColorWarning {
					o = outcome{core.ColorWarning, "Models set, without zero data retention",
						"Free models have no zero data retention endpoint, so at least one pass gives up that guarantee. " +
							"The provider page of `/aimod status` says which."}
				} else {
					o = outcome{core.ColorSuccess, "Models set", "`/aimod models show` prices them at this server's traffic."}
				}
			}
		}
	case "word":
		noun := strings.HasPrefix(strings.ToLower(strings.TrimSpace(v["noun"])), "y")
		o = p.addWord(ctx, ix, v["word"], v["replacement"], noun)
	default:
		o = outcome{core.ColorError, "That form is out of date", "Run `/aimod settings` again."}
	}
	p.redraw(ctx, ix, pageOf[verb], "", o.note())
}

// setExempt is the exempt channel and role pickers. Each one added or
// removed is audited on its own, as the subcommands did one at a time.
func (p *Plugin) setExempt(ctx context.Context, ix *core.Ix, channels bool) outcome {
	id, render, action, save := panelCfg+"exrole", core.MentionRole, "aimod.exempt_role", p.store.SetExemptRoles
	if channels {
		id, render, action, save = panelCfg+"exch", core.MentionChannel, "aimod.exempt_channel", p.store.SetExemptChannels
	}
	added, removed := ix.SelectDiff(id)
	if len(added)+len(removed) == 0 {
		return outcome{}
	}
	cfg, err := p.store.Config(ctx, ix.GuildID)
	if err != nil {
		return failed("Failed to read the configuration", err)
	}
	next := cfg.ExemptRoleIDs
	if channels {
		next = cfg.ExemptChannelIDs
	}
	next = slices.Clone(next)
	for _, a := range added {
		if !slices.Contains(next, a) {
			next = append(next, a)
		}
	}
	next = slices.DeleteFunc(next, func(x string) bool { return slices.Contains(removed, x) })
	if err := save(ctx, ix.GuildID, next); err != nil {
		return failed("Failed to update the exemptions", err)
	}
	for _, a := range added {
		p.auditConfig(ctx, ix, action, "", render(a)+" "+exemptWord(true))
	}
	for _, r := range removed {
		p.auditConfig(ctx, ix, action, "", render(r)+" "+exemptWord(false))
	}
	return outcome{core.ColorSuccess, "Exemptions updated", ""}
}

// Help is this plugin's page in /help.
func (p *Plugin) Help() core.Help {
	return core.Help{
		Title: "AI moderation",
		Line:  "reads messages against discord's community guidelines",
		About: "i read messages against discord's own guidelines and, where this server says so, remove or tidy the ones that cross a line. a cheap pass reads everything and a better one double checks anything flagged before i touch it. whoever i act on is told why in a dm, and mods can put it back with `/aimod undo`. `/aimod why` shows why a message was changed. admins set it up on `/aimod settings`.",
	}
}
