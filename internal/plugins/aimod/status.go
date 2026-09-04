package aimod

import (
	"context"
	"fmt"
	"strings"

	"github.com/bwmarrin/discordgo"

	"github.com/6586x57890143/merlin/internal/core"
)

// /aimod status, in pages.
//
// It was one embed and it had outgrown the format: mode, spend, scan counts,
// two bucket lists, evidence, sanctions, the gateway and its privacy
// position, the account balance and its runway, the local pre-filter's
// training figures and the tip jar, all stacked into a single screen that a
// moderator had to scroll past on a phone to find the one line they came for.
// Discord's own limits were the next thing to go: an embed caps at 25 fields
// and 6000 bytes across the whole thing, and the per-guild opt-out list added
// below is unbounded.
//
// Pages rather than a shorter embed, because none of it was filler. Every
// field answers a question somebody actually asks, they are just not the same
// question, and the split is by who is asking: is it working (overview), what
// does it act on (policy), what is it costing and through whom (provider),
// and who is not covered (opt-outs).
//
// Rendering is stateless and rebuilt from live data on every click, the same
// choice /config setup makes and for the same reason: there is no session to
// expire, two moderators can page through it at once, and a figure cannot go
// stale between clicks. What that costs is one balance lookup per click,
// which is why the click path defers (core.DeferUpdate) before doing the
// work: the gateway call is bounded by httpTimeout, comfortably past
// Discord's 3-second window to acknowledge an interaction.

// statusPagePrefix namespaces the Prev/Next buttons. The target page rides in
// the CustomID itself, so nothing is held server side.
const statusPagePrefix = "aimod:status:page:"

// statusPage is one screen. color is the whole report's severity rather than
// the page's own: an admin who pages away from the problem should not see the
// embed turn green, because nothing was fixed by scrolling.
type statusPage struct {
	name   string
	body   string
	fields []*discordgo.MessageEmbedField
}

func (p *Plugin) handleStatus(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate) {
	if err := core.DeferResponse(s, i); err != nil {
		p.log.Error("aimod: defer status", "guild", i.GuildID, "err", err)
		return
	}
	pages, color, err := p.statusPages(ctx, i.GuildID)
	if err != nil {
		_ = core.FollowUpErr(s, i, "Failed to read the configuration", err)
		return
	}
	embed, components := renderStatusPage(pages, color, 0)
	if err := core.FollowUpEmbedWithComponents(s, i, embed, components); err != nil {
		p.log.Error("aimod: respond status", "guild", i.GuildID, "err", err)
	}
}

// handleStatusPage answers a Prev/Next click by rebuilding the whole report
// and rendering the requested page of it.
func (p *Plugin) handleStatusPage(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate, customID string) {
	page, err := core.ParsePaginationPage(customID, statusPagePrefix)
	if err != nil {
		// A page number that will not parse can only come from a hand-built
		// CustomID. Page 0 rather than an error: Paginate clamps anyway, and
		// the worst outcome is somebody sees the overview.
		p.log.Error("aimod: parse status page", "custom_id", customID, "err", err)
		page = 0
	}
	if err := core.DeferUpdate(s, i); err != nil {
		p.log.Error("aimod: defer status page", "guild", i.GuildID, "err", err)
		return
	}
	pages, color, err := p.statusPages(ctx, i.GuildID)
	if err != nil {
		_ = core.FollowUpErr(s, i, "Failed to read the configuration", err)
		return
	}
	embed, components := renderStatusPage(pages, color, page)
	if err := core.FollowUpEmbedWithComponents(s, i, embed, components); err != nil {
		p.log.Error("aimod: update status page", "guild", i.GuildID, "err", err)
	}
}

// renderStatusPage turns one page of an already-built report into the embed
// and controls to send. Split out from statusPages so the page arithmetic is
// testable without a Discord session or a gateway.
func renderStatusPage(pages []statusPage, color, page int) (*discordgo.MessageEmbed, []discordgo.MessageComponent) {
	if len(pages) == 0 {
		return core.NewEmbed(color, "AI moderation", "Nothing to report."), nil
	}
	page = min(max(page, 0), len(pages)-1)
	pg := pages[page]
	embed := core.NewEmbed(color, "AI moderation: "+pg.name, core.TruncateEmbedDescription(pg.body), pg.fields...)
	return embed, core.PaginationRow(statusPagePrefix, page, len(pages))
}

