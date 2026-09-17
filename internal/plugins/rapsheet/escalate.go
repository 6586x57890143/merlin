package rapsheet

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/6586x57890143/merlin/internal/core"
	"github.com/6586x57890143/merlin/internal/discordguard"
	"github.com/6586x57890143/merlin/internal/voice"
)

// The ladder: what a record adds up to, and what happens when it crosses a
// line.
//
// Every scored entry re-reads the member's whole sheet (their link group's,
// if they have one), sums the decayed points, and asks Ladder which band
// that reaches. Crossing into a band the member has not been actioned at
// inside a half-life owes that band's consequence. In suggest mode the
// consequence is posted to the mod channel with an Apply button; in auto
// mode it is applied, with two exceptions that are the whole safety
// argument. It is never applied against staff (CanModerate with a nil
// actor, the same rule as roles.JailAutomatic), and it is never applied on
// the back of a moderator's own command: a mod who just typed /rapsheet
// warn decided the consequence for that offence was a warning, and a bot
// jailing on top of it is the surprise this rule exists to prevent. They
// get a suggestion instead, one click away.
//
// Idempotency lives in the ledger, not in memory. A suggestion is an entry
// (kind suggestion, source ladder, the band it was for) and so is a
// consequence the ladder applied, so "already suggested or actioned at this
// band" is a read of the sheet, survives restarts, and shows in the case
// file. A standing consequence at least as strong as the recommendation
// also satisfies it: the ladder never suggests a 24h jail over the 24h jail
// aimod just applied, or over a mod's longer one.

// Jailer is the narrow slice of internal/plugins/roles this plugin needs,
// the same seam aimod uses. Satisfied structurally by *roles.Plugin and
// wired in cmd/bot/main.go. Nil means jail bands cannot be applied and say
// so.
type Jailer interface {
	JailAutomatic(ctx context.Context, guildID, userID string, duration time.Duration, reason string, targetConsented bool) error
}

// WithJailer attaches the jail mechanism jail bands are applied through.
func (p *Plugin) WithJailer(j Jailer) *Plugin {
	p.jailer = j
	return p
}

const (
	actionApply = "rapsheet.apply"

	suggestPrefix        = "rapsheet:sugg:"
	suggestApplyPrefix   = suggestPrefix + "apply:"
	suggestDismissPrefix = suggestPrefix + "dismiss:"
)

// Priors implements aimod.History: the member's scored offences (across
// their link group) since `since`. ok is false when the rapsheet is
// disabled in the guild, so aimod falls back to its own count rather than
// reading a switched-off ledger as a clean record.
func (p *Plugin) Priors(ctx context.Context, guildID, userID string, since time.Time) (int, bool, error) {
	if !p.enabled(guildID) {
		return 0, false, nil
	}
	n, err := p.store.CountScored(ctx, guildID, p.group(ctx, guildID, userID), since)
	if err != nil {
		return 0, true, err
	}
	return n, true, nil
}

// escalate runs after every recorded entry and decides whether the ladder
// owes anything. Never returns an error: nothing here may fail the entry.
// What it returns is one line for the confirmation the moderator sees:
// what the ladder did, or "" when it did nothing.
func (p *Plugin) escalate(ctx context.Context, cfg Config, e Entry) string {
	switch {
	case cfg.EscalationMode == ModeOff:
		return ""
	case e.Voided(), e.Points == 0, e.Source == SourceLadder:
		return ""
	}
	switch e.Kind {
	case KindNote, KindUnban, KindRelease, KindSuggestion:
		return ""
	}

	sh, err := p.loadSheet(ctx, cfg, e.GuildID, e.UserID)
	if err != nil {
		p.log.Error("rapsheet: load sheet for escalation", "guild", e.GuildID, "user", e.UserID, "err", err)
		return ""
	}
	rec := sh.Rec
	if rec.Action == ActionNone {
		return ""
	}
	now := p.now()
	if prior, ok := ladderRowAtOrAbove(sh.Entries, rec.Band, now.Add(-cfg.HalfLife)); ok {
		verb := "applied"
		if prior.Kind == KindSuggestion {
			verb = "suggested"
		}
		return fmt.Sprintf("Their record is in the %s band; the ladder already %s it (case #%d).", recWords(rec), verb, prior.ID)
	}
	if st, ok := standing(sh.Entries, now); ok && covers(st, rec, now) {
		return ""
	}

	switch {
	case cfg.EscalationMode == ModeAuto && (e.Source == SourceAIMod || e.Source == SourceDiscord):
		if _, err := p.applyLadder(ctx, cfg, e.UserID, rec, sh.Score, core.ActorSystem); err != nil {
			// Refused (staff, unresolvable) or failed. Either way the
			// consequence row is voided with the reason, so the sheet shows
			// what was tried, and the next crossing tries again.
			p.log.Warn("rapsheet: automatic escalation not applied", "guild", e.GuildID, "user", e.UserID, "action", rec.Action, "err", err)
			return fmt.Sprintf("The ladder reached %s but could not apply it: %v.", recWords(rec), err)
		}
		return fmt.Sprintf("The ladder applied %s.", recWords(rec))
	default:
		return p.suggest(ctx, cfg, e, rec, sh)
	}
}

