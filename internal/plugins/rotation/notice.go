package rotation

import (
	"context"
	"fmt"
	"time"

	"github.com/6586x57890143/merlin/internal/core"
	"github.com/6586x57890143/merlin/internal/discordguard"
	"github.com/6586x57890143/merlin/internal/scheduler"
	"github.com/6586x57890143/merlin/internal/settings"
	"github.com/6586x57890143/merlin/internal/voice"
)

// noticeInterval is how often the heads-up job checks whether any of a
// guild's rotations is close enough to warn about.
//
// A minute, matching roles-sweep rather than rotation-sweep's hour: the
// whole value of this message is that the number in it is roughly right, and
// an hourly check could only ever say "some time in the next hour". The
// cost is a cache read plus one cheap query per guild per minute, and no
// Discord call at all except in the minute a notice is actually due.
const noticeInterval = time.Minute

// noticeJobName is the per-guild job key suffix.
const noticeJobName = "rotation-notice"

// noticePruneAfter is how long a claim outlives the rotation it refers to.
// Kept briefly rather than deleted immediately so that a rotation running
// late cannot have its claim pruned out from under it and be warned twice.
const noticePruneAfter = 2 * time.Hour

// makeNoticeJob returns the per-guild job that posts pre-rotation warnings.
//
// One job per guild rather than one per rotating channel, for the same
// reason the sweep is per guild: a guild has a handful of rotations at
// most, and a single job that walks them is less Scheduler bookkeeping than
// N jobs that each wake up to do nothing.
func (p *Plugin) makeNoticeJob(guildID string) func(ctx context.Context) error {
	return func(ctx context.Context) error {
		if err := p.postDueNotices(ctx, guildID); err != nil {
			if discordguard.Skipped(err) {
				return nil
			}
			return err
		}
		return nil
	}
}

// postDueNotices warns any of guildID's channels whose rotation is closer
// than its configured lead time.
//
// A failure on one channel does not abort the rest, matching sweep's "one
// bad row doesn't block the others" policy: these are independent channels
// and there is no reason a problem with one should cost another its notice.
func (p *Plugin) postDueNotices(ctx context.Context, guildID string) error {
	now := p.now()
	if err := p.notices.PruneNotices(ctx, now.Add(-noticePruneAfter)); err != nil {
		// Housekeeping. Failing here would stop notices going out over a
		// tidiness problem.
		p.log.Error("rotation notice: prune old claims", "guild", guildID, "err", err)
	}

	for _, rc := range p.settings.RotationChannels(guildID) {
		if err := p.noticeForChannel(ctx, guildID, rc, now); err != nil {
			if discordguard.Skipped(err) {
				return err
			}
			p.log.Error("rotation notice: could not post", "guild", guildID, "channel", rc.ChannelID, "err", err)
		}
	}
	return nil
}

func (p *Plugin) noticeForChannel(ctx context.Context, guildID string, rc settings.RotationChannel, now time.Time) error {
	if rc.NoticeLeadMinutes <= 0 {
		return nil
	}
	lead := time.Duration(rc.NoticeLeadMinutes) * time.Minute

	due, ok, err := p.sched.NextDue(ctx, scheduler.JobKey(guildID, rotationJobName(rc.ID)))
	if err != nil {
		return fmt.Errorf("read next due: %w", err)
	}
	if !ok {
		// No future instant to count down to. Either the job is not
		// registered, or it is already due (never run, or overdue after a
		// failure). An overdue rotation is the important case: warning that
		// a channel wipes "in 10 minutes" when it is actually about to fire
		// on the next tick would be wrong in the one direction that matters,
		// and the rotation itself is already late enough to be somebody's
		// problem without a misleading countdown on top.
		return nil
	}

	remaining := due.Sub(now)
	if remaining > lead || remaining <= 0 {
		return nil
	}

	// Claim before posting. The alternative, posting and then recording,
	// double-warns whenever the record fails, and being told twice that a
	// channel is about to wipe reads as a bot malfunctioning. The failure
	// mode this way is a missed notice, which nobody notices.
	claimed, err := p.notices.ClaimNotice(ctx, rc.ID, due)
	if err != nil {
		return fmt.Errorf("claim notice: %w", err)
	}
	if !claimed {
		return nil
	}

	line := p.voice.Line(ctx, guildID, voice.KeyRotationHeadsUp, map[string]string{
		"when": core.FormatDuration(remaining.Round(time.Minute)),
	})
	if line == "" {
		return nil
	}
	if _, err := p.ops(guildID).ChannelMessageSend(rc.ChannelID, line); err != nil {
		return fmt.Errorf("post notice: %w", err)
	}
	p.log.Info("rotation notice: warned channel", "guild", guildID, "channel", rc.ChannelID, "in", remaining)
	return nil
}

// reconcileNoticeJob registers or unregisters guildID's notice job to match
// whether any of its rotations actually wants one.
//
// Same rule as reconcileSweepJob: a job exists only where it has work. A
// guild that has turned every lead time off should not have a job waking up
// every minute to decide there is nothing to do.
//
// Callers must already hold p.mu, matching reconcileSweepJob. Both are
// called from reconcile, which takes the lock for the whole pass so the job
// set cannot be observed half-updated. Taking it again here deadlocks, since
// sync.Mutex is not reentrant.
func (p *Plugin) reconcileNoticeJob(guildID string) {
	wanted := false
	for _, rc := range p.settings.RotationChannels(guildID) {
		if rc.NoticeLeadMinutes > 0 {
			wanted = true
			break
		}
	}

	key := scheduler.JobKey(guildID, noticeJobName)
	registered := p.noticeRegistered[guildID]

	switch {
	case wanted && !registered:
		if err := p.sched.Register(key, core.CronSpec{Schedule: core.IntervalSchedule{Interval: noticeInterval}}, p.makeNoticeJob(guildID)); err != nil {
			p.log.Error("rotation: register notice job", "guild", guildID, "err", err)
			return
		}
		p.noticeRegistered[guildID] = true
	case !wanted && registered:
		if err := p.sched.Unregister(key); err != nil {
			p.log.Error("rotation: unregister notice job", "guild", guildID, "err", err)
			return
		}
		delete(p.noticeRegistered, guildID)
	}
}