// statusPages builds the whole report and the one severity colour every page
// wears.
//
// Severity is tracked as the pages are built rather than recovered by
// scanning the finished text for warning glyphs, the same mistake
// /config status made once and had to be fixed for: that read the control
// signal for the response colour out of prose this bot does not author
// (interpolated Discord and database errors), so any error string containing
// a warning glyph turned a healthy server amber.
func (p *Plugin) statusPages(ctx context.Context, guildID string) ([]statusPage, int, error) {
	cfg, err := p.store.Config(ctx, guildID)
	if err != nil {
		return nil, core.ColorError, err
	}
	spend, err := p.store.SpendToday(ctx, guildID, today(p.now()))
	if err != nil {
		return nil, core.ColorError, err
	}

	color := core.ColorSuccess
	worse := func(c int) {
		// Only ever downward, and only from success: a provider warning must
		// not paint over "nothing is being scanned", which is the one line
		// somebody reading this screen most needs to be told first.
		if color == core.ColorSuccess || (color == core.ColorInfo && c != core.ColorSuccess) {
			color = c
		}
	}

	var body strings.Builder
	switch {
	case !p.scanning:
		worse(core.ColorError)
		body.WriteString("**Nothing is being scanned.** This bot was started without Discord's Message Content intent, " +
			"so it cannot read messages at all. The operator needs to tick \"Message Content Intent\" in the Discord " +
			"Developer Portal and set `MERLIN_ENABLE_MESSAGE_CONTENT_INTENT=1`.")
	case cfg.Mode == ModeOff:
		worse(core.ColorWarning)
		body.WriteString("**Off.** Nothing is being scanned. `/aimod configure mode flag` to start watching without acting.")
	case len(cfg.APIKeySealed) == 0 && len(cfg.OrcaKeySealed) == 0:
		worse(core.ColorWarning)
		body.WriteString("**No API key.** Only the built-in pattern checks are running. `/aimod configure key` to fix.")
	case cfg.Mode == ModeFlag:
		worse(core.ColorInfo)
		body.WriteString("**Flagging only.** Decisions are recorded and posted to the audit log; no message is touched.")
	default:
		body.WriteString("**Enforcing.** Messages are removed or rewritten per `/aimod policy list`.")
	}

	if remaining := cfg.DailyBudgetUSD - spend.SpentUSD; remaining <= 0 && cfg.Mode != ModeOff {
		worse(core.ColorWarning)
		fmt.Fprintf(&body, "\n\n**Budget spent for today.** %s of %s used. Pattern checks still run; "+
			"nothing is being sent to a model until midnight UTC.", formatUSD(spend.SpentUSD), formatUSD(cfg.DailyBudgetUSD))
	}

	pages := []statusPage{{
		name: "overview",
		body: body.String(),
		fields: []*discordgo.MessageEmbedField{
			{Name: "Spent today", Value: fmt.Sprintf("%s of %s", formatUSD(spend.SpentUSD), formatUSD(cfg.DailyBudgetUSD)), Inline: true},
			{Name: "Messages scanned today", Value: fmt.Sprintf("%d", spend.Scanned), Inline: true},
			{Name: "Model calls today", Value: fmt.Sprintf("%d cheap, %d deep", spend.FastCalls, spend.DeepCalls), Inline: true},
		},
	}}

	pages = append(pages, policyPage(cfg))
	pages = append(pages, p.providerPage(ctx, cfg, worse))
	pages = append(pages, optOutPages(cfg)...)
	return pages, color, nil
}

// policyPage is what this guild acts on: pure config, no network, no clock.
func policyPage(cfg Config) statusPage {
	var enforcing, watching []string
	for _, b := range AllBuckets {
		switch EffectiveAction(cfg.BucketActions, b) {
		case ActionOff:
		case ActionFlag:
			watching = append(watching, string(b))
		default:
			enforcing = append(enforcing, string(b))
		}
	}
	return statusPage{
		name: "what it acts on",
		body: "What happens to a message that breaches each policy area. `/aimod policy list` for the full table, " +
			"`/aimod policy set` to change one.",
		fields: []*discordgo.MessageEmbedField{
			{Name: "Acting on", Value: core.TruncateEmbedField(orNone(enforcing))},
			{Name: "Watching only", Value: core.TruncateEmbedField(orNone(watching))},
			{Name: "Evidence kept", Value: evidenceWord(cfg.EvidenceHours), Inline: true},
			{Name: "Sanctions", Value: string(cfg.SanctionAction), Inline: true},
			{Name: "Exempt channels", Value: core.TruncateEmbedField(mentionList(cfg.ExemptChannelIDs, core.MentionChannel))},
			{Name: "Exempt roles", Value: core.TruncateEmbedField(mentionList(cfg.ExemptRoleIDs, core.MentionRole))},
		},
	}
}