// ladderRowAtOrAbove is the ladder's most recent live suggestion or
// consequence at band or above since `since`. Voided rows do not count: a
// dismissed suggestion is a mod saying "not this time", and a later
// crossing of the same band, after more offences, is a different time.
func ladderRowAtOrAbove(entries []Entry, band int, since time.Time) (Entry, bool) {
	for _, e := range entries {
		if e.Source != SourceLadder || e.Voided() || e.CreatedAt.Before(since) || e.Band < band {
			continue
		}
		return e, true
	}
	return Entry{}, false
}

// covers reports whether a standing consequence already does at least what
// the recommendation asks: as strong an action, lasting at least as long.
func covers(st Entry, rec Recommendation, now time.Time) bool {
	if kindStrength(st.Kind) < rec.Action.Strength() {
		return false
	}
	if st.EndsAt == nil {
		return true
	}
	return !st.EndsAt.Before(now.Add(rec.Duration))
}

// suggest records the recommendation and posts it for a moderator.
//
// Nothing is recorded when there is nowhere to post: a suggestion nobody
// can see would still satisfy the idempotency check and silence the band
// for a half-life, so a guild that sets its mod channel later would hear
// nothing about the members who crossed a line before it did.
func (p *Plugin) suggest(ctx context.Context, cfg Config, trigger Entry, rec Recommendation, sh sheet) string {
	userID := trigger.UserID
	if cfg.ModChannelID == "" {
		p.log.Warn("rapsheet: ladder has a suggestion and no mod channel to post it in; set one with /rapsheet configure mod-channel",
			"guild", cfg.GuildID, "user", userID, "action", rec.Action)
		return fmt.Sprintf("Their record reached %s, but no mod channel is set to suggest it in.", recWords(rec))
	}
	// The reason is parsed back by settle and shown on the sheet, so it
	// carries the score and what tipped it: the case a moderator opens
	// first when deciding.
	e, written, err := p.record(ctx, cfg, newEntry{
		GuildID: cfg.GuildID, UserID: userID, Kind: KindSuggestion, Category: CategoryOther,
		ActorID: core.ActorSystem, Reason: fmt.Sprintf("score %.0f after #%d: %s", sh.Score, trigger.ID, recWords(rec)),
		Duration: rec.Duration, Source: SourceLadder, Band: rec.Band,
	})
	if err != nil || !written {
		p.log.Error("rapsheet: record suggestion", "guild", cfg.GuildID, "user", userID, "err", err)
		return ""
	}
	var standingLine string
	if st, ok := standing(sh.Entries, p.now()); ok {
		standingLine = "Already " + standingWords(st) + " (#" + strconv.FormatInt(st.ID, 10) + ")."
	}
	embed, components := suggestionEmbed(e, rec, sh.Score, tippedLine(trigger, p.now(), cfg.HalfLife), standingLine, "", false)
	if _, err := p.ops(cfg.GuildID).ChannelMessageSendComplex(cfg.ModChannelID, &discordgo.MessageSend{
		Embeds:     []*discordgo.MessageEmbed{embed},
		Components: components,
		Files:      core.EmbedFiles(embed),
	}); err != nil {
		p.log.Error("rapsheet: post suggestion", "guild", cfg.GuildID, "channel", cfg.ModChannelID, "err", err)
		return fmt.Sprintf("The ladder reached %s; the suggestion could not be posted in %s.", recWords(rec), core.MentionChannel(cfg.ModChannelID))
	}
	return fmt.Sprintf("The ladder suggested %s in %s.", recWords(rec), core.MentionChannel(cfg.ModChannelID))
}

