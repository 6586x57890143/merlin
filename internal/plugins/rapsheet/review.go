package rapsheet

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/6586x57890143/merlin/internal/core"
	"github.com/6586x57890143/merlin/internal/scheduler"
)

// The model, used for reading rather than deciding.
//
// Three things here ask a model for prose: /rapsheet summary turns one
// member's sheet into a paragraph a mod can read in the time it takes to
// decide; a weekly review reads a month of the guild's entries looking for
// the things a ledger makes visible and nobody has time to look for (the
// same offence sentenced three different ways, warnings with no reason,
// voids clustering on one moderator); and a possible alt gets a second
// opinion on the notice, with a reason a mod can read. None of them decides
// anything: a summary is not a sanction, a review is a post, an opinion is
// a line under a Link button a human still has to click.
//
// The model is aimod's, reached through Reviewer and wired in
// cmd/bot/main.go, so it runs on the guild's key, against the guild's
// budget, under the same ZDR and provider rules as the classifier. What is
// sent is ledger text only: kinds, categories, points, dates, durations,
// the reasons moderators typed, and whether the actor was a person or the
// bot. Never a user id, never message content. A guild with no key, or one
// whose budget is spent, gets the plain version of each surface and a line
// saying why.

// Reviewer is the narrow slice of aimod this plugin uses. *aimod.Plugin
// satisfies it structurally. Nil means no model, and every surface here
// degrades to its plain form.
type Reviewer interface {
	Complete(ctx context.Context, guildID, system, user string) (string, error)
}

// WithReviewer attaches the model.
func (p *Plugin) WithReviewer(r Reviewer) *Plugin {
	p.reviewer = r
	return p
}

// unavailable reports whether err is the reviewer saying it cannot run
// (no key, budget spent) rather than that it failed. Checked through an
// anonymous interface so this package needs nothing of aimod's.
func unavailable(err error) bool {
	var u interface{ Unavailable() bool }
	return errors.As(err, &u) && u.Unavailable()
}

const (
	actionSummary = "rapsheet.summary"

	// summaryTimeout bounds one summary; opinionTimeout one alt opinion,
	// which rides on a join and must not hold the notice for long.
	summaryTimeout = 60 * time.Second
	opinionTimeout = 15 * time.Second

	// reviewWindow is how far back the weekly review reads, and
	// reviewMaxEntries how much of it. A month at 200 entries is a busy
	// server's worth; past that the review is a report, not a glance.
	reviewWindow     = 30 * 24 * time.Hour
	reviewMaxEntries = 200
	// reviewMaxChars caps the posted review. The mod channel is read in
	// passing, and a review that needs scrolling is not read at all.
	reviewMaxChars = 1500
)

// reviewSchedule is Monday morning UTC: after the weekend, which is when a
// small server sees most of its trouble, and early enough that the review
// is there when the first moderator looks in.
var reviewSchedule = func() core.CalendarSchedule {
	monday := time.Monday
	return core.CalendarSchedule{Weekday: &monday, HourUTC: 9}
}()

func reviewKey(guildID string) string { return scheduler.JobKey(guildID, "rapsheet-review") }

// --- the sheet as text ---------------------------------------------------------

// sheetText renders entries for a model: the same facts the sheet shows a
// moderator, without the mentions. Ages are in days, so the model is not
// asked to do date arithmetic.
func sheetText(entries []Entry, now time.Time, halfLife time.Duration) string {
	var b strings.Builder
	for _, e := range entries {
		age := now.Sub(e.CreatedAt)
		fmt.Fprintf(&b, "- #%d, %s, %s", e.ID, kindWords(e), agoWords(age))
		if e.Points > 0 {
			fmt.Fprintf(&b, ", %s, %d points (%.0f now)", categoryLabel(e.Category), e.Points, Decayed(e.Points, age, halfLife))
		}
		if e.ActorID == core.ActorSystem || e.ActorID == "" {
			b.WriteString(", by the bot")
		} else {
			b.WriteString(", by a moderator")
		}
		if e.Source == SourceAIMod {
			b.WriteString(" (AI moderation)")
		}
		if e.Reason != "" {
			fmt.Fprintf(&b, ": %q", clip(oneLine(e.Reason), maxReasonShown))
		}
		if e.Voided() {
			b.WriteString(" [VOIDED")
			if e.VoidReason != "" {
				fmt.Fprintf(&b, ": %q", clip(oneLine(e.VoidReason), maxVoidReasonShown))
			}
			b.WriteString("]")
		}
		b.WriteString("\n")
	}
	return b.String()
}

