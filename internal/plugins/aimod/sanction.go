package aimod

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/6586x57890143/merlin/internal/core"
	"github.com/6586x57890143/merlin/internal/discordguard"
)

// Sanctions: jailing the member behind a confirmed violation, for a length
// that scales with how bad it was and how often they have done it.
//
// Jail rather than Discord's own timeout, everywhere it is available. A
// timeout is a mute; jail strips the member back to a marker role with a
// channel allowlist, keeps a snapshot so everything comes back, releases
// itself on a schedule, shows up in /roles list, and is undone by any mod
// with /roles release. It is this bot's moderation primitive and an
// automated action should not invent a second, weaker one alongside it.
// Timeout survives only as the fallback for when jail cannot be applied at
// all; see sanction below.

// Jailer is the narrow slice of internal/plugins/roles this plugin needs.
//
// Declared here and satisfied structurally by *roles.Plugin, wired once in
// cmd/bot/main.go, so this package does not import that one. The same seam
// pattern as rotation.SettingsProvider and adminconfig.SettingsAdmin, and
// the reason plugins in this codebase can depend on each other's behaviour
// without depending on each other's packages.
type Jailer interface {
	// targetConsented waives the rank check that would otherwise refuse an
	// admin-equivalent target. It is set only from this plugin's own opt-in
	// list, which only the member themselves (or the bootstrap operator) can
	// put them on. See optin.go.
	JailAutomatic(ctx context.Context, guildID, userID string, duration time.Duration, reason string, targetConsented bool) error
}

// Base sentences by how serious the policy area is.
//
// The severity comes from the policy file, so the ladder and the definition
// it is scaling cannot drift apart: adding a bucket means writing its
// severity down in the same file as everything else about it.
//
// The critical tier is a day rather than an hour because those are the
// categories that get a server terminated, and a member who posted one is a
// member a moderator needs time to look at properly, in the morning, rather
// than one who should be back in the channel before anybody notices.
var severityBase = map[string]time.Duration{
	"critical": 24 * time.Hour,
	"high":     8 * time.Hour,
	"medium":   2 * time.Hour,
	"low":      30 * time.Minute,
}

// unknownSeverityBase is used for a policy file whose severity is missing or
// unrecognised. The middle of the ladder, not the top: a configuration gap
// should not hand somebody a day in jail.
const unknownSeverityBase = 2 * time.Hour

// abuseBase is the sentence for exhausting the per-member scan ceiling,
// which is to say for deliberately burning the server's moderation budget.
// It sits between medium and high: the behaviour is unambiguous, but the
// evidence is statistical rather than a specific thing somebody said.
const abuseBase = 4 * time.Hour

// Repeat escalation. Each prior sanction inside repeatWindow doubles the
// sentence, up to maxRepeatDoublings, and the whole thing is capped.
//
// Doubling rather than adding, because the signal is about the person and
// not the message: someone on their fourth removal this month is not making
// the same mistake four times, and a linear ladder takes far too long to say
// so. Capped because an unbounded doubling reaches "effectively permanent"
// in about a week of bad days, and a permanent ban dressed up as arithmetic
// is a decision a human should be making.
const (
	repeatWindow       = 30 * 24 * time.Hour
	maxRepeatDoublings = 3
	maxSanction        = 7 * 24 * time.Hour
)

// severityOf reads a bucket's severity out of the loaded catalogue, so the
// ladder and the definition it is scaling cannot drift apart. An unknown
// bucket yields an empty string, which sanctionFor lands mid-ladder.
func (p *Plugin) severityOf(bucket Bucket) string {
	if pol, ok := p.policies[bucket]; ok {
		return pol.Severity
	}
	return ""
}

// sanctionFor computes how long a member should be jailed.
//
// Pure, so the ladder can be tested exhaustively without a database, a
// Discord session or a model. That matters more here than in most of this
// package: this function decides how long a real person loses access to a
// community for, and it should be possible to read its whole behaviour off
// a table of inputs.
func sanctionFor(severity string, priorSanctions int) time.Duration {
	base, ok := severityBase[severity]
	if !ok {
		base = unknownSeverityBase
	}
	return escalate(base, priorSanctions)
}

func escalate(base time.Duration, priorSanctions int) time.Duration {
	doublings := min(max(priorSanctions, 0), maxRepeatDoublings)
	d := base << doublings
	return min(d, maxSanction)
}

