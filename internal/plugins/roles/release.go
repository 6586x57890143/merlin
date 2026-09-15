package roles

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/6586x57890143/merlin/internal/core"
	"github.com/6586x57890143/merlin/internal/discordguard"
)

// Automatic release and grant expiry fire on their own timers, at the
// instant the record is due, rather than waiting for the sweep to notice. A
// five minute jail used to end anywhere up to about ninety seconds late
// (the sweep period, plus the scheduler's tick, plus its per-job jitter),
// which next to an instant /roles jail reads as merlin having forgotten.
//
// The sweep stays, as backstop and retry path: a timer that fails logs and
// leaves the row for it, dry-run and pause leave the row for it, and after
// a restart it re-arms whatever it sees coming (armLookahead) so nothing
// falls due between two sweeps without a timer already waiting. Timers are
// in-process only; the row in Postgres is the record, exactly as before.

const (
	// releaseTimeout bounds one timer-driven release, the role jobTimeout
	// plays for a sweep run. It covers the store, audit and DM calls; the
	// Discord ops take no context. If the roles were restored and the
	// DeleteJail after them times out, the sweep re-reads the row, finds the
	// marker gone, and drops it: it self-heals.
	releaseTimeout = time.Minute
	// armLookahead is how far ahead of due the sweep arms timers for rows it
	// did not see written (a restart, a guild re-added). The worst gap
	// between two healthy sweep starts is the 60s period, plus up to 6s of
	// per-job jitter, plus the 30s tick, plus the previous run itself, so
	// about 96s and the evasion pass; a row is therefore always armed before
	// it is due.
	armLookahead = 3 * time.Minute
)

// errReleaseInProgress reports that another caller (the timer, the sweep, a
// mod) holds this row's claim right now. Not a failure and not a success:
// the sweep and the timer leave the row to whoever holds it, and the command
// handlers say so rather than reporting a release that may still fail.
var errReleaseInProgress = errors.New("roles: release already in progress")

func jailKey(guildID, userID string) string          { return guildID + ":" + userID }
func grantKey(guildID, userID, roleID string) string { return guildID + ":" + userID + ":" + roleID }

// arm schedules fire at "at" under key, replacing any timer already there.
// A due-or-past instant fires immediately.
func (p *Plugin) arm(key string, at time.Time, fire func()) {
	p.timerMu.Lock()
	defer p.timerMu.Unlock()
	if t, ok := p.timers[key]; ok {
		t.Stop()
	}
	var t *time.Timer
	t = p.afterFunc(at.Sub(p.now()), func() {
		// Only this timer's own entry is removed: a re-arm may already have
		// put a newer one under the same key. The lock also orders this
		// after the assignment below for a timer that fires at once.
		p.timerMu.Lock()
		if p.timers[key] == t {
			delete(p.timers, key)
		}
		p.timerMu.Unlock()
		defer func() {
			// A timer goroutine has nobody above it to recover; the process
			// would go down with it, taking every other guild's jails along.
			if r := recover(); r != nil {
				p.log.Error("roles: release timer panicked", "key", key, "panic", r)
			}
		}()
		fire()
	})
	p.timers[key] = t
}

// disarm stops every timer whose key starts with prefix: one guild
// (ForgetGuild), or everything ("", Shutdown). Prefix, so only ever call it
// with a guild ID and its colon or the empty string: a jail key is a strict
// prefix of that member's grant keys.
func (p *Plugin) disarm(prefix string) {
	p.timerMu.Lock()
	defer p.timerMu.Unlock()
	for key, t := range p.timers {
		if strings.HasPrefix(key, prefix) {
			t.Stop()
			delete(p.timers, key)
		}
	}
}

// claim marks key as being released right now, so a timer and a sweep that
// land on the same row in the same moment do not both restore, audit and
// DM. The second caller treats the row as handled.
func (p *Plugin) claim(key string) bool {
	p.timerMu.Lock()
	defer p.timerMu.Unlock()
	if p.inFlight[key] {
		return false
	}
	p.inFlight[key] = true
	return true
}

func (p *Plugin) unclaim(key string) {
	p.timerMu.Lock()
	delete(p.inFlight, key)
	p.timerMu.Unlock()
}

func (p *Plugin) armJailRelease(guildID, userID string, at time.Time) {
	p.arm(jailKey(guildID, userID), at, func() { p.fireJailRelease(guildID, userID) })
}

func (p *Plugin) armGrantRevoke(guildID, userID, roleID string, at time.Time) {
	p.arm(grantKey(guildID, userID, roleID), at, func() { p.fireGrantRevoke(guildID, userID, roleID) })
}

// fireJailRelease is the timer's end of a jail. It re-reads the row rather
// than trusting what it was armed with: the jail may have been released by
// hand, or re-dated, and a re-date's old timer that was already running when
// Stop was called must not let the member out on the old date.
func (p *Plugin) fireJailRelease(guildID, userID string) {
	ctx, cancel := context.WithTimeout(context.Background(), releaseTimeout)
	defer cancel()
	rec, ok, err := p.store.GetJail(ctx, guildID, userID)
	if err != nil {
		p.log.Error("roles: release timer: look up jail", "guild", guildID, "user", userID, "err", err)
		return
	}
	if !ok || rec.ReleaseAt == nil || rec.ReleaseAt.After(p.now()) {
		return
	}
	// Same rule as the sweep: a rehearsing guild keeps its pending work, and
	// the first sweep after dry-run ends releases it.
	if p.dryRun(guildID) {
		return
	}
	if err := p.releaseJail(ctx, guildID, userID, rec); err != nil && !discordguard.Skipped(err) && !errors.Is(err, errReleaseInProgress) {
		p.log.Error("roles: release timer: release failed, sweep will retry", "guild", guildID, "user", userID, "err", err)
	}
}

func (p *Plugin) fireGrantRevoke(guildID, userID, roleID string) {
	ctx, cancel := context.WithTimeout(context.Background(), releaseTimeout)
	defer cancel()
	rec, ok, err := p.store.GetGrant(ctx, guildID, userID, roleID)
	if err != nil {
		p.log.Error("roles: expiry timer: look up grant", "guild", guildID, "user", userID, "role", roleID, "err", err)
		return
	}
	if !ok || rec.ExpiresAt == nil || rec.ExpiresAt.After(p.now()) {
		return
	}
	if p.dryRun(guildID) {
		return
	}
	if err := p.revokeGrant(ctx, guildID, userID, roleID, core.ActorSystem); err != nil && !discordguard.Skipped(err) && !errors.Is(err, errReleaseInProgress) {
		p.log.Error("roles: expiry timer: revoke failed, sweep will retry", "guild", guildID, "user", userID, "role", roleID, "err", err)
	}
}
