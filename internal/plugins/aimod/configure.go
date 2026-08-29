package aimod

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/6586x57890143/merlin/internal/core"
)

// Admin surfaces are deliberately not routed through internal/voice. They
// are read by somebody making a decision, often while something is going
// wrong, and warmth costs scanning speed. Same reasoning as /config and
// /rotation configure.

// budgetCeiling is the largest daily budget this command will accept.
//
// Not Discord's limit and not OpenRouter's: a guard against a typo. An admin
// meaning 5.00 and typing 500.00 has made a mistake that this plugin would
// otherwise spend all night carrying out. Anyone who genuinely wants to
// spend more than this per day per guild can raise it in code, having
// thought about it.
const budgetCeiling = 100.0

// evidenceCeiling bounds the retention window. Long enough for a moderator
// to come back from a weekend and reverse something; not so long that this
// plugin quietly becomes a message archive, which is the opposite of what
// rotation exists to provide.
const evidenceCeiling = 24 * 14

func (p *Plugin) handleSetKey(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate) {
	key := strings.TrimSpace(core.LeafArgs(i)["key"].Value.(string))
	if key == "" {
		core.RespondErr(s, i, "No key given", errors.New("paste an OpenRouter API key"))
		return
	}

	sealed, err := p.sealer.seal(key)
	if err != nil {
		// The MERLIN_SECRET_KEY case reaches here, and its message is
		// actionable on purpose: an admin who cannot fix it can at least
		// tell their operator exactly what to set.
		core.RespondErr(s, i, "Cannot store the key", err)
		return
	}
	if err := p.store.SetAPIKey(ctx, i.GuildID, sealed); err != nil {
		core.RespondErr(s, i, "Failed to store the key", err)
		return
	}

	// The key itself never reaches the audit row or the log. maskKey's tail
	// is enough to tell two keys apart, which is all an audit trail needs.
	p.auditConfig(ctx, i, "aimod.key_set", "", maskKey(key))
	core.RespondOK(s, i, "Key stored",
		fmt.Sprintf("Stored encrypted as `%s`. It is never shown again, and never written to the logs.\n\n"+
			"Give this key its own spend limit on openrouter.ai as well: this bot's daily budget is a second line of "+
			"defence, not the only one.\n\nRun `/aimod configure mode flag` next and watch it for a week before enforcing.",
			maskKey(key)))
}

func (p *Plugin) handleSetMode(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate) {
	mode := Mode(core.LeafArgs(i)["mode"].Value.(string))
	if !slices.Contains(Modes, mode) {
		core.RespondErr(s, i, "Unknown mode", fmt.Errorf("%q is not a mode", mode))
		return
	}
	cfg, err := p.store.Config(ctx, i.GuildID)
	if err != nil {
		core.RespondErr(s, i, "Failed to read the configuration", err)
		return
	}
	if mode != ModeOff && len(cfg.APIKeySealed) == 0 {
		core.RespondWarn(s, i, "No API key configured",
			"Set one with `/aimod configure key` first. Without it only the built-in pattern checks run, "+
				"and those catch phishing and leaked credentials, nothing that needs reading a sentence.")
		return
	}
	if err := p.store.SetMode(ctx, i.GuildID, mode); err != nil {
		core.RespondErr(s, i, "Failed to set the mode", err)
		return
	}
	p.auditConfig(ctx, i, "aimod.mode_set", string(cfg.Mode), string(mode))

	switch mode {
	case ModeEnforce:
		core.RespondOK(s, i, "Enforcing", "Messages will now be removed or rewritten according to `/aimod policy list`. "+
			"Every action is reversible with `/aimod undo` while the evidence window lasts.")
	case ModeFlag:
		core.RespondOK(s, i, "Flagging only", "Nothing will be touched. Every decision is recorded and posted to the audit log, "+
			"so you can see exactly what enforcing would have done.")
	default:
		core.RespondOK(s, i, "Off", "Nothing is being scanned.")
	}
}

