package rapsheet

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/6586x57890143/merlin/internal/core"
	"github.com/6586x57890143/merlin/internal/discordguard"
	"github.com/6586x57890143/merlin/internal/scheduler"
	"github.com/6586x57890143/merlin/internal/voice"
	"github.com/disgoorg/disgo/rest"
)

// Timeouts, kicks and bans: the consequences this plugin applies itself,
// and the sweep that lifts a temporary ban when its time is up.
//
// Jail stays roles' primitive and is reached through the Jailer seam; these
// three are Discord's own, and until now merlin had no ban or kick at all.
// Every one follows roles.applyJail's ordering: the entry is written first,
// then the member is told, then Discord is asked, and a refusal voids the
// entry with the error rather than leaving a record of something that did
// not happen. Told before asked, because a banned or kicked member shares no
// server with the bot and Discord will not deliver the DM afterwards.

const (
	actionTimeout = "rapsheet.timeout"
	actionKick    = "rapsheet.kick"
	actionBan     = "rapsheet.ban"
	actionUnban   = "rapsheet.unban"

	// sweepInterval matches roles-sweep: a ban that lifts up to a minute
	// late is fine, one that lifts an hour late is a complaint.
	sweepInterval = time.Minute

	// maxBanDuration bounds a temporary ban. A year is far past anything
	// the ladder reaches and long enough that anybody wanting more means
	// permanent, which is its own explicit option.
	maxBanDuration = 365 * 24 * time.Hour

	// maxDeleteDays is Discord's ceiling on message history deleted with a
	// ban.
	maxDeleteDays = 7
)

// --- timeout ------------------------------------------------------------------

func (p *Plugin) handleTimeout(ctx context.Context, ix *core.Ix) {
	args := ix.Args()
	userID := args["user"].String()
	category := Category(args["category"].String())
	reason := strings.TrimSpace(args["reason"].String())
	duration, err := core.ParseFlexibleDuration(args["duration"].String())
	if err != nil {
		ix.RespondErr("Timeout", err)
		return
	}
	if duration <= 0 || duration > maxDiscordTimeout {
		ix.RespondErr("Timeout", fmt.Errorf("a timeout must be between a minute and %s", core.FormatDuration(maxDiscordTimeout)))
		return
	}
	if err := ix.Defer(); err != nil {
		p.log.Error("rapsheet: defer timeout", "err", err)
		return
	}
	_, present, err := p.checkTarget(ctx, ix, userID)
	if err != nil {
		_ = ix.FollowUpErr("Not timed out", err)
		return
	}
	if !present {
		_ = ix.FollowUpErr("Not timed out", fmt.Errorf("%s is not in this server", core.MentionUser(userID)))
		return
	}

	cfg := p.config(ctx, ix.GuildID)
	until := p.now().Add(duration)
	e, _, err := p.record(ctx, cfg, newEntry{
		GuildID: ix.GuildID, UserID: userID, Kind: KindTimeout, Category: category, ActorID: actorID(ix),
		Reason: reason, Duration: duration, EndsAt: &until, Source: SourceCommand, Identity: resolvedUser(ix, userID),
	})
	if err != nil {
		_ = ix.FollowUpErr("Timeout", err)
		return
	}
	if err := p.ops(ix.GuildID).TimeoutMember(core.ID(ix.GuildID), core.ID(userID), &until); err != nil {
		p.voidFailed(ctx, e, err)
		_ = ix.FollowUpErr("Timeout", fmt.Errorf("case #%d was voided because Discord refused the timeout: %w", e.ID, err))
		return
	}
	p.dm(ctx, ix.GuildID, userID, voice.KeyTimeoutNotice, "Timed out", core.ColorWarning,
		map[string]string{"guild": p.guildName(ix.GuildID), "until": relativeTimestamp(until)}, append(reasonFields(category, reason), nextStepField())...)
	p.auditEntry(ctx, "rapsheet.timeout", e)
	_ = ix.FollowUpOK("Timed out", p.caseSummary(ctx, cfg, e))
}

// --- kick -------------------------------------------------------------------------