func agoWords(d time.Duration) string {
	switch {
	case d < time.Hour:
		return "less than an hour ago"
	case d < 2*time.Hour:
		return "an hour ago"
	case d < 24*time.Hour:
		return fmt.Sprintf("%d hours ago", int(d.Hours()))
	case d < 48*time.Hour:
		return "yesterday"
	default:
		return fmt.Sprintf("%d days ago", int(d.Hours()/24))
	}
}

const summarySystem = `You are helping a Discord moderator read one member's moderation record. Write a short, plain summary in at most 120 words: what has happened, whether there is a pattern (same category repeating, escalating, or isolated incidents), how recent it is, and what the server's ladder says the record currently stands at. Voided entries were mistakes and count for nothing; mention them only if they matter to the picture. Do not recommend a punishment and do not invent facts that are not in the record. Plain sentences, no headings, no bullet points, no em dashes.`

// --- /rapsheet summary ------------------------------------------------------------

func (p *Plugin) handleSummary(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate) {
	userID := core.LeafArgs(i)["user"].Value.(string)
	if err := core.DeferResponse(s, i); err != nil {
		p.log.Error("rapsheet: defer summary", "err", err)
		return
	}
	cfg := p.config(ctx, i.GuildID)
	sh, err := p.loadSheet(ctx, cfg, i.GuildID, userID)
	if err != nil {
		_ = core.FollowUpErr(s, i, "Summary", err)
		return
	}
	name := displayName(resolvedUser(i, userID), nil, userID)
	if len(sh.Entries) == 0 {
		_ = core.FollowUpOK(s, i, "Summary: "+name, "Nothing on record, so nothing to summarise.")
		return
	}
	if p.reviewer == nil {
		p.plainInsteadOfSummary(ctx, s, i, cfg, userID, name, "No model is wired into this build.")
		return
	}
	user := fmt.Sprintf("Server ladder: %s.\nCurrent score: %.0f, which is the %s band.\n\nRecord, newest first:\n%s",
		bandsWords(effectiveBands(cfg.Bands)), sh.Score, recWords(sh.Rec), sheetText(sh.Entries, p.now(), cfg.HalfLife))
	cctx, cancel := context.WithTimeout(ctx, summaryTimeout)
	defer cancel()
	out, err := p.reviewer.Complete(cctx, i.GuildID, summarySystem, user)
	if err != nil {
		if unavailable(err) {
			p.plainInsteadOfSummary(ctx, s, i, cfg, userID, name, err.Error())
			return
		}
		_ = core.FollowUpErr(s, i, "Summary", fmt.Errorf("the model did not answer: %w", err))
		return
	}
	embed := core.NewEmbed(core.ColorInfo, "Summary: "+name, core.TruncateEmbedDescription(strings.TrimSpace(out)),
		&discordgo.MessageEmbedField{Name: "Score", Value: fmt.Sprintf("%.0f (%s)", sh.Score, recWords(sh.Rec)), Inline: true},
		&discordgo.MessageEmbedField{Name: "Entries", Value: fmt.Sprintf("%d", len(sh.Entries)), Inline: true},
		&discordgo.MessageEmbedField{Name: "Written by", Value: "a model, from the record above; `/rapsheet view` is the record itself"})
	if err := core.FollowUpEmbed(s, i, embed); err != nil {
		p.log.Error("rapsheet: summary follow-up", "err", err)
	}
}