func (p *Plugin) handleSetBudget(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate) {
	usd := core.LeafArgs(i)["usd"].FloatValue()
	if usd < 0 || usd > budgetCeiling {
		core.RespondErr(s, i, "Budget out of range",
			fmt.Errorf("give a figure between 0 and %s per day", formatUSD(budgetCeiling)))
		return
	}
	if err := p.store.SetBudget(ctx, i.GuildID, usd); err != nil {
		core.RespondErr(s, i, "Failed to set the budget", err)
		return
	}
	p.auditConfig(ctx, i, "aimod.budget_set", "", formatUSD(usd))
	core.RespondOK(s, i, "Budget set",
		fmt.Sprintf("Up to %s per UTC day. Past that, scanning falls back to the free pattern checks until midnight "+
			"and one note is posted to the audit log.\n\nRun `/aimod models show` to see what that buys at this "+
			"server's actual traffic.", formatUSD(usd)))
}

func (p *Plugin) handleSetEvidence(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate) {
	hours := int(core.LeafArgs(i)["hours"].IntValue())
	if hours < 0 || hours > evidenceCeiling {
		core.RespondErr(s, i, "Out of range", fmt.Errorf("give a number of hours between 0 and %d", evidenceCeiling))
		return
	}
	if err := p.store.SetEvidenceHours(ctx, i.GuildID, hours); err != nil {
		core.RespondErr(s, i, "Failed to set evidence retention", err)
		return
	}
	p.auditConfig(ctx, i, "aimod.evidence_set", "", fmt.Sprintf("%d hours", hours))

	if hours == 0 {
		core.RespondWarn(s, i, "Keeping no evidence",
			"Only IDs and the verdict are recorded now. `/aimod why` will not be able to show what a message said, "+
				"and `/aimod undo` will not work at all, because there is nothing left to restore.")
		return
	}
	// Shortening the window applies to messages already stored, not only to
	// future ones, and saying so is the point: the alternative is an admin
	// believing they have tightened retention while yesterday's text sits
	// there on the old schedule.
	core.RespondOK(s, i, "Evidence retention set",
		fmt.Sprintf("The original text of a moderated message is kept for %d hours, then cleared. "+
			"This applies to everything already stored as well, not just to new incidents.", hours))
}

func (p *Plugin) handleExemptChannel(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate) {
	opts := core.LeafArgs(i)
	channelID := opts["channel"].Value.(string)
	exempt := opts["exempt"].BoolValue()

	cfg, err := p.store.Config(ctx, i.GuildID)
	if err != nil {
		core.RespondErr(s, i, "Failed to read the configuration", err)
		return
	}
	next, changed := toggle(cfg.ExemptChannelIDs, channelID, exempt)
	if !changed {
		core.RespondInfo(s, i, "No change", fmt.Sprintf("%s was already %s.", core.MentionChannel(channelID), exemptWord(exempt)))
		return
	}
	if err := p.store.SetExemptChannels(ctx, i.GuildID, next); err != nil {
		core.RespondErr(s, i, "Failed to update exempt channels", err)
		return
	}
	p.auditConfig(ctx, i, "aimod.exempt_channel", "", core.MentionChannel(channelID)+" "+exemptWord(exempt))
	core.RespondOK(s, i, "Updated", fmt.Sprintf("%s is now %s.", core.MentionChannel(channelID), exemptWord(exempt)))
}

func (p *Plugin) handleExemptRole(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate) {
	opts := core.LeafArgs(i)
	roleID := opts["role"].Value.(string)
	exempt := opts["exempt"].BoolValue()

	cfg, err := p.store.Config(ctx, i.GuildID)
	if err != nil {
		core.RespondErr(s, i, "Failed to read the configuration", err)
		return
	}
	next, changed := toggle(cfg.ExemptRoleIDs, roleID, exempt)
	if !changed {
		core.RespondInfo(s, i, "No change", fmt.Sprintf("%s was already %s.", core.MentionRole(roleID), exemptWord(exempt)))
		return
	}
	if err := p.store.SetExemptRoles(ctx, i.GuildID, next); err != nil {
		core.RespondErr(s, i, "Failed to update exempt roles", err)
		return
	}
	p.auditConfig(ctx, i, "aimod.exempt_role", "", core.MentionRole(roleID)+" "+exemptWord(exempt))
	core.RespondOK(s, i, "Updated", fmt.Sprintf("Members holding %s are now %s.", core.MentionRole(roleID), exemptWord(exempt)))
}