func (p *Plugin) handleKick(ctx context.Context, ix *core.Ix) {
	args := ix.Args()
	userID := args["user"].String()
	category := Category(args["category"].String())
	reason := strings.TrimSpace(args["reason"].String())

	if err := ix.Defer(); err != nil {
		p.log.Error("rapsheet: defer kick", "err", err)
		return
	}
	_, present, err := p.checkTarget(ctx, ix, userID)
	if err != nil {
		_ = ix.FollowUpErr("Not kicked", err)
		return
	}
	if !present {
		// A kick of somebody who already left is a no-op Discord would
		// refuse, and a record of it would be a lie.
		_ = ix.FollowUpErr("Not kicked", fmt.Errorf("%s is not in this server", core.MentionUser(userID)))
		return
	}

	cfg := p.config(ctx, ix.GuildID)
	e, _, err := p.record(ctx, cfg, newEntry{
		GuildID: ix.GuildID, UserID: userID, Kind: KindKick, Category: category, ActorID: actorID(ix),
		Reason: reason, Source: SourceCommand, Identity: resolvedUser(ix, userID),
	})
	if err != nil {
		_ = ix.FollowUpErr("Kick", err)
		return
	}
	p.dm(ctx, ix.GuildID, userID, voice.KeyKickNotice, "Removed from the server", core.ColorWarning,
		map[string]string{"guild": p.guildName(ix.GuildID)}, reasonFields(category, reason)...)
	if err := p.ops(ix.GuildID).RemoveMember(core.ID(ix.GuildID), core.ID(userID), auditReason(e)); err != nil {
		p.voidFailed(ctx, e, err)
		_ = ix.FollowUpErr("Kick", fmt.Errorf("case #%d was voided because Discord refused the kick: %w", e.ID, err))
		return
	}
	p.auditEntry(ctx, "rapsheet.kick", e)
	_ = ix.FollowUpOK("Kicked", p.caseSummary(ctx, cfg, e))
}

// --- ban / unban --------------------------------------------------------------------

func (p *Plugin) handleBan(ctx context.Context, ix *core.Ix) {
	args := ix.Args()
	userID := args["user"].String()
	category := Category(args["category"].String())
	reason := strings.TrimSpace(args["reason"].String())

	var (
		duration  time.Duration
		permanent bool
		days      int
	)
	if a, ok := args["permanent"]; ok {
		permanent = a.Bool()
	}
	if a, ok := args["duration"]; ok {
		d, err := core.ParseFlexibleDuration(a.String())
		if err != nil {
			ix.RespondErr("Ban", err)
			return
		}
		duration = d
	}
	if a, ok := args["delete_message_days"]; ok {
		days = int(int64(a.Int()))
	}
	// Exactly one of the two, stated. A ban with neither would have to
	// default to one of them, and whichever it picked would be the wrong
	// surprise for somebody: a ban that quietly lifts, or one that quietly
	// never does.
	switch {
	case permanent && duration > 0:
		ix.RespondErr("Ban", errors.New("give a duration or say permanent, not both"))
		return
	case !permanent && duration <= 0:
		ix.RespondErr("Ban", errors.New("give a duration (like 7d), or say permanent: true"))
		return
	case duration > maxBanDuration:
		ix.RespondErr("Ban", fmt.Errorf("a temporary ban can be at most %s; past that, say permanent", core.FormatDuration(maxBanDuration)))
		return
	}

	if err := ix.Defer(); err != nil {
		p.log.Error("rapsheet: defer ban", "err", err)
		return
	}
	// A ban works on somebody who has already left, which is most of what
	// bans are for, so presence is not required here.
	if _, _, err := p.checkTarget(ctx, ix, userID); err != nil {
		_ = ix.FollowUpErr("Not banned", err)
		return
	}
	if _, active, err := p.store.ActiveBan(ctx, ix.GuildID, userID); err == nil && active {
		_ = ix.FollowUpErr("Not banned", fmt.Errorf("%s is already banned; `/rapsheet unban` first if the sentence should change", core.MentionUser(userID)))
		return
	}

	cfg := p.config(ctx, ix.GuildID)
	in := newEntry{
		GuildID: ix.GuildID, UserID: userID, Kind: KindBan, Category: category, ActorID: actorID(ix),
		Reason: reason, Source: SourceCommand, Identity: resolvedUser(ix, userID),
	}
	vars := map[string]string{"guild": p.guildName(ix.GuildID)}
	key := voice.KeyBanPermanentNotice
	if !permanent {
		until := p.now().Add(duration)
		in.Duration, in.EndsAt = duration, &until
		vars["until"] = relativeTimestamp(until)
		key = voice.KeyBanNotice
	}
	e, _, err := p.record(ctx, cfg, in)
	if err != nil {
		_ = ix.FollowUpErr("Ban", err)
		return
	}
	p.dm(ctx, ix.GuildID, userID, key, "Banned", core.ColorError, vars, reasonFields(category, reason)...)
	if err := p.ops(ix.GuildID).AddBan(core.ID(ix.GuildID), core.ID(userID), time.Duration(days)*24*time.Hour, auditReason(e)); err != nil {
		p.voidFailed(ctx, e, err)
		_ = ix.FollowUpErr("Ban", fmt.Errorf("case #%d was voided because Discord refused the ban: %w", e.ID, err))
		return
	}
	p.auditEntry(ctx, "rapsheet.ban", e)
	p.mu.Lock()
	p.reconcileSweepJob(ctx, ix.GuildID)
	p.mu.Unlock()
	_ = ix.FollowUpOK("Banned", p.caseSummary(ctx, cfg, e))
}