// providerPage is the money and the plumbing: which gateway, what it costs in
// guarantees, what is left on the key, and what the local pre-filter is
// saving. The one page that reaches the network.
func (p *Plugin) providerPage(ctx context.Context, cfg Config, worse func(int)) statusPage {
	// Which gateway this guild's traffic goes through, and what that costs it
	// in guarantees. Named rather than assumed: the two differ on the one
	// thing this plugin promises about member text, and an admin reading a
	// status screen should not have to infer it from which key they pasted.
	spec, sealed := route(cfg)
	page := statusPage{
		name: "provider and cost",
		fields: []*discordgo.MessageEmbedField{{
			Name:  "Provider",
			Value: core.TruncateEmbedField(spec.label + "\n" + privacyLine(spec)),
		}},
	}

	// The live account balance, which is a different question from this
	// server's own cap and the one that actually stops the key working.
	if len(sealed) > 0 {
		if plain, err := p.sealer.Open(sealed); err != nil {
			worse(core.ColorError)
			page.fields = append(page.fields, &discordgo.MessageEmbedField{
				Name: "API key", Value: "stored, but cannot be decrypted: " + err.Error()})
		} else if info, err := p.keyInfo(ctx, spec, plain); err != nil {
			worse(core.ColorWarning)
			page.fields = append(page.fields, &discordgo.MessageEmbedField{
				Name: spec.label, Value: core.TruncateEmbedField("could not reach " + spec.label + ": " + err.Error())})
		} else {
			// A key with no limit set reports LimitRemaining nil, so there is
			// no denominator and no balance to gauge. Saying so beats
			// inventing a bar out of an unknown, and on a free tier there is
			// genuinely nothing to draw: what runs out there is a request rate.
			balance := "no limit set on this key"
			if spec == orcaRouter {
				balance = "free tier: no balance to run down, only a request rate"
			}
			if info.LimitRemaining != nil {
				balance = formatUSD(*info.LimitRemaining) + " left on this key"
				if info.Limit != nil && *info.Limit > 0 {
					balance = bar(*info.LimitRemaining/(*info.Limit), barWidth) + "\n" +
						formatUSD(*info.LimitRemaining) + " of " + formatUSD(*info.Limit)
				}
				// core.FormatDuration rather than humanRunway: this is a mod
				// surface, and every admin or audit duration in this codebase
				// is the compact form. /aimod funding, which members read,
				// deliberately spells the same number out as prose.
				if left, ok := p.runway(ctx, cfg.GuildID, *info.LimitRemaining); ok {
					balance += "\n" + core.FormatDuration(left) + " left at the last week's rate"
					if left <= lowCreditRunway {
						worse(core.ColorWarning)
					}
				}
			}
			page.fields = append(page.fields, &discordgo.MessageEmbedField{
				Name:  spec.label + " account",
				Value: core.TruncateEmbedField(fmt.Sprintf("%s\n%s used across all uses of this key today", balance, formatUSD(info.UsageDaily))),
			})
		}
	}

	// The local pre-filter. Reported whenever it is not off, including in
	// shadow, because shadow exists precisely to produce a number an admin
	// reads here before deciding.
	if cfg.TriageMode != TriageOff {
		page.fields = append(page.fields, &discordgo.MessageEmbedField{
			Name:  "Local pre-filter",
			Value: core.TruncateEmbedField(p.triageLine(ctx, cfg)),
		})
	}

	// The tip jar, if there is one. Shown here as well as on /aimod funding so
	// a mod checking why scanning stopped can see whether the money to restart
	// it is already sitting there.
	if f, err := p.store.Funding(ctx, cfg.GuildID); err == nil && f.Configured() {
		jar := formatUSD(f.BalanceUSD) + " waiting to be loaded"
		if f.Donations > 0 {
			jar += fmt.Sprintf(", %s raised across %d %s", formatUSD(f.ReceivedUSD), f.Donations,
				plural(f.Donations, "donation", "donations"))
		}
		// The heading names the networks the jar is actually read on, derived
		// from its own address rather than written as a constant. This is the
		// mod-facing screen, so it stays compact and leaves the full warning,
		// the routing advice and the explorer link to /aimod funding.
		name := "Tip jar"
		if family := familyFor(f.Address); family != nil {
			name += " (" + family.networks + ")"
		}
		page.fields = append(page.fields, &discordgo.MessageEmbedField{Name: name, Value: core.TruncateEmbedField(jar)})
	}
	return page
}