// sanction jails the member behind one confirmed violation.
//
// Everything about it is best-effort and non-fatal: the message has already
// been dealt with, the incident is already recorded, and a jail that could
// not be applied must never turn a successful removal into a failed one.
// That is the same log-and-continue policy every audit call site in this
// codebase follows, for the same reason.
func (p *Plugin) sanction(ctx context.Context, cfg Config, c candidate, bucket Bucket, reason string) {
	// Explicitly SanctionJail, not "anything except off". A guild row holding
	// an empty or unrecognised value would otherwise jail people, and the one
	// direction this must never fail in is toward punishing somebody because
	// a column was not what this build expected.
	if cfg.Mode != ModeEnforce || cfg.SanctionAction != SanctionJail {
		return
	}

	priors, err := p.store.CountSanctions(ctx, cfg.GuildID, c.AuthorID, p.now().Add(-repeatWindow))
	if err != nil {
		// Not fatal, but it must fail toward leniency: an unreadable history
		// is not evidence of a history.
		p.log.Error("aimod: count prior sanctions", "guild", cfg.GuildID, "err", err)
		priors = 0
	}
	duration := sanctionFor(p.severityOf(bucket), priors)

	p.applySanction(ctx, cfg, c, bucket, duration, priors, fmt.Sprintf(
		"automatic: %s (Discord %s policy)", reason, strings.ReplaceAll(string(bucket), "_", " ")))
}

// sanctionForAbuse jails a member for exhausting their scan ceiling, and
// clears the messages that got them there.
//
// The clearing is the point, and without it the ceiling was an exploit. Past
// it this plugin stops paying to confirm anything that member posts, so their
// messages carry on being flagged and stop being removed: five deliberate
// trips bought ten minutes of saying whatever you liked. The free rungs still
// ran throughout, so a phishing link or a leaked token was still deleted, but
// anything that needed a model to read it was not.
//
// A jail alone does not fix that. It stops the next message and leaves every
// message already posted during the window sitting in the channel, which is
// the half a moderator would have to clean up by hand. So the flags raised
// while the member was over the ceiling are collected and acted on together,
// with the jail, in one pass.
func (p *Plugin) sanctionForAbuse(ctx context.Context, cfg Config, c candidate) {
	priors, err := p.store.CountSanctions(ctx, cfg.GuildID, c.AuthorID, p.now().Add(-repeatWindow))
	if err != nil {
		p.log.Error("aimod: count prior sanctions", "guild", cfg.GuildID, "err", err)
		priors = 0
	}
	p.clearPendingFlags(ctx, cfg, c.AuthorID)
	// BucketSpam, which is where Discord's own platform-manipulation rules
	// live, and the closest honest label for "generating flagged content
	// faster than this server can afford to read it".
	p.applySanction(ctx, cfg, c, BucketSpam, escalate(abuseBase, priors), priors,
		"automatic: repeatedly tripping the moderation filter, which drains this server's scanning budget")
}

// applySanction is the one place a jail is actually applied, so the
// violation path and the abuse path cannot diverge in behaviour, only in the
// duration they arrived at. Same reasoning as roles.jailMany being shared
// between the single and bulk jail commands.
func (p *Plugin) applySanction(ctx context.Context, cfg Config, c candidate, bucket Bucket, duration time.Duration, priors int, reason string) {
	// Recorded before the jail is attempted, and recorded whether or not it
	// succeeds. This row is what the *next* offence counts, so losing it
	// because Discord was briefly unreachable would quietly reset somebody's
	// history to zero, which is the one direction this ladder must not
	// silently move in.
	if _, err := p.store.RecordIncident(ctx, Incident{
		GuildID:   cfg.GuildID,
		ChannelID: c.ChannelID,
		MessageID: c.MessageID + ":sanction",
		AuthorID:  c.AuthorID,
		// The policy area that actually triggered this, not a placeholder:
		// the row is what /aimod why reads back to a moderator deciding
		// whether the ladder got the length right, and a sanction filed
		// under the wrong rule makes that judgement impossible.
		Bucket:     bucket,
		Action:     ActionSanction,
		Confidence: 1,
		Reason:     fmt.Sprintf("%s (jailed %s, %d prior in the last 30 days)", reason, core.FormatDuration(duration), priors),
		CreatedAt:  p.now(),
	}); err != nil {
		p.log.Error("aimod: record sanction", "guild", cfg.GuildID, "user", c.AuthorID, "err", err)
	}

	err := p.jailOrTimeout(ctx, cfg, c.AuthorID, duration, reason)
	detail := fmt.Sprintf("%s jailed for %s (%d prior sanction(s) in the last 30 days). %s",
		core.MentionUser(c.AuthorID), core.FormatDuration(duration), priors, reason)
	if err != nil {
		detail = fmt.Sprintf("%s should have been jailed for %s but could not be: %v",
			core.MentionUser(c.AuthorID), core.FormatDuration(duration), err)
		p.log.Error("aimod: sanction failed", "guild", cfg.GuildID, "user", c.AuthorID, "err", err)
	}
	if aerr := p.auditWriter.Record(ctx, cfg.GuildID, core.ActorSystem, "aimod.sanctioned", c.AuthorID, detail); aerr != nil {
		p.log.Error("aimod: audit sanction", "guild", cfg.GuildID, "err", aerr)
	}
}