func (p *Plugin) handleSetSanction(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate) {
	action := SanctionAction(core.LeafArgs(i)["action"].Value.(string))
	if !slices.Contains(SanctionActions, action) {
		core.RespondErr(s, i, "Unknown action", fmt.Errorf("%q is not a sanction action", action))
		return
	}
	if err := p.store.SetSanctionAction(ctx, i.GuildID, action); err != nil {
		core.RespondErr(s, i, "Failed to set the sanction response", err)
		return
	}
	p.auditConfig(ctx, i, "aimod.sanction_action_set", "", string(action))

	// The ceiling is described first and separately, because it applies
	// whatever this is set to. An admin reading only the branch below could
	// otherwise come away thinking "off" means one member can spend the
	// whole day's budget, which is the opposite of what it means.
	body := fmt.Sprintf("A member whose messages trip the filter more than %d times in %s stops being sent to a model "+
		"for the rest of that window, whatever this is set to. That part is not optional: it is what stops one person "+
		"spending this server's whole daily budget.\n\n", maxUserDeep, core.FormatDuration(meterWindow))
	switch action {
	case SanctionJail:
		body += fmt.Sprintf("On top of that, a confirmed violation now jails the member. The sentence starts at %s for "+
			"the most serious policy areas and %s for the least, and doubles for each prior sanction in the last 30 "+
			"days, capped at %s.\n\nModerators, admins and the server owner are never jailed automatically unless "+
			"they have asked to be with `/aimod moderate-me`.",
			core.FormatDuration(severityBase["critical"]), core.FormatDuration(severityBase["low"]),
			core.FormatDuration(maxSanction))
	case SanctionFlag:
		body += "On top of that it will be reported to the audit log for a human to decide on. Nobody is jailed automatically."
	default:
		body += "Nothing else will happen. The message is still dealt with; only the sanction that would follow it is off."
	}
	core.RespondOK(s, i, "Sanctions set", body)
}

func (p *Plugin) handleConfigureShow(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate) {
	cfg, err := p.store.Config(ctx, i.GuildID)
	if err != nil {
		core.RespondErr(s, i, "Failed to read the configuration", err)
		return
	}

	key := "not set"
	if len(cfg.APIKeySealed) > 0 {
		if plain, err := p.sealer.open(cfg.APIKeySealed); err == nil {
			key = maskKey(plain)
		} else {
			key = "stored, but unreadable: " + err.Error()
		}
	}

	fields := []*discordgo.MessageEmbedField{
		{Name: "Mode", Value: string(cfg.Mode), Inline: true},
		{Name: "API key", Value: key, Inline: true},
		{Name: "Daily budget", Value: formatUSD(cfg.DailyBudgetUSD), Inline: true},
		{Name: "Evidence kept", Value: evidenceWord(cfg.EvidenceHours), Inline: true},
		{Name: "Sanctions", Value: string(cfg.SanctionAction), Inline: true},
		{Name: "Fast models", Value: core.TruncateEmbedField(modelList(cfg.FastModels, defaultFastModels))},
		{Name: "Deep models", Value: core.TruncateEmbedField(modelList(cfg.DeepModels, defaultDeepModels))},
		{Name: "Exempt channels", Value: core.TruncateEmbedField(mentionList(cfg.ExemptChannelIDs, core.MentionChannel))},
		{Name: "Exempt roles", Value: core.TruncateEmbedField(mentionList(cfg.ExemptRoleIDs, core.MentionRole))},
	}
	if err := core.RespondEmbed(s, i, core.NewEmbed(core.ColorInfo, "AI moderation configuration", "", fields...)); err != nil {
		p.log.Error("aimod: respond configure show", "guild", i.GuildID, "err", err)
	}
}

