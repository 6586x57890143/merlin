package aimod

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/6586x57890143/merlin/internal/core"
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
	p.commands.HandleComponent(p.Name(), panelView, viewSpec, p.handlePanelView)
	p.commands.HandleComponent(p.Name(), panelCfg, cfgSpec, p.handlePanelSet)
	p.commands.HandleComponent(p.Name(), panelPol, policySpec, p.handlePanelSet)
	p.commands.HandleModal(p.Name(), formCfg, cfgSpec, p.handleForm)
	p.commands.HandleModal(p.Name(), formPol, policySpec, p.handleForm)
}

func (p *Plugin) handleSettings(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate) {
	if err := core.RespondPanel(s, i, p.panel(ctx, i, "scanning", "", "")); err != nil {
		p.log.Error("aimod: panel response failed", "err", err)
	}
}

// handlePanelView turns pages (view:page) and picks a policy area
// (view:bucket).
func (p *Plugin) handlePanelView(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate, customID string) {
	page, arg := core.FirstValue(i), ""
	if strings.TrimPrefix(customID, panelView) == "bucket" {
		page, arg = "policy", core.FirstValue(i)
	}
	p.redraw(ctx, s, i, page, arg, "")
}

func (p *Plugin) redraw(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate, page, arg, note string) {
	if err := core.UpdatePanel(s, i, p.panel(ctx, i, page, arg, note)); err != nil {
		p.log.Error("aimod: panel update failed", "err", err)
	}
}