// plainInsteadOfSummary renders the sheet the ordinary way with a line
// saying why there is no prose.
func (p *Plugin) plainInsteadOfSummary(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate, cfg Config, userID, name, why string) {
	embed, components, err := p.renderFor(ctx, i.GuildID, userID, resolvedUser(i, userID), 0, false)
	if err != nil {
		_ = core.FollowUpErr(s, i, "Summary", err)
		return
	}
	embed.Title = "Summary unavailable: " + name
	embed.Description = core.TruncateEmbedDescription("No summary: " + why + "\n\n" + embed.Description)
	if err := core.FollowUpEmbedWithComponents(s, i, embed, components); err != nil {
		p.log.Error("rapsheet: plain summary follow-up", "err", err)
	}
}

// --- the weekly review ---------------------------------------------------------------

const reviewSystem = `You are reviewing a month of moderation decisions on a Discord server, from a ledger the moderators keep. Your job is consistency, not judgement of the members. Look for: the same category sentenced very differently; warnings or bans recorded with no reason; bans below the ladder's ban band, or nothing done where the ladder was reached; voided entries clustering on one moderator or one day; days with far more entries than usual. For each thing you find, say what and cite the case numbers. If the month looks consistent, say so in one sentence. At most 200 words, plain sentences, no headings, no em dashes. Never name or guess at who a moderator is; they appear only as "a moderator".`

// reconcileReviewJob registers the weekly review only where it can run:
// a mode other than off, a mod channel to post in, and a model to write
// it. Assumes the caller holds p.mu.
func (p *Plugin) reconcileReviewJob(ctx context.Context, guildID string) {
	if p.sched == nil {
		return
	}
	cfg := p.config(ctx, guildID)
	want := p.reviewer != nil && cfg.EscalationMode != ModeOff && cfg.ModChannelID != ""
	key := reviewKey(guildID)
	switch {
	case want && !p.reviewRegistered[guildID]:
		if err := p.sched.Register(key, core.CronSpec{Schedule: reviewSchedule},
			func(ctx context.Context) error { return p.weeklyReview(ctx, guildID) }); err != nil {
			p.log.Error("rapsheet: register review job", "guild", guildID, "err", err)
			return
		}
		// Seeded, so a freshly configured guild gets its first review next
		// Monday rather than on the next tick: the review costs a model
		// call, and "you turned this on" is not a week of decisions.
		if err := p.sched.Seed(ctx, key, p.now()); err != nil {
			p.log.Error("rapsheet: seed review job", "guild", guildID, "err", err)
		}
		p.reviewRegistered[guildID] = true
	case !want && p.reviewRegistered[guildID]:
		if err := p.sched.Unregister(key); err != nil {
			p.log.Error("rapsheet: unregister review job", "guild", guildID, "err", err)
			return
		}
		delete(p.reviewRegistered, guildID)
	}
}

