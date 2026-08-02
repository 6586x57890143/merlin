package rotation

import (
	"fmt"
	"time"

	"github.com/6586x57890143/merlin/internal/settings"
)

// resolveSticky returns rc's ordered sticky messages, or nil if sticky
// reposting isn't enabled. Message text lives directly on the
// settings.RotationChannel record (set via /rotation configure sticky);
// there's no separate named-template table to resolve against.
func resolveSticky(rc settings.RotationChannel) []string {
	if !rc.StickyEnabled {
		return nil
	}
	return rc.StickyMessages
}

// retentionNotice is the transparency message posted in every freshly
// rotated channel (spec.MD §6 step 7), genuinely useful to members and
// doubles as documentation of the retention policy if it's ever questioned.
// Given a birdlike flavor (Merlin is a bird, the falcon species) per
// spec.MD, without dropping any of the required information: the reset
// cadence and how long content survives after a rotation.
//
// It takes the whole config, not just the interval. It used to be handed
// rc.IntervalMinutes and told members "nothing posted here roosts longer than
// [interval]", a statement about the *rotation cadence* presented as the
// retention policy, and the two are independent settings. A channel rotating
// daily with a 3-hour retention had content deleted far sooner than the
// notice implied; one with retention unset had content kept indefinitely
// while the notice promised deletion outright. Publishing a false retention
// claim is worse than publishing none, in a community whose whole reason for
// running this feature is being able to point at what it actually does.
func retentionNotice(rc settings.RotationChannel) string {
	cadence := humanDuration(time.Duration(rc.IntervalMinutes) * time.Minute)
	if rc.RetentionHours == nil {
		return fmt.Sprintf(
			"🦅 Merlins travel light, so this nest gets a fresh perch every %s. Retired perches are tucked "+
				"out of sight where only the flock's keepers can reach them.",
			cadence,
		)
	}
	return fmt.Sprintf(
		"🦅 Merlins travel light, so this nest gets a fresh perch every %s, and once a perch is retired "+
			"nothing on it roosts more than %s before it's gone for good.",
		cadence,
		humanDuration(time.Duration(*rc.RetentionHours)*time.Hour),
	)
}

// humanDuration renders d as a member-facing phrase ("3 days", "18 hours"),
// the prose counterpart to core.FormatDuration's compact "3d"/"18h" used
// in command output, picking the same unit (whole days if it divides
// evenly, otherwise hours) so both ends of this bot describe a given
// interval/retention window the same way.
func humanDuration(d time.Duration) string {
	hours := int(d / time.Hour)
	if hours > 0 && hours%24 == 0 {
		days := hours / 24
		if days == 1 {
			return "1 day"
		}
		return fmt.Sprintf("%d days", days)
	}
	if hours == 1 {
		return "1 hour"
	}
	return fmt.Sprintf("%d hours", hours)
}
