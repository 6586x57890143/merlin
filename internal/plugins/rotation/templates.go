package rotation

import (
	"fmt"

	"github.com/6586x57890143/merlin/internal/config"
)

// resolveSticky returns the ordered sticky messages for rc, or nil if
// sticky reposting isn't enabled. Config validation
// (internal/config/rotation_validate.go) already guarantees an enabled
// sticky's template resolves, so a missing template here is treated as
// "nothing to post" rather than an error — config may have hot-reloaded
// between job registration and this run.
func resolveSticky(rc config.RotationConfig, global config.GlobalConfig) []string {
	if !rc.Sticky.Enabled {
		return nil
	}
	tmpl, ok := global.StickyTemplates[rc.Sticky.Template]
	if !ok {
		return nil
	}
	return tmpl.Messages
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
			"longer than that. The one exception: anything caught up in an open moderation report stays put until that's resolved.",
		intervalHours,
	)
}