// tippedLine is the "what tipped it" block of a suggestion: the entry in
// the sheet's own format, with what it counts for now.
func tippedLine(trigger Entry, now time.Time, halfLife time.Duration) string {
	var b strings.Builder
	fmt.Fprintf(&b, "**Tipped by:** #%d %s", trigger.ID, kindWords(trigger))
	if trigger.Points > 0 {
		fmt.Fprintf(&b, " · %s · %d pts (%.0f now)", categoryLabel(trigger.Category), trigger.Points, Decayed(trigger.Points, now.Sub(trigger.CreatedAt), halfLife))
	}
	if trigger.Reason != "" {
		b.WriteString("\n> " + clip(oneLine(trigger.Reason), maxReasonShown))
	}
	return b.String()
}

// suggestionEmbed is the mod-channel post. tipped is tippedLine's block
// (empty once settled and read back off the suggestion's reason instead);
// standingLine says what the member is already under, the main reason a
// mod dismisses. note is appended when there is something to say; settled
// drops the buttons and the invitation to click them, which happens once
// the suggestion has been applied or dismissed and never for a note that
// leaves it open (a mod without the rank to apply a ban).
func suggestionEmbed(e Entry, rec Recommendation, score float64, tipped, standingLine, note string, settled bool) (*discordgo.MessageEmbed, []discordgo.MessageComponent) {
	var b strings.Builder
	fmt.Fprintf(&b, "%s's record has reached **%.0f points**, which puts them at **%s** on the ladder.", core.MentionUser(e.UserID), score, recWords(rec))
	if tipped != "" {
		b.WriteString("\n\n" + tipped)
	} else if _, after, ok := strings.Cut(e.Reason, "after #"); ok {
		if n, _, ok := strings.Cut(after, ":"); ok {
			fmt.Fprintf(&b, "\n\n**Tipped by:** case #%s", n)
		}
	}
	if standingLine != "" {
		b.WriteString("\n\n" + standingLine)
	}
	if settled {
		fmt.Fprintf(&b, "\n\nThis is case #%d on their sheet.", e.ID)
	} else {
		fmt.Fprintf(&b, "\n\nApply it, or dismiss it; either way it is case #%d on their sheet. `/rapsheet view` shows the whole record.", e.ID)
	}
	if note != "" {
		b.WriteString("\n\n" + note)
	}
	color := core.ColorWarning
	if settled {
		color = core.ColorInfo
	}
	embed := core.NewEmbed(color, "Ladder: "+recWords(rec), core.TruncateEmbedDescription(b.String()))
	if settled {
		return embed, nil
	}
	id := strconv.FormatInt(e.ID, 10)
	return embed, []discordgo.MessageComponent{discordgo.ActionsRow{Components: []discordgo.MessageComponent{
		discordgo.Button{Label: "Apply " + recWords(rec), Style: discordgo.DangerButton, CustomID: suggestApplyPrefix + id},
		discordgo.Button{Label: "Dismiss", Style: discordgo.SecondaryButton, CustomID: suggestDismissPrefix + id},
	}}}
}

