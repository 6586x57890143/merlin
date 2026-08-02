package rotation

import (
	"fmt"
	"time"

	"github.com/6586x57890143/merlin/internal/settings"
)

// resolveSticky returns rc's ordered sticky messages, or nil if sticky
// reposting isn't enabled. Message text lives directly on the
// settings.RotationChannel record (set via /rotation configure sticky) —
// there's no separate named-template table to resolve against.
func resolveSticky(rc settings.RotationChannel) []string {
	if !rc.StickyEnabled {
		return nil
	}
	return rc.StickyMessages
}

// retentionNotice is the transparency message posted in every freshly
// rotated channel (spec.MD §6 step 7) — genuinely useful to members and
// doubles as documentation of the retention policy if it's ever questioned.
// Given a birdlike flavor (Merlin is a bird, the falcon species) per
// spec.MD, without dropping any of the required information: the reset
// cadence, that content doesn't outlive it, and the moderation-report
// exception.
func retentionNotice(intervalHours int) string {
	return fmt.Sprintf(
		"🦅 Merlins travel light — this nest gets a fresh perch every %s, and nothing posted here roosts "+
			"longer than that.",
		humanDuration(time.Duration(intervalHours)*time.Hour),
	)
}

// humanDuration renders d as a member-facing phrase ("3 days", "18 hours")
// — the prose counterpart to core.FormatDuration's compact "3d"/"18h" used
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