// optOutPages lists who this guild is not moderating, one core.PageSize slice
// per page.
//
// Always at least one page, even with the feature off and nobody on the list,
// and that is deliberate. This is the only surface that answers "is anyone
// exempt from the filter here", and a page that appeared only once somebody
// had already opted out would answer it by being absent, which is not an
// answer a moderator can act on.
//
// The whole list is rendered even when the guild switch is off, marked as not
// in effect. Those stored choices come back the moment an owner turns the
// switch on again (SetMemberOptOut deliberately does not clear them), so an
// owner deciding whether to turn it on is entitled to see who it would cover.
func optOutPages(cfg Config) []statusPage {
	total := max(1, (len(cfg.OptOutUserIDs)+core.PageSize-1)/core.PageSize)
	pages := make([]statusPage, 0, total)
	for n := range total {
		ids, _, _ := core.Paginate(cfg.OptOutUserIDs, n)

		name := "opted out"
		if total > 1 {
			name = fmt.Sprintf("opted out (%d/%d)", n+1, total)
		}

		body := "**Member opt-out is off.** Everyone in this server is covered by the filter. " +
			"Only the server owner can change that, with `/aimod configure member-opt-out`."
		if cfg.MemberOptOut {
			body = "**Member opt-out is on.** Anyone here can run `/aimod opt-out` and their messages stop being " +
				"sent to a model. Nobody can opt anybody else out.\n\n" +
				"Still applies to everyone regardless: the built-in pattern checks, and anything reading as child " +
				"safety. Neither has an opt-out on this bot."
		}

		listed := "nobody"
		if len(ids) > 0 {
			listed = mentionList(ids, core.MentionUser)
		}
		heading := fmt.Sprintf("Opted out (%d)", len(cfg.OptOutUserIDs))
		if !cfg.MemberOptOut && len(cfg.OptOutUserIDs) > 0 {
			// Kept rather than cleared when the switch went off, so it has to
			// be labelled: a list under a heading that does not say so reads
			// as people who are exempt right now, and they are not.
			heading = fmt.Sprintf("Opted out (%d, not in effect)", len(cfg.OptOutUserIDs))
		}

		pages = append(pages, statusPage{
			name:   name,
			body:   body,
			fields: []*discordgo.MessageEmbedField{{Name: heading, Value: core.TruncateEmbedField(listed)}},
		})
	}
	return pages
}

// triageLine describes what the local rung is doing, in the terms an admin
// needs to decide whether to trust it.
//
// The miss count is reported even when it is zero, and deliberately next to
// how many messages it was measured over. "Missed 0" means nothing on its own;
// "missed 0 of 340 sampled" is the sentence that earns a mode change.
func (p *Plugin) triageLine(ctx context.Context, cfg Config) string {
	st := p.triageFor(ctx, cfg.GuildID).Stats()

	var b strings.Builder
	if !st.Ready {
		fmt.Fprintf(&b, "warming up: %d of %d messages learned from, skipping nothing until then",
			st.Examples, triageWarmup)
	} else {
		fmt.Fprintf(&b, "trained on %d messages", st.Examples)
	}

	if st.Considered > 0 {
		share := 100 * float64(st.WouldSkip) / float64(st.Considered)
		verb := "would have saved"
		if cfg.TriageMode == TriageOn {
			verb = "saved"
		}
		fmt.Fprintf(&b, "\n%s %.0f%% of model calls this session (%d of %d)",
			verb, share, st.WouldSkip, st.Considered)
	}
	if st.Sampled > 0 {
		fmt.Fprintf(&b, "\nmissed %d of %d sampled checks", st.Missed, st.Sampled)
	}
	if cfg.TriageMode == TriageShadow {
		b.WriteString("\nshadow mode: changing nothing yet. `/aimod configure triage on` to act on it.")
	}
	return b.String()
}
