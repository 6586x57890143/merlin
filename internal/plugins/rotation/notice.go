package rotation

import (
	"context"
	"fmt"
	"time"

	"github.com/bwmarrin/discordgo"

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

// noticeForChannel warns rc's channel if its rotation is inside the lead
// window, and otherwise does nothing.
//
// Every "do nothing" branch logs why at debug level. That is not decoration:
// this function has five separate ways to stay quiet, they all return
// success, and /scheduler run-now reports success for all of them, so a
// manual run that posts nothing is indistinguishable from a broken one
// without this. Debug rather than info because the overwhelmingly common
// answer is "the rotation is hours away", once a minute per channel forever.
// An operator asking the question raises log_level in config.yaml, which
// SIGHUP applies without a restart (config.Loader.OnReload), and re-runs the
// job.
func (p *Plugin) noticeForChannel(ctx context.Context, guildID string, rc settings.RotationChannel, now time.Time) error {
	quiet := func(why string, args ...any) {
		p.log.Debug("rotation notice: nothing to post",
			append([]any{"guild", guildID, "channel", rc.ChannelID, "why", why}, args...)...)
	}

	if rc.NoticeLeadMinutes <= 0 {
		quiet("no lead configured, the heads-up is switched off for this channel")
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
		quiet("the rotation has no future due instant: it is unregistered, has never run, or is overdue")
		return nil
	}

	remaining := due.Sub(now)
	if remaining > lead {
		quiet("the rotation is further out than the lead", "due_in", remaining, "lead", lead)
		return nil
	}
	if remaining <= 0 {
		quiet("the due instant has already passed", "due", due)
		return nil
	}
	// Under a minute out, the countdown rounds to zero and the message reads
	// "this channel resets in 0 minutes", which is both wrong and obviously
	// broken to anyone who sees it. Same reasoning as the overdue case above:
	// the rotation is about to fire on the next tick anyway, so a warning
	// buys nobody anything, and staying quiet is the failure mode this
	// feature already prefers. Deliberately checked before the claim, so the
	// window is not spent on a message that is never sent.
	if remaining < time.Minute {
		quiet("the rotation is under a minute away, a countdown would round to zero", "due", due)
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
		quiet("this rotation has already been warned about", "due", due)
		return nil
	}

	// A channel on generic disclosure gets the same courtesy warning without
	// the countdown. Naming the remaining minutes would hand over the
	// rotation schedule the guild chose not to publish, and doing it in the
	// message that immediately precedes the deliberately vague intro notice
	// would make the setting pointless. Turning the heads-up off entirely is
	// still a separate decision: notice_lead_minutes = 0, handled above.
	key, vars := voice.KeyRotationHeadsUp, map[string]string{
		// humanDuration, not core.FormatDuration: this is a member-facing
		// message and its sibling intro notice in the same channel says
		// "18 hours", so saying "18h" here would have the same bot describe
		// the same window two different ways minutes apart.
		"when": humanDuration(remaining.Round(time.Minute)),
	}
	if rc.Disclosure.Resolve() == settings.DisclosureGeneric {
		key, vars = voice.KeyRotationHeadsUpGeneric, nil
	}

	line := p.voice.Line(ctx, guildID, key, vars)
	if line == "" {
		quiet("voice returned nothing to say")
		return nil
	}
	// An embed rather than bare content, matching the intro notice posted
	// into this same channel when the rotation actually lands. ColorWarning
	// (moodForColor -> MoodWarn) rather than the intro's ColorPrimary
	// (MoodNotice) on purpose: this one is asking people to wrap up now,
	// and the pair reads as escalate-then-settle.
	notice := core.NewEmbed(core.ColorWarning, "", line)
	if _, err := p.ops(guildID).ChannelMessageSendComplex(rc.ChannelID, &discordgo.MessageSend{
		Embed: notice,
		// Without the files the thumbnail URL points at an attachment that
		// was never uploaded, which Discord renders as a broken frame.
		Files: core.EmbedFiles(notice),
	}); err != nil {
		// A guard skip is the one failure where we know for certain that
		// Discord was never called: allow returns before touching the
		// session. Keeping the claim there would spend the window on a
		// message that was deliberately not sent, so an operator who clears
		// dry-run with minutes still on the clock would get silence for a
		// rotation the bot had already marked as warned. Release, and the
		// next tick inside the window warns properly.
		//
		// Any other error keeps its claim, deliberately. A send that
		// genuinely failed may still have reached Discord with only the
		// response lost, and this feature's stated preference is a missed
		// notice over a duplicate one.
		if discordguard.Skipped(err) {
			if relErr := p.notices.ReleaseNotice(ctx, rc.ID, due); relErr != nil {
				p.log.Error("rotation notice: release claim after a skipped send",
					"guild", guildID, "channel", rc.ChannelID, "err", relErr)
			}
		}
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