func (p *Plugin) handleStatus(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate) {
	if err := core.DeferResponse(s, i); err != nil {
		p.log.Error("aimod: defer status", "guild", i.GuildID, "err", err)
		return
	}

	cfg, err := p.store.Config(ctx, i.GuildID)
	if err != nil {
		_ = core.FollowUpErr(s, i, "Failed to read the configuration", err)
		return
	}
	spend, err := p.store.SpendToday(ctx, i.GuildID, today(p.now()))
	if err != nil {
		_ = core.FollowUpErr(s, i, "Failed to read today's spend", err)
		return
	}

	// Severity is tracked as the body is built rather than recovered by
	// scanning the finished text for warning glyphs, the same mistake
	// /config status made once and had to be fixed for.
	color := core.ColorSuccess
	var body strings.Builder

	switch {
	case !p.scanning:
		color = core.ColorError
		body.WriteString("**Nothing is being scanned.** This bot was started without Discord's Message Content intent, " +
			"so it cannot read messages at all. The operator needs to tick \"Message Content Intent\" in the Discord " +
			"Developer Portal and set `MERLIN_ENABLE_MESSAGE_CONTENT_INTENT=1`.\n\n")
	case cfg.Mode == ModeOff:
		color = core.ColorWarning
		body.WriteString("**Off.** Nothing is being scanned. `/aimod configure mode flag` to start watching without acting.\n\n")
	case len(cfg.APIKeySealed) == 0:
		color = core.ColorWarning
		body.WriteString("**No API key.** Only the built-in pattern checks are running. `/aimod configure key` to fix.\n\n")
	case cfg.Mode == ModeFlag:
		color = core.ColorInfo
		body.WriteString("**Flagging only.** Decisions are recorded and posted to the audit log; no message is touched.\n\n")
	default:
		body.WriteString("**Enforcing.** Messages are removed or rewritten per `/aimod policy list`.\n\n")
	}

	remaining := cfg.DailyBudgetUSD - spend.SpentUSD
	if remaining <= 0 && cfg.Mode != ModeOff {
		color = core.ColorWarning
		fmt.Fprintf(&body, "**Budget spent for today.** %s of %s used. Pattern checks still run; "+
			"nothing is being sent to a model until midnight UTC.\n\n", formatUSD(spend.SpentUSD), formatUSD(cfg.DailyBudgetUSD))
	}

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

	fields := []*discordgo.MessageEmbedField{
		{Name: "Spent today", Value: fmt.Sprintf("%s of %s", formatUSD(spend.SpentUSD), formatUSD(cfg.DailyBudgetUSD)), Inline: true},
		{Name: "Messages scanned today", Value: fmt.Sprintf("%d", spend.Scanned), Inline: true},
		{Name: "Model calls today", Value: fmt.Sprintf("%d cheap, %d deep", spend.FastCalls, spend.DeepCalls), Inline: true},
		{Name: "Acting on", Value: core.TruncateEmbedField(orNone(enforcing))},
		{Name: "Watching only", Value: core.TruncateEmbedField(orNone(watching))},
		{Name: "Evidence kept", Value: evidenceWord(cfg.EvidenceHours), Inline: true},
		{Name: "Sanctions", Value: string(cfg.SanctionAction), Inline: true},
	}

	// The live account balance, which is a different question from this
	// server's own cap and the one that actually stops the key working.
	if len(cfg.APIKeySealed) > 0 {
		if plain, err := p.sealer.open(cfg.APIKeySealed); err != nil {
			color = core.ColorError
			fields = append(fields, &discordgo.MessageEmbedField{Name: "API key", Value: "stored, but cannot be decrypted: " + err.Error()})
		} else if info, err := p.keyInfo(ctx, plain); err != nil {
			color = core.ColorWarning
			fields = append(fields, &discordgo.MessageEmbedField{Name: "OpenRouter", Value: core.TruncateEmbedField("could not reach OpenRouter: " + err.Error())})
		} else {
			// A key with no limit set reports LimitRemaining nil, so there is
			// no denominator and no balance to gauge. Saying so beats inventing
			// a bar out of an unknown.
			balance := "no limit set on this key"
			if info.LimitRemaining != nil {
				balance = formatUSD(*info.LimitRemaining) + " left on this key"
				if info.Limit != nil && *info.Limit > 0 {
					balance = bar(*info.LimitRemaining/(*info.Limit), barWidth) + "\n" +
						formatUSD(*info.LimitRemaining) + " of " + formatUSD(*info.Limit)
				}
				// core.FormatDuration rather than humanRunway: this is a mod
				// surface, and every admin or audit duration in this codebase is
				// the compact form. /aimod funding, which members read,
				// deliberately spells the same number out as prose.
				if left, ok := p.runway(ctx, i.GuildID, *info.LimitRemaining); ok {
					balance += "\n" + core.FormatDuration(left) + " left at the last week's rate"
					if left <= lowCreditRunway && color == core.ColorSuccess {
						color = core.ColorWarning
					}
				}
			}
			fields = append(fields, &discordgo.MessageEmbedField{
				Name:  "OpenRouter account",
				Value: core.TruncateEmbedField(fmt.Sprintf("%s\n%s used across all uses of this key today", balance, formatUSD(info.UsageDaily))),
			})
		}
	}

	// The tip jar, if there is one. Shown here as well as on /aimod funding
	// so a mod checking why scanning stopped can see whether the money to
	// restart it is already sitting there.
	if f, err := p.store.Funding(ctx, i.GuildID); err == nil && f.Configured() {
		jar := formatUSD(f.BalanceUSD) + " waiting to be loaded"
		if f.Donations > 0 {
			jar += fmt.Sprintf(", %s raised across %d %s", formatUSD(f.ReceivedUSD), f.Donations,
				plural(f.Donations, "donation", "donations"))
		}
		fields = append(fields, &discordgo.MessageEmbedField{
			Name:  "Tip jar (USDC on Base)",
			Value: core.TruncateEmbedField(jar),
		})
	}

	embed := core.NewEmbed(color, "AI moderation", core.TruncateEmbedDescription(body.String()), fields...)
	if err := core.FollowUpEmbed(s, i, embed); err != nil {
		p.log.Error("aimod: respond status", "guild", i.GuildID, "err", err)
	}
}