// jailOrTimeout applies the jail, falling back to Discord's own timeout only
// when jail is genuinely unavailable.
//
// The fallback exists for one situation and should stay narrow: the roles
// plugin is not wired in, is disabled for this guild, or its jail is failing
// (a deleted marker role the bot cannot recreate, a role hierarchy it cannot
// reach, a member holding roles above the bot's own). In all of those the
// choice is a timeout or nothing at all, and nothing at all means a member
// who just posted something that gets servers terminated stays in the
// channel. A timeout is weaker, but it is not nothing, and a moderator
// reading the audit entry can see exactly which one happened and why.
func (p *Plugin) jailOrTimeout(ctx context.Context, cfg Config, userID string, duration time.Duration, reason string) error {
	consented := sanctionable(cfg, userID)
	if p.jailer != nil {
		err := p.jailer.JailAutomatic(ctx, cfg.GuildID, userID, duration, reason, consented)
		if err == nil || discordguard.Skipped(err) {
			return nil
		}
		p.log.Warn("aimod: jail unavailable, falling back to a Discord timeout",
			"guild", cfg.GuildID, "user", userID, "err", err)
	}
	return p.timeoutMember(ctx, cfg.GuildID, userID, duration, consented)
}

// clearPendingFlags removes the messages a member had flagged while they were
// over their scan ceiling.
//
// Bounded by the meter window rather than by all of history: these are the
// messages of this incident, not a licence to go back through somebody's
// record and delete it. Deliberately narrow in three more ways, all of them
// in PendingFlags: only flags, never something already acted on, and never
// something a moderator reversed, because re-deleting a message a human
// deliberately restored would be the bot overruling them.
//
// Best-effort throughout. The jail is the part that stops the flood; a
// message that cannot be deleted is a message a moderator can still delete,
// and none of it may fail the sanction.
func (p *Plugin) clearPendingFlags(ctx context.Context, cfg Config, userID string) {
	pending, err := p.store.PendingFlags(ctx, cfg.GuildID, userID, p.now().Add(-meterWindow))
	if err != nil {
		p.log.Error("aimod: read pending flags", "guild", cfg.GuildID, "user", userID, "err", err)
		return
	}

	var removed int
	for _, inc := range pending {
		// The bucket's own action still decides. A guild that set a policy
		// area to flag meant flag, and somebody tripping a ceiling is not a
		// reason to start enforcing a rule they switched off.
		if !EffectiveAction(cfg.BucketActions, inc.Bucket).acts() {
			continue
		}
		err := p.ops(cfg.GuildID).ChannelMessageDelete(inc.ChannelID, inc.MessageID)
		switch {
		case err == nil:
			removed++
			if merr := p.store.MarkActioned(ctx, inc.ID, ActionRemove); merr != nil {
				p.log.Error("aimod: mark flag actioned", "guild", cfg.GuildID, "incident", inc.ID, "err", merr)
			}
		case discordguard.Skipped(err), core.IsUnknownResource(err):
			// Paused, dry-run, or somebody got there first. Neither is a
			// failure and neither should be retried.
		default:
			p.log.Error("aimod: clear pending flag", "guild", cfg.GuildID, "message", inc.MessageID, "err", err)
		}
	}

	if removed > 0 {
		p.log.Info("aimod: cleared flagged messages after a scan-ceiling sanction",
			"guild", cfg.GuildID, "user", userID, "removed", removed)
	}
}