// handleSuggestion is both buttons. Everything is re-derived from the
// ledger on the click: the suggestion may have been dismissed or applied
// by another mod, the band may have been reconfigured, and a click on a
// week-old message is still a click.
func (p *Plugin) handleSuggestion(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate, customID string) {
	apply := strings.HasPrefix(customID, suggestApplyPrefix)
	idStr := strings.TrimPrefix(strings.TrimPrefix(customID, suggestApplyPrefix), suggestDismissPrefix)
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		p.log.Error("rapsheet: parse suggestion id", "custom_id", customID, "err", err)
		return
	}
	if err := core.DeferUpdate(s, i); err != nil {
		p.log.Error("rapsheet: defer suggestion click", "err", err)
		return
	}
	cfg := p.config(ctx, i.GuildID)
	sug, err := p.store.Entry(ctx, i.GuildID, id)
	if err != nil || sug.Kind != KindSuggestion {
		p.log.Error("rapsheet: suggestion click on a missing case", "guild", i.GuildID, "case", id, "err", err)
		return
	}
	rec, ok := bandRecommendation(cfg, sug.Band)
	if !ok {
		p.settle(s, i, sug, rec, "The ladder has been reconfigured since; this band no longer exists.", true)
		return
	}
	if sug.Voided() {
		p.settle(s, i, sug, rec, fmt.Sprintf("Already dismissed by %s.", actorWords(sug.VoidedBy, false)), true)
		return
	}

	if !apply {
		now := p.now()
		if err := p.store.Void(ctx, i.GuildID, sug.ID, actorID(i), "dismissed", now); err != nil {
			p.log.Error("rapsheet: dismiss suggestion", "guild", i.GuildID, "case", sug.ID, "err", err)
			return
		}
		sug.VoidedAt, sug.VoidedBy, sug.VoidReason = &now, actorID(i), "dismissed"
		p.afterAmend(ctx, sug)
		if err := p.audit.Record(ctx, i.GuildID, actorID(i), "rapsheet.suggestion_dismissed", "",
			fmt.Sprintf("case #%d user=%s %s", sug.ID, core.MentionUser(sug.UserID), recWords(rec))); err != nil {
			p.log.Error("rapsheet: audit dismiss", "guild", i.GuildID, "err", err)
		}
		p.settle(s, i, sug, rec, fmt.Sprintf("Dismissed by %s.", core.MentionUser(actorID(i))), true)
		return
	}

	// A ban is TierAdmin however the button was registered. The component
	// itself is TierMod so a mod can apply a jail, and the bar is raised
	// here for the one action that would otherwise let a button do what the
	// command refuses.
	if rec.Action == ActionBan {
		if err := p.perms.Authorize(i, core.PermSpec{Tier: core.TierAdmin, Action: actionBan}); err != nil {
			// Still open: the buttons stay for an admin.
			p.settle(s, i, sug, rec, fmt.Sprintf("%s tried to apply this, but a ban needs an admin.", core.MentionUser(actorID(i))), false)
			return
		}
	}
	// Somebody else may have applied it between the post and this click,
	// or two mods may be clicking at once.
	sh, err := p.loadSheet(ctx, cfg, i.GuildID, sug.UserID)
	if err != nil {
		p.log.Error("rapsheet: load sheet on apply", "guild", i.GuildID, "err", err)
		return
	}
	if maxAppliedBand(sh.Entries, sug.CreatedAt) >= sug.Band {
		p.settle(s, i, sug, rec, "Already applied.", true)
		return
	}
	if sh.Rec.Band < sug.Band {
		// Something was voided since; the record no longer reaches this
		// band and applying would punish for points that are gone.
		p.withdrawStaleSuggestions(ctx, cfg, i.GuildID, sug.UserID)
		p.settle(s, i, sug, rec, fmt.Sprintf("Withdrawn: their record no longer reaches this band (score %.0f now).", sh.Score), true)
		return
	}
	if !p.claim(sug.ID) {
		return
	}
	defer p.unclaim(sug.ID)

	outcome, err := p.applyLadder(ctx, cfg, sug.UserID, rec, sh.Score, actorID(i))
	if err != nil {
		// Still open: a transient failure (Discord down, jail role missing)
		// is something an admin may fix and retry.
		p.settle(s, i, sug, rec, fmt.Sprintf("%s tried to apply this and it failed: %v", core.MentionUser(actorID(i)), err), false)
		return
	}
	p.settle(s, i, sug, rec, fmt.Sprintf("Applied by %s: %s.", core.MentionUser(actorID(i)), outcome), true)
}