func (p *Plugin) handleUnban(ctx context.Context, ix *core.Ix) {
	args := ix.Args()
	userID := args["user"].String()
	reason := strings.TrimSpace(args["reason"].String())

	if err := ix.Defer(); err != nil {
		p.log.Error("rapsheet: defer unban", "err", err)
		return
	}
	err := p.ops(ix.GuildID).DeleteBan(core.ID(ix.GuildID), core.ID(userID))
	switch {
	case err == nil:
	case core.HasDiscordErrorCode(err, int(rest.JSONErrorCodeUnknownBan)):
		// Not banned as far as Discord is concerned. Still worth settling
		// the ledger below, in case a row says otherwise.
	default:
		_ = ix.FollowUpErr("Unban", err)
		return
	}
	if _, lifted, lerr := p.liftBan(ctx, ix.GuildID, userID); lerr != nil {
		p.log.Error("rapsheet: mark ban lifted", "guild", ix.GuildID, "user", userID, "err", lerr)
	} else if !lifted && err != nil {
		_ = ix.FollowUpErr("Unban", fmt.Errorf("%s is not banned", core.MentionUser(userID)))
		return
	}
	cfg := p.config(ctx, ix.GuildID)
	e, _, rerr := p.record(ctx, cfg, newEntry{
		GuildID: ix.GuildID, UserID: userID, Kind: KindUnban, ActorID: actorID(ix), Reason: reason,
		Source: SourceCommand, Identity: resolvedUser(ix, userID),
	})
	if rerr != nil {
		p.log.Error("rapsheet: record unban", "guild", ix.GuildID, "user", userID, "err", rerr)
	} else {
		p.auditEntry(ctx, "rapsheet.unban", e)
	}
	p.mu.Lock()
	p.reconcileSweepJob(ctx, ix.GuildID)
	p.mu.Unlock()
	_ = ix.FollowUpOK("Unbanned", fmt.Sprintf("%s is unbanned.", core.MentionUser(userID)))
}

// liftBan marks the member's standing ban lifted, if there is one.
func (p *Plugin) liftBan(ctx context.Context, guildID, userID string) (Entry, bool, error) {
	e, ok, err := p.store.ActiveBan(ctx, guildID, userID)
	if err != nil || !ok {
		return Entry{}, false, err
	}
	if err := p.store.MarkLifted(ctx, e.ID, p.now()); err != nil {
		return Entry{}, false, err
	}
	return e, true, nil
}

// voidFailed strikes an entry whose action Discord refused, keeping the
// refusal as the void reason so the sheet shows what was tried.
func (p *Plugin) voidFailed(ctx context.Context, e Entry, cause error) {
	now := p.now()
	if err := p.store.Void(ctx, e.GuildID, e.ID, core.ActorSystem, "could not be applied: "+cause.Error(), now); err != nil {
		p.log.Error("rapsheet: void failed action", "guild", e.GuildID, "case", e.ID, "err", err)
		return
	}
	e.VoidedAt, e.VoidedBy, e.VoidReason = &now, core.ActorSystem, "could not be applied: "+cause.Error()
	p.afterAmend(ctx, e)
}

// auditReason is what Discord's own audit log shows next to merlin's name:
// the case number, so a moderator reading that log can find the sheet, and
// the reason. Discord caps it at 512 characters.
func auditReason(e Entry) string {
	r := fmt.Sprintf("rapsheet #%d: %s", e.ID, e.Reason)
	if len(r) > 512 {
		r = r[:512]
	}
	return r
}