func (p *Plugin) handlePolicyList(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate) {
	cfg, err := p.store.Config(ctx, i.GuildID)
	if err != nil {
		core.RespondErr(s, i, "Failed to read the configuration", err)
		return
	}

	var body strings.Builder
	body.WriteString("What this server does about each of Discord's guideline areas. " +
		"`/aimod policy explain <area>` shows exactly where each one starts and stops.\n\n")
	for _, b := range AllBuckets {
		action := EffectiveAction(cfg.BucketActions, b)
		note := ""
		if b == BucketChildSafety {
			note = " (cannot be changed)"
		}
		pol := p.policies[b]
		fmt.Fprintf(&body, "**%s** - `%s`%s\n%s\n\n", b, action, note, pol.Short)
	}

	embed := core.NewEmbed(core.ColorInfo, "Policy areas", core.TruncateEmbedDescription(body.String()))
	if err := core.RespondEmbed(s, i, embed); err != nil {
		p.log.Error("aimod: respond policy list", "guild", i.GuildID, "err", err)
	}
}

func (p *Plugin) handlePolicyExplain(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate) {
	bucket := Bucket(core.LeafArgs(i)["policy"].Value.(string))
	pol, ok := p.policies[bucket]
	if !ok {
		core.RespondErr(s, i, "Unknown policy area", fmt.Errorf("%q is not a policy area", bucket))
		return
	}
	cfg, err := p.store.Config(ctx, i.GuildID)
	if err != nil {
		core.RespondErr(s, i, "Failed to read the configuration", err)
		return
	}

	fields := []*discordgo.MessageEmbedField{
		{Name: "This server does", Value: string(EffectiveAction(cfg.BucketActions, bucket)), Inline: true},
		{Name: "Definitions", Value: core.TruncateEmbedField(bullets(pol.Definitions))},
		{Name: "This IS a violation", Value: core.TruncateEmbedField(bullets(pol.Violations))},
		// Shown to moderators on purpose, and given equal billing. This is
		// the half people argue about, and the half that decides whether the
		// filter is trustworthy on a server that likes to argue.
		{Name: "This is NOT a violation", Value: core.TruncateEmbedField(bullets(pol.NotViolations))},
	}
	embed := core.NewEmbed(core.ColorInfo, "Policy: "+string(bucket), pol.Short, fields...)
	if err := core.RespondEmbed(s, i, embed); err != nil {
		p.log.Error("aimod: respond policy explain", "guild", i.GuildID, "err", err)
	}
}