func (p *Plugin) panel(ctx context.Context, i *discordgo.InteractionCreate, page, arg, note string) core.Panel {
	perms := p.commands.Perms()
	canCfg, canPol := perms.Allows(i, cfgSpec), perms.Allows(i, policySpec)
	pn := core.Panel{Color: core.ColorInfo, Title: "AI moderation", Plugin: p.Name(), Note: note,
		Line:   "Checks messages against Discord's Community Guidelines",
		Footer: "Reports stay commands: /aimod status, /aimod models show, /aimod calibrate show. Every change is audited."}
	cfg, err := p.store.Config(ctx, i.GuildID)
	if err != nil {
		pn.Color = core.ColorError
		pn.Body = []discordgo.MessageComponent{core.Text("✗ The configuration could not be read: " + err.Error())}
		return pn
	}
	spec, _ := route(cfg)

	switch page {
	case "policy":
		pn.Body = p.policyBody(cfg, Bucket(arg), canPol)

	case "models":
		pn.Body = []discordgo.MessageComponent{
			core.Text("The **cheap pass** reads every message past the free checks; the **deep pass** re-reads only what it flags, and is the only one that can delete anything.\n" +
				"**Cheap:** " + strings.Join(modelsOr(cfg.FastModels, spec.fastModels), ", ") + stackNote(cfg.FastModels) +
				"\n**Deep:** " + strings.Join(modelsOr(cfg.DeepModels, spec.deepModels), ", ") + stackNote(cfg.DeepModels)),
			core.Row(core.Button("Change models", panelCfg+"models", discordgo.PrimaryButton, !canCfg)),
		}
		pn.Footer = "/aimod models show prices these at this server's traffic; /aimod models compare prices a switch."

	case "words":
		list := "Nothing listed."
		var opts []discordgo.SelectMenuOption
		if len(cfg.WordList) > 0 {
			lines := make([]string, 0, len(cfg.WordList))
			for _, w := range cfg.WordList {
				lines = append(lines, "- "+describeWord(w))
				opts = append(opts, discordgo.SelectMenuOption{Label: w.Word, Value: w.Word})
			}
			list = core.TruncateEmbedField(strings.Join(lines, "\n"))
		}
		pn.Body = []discordgo.MessageComponent{
			core.Text("Rewritten or removed with no model involved. A listed word also catches w.o.r.k, w0rk and working.\n" + list),
			core.Row(core.Button("Add a word", panelPol+"word", discordgo.PrimaryButton, !canPol)),
		}
		if len(opts) > 0 {
			pn.Body = append(pn.Body, core.Row(core.StringSelect(panelPol+"unword", "Take a word off the list", opts, false, !canPol)))
		}

	case "calibration":
		ran := "never"
		if !cfg.CalibrationRanAt.IsZero() {
			ran = core.FormatDuration(p.now().Sub(cfg.CalibrationRanAt).Round(time.Minute)) + " ago"
		}
		var modes []discordgo.SelectMenuOption
		for _, m := range CalibrationModes {
			modes = append(modes, discordgo.SelectMenuOption{Label: "Weekly review: " + string(m), Value: string(m), Default: m == cfg.CalibrationMode})
		}
		pn.Body = []discordgo.MessageComponent{
			core.Text("Each week the filter reviews what it did here and proposes, or applies, examples that tune it to how this server talks.\n" +
				core.Readout([][2]string{
					{"review", string(cfg.CalibrationMode)},
					{"last run", ran},
					{"in force", strconv.Itoa(len(cfg.Calibration)) + " example(s)"},
					{"proposed", strconv.Itoa(len(cfg.CalibrationPending)) + " example(s)"},
				})),
			core.Row(core.StringSelect(panelPol+"calmode", "Weekly review", modes, false, !canPol)),
			core.Row(
				core.Button("Review now", panelPol+"calnow", discordgo.PrimaryButton, !canPol || cfg.Mode == ModeOff),
				core.Button("Apply proposal", panelPol+"calapply", discordgo.SuccessButton, !canPol || len(cfg.CalibrationPending) == 0),
				core.Button("Clear", panelPol+"calclear", discordgo.DangerButton, !canPol || len(cfg.Calibration)+len(cfg.CalibrationPending) == 0),
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
		owner, _ := p.ownerOrOperator(i.GuildID, actorOf(i))
		optBtn := core.Button("Offer it", panelPol+"optout:on", discordgo.SecondaryButton, !canPol || !owner)
		if cfg.MemberOptOut {
			optBtn = core.Button("Stop offering", panelPol+"optout:off", discordgo.SecondaryButton, !canPol || !owner)
		}
		modeOpts := []discordgo.SelectMenuOption{
			{Label: "Off", Value: string(ModeOff), Description: "Nothing is scanned"},
			{Label: "Flag", Value: string(ModeFlag), Description: "Watch what it would do; touch nothing"},
			{Label: "Enforce", Value: string(ModeEnforce), Description: "Remove or rewrite per the policy page"},
		}
		for n := range modeOpts {
			modeOpts[n].Default = Mode(modeOpts[n].Value) == cfg.Mode
		}
		sanctionOpts := []discordgo.SelectMenuOption{
			{Label: "No sanctions", Value: string(SanctionOff), Description: "The message is dealt with, nothing else"},
			{Label: "Report sanctions", Value: string(SanctionFlag), Description: "To the audit log for a human to decide"},
			{Label: "Jail on confirmed violations", Value: string(SanctionJail), Description: "Escalating with each prior sanction"},
		}
		for n := range sanctionOpts {
			sanctionOpts[n].Default = SanctionAction(sanctionOpts[n].Value) == cfg.SanctionAction
		}
		triageOpts := []discordgo.SelectMenuOption{
			{Label: "Pre-filter off", Value: string(TriageOff), Description: "Send everything to a model"},
			{Label: "Pre-filter in shadow", Value: string(TriageShadow), Description: "Learn and keep score, change nothing"},
			{Label: "Pre-filter on", Value: string(TriageOn), Description: "Skip the model call when confident"},
		}
		for n := range triageOpts {
			triageOpts[n].Default = TriageMode(triageOpts[n].Value) == cfg.TriageMode
		}
		pn.Body = []discordgo.MessageComponent{
			core.Text(core.Readout([][2]string{
				{"mode", string(cfg.Mode)},
				{"key", key},
				{"budget", formatUSD(cfg.DailyBudgetUSD) + " a day"},
				{"evidence", evidenceWord(cfg.EvidenceHours)},
			})),
			core.Row(core.StringSelect(panelCfg+"mode", "Mode", modeOpts, false, !canCfg)),
			core.Row(
				core.Button("API key", panelCfg+"key", discordgo.SecondaryButton, !canCfg),
				core.Button("Budget and evidence", panelCfg+"limits", discordgo.SecondaryButton, !canCfg),
			),
			core.Text("**Not scanned**: these channels, and members holding these roles."),
			core.Row(core.EntitySelect(discordgo.ChannelSelectMenu, panelCfg+"exch", "Exempt channels", cfg.ExemptChannelIDs, core.MaxSelectValues, !canCfg)),
			core.Row(core.EntitySelect(discordgo.RoleSelectMenu, panelCfg+"exrole", "Exempt roles", cfg.ExemptRoleIDs, core.MaxSelectValues, !canCfg)),
			core.Row(core.StringSelect(panelPol+"sanction", "Sanctions", sanctionOpts, false, !canPol)),
			core.Row(core.StringSelect(panelPol+"triage", "Local pre-filter", triageOpts, false, !canPol)),
			core.Switch(fmt.Sprintf("**Member opt-out** is %s (%d opted out). Server owner only: it is the one setting that makes the filter cover less.", onOff(cfg.MemberOptOut), len(cfg.OptOutUserIDs)), optBtn),
		}
	}

	opts := make([]discordgo.SelectMenuOption, len(aimodPages))
	for n, pg := range aimodPages {
		opts[n] = discordgo.SelectMenuOption{Label: pg.label, Value: pg.key, Description: pg.about, Default: pg.key == page}
	}
	pn.Body = append(pn.Body, core.Gap(), core.Row(core.StringSelect(panelView+"page", "Go to...", opts, false, false)))
	return pn
}

func stackNote(pinned []string) string {
	if len(pinned) == 0 {
		return " (merlin's defaults)"
	}
	return ""
}

func actorOf(i *discordgo.InteractionCreate) string {
	if i.Member != nil && i.Member.User != nil {
		return i.Member.User.ID
	}
	return ""
}

// policyBody is every area and what it does here, and with an area picked,
// exactly what it covers and does not, and the four things it can do. The
// "not a violation" half gets equal billing: it is the half people argue
// about, and the half that decides whether the filter is trustworthy on a
// server that likes to argue.
func (p *Plugin) policyBody(cfg Config, bucket Bucket, can bool) []discordgo.MessageComponent {
	var rows [][2]string
	var opts []discordgo.SelectMenuOption
	for _, b := range AllBuckets {
		rows = append(rows, [2]string{string(b), string(EffectiveAction(cfg.BucketActions, b))})
		opts = append(opts, discordgo.SelectMenuOption{Label: string(b), Value: string(b), Description: truncate(p.policies[b].Short, 100), Default: b == bucket})
	}
	body := []discordgo.MessageComponent{
		core.Text("What this server does in each of Discord's guideline areas.\n" + core.Readout(rows)),
		core.Row(core.StringSelect(panelView+"bucket", "Pick an area to read or change", opts, false, false)),
	}
	pol, ok := p.policies[bucket]
	if !ok {
		return body
	}
	current := EffectiveAction(cfg.BucketActions, bucket)
	var btns []discordgo.MessageComponent
	for _, a := range Actions {
		style := discordgo.SecondaryButton
		if a == current {
			style = discordgo.PrimaryButton
		}
		btns = append(btns, core.Button(string(a), panelPol+"bucket:"+string(bucket)+":"+string(a), style, !can || a == current || bucket == BucketChildSafety))
	}
	text := fmt.Sprintf("**%s** · `%s`\n%s\n**Is a violation**\n%s\n**Is not**\n%s", bucket, current, pol.Short,
		bullets(pol.Violations), bullets(pol.NotViolations))
	if bucket == BucketChildSafety {
		text += "\n-# Always removes. A termination offence for the server and a reporting obligation for Discord, so nothing turns it off."
	}
	return append(body, core.Text(core.TruncateEmbedDescription(text)), core.Row(btns...))
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

func (p *Plugin) handlePanelSet(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate, customID string) {
	verb, arg, _ := strings.Cut(strings.TrimPrefix(strings.TrimPrefix(customID, panelCfg), panelPol), ":")
	var o outcome
	switch verb {
	case "key":
		p.openForm(s, i, formCfg+"key", "Model provider key",
			core.Field("key", "API key (stored encrypted, never shown again)", "", "sk-orca-... or sk-or-...", false, true),
			core.Field("provider", "Provider, only if the key's prefix isn't known", "", "orcarouter or openrouter", false, false))
		return
	case "limits":
		if cfg, err := p.store.Config(ctx, i.GuildID); err == nil {
			p.openForm(s, i, formCfg+"limits", "Budget and evidence",
				core.Field("usd", "Most to spend per UTC day, in USD", strconv.FormatFloat(cfg.DailyBudgetUSD, 'f', -1, 64), "5", false, true),
				core.Field("hours", "Hours to keep a moderated message's text", strconv.Itoa(cfg.EvidenceHours), "0 keeps nothing and makes undo impossible", false, true))
			return
		}
		o = outcome{core.ColorError, "Failed to read the configuration", ""}
	case "models":
		if cfg, err := p.store.Config(ctx, i.GuildID); err == nil {
			p.openForm(s, i, formCfg+"models", "Models",
				core.Field("fast", "Cheap pass: model IDs, cheapest first", stackInput(cfg.FastModels), "comma separated, or default", false, true),
				core.Field("deep", "Deep pass: model IDs, best first", stackInput(cfg.DeepModels), "comma separated, or default", false, true))
			return
		}
		o = outcome{core.ColorError, "Failed to read the configuration", ""}
	case "word":
		p.openForm(s, i, formPol+"word", "Add a word",
			core.Field("word", "Word (letters, digits and spaces)", "", "work", false, true),
			core.Field("replacement", "What it becomes; empty removes the message", "", "the mines", false, false),
			core.Field("noun", "Bare noun only? yes or no", "no", "yes: \"at work\" matches, \"this works\" passes", false, false))
		return
	case "mode":
		o = p.setMode(ctx, i, Mode(core.FirstValue(i)))
	case "exch", "exrole":
		o = p.setExempt(ctx, i, verb == "exch")
	case "sanction":
		o = p.setSanction(ctx, i, SanctionAction(core.FirstValue(i)))
	case "triage":
		o = p.setTriage(ctx, i, TriageMode(core.FirstValue(i)))
	case "optout":
		o = p.setMemberOptOut(ctx, i, arg == "on")
	case "bucket":
		bucket, action, _ := strings.Cut(arg, ":")
		o = p.setPolicy(ctx, i, Bucket(bucket), Action(action))
		arg = bucket
	case "unword":
		o = p.removeWord(ctx, i, core.FirstValue(i))
	case "calmode":
		o = p.setCalibrationMode(ctx, i, CalibrationMode(core.FirstValue(i)))
	case "calapply":
		o = p.calibrateApply(ctx, i)
	case "calclear":
		o = p.calibrateClear(ctx, i)
	case "calnow":
		// A transcript fetch plus the largest model call this plugin makes,
		// nowhere near Discord's 3 second window.
		if err := core.DeferPanelUpdate(s, i); err != nil {
			p.log.Error("aimod: defer calibrate", "err", err)
			return
		}
		o = p.calibrateNow(ctx, i)
		if err := core.FollowUpPanel(s, i, p.panel(ctx, i, "calibration", "", o.note())); err != nil {
			p.log.Error("aimod: panel follow-up failed", "err", err)
		}
		return
	default:
		o = outcome{core.ColorError, "That control is out of date", "Run `/aimod settings` again."}
	}
	p.redraw(ctx, s, i, pageOf[verb], arg, o.note())
}

func (p *Plugin) openForm(s *discordgo.Session, i *discordgo.InteractionCreate, customID, title string, fields ...discordgo.MessageComponent) {
	if err := core.OpenModal(s, i, customID, title, fields...); err != nil {
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
func (p *Plugin) handleForm(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate, customID string) {
	verb := strings.TrimPrefix(strings.TrimPrefix(customID, formCfg), formPol)
	v := core.ModalValues(i)
	var o outcome
	switch verb {
	case "key":
		o = p.setKey(ctx, i, v["key"], v["provider"])
	case "limits":
		usd, err := strconv.ParseFloat(strings.TrimPrefix(strings.TrimSpace(v["usd"]), "$"), 64)
		hours, herr := strconv.Atoi(strings.TrimSpace(v["hours"]))
		switch {
		case err != nil:
			o = outcome{core.ColorError, "That budget isn't a number", "Give dollars a day, like 5 or 2.50."}
		case herr != nil:
			o = outcome{core.ColorError, "That evidence window isn't a number", "Give whole hours, like 48."}
		default:
			if o = p.setBudget(ctx, i, usd); o.color != core.ColorError {
				o = p.setEvidence(ctx, i, hours)
			}
		}
	case "models":
		if o = p.setStack(ctx, i, v["fast"], true); o.color != core.ColorError {
			o = p.setStack(ctx, i, v["deep"], false)
			if o.color != core.ColorError {
				o = outcome{core.ColorSuccess, "Models set", "`/aimod models show` prices them at this server's traffic."}
			}
		}
	case "word":
		noun := strings.HasPrefix(strings.ToLower(strings.TrimSpace(v["noun"])), "y")
		o = p.addWord(ctx, i, v["word"], v["replacement"], noun)
	default:
		o = outcome{core.ColorError, "That form is out of date", "Run `/aimod settings` again."}
	}
	p.redraw(ctx, s, i, pageOf[verb], "", o.note())
}

// setExempt is the exempt channel and role pickers. Each one added or
// removed is audited on its own, as the subcommands did one at a time.
func (p *Plugin) setExempt(ctx context.Context, i *discordgo.InteractionCreate, channels bool) outcome {
	id, render, action, save := panelCfg+"exrole", core.MentionRole, "aimod.exempt_role", p.store.SetExemptRoles
	if channels {
		id, render, action, save = panelCfg+"exch", core.MentionChannel, "aimod.exempt_channel", p.store.SetExemptChannels
	}
	added, removed := core.SelectDiff(i, id)
	if len(added)+len(removed) == 0 {
		return outcome{}
	}
	cfg, err := p.store.Config(ctx, i.GuildID)
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
	if err := save(ctx, i.GuildID, next); err != nil {
		return failed("Failed to update the exemptions", err)
	}
	for _, a := range added {
		p.auditConfig(ctx, i, action, "", render(a)+" "+exemptWord(true))
	}
	for _, r := range removed {
		p.auditConfig(ctx, i, action, "", render(r)+" "+exemptWord(false))
	}
	return outcome{core.ColorSuccess, "Exemptions updated", ""}
}