// --- the sweep -------------------------------------------------------------------

func sweepKey(guildID string) string { return scheduler.JobKey(guildID, "rapsheet-sweep") }

// reconcileSweepJob registers the per-guild unban sweep only where a
// temporary ban is pending, and unregisters it once none is. Same "a job
// exists only where it has work" rule as rotation's and contest's jobs.
//
// Assumes the caller holds p.mu, exactly as those do. A failed count leaves
// the current registration untouched rather than guessing in either
// direction: unregistering on a transient error would leave somebody
// banned past their time.
func (p *Plugin) reconcileSweepJob(ctx context.Context, guildID string) {
	if p.sched == nil {
		return
	}
	n, err := p.store.CountPendingBans(ctx, guildID)
	if err != nil {
		p.log.Error("rapsheet: count pending bans", "guild", guildID, "err", err)
		return
	}
	key := sweepKey(guildID)
	switch {
	case n > 0 && !p.sweepRegistered[guildID]:
		if err := p.sched.Register(key,
			core.CronSpec{Schedule: core.IntervalSchedule{Interval: sweepInterval}},
			func(ctx context.Context) error { return p.sweep(ctx, guildID) },
		); err != nil {
			p.log.Error("rapsheet: register sweep job", "guild", guildID, "err", err)
			return
		}
		p.sweepRegistered[guildID] = true
	case n == 0 && p.sweepRegistered[guildID]:
		if err := p.sched.Unregister(key); err != nil {
			p.log.Error("rapsheet: unregister sweep job", "guild", guildID, "err", err)
			return
		}
		delete(p.sweepRegistered, guildID)
	}
}

// sweep lifts every temporary ban whose time is up.
//
// Only untrack on "gone", never on "failed": a ban Discord says does not
// exist (Unknown Ban) was lifted by a human, and the audit-log ingestion
// recorded their unban, so the row is marked lifted and nothing else is
// written. Any other failure leaves the row for the next tick, and is
// returned so the Scheduler's backoff and wedged-job alert see it. A guild
// that is paused or in dry-run gets its bans lifted on the first sweep
// after it stops being so, the same rule roles' sweep follows.
func (p *Plugin) sweep(ctx context.Context, guildID string) error {
	due, err := p.store.DueBans(ctx, guildID, p.now())
	if err != nil {
		return fmt.Errorf("rapsheet sweep: due bans: %w", err)
	}
	var firstErr error
	for _, e := range due {
		err := p.ops(guildID).DeleteBan(core.ID(guildID), core.ID(e.UserID))
		switch {
		case err == nil:
			if err := p.store.MarkLifted(ctx, e.ID, p.now()); err != nil {
				p.log.Error("rapsheet sweep: mark lifted", "guild", guildID, "case", e.ID, "err", err)
				continue
			}
			cfg := p.config(ctx, guildID)
			if _, _, rerr := p.record(ctx, cfg, newEntry{
				GuildID: guildID, UserID: e.UserID, Kind: KindUnban, ActorID: core.ActorSystem,
				Reason: fmt.Sprintf("ban #%d served", e.ID), Source: SourceSweep,
			}); rerr != nil {
				p.log.Error("rapsheet sweep: record unban", "guild", guildID, "case", e.ID, "err", rerr)
			}
			if aerr := p.audit.Record(ctx, guildID, core.ActorSystem, "rapsheet.unban", "",
				fmt.Sprintf("case #%d user=%s ban served", e.ID, core.MentionUser(e.UserID))); aerr != nil {
				p.log.Error("rapsheet sweep: audit unban", "guild", guildID, "err", aerr)
			}
		case core.HasDiscordErrorCode(err, int(rest.JSONErrorCodeUnknownBan)):
			p.log.Info("rapsheet sweep: ban already lifted by hand", "guild", guildID, "case", e.ID)
			if err := p.store.MarkLifted(ctx, e.ID, p.now()); err != nil {
				p.log.Error("rapsheet sweep: mark lifted", "guild", guildID, "case", e.ID, "err", err)
			}
		case discordguard.Skipped(err):
			// Paused or dry-run: the guild deliberately stopped the bot
			// acting. Not a failure, and the row waits.
		default:
			p.log.Error("rapsheet sweep: unban failed, will retry", "guild", guildID, "case", e.ID, "err", err)
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	p.mu.Lock()
	p.reconcileSweepJob(ctx, guildID)
	p.mu.Unlock()
	return firstErr
}