func (p *Plugin) handlePolicySet(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate) {
	opts := core.LeafArgs(i)
	bucket := Bucket(opts["policy"].Value.(string))
	action := Action(opts["action"].Value.(string))

	if !known(bucket) {
		core.RespondErr(s, i, "Unknown policy area", fmt.Errorf("%q is not a policy area", bucket))
		return
	}
	if !action.valid() {
		core.RespondErr(s, i, "Unknown action", fmt.Errorf("%q is not an action", action))
		return
	}
	// The same shape as adminconfig.validateTierChange and its refusal to
	// let a guild lower config.mutate below Admin: a pure, testable guard
	// against the one setting whose only effect is to disarm the server.
	if bucket == BucketChildSafety {
		core.RespondWarn(s, i, "That one is not configurable",
			"Child safety always removes. It is a termination offence for the server and a reporting obligation for "+
				"Discord, so there is no setting that turns it off. Everything else here is yours to decide.")
		return
	}

	cfg, err := p.store.Config(ctx, i.GuildID)
	if err != nil {
		core.RespondErr(s, i, "Failed to read the configuration", err)
		return
	}
	before := EffectiveAction(cfg.BucketActions, bucket)
	if err := p.store.SetBucketAction(ctx, i.GuildID, bucket, action); err != nil {
		core.RespondErr(s, i, "Failed to set the policy", err)
		return
	}
	p.auditConfig(ctx, i, "aimod.policy_set", string(bucket)+" "+string(before), string(bucket)+" "+string(action))

	body := fmt.Sprintf("`%s` is now `%s`.", bucket, action)
	if action.acts() && cfg.Mode != ModeEnforce {
		body += "\n\nThis server is not in enforce mode, so nothing will actually be touched yet. " +
			"`/aimod configure mode enforce` when you are ready."
	}
	core.RespondOK(s, i, "Policy updated", body)
}

func (p *Plugin) handleWhy(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate) {
	messageID := strings.TrimSpace(core.LeafArgs(i)["message_id"].Value.(string))
	inc, err := p.store.IncidentByMessage(ctx, i.GuildID, messageID)
	if err != nil {
		if errors.Is(err, ErrNoIncident) {
			core.RespondInfo(s, i, "Nothing recorded",
				"This bot has not acted on that message. If it disappeared, something else removed it.")
			return
		}
		core.RespondErr(s, i, "Failed to look it up", err)
		return
	}

	fields := []*discordgo.MessageEmbedField{
		{Name: "Policy", Value: string(inc.Bucket), Inline: true},
		{Name: "Action", Value: string(inc.Action), Inline: true},
		{Name: "Confidence", Value: fmt.Sprintf("%.0f%%", inc.Confidence*100), Inline: true},
		{Name: "Author", Value: core.MentionUser(inc.AuthorID), Inline: true},
		{Name: "Channel", Value: core.MentionChannel(inc.ChannelID), Inline: true},
		{Name: "When", Value: fmt.Sprintf("<t:%d:R>", inc.CreatedAt.Unix()), Inline: true},
		{Name: "Reason", Value: core.TruncateEmbedField(orNone([]string{inc.Reason}))},
	}
	if inc.Content != "" {
		fields = append(fields, &discordgo.MessageEmbedField{Name: "Original", Value: core.TruncateEmbedField(inc.Content)})
	} else {
		fields = append(fields, &discordgo.MessageEmbedField{
			Name:  "Original",
			Value: "not stored, or the evidence window has passed. `/aimod undo` cannot restore it.",
		})
	}
	if inc.Undone {
		fields = append(fields, &discordgo.MessageEmbedField{Name: "Status", Value: "already reversed with /aimod undo"})
	}
	// The member's standing, not just this message's. Sanctions are recorded
	// under a synthetic message ID nobody would ever think to look up, so
	// without this line the escalation ladder is invisible to the one person
	// deciding whether it got the length right.
	if n, err := p.store.CountSanctions(ctx, i.GuildID, inc.AuthorID, p.now().Add(-repeatWindow)); err != nil {
		p.log.Error("aimod: count prior sanctions", "guild", i.GuildID, "err", err)
	} else {
		prior := fmt.Sprintf("%d in the last 30 days", n)
		if n > 1 {
			prior += "\nTheir next one would be " + core.FormatDuration(sanctionFor(p.severityOf(inc.Bucket), n))
		}
		fields = append(fields, &discordgo.MessageEmbedField{Name: "This member's record", Value: prior})
	}

	if err := core.RespondEmbed(s, i, core.NewEmbed(core.ColorInfo, "Why that message was moderated", "", fields...)); err != nil {
		p.log.Error("aimod: respond why", "guild", i.GuildID, "err", err)
	}
}