// bandRecommendation looks a stored band index up in the current ladder.
func bandRecommendation(cfg Config, band int) (Recommendation, bool) {
	bands := cfg.Bands
	if len(bands) == 0 {
		bands = defaultBands
	}
	if band < 0 || band >= len(bands) {
		return Recommendation{}, false
	}
	b := bands[band]
	return Recommendation{Band: band, Action: b.Action, Duration: b.Duration}, true
}

// maxAppliedBand is the highest band a ladder *consequence* (not a
// suggestion) has been recorded for since `since`.
func maxAppliedBand(entries []Entry, since time.Time) int {
	best := -1
	for _, e := range entries {
		if e.Source != SourceLadder || e.Kind == KindSuggestion || e.Voided() || e.CreatedAt.Before(since) {
			continue
		}
		if e.Band > best {
			best = e.Band
		}
	}
	return best
}

func (p *Plugin) settle(s *discordgo.Session, i *discordgo.InteractionCreate, sug Entry, rec Recommendation, outcome string, settled bool) {
	var score float64
	_, _ = fmt.Sscanf(sug.Reason, "score %f", &score)
	// Settled from the stored suggestion alone; the tipping entry is
	// re-read so the settled message still says why.
	tipped := ""
	if _, after, ok := strings.Cut(sug.Reason, "after #"); ok {
		if n, _, ok := strings.Cut(after, ":"); ok {
			if id, err := strconv.ParseInt(n, 10, 64); err == nil {
				if trig, err := p.store.Entry(context.Background(), sug.GuildID, id); err == nil {
					tipped = tippedLine(trig, p.now(), p.config(context.Background(), sug.GuildID).HalfLife)
				}
			}
		}
	}
	embed, components := suggestionEmbed(sug, rec, score, tipped, "", outcome, settled)
	if err := core.UpdateEmbedWithComponents(s, i, embed, components); err != nil {
		p.log.Error("rapsheet: update suggestion message", "guild", i.GuildID, "err", err)
	}
}

func (p *Plugin) claim(id int64) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.applying[id] {
		return false
	}
	p.applying[id] = true
	return true
}

func (p *Plugin) unclaim(id int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.applying, id)
}

