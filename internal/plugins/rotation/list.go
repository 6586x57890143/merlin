package rotation

import (
	"context"
	"time"

	"github.com/6586x57890143/merlin/internal/scheduler"
	"github.com/6586x57890143/merlin/internal/settings"
)

// disclosureLabel renders a disclosure mode the way an admin reading a list
// thinks about it: what the channel gets told, not the stored enum.
func disclosureLabel(d settings.Disclosure) string {
	switch d.Resolve() {
	case settings.DisclosureCadence:
		return "rotation schedule only"
	case settings.DisclosureRetention:
		return "archive window only"
	case settings.DisclosureGeneric:
		return "just that it rotated"
	default:
		return "schedule + archive window"
	}
}

// visibilityLabel marks the legacy whitelist mode as legacy. It is no longer
// offered by /rotation configure, so an admin who finds one on a slot
// imported from pre-Milestone-4 YAML has no other way to learn where it came
// from or why they cannot select it.
func visibilityLabel(v string) string {
	if v == "whitelist" {
		return "mods + whitelist (legacy)"
	}
	return v
}

// nextRotationText renders the countdown to rc's next rotation, spelling out
// the two cases where there is no countdown to give rather than leaving the
// field blank.
//
// "due now" covers a job that has never run as well as one running late after
// a failure. They are worth saying out loud because they are also exactly when
// the heads-up deliberately stays silent: an overdue rotation fires on the next
// tick, and counting down to it would be wrong in the one direction that
// matters.
func (p *Plugin) nextRotationText(ctx context.Context, rc settings.RotationChannel) string {
	due, ok, err := p.sched.NextDue(ctx, scheduler.JobKey(rc.GuildID, rotationJobName(rc.ID)))
	if err != nil {
		p.log.Error("rotation: read next due for detail view", "guild", rc.GuildID, "channel", rc.ChannelID, "err", err)
		return "unknown"
	}
	if !ok {
		return "due now"
	}
	remaining := due.Sub(p.now())
	if remaining <= 0 {
		return "due now"
	}
	return "in " + humanDuration(remaining.Round(time.Minute))
}