func (p *Plugin) handleUndo(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate) {
	messageID := strings.TrimSpace(core.LeafArgs(i)["message_id"].Value.(string))
	if err := core.DeferResponse(s, i); err != nil {
		p.log.Error("aimod: defer undo", "guild", i.GuildID, "err", err)
		return
	}

	inc, err := p.store.IncidentByMessage(ctx, i.GuildID, messageID)
	if err != nil {
		if errors.Is(err, ErrNoIncident) {
			_ = core.FollowUpErr(s, i, "Nothing to undo", errors.New("this bot has not acted on that message"))
			return
		}
		_ = core.FollowUpErr(s, i, "Failed to look it up", err)
		return
	}
	if inc.Undone {
		_ = core.FollowUpErr(s, i, "Already undone", errors.New("that incident has already been reversed"))
		return
	}
	if err := p.undo(ctx, i.GuildID, inc); err != nil {
		_ = core.FollowUpErr(s, i, "Could not undo it", err)
		return
	}
	p.auditConfig(ctx, i, "aimod.undone", messageID, string(inc.Bucket)+" in "+core.MentionChannel(inc.ChannelID))

	if err := core.FollowUpOK(s, i, "Undone",
		fmt.Sprintf("Reposted %s's message in %s under their own name, and marked the incident reversed.\n\n"+
			"Discord has no way to restore the original, so this is a repost rather than a true undelete.",
			core.MentionUser(inc.AuthorID), core.MentionChannel(inc.ChannelID))); err != nil {
		p.log.Error("aimod: respond undo", "guild", i.GuildID, "err", err)
	}
}

// auditConfig records an admin-driven change, with the invoking member as
// the actor. Distinct from Plugin.audit in enforce.go, which records
// automated action against core.ActorSystem.
func (p *Plugin) auditConfig(ctx context.Context, i *discordgo.InteractionCreate, action, oldValue, newValue string) {
	actorID := ""
	if i.Member != nil && i.Member.User != nil {
		actorID = i.Member.User.ID
	}
	if err := p.auditWriter.Record(ctx, i.GuildID, actorID, action, oldValue, newValue); err != nil {
		p.log.Error("aimod: audit record failed", "action", action, "err", err)
	}
}

func toggle(list []string, id string, want bool) (next []string, changed bool) {
	has := slices.Contains(list, id)
	switch {
	case want && has, !want && !has:
		return list, false
	case want:
		return append(slices.Clone(list), id), true
	default:
		out := make([]string, 0, len(list))
		for _, v := range list {
			if v != id {
				out = append(out, v)
			}
		}
		return out, true
	}
}

func exemptWord(exempt bool) string {
	if exempt {
		return "exempt from scanning"
	}
	return "being scanned"
}

func evidenceWord(hours int) string {
	if hours == 0 {
		return "nothing kept"
	}
	return core.FormatDuration(time.Duration(hours) * time.Hour)
}

func modelList(configured, fallback []string) string {
	if len(configured) == 0 {
		return strings.Join(fallback, "\n") + "\n_(merlin's defaults, tracked automatically)_"
	}
	return strings.Join(configured, "\n")
}

func mentionList(ids []string, render func(string) string) string {
	if len(ids) == 0 {
		return "none"
	}
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		out = append(out, render(id))
	}
	return strings.Join(out, " ")
}

func bullets(lines []string) string {
	if len(lines) == 0 {
		return "none"
	}
	return "- " + strings.Join(lines, "\n- ")
}

func orNone(lines []string) string {
	var kept []string
	for _, l := range lines {
		if strings.TrimSpace(l) != "" {
			kept = append(kept, l)
		}
	}
	if len(kept) == 0 {
		return "none"
	}
	sort.Strings(kept)
	return strings.Join(kept, ", ")
}