func (p *Plugin) weeklyReview(ctx context.Context, guildID string) error {
	cfg := p.config(ctx, guildID)
	if p.reviewer == nil || cfg.ModChannelID == "" {
		return nil
	}
	entries, err := p.store.GuildEntries(ctx, guildID, p.now().Add(-reviewWindow), reviewMaxEntries)
	if err != nil {
		return fmt.Errorf("rapsheet review: read entries: %w", err)
	}
	if len(entries) == 0 {
		return nil
	}
	// Grouped by category so the model sees like with like, which is the
	// comparison it is being asked to make.
	sort.SliceStable(entries, func(a, b int) bool {
		if entries[a].Category != entries[b].Category {
			return entries[a].Category < entries[b].Category
		}
		return entries[a].CreatedAt.After(entries[b].CreatedAt)
	})
	user := fmt.Sprintf("Server ladder: %s.\nHalf-life: %s.\n\nEntries from the last 30 days, grouped by category:\n%s",
		bandsWords(effectiveBands(cfg.Bands)), core.FormatDuration(cfg.HalfLife), sheetText(entries, p.now(), cfg.HalfLife))
	out, err := p.reviewer.Complete(ctx, guildID, reviewSystem, user)
	if err != nil {
		if unavailable(err) {
			// No key or no budget is a configuration, not a wedged job.
			p.log.Info("rapsheet review: model unavailable, skipping", "guild", guildID, "err", err)
			return nil
		}
		return fmt.Errorf("rapsheet review: %w", err)
	}
	out = strings.TrimSpace(out)
	if len(out) > reviewMaxChars {
		out = out[:reviewMaxChars-3] + "..."
	}
	embed := core.NewEmbed(core.ColorInfo, "Weekly rapsheet review",
		fmt.Sprintf("%d entries in the last 30 days. What a model reading them noticed:\n\n%s\n\n"+
			"Read it as a prompt to look, not a verdict; `/rapsheet view` is the record.", len(entries), out))
	if _, err := p.ops(guildID).ChannelMessageSendComplex(cfg.ModChannelID, &discordgo.MessageSend{
		Embeds: []*discordgo.MessageEmbed{embed}, Files: core.EmbedFiles(embed),
	}); err != nil {
		return fmt.Errorf("rapsheet review: post: %w", err)
	}
	return nil
}

// --- the alt second opinion -----------------------------------------------------------

const opinionSystem = `A member has just joined a Discord server and some cheap signals suggest they might be the same person as an account already on the server's moderation record. You are given only what the bot can see: account creation times, join time, whether the uploaded avatar is identical, how similar the names are, and what the account on record was recently sanctioned for. In one or two plain sentences, say how strong you think the match is and why, and what a moderator could check that the bot cannot (a shared writing style, a self-introduction, the same friends). Be honest about weak evidence: identical avatars are strong, similar names alone are weak, and a join shortly after somebody's ban is suggestive but common on a busy server. No em dashes.`

// altOpinion asks the model for a line under the Link button. Best effort,
// short timeout; "" on any failure, and the notice goes out without it.
func (p *Plugin) altOpinion(ctx context.Context, cfg Config, joiner *discordgo.User, joinedAt time.Time, h AltHint, candidate CaseFile) string {
	if p.reviewer == nil {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Signals the bot found: %s (score %d).\n", strings.Join(h.Signals, "; "), h.Score)
	if made, err := discordgo.SnowflakeTimestamp(joiner.ID); err == nil {
		fmt.Fprintf(&b, "Joiner's account was created %s and joined %s.\n", agoWords(joinedAt.Sub(made)), agoWords(p.now().Sub(joinedAt)))
	}
	if made, err := discordgo.SnowflakeTimestamp(candidate.UserID); err == nil {
		fmt.Fprintf(&b, "Account on record was created %s.\n", agoWords(p.now().Sub(made)))
	}
	fmt.Fprintf(&b, "Names: joiner %q / %q, on record %q / %q.\n", joiner.Username, joiner.GlobalName, candidate.Username, candidate.GlobalName)
	if sh, err := p.loadSheet(ctx, cfg, cfg.GuildID, candidate.UserID); err == nil {
		fmt.Fprintf(&b, "Record of the account on record (score %.0f):\n%s", sh.Score, sheetText(sh.Entries, p.now(), cfg.HalfLife))
	}
	cctx, cancel := context.WithTimeout(ctx, opinionTimeout)
	defer cancel()
	out, err := p.reviewer.Complete(cctx, cfg.GuildID, opinionSystem, b.String())
	if err != nil {
		if !unavailable(err) {
			p.log.Warn("rapsheet: alt opinion", "guild", cfg.GuildID, "err", err)
		}
		return ""
	}
	return clip(strings.TrimSpace(out), 400)
}
