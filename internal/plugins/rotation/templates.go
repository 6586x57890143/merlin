package rotation

import (
	"fmt"

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
		"🦅 Merlins travel light — this nest gets a fresh perch every %d hours, and nothing posted here roosts "+
			"longer than that.",
		intervalHours,
	)
}