// applyLadder carries out a recommendation against userID. actor is who
// decided it: core.ActorSystem for auto mode, the clicking mod for a
// suggestion.
//
// Refusals come first and cost nothing: the bootstrap operator, anyone
// CanModerate says is staff, anyone whose rank cannot be resolved. Then the
// consequence entry is written, then Discord (or roles) is asked, and a
// refusal voids the entry with the reason, exactly as the command leaves
// do. The reason handed to roles carries ladderReasonPrefix so the jail
// coming back over the bus is recognised as this one.
func (p *Plugin) applyLadder(ctx context.Context, cfg Config, userID string, rec Recommendation, score float64, actor string) (string, error) {
	if p.perms.IsBootstrapAdmin(userID) {
		return "", errors.New("the bootstrap operator cannot be actioned automatically")
	}
	member, err := p.ops(cfg.GuildID).GuildMember(cfg.GuildID, userID)
	present := err == nil
	if err != nil {
		if !core.IsUnknownResource(err) {
			return "", fmt.Errorf("look up member: %w", err)
		}
		member = &discordgo.Member{User: &discordgo.User{ID: userID}}
	}
	if err := p.perms.CanModerate(cfg.GuildID, nil, userID, member.Roles); err != nil {
		return "", fmt.Errorf("refused: %w", err)
	}
	if !present && rec.Action != ActionBan {
		return "", fmt.Errorf("%s is not in the server", core.MentionUser(userID))
	}

	kind := KindNote
	switch rec.Action {
	case ActionJail:
		kind = KindJail
	case ActionTimeout:
		kind = KindTimeout
	case ActionBan:
		kind = KindBan
	}
	in := newEntry{
		GuildID: cfg.GuildID, UserID: userID, Kind: kind, Category: CategoryServerRule, ActorID: actor,
		Reason: fmt.Sprintf("ladder: score %.0f reached %s", score, recWords(rec)), Source: SourceLadder, Band: rec.Band,
	}
	if rec.Duration > 0 {
		until := p.now().Add(rec.Duration)
		in.Duration, in.EndsAt = rec.Duration, &until
	}
	e, _, err := p.record(ctx, cfg, in)
	if err != nil {
		return "", err
	}
	reason := fmt.Sprintf("%s%d: score %.0f reached %s", ladderReasonPrefix, e.ID, score, recWords(rec))
	guild := p.guildName(cfg.GuildID)

	var applyErr error
	outcome := recWords(rec)
	switch rec.Action {
	case ActionNotice:
		p.dm(ctx, cfg.GuildID, userID, voice.KeyStrikeNotice, "A note about your record", core.ColorWarning,
			map[string]string{"guild": guild})
		outcome = "notice sent"
	case ActionJail:
		if p.jailer == nil {
			applyErr = errors.New("jail is not available in this build")
		} else {
			applyErr = p.jailer.JailAutomatic(ctx, cfg.GuildID, userID, rec.Duration, reason, false)
		}
	case ActionTimeout:
		applyErr = p.ops(cfg.GuildID).GuildMemberTimeout(cfg.GuildID, userID, e.EndsAt)
		if applyErr == nil {
			p.dm(ctx, cfg.GuildID, userID, voice.KeyTimeoutNotice, "Timed out", core.ColorWarning,
				map[string]string{"guild": guild, "until": relativeTimestamp(*e.EndsAt)}, reasonFields(CategoryServerRule, "your record reached "+recWords(rec))...)
		}
	case ActionBan:
		p.dm(ctx, cfg.GuildID, userID, voice.KeyBanNotice, "Banned", core.ColorError,
			map[string]string{"guild": guild, "until": relativeTimestamp(*e.EndsAt)}, reasonFields(CategoryServerRule, "your record reached "+recWords(rec))...)
		applyErr = p.ops(cfg.GuildID).GuildBanCreateWithReason(cfg.GuildID, userID, reason, 0)
	}

	switch {
	case applyErr == nil:
	case discordguard.Skipped(applyErr):
		p.voidFailed(ctx, e, errors.New("paused or dry-run"))
		return "", applyErr
	default:
		p.voidFailed(ctx, e, applyErr)
		return "", applyErr
	}

	if err := p.audit.Record(ctx, cfg.GuildID, actor, "rapsheet.escalated", "",
		fmt.Sprintf("case #%d user=%s score=%.0f %s", e.ID, core.MentionUser(userID), score, recWords(rec))); err != nil {
		p.log.Error("rapsheet: audit escalation", "guild", cfg.GuildID, "err", err)
	}
	if rec.Action == ActionBan {
		p.mu.Lock()
		p.reconcileSweepJob(ctx, cfg.GuildID)
		p.mu.Unlock()
	}
	return outcome, nil
}

// withdrawStaleSuggestions voids open suggestions the record no longer
// supports, after an entry was voided or reversed. A suggestion made off a
// warning that turned out to be a misread would otherwise sit open in the
// mod channel asking for a jail nobody is owed, and, since an open
// suggestion satisfies the band check, it would also silence every lower
// band for a half-life. The mod-channel post is not edited here (its message
// id is not kept); a click on it finds the suggestion voided and says so.
func (p *Plugin) withdrawStaleSuggestions(ctx context.Context, cfg Config, guildID, userID string) {
	sh, err := p.loadSheet(ctx, cfg, guildID, userID)
	if err != nil {
		p.log.Error("rapsheet: load sheet to withdraw suggestions", "guild", guildID, "user", userID, "err", err)
		return
	}
	for _, e := range sh.Entries {
		if e.Kind != KindSuggestion || e.Voided() || e.Band <= sh.Rec.Band {
			continue
		}
		now := p.now()
		reason := fmt.Sprintf("withdrawn: the record no longer reaches this band (score %.0f)", sh.Score)
		if err := p.store.Void(ctx, guildID, e.ID, core.ActorSystem, reason, now); err != nil {
			p.log.Error("rapsheet: withdraw suggestion", "guild", guildID, "case", e.ID, "err", err)
			continue
		}
		e.VoidedAt, e.VoidedBy, e.VoidReason = &now, core.ActorSystem, reason
		p.afterAmend(ctx, e)
	}
}
