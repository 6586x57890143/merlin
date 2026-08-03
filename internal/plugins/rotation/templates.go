package rotation

import (
	"context"
	"fmt"
	"time"

	"github.com/6586x57890143/merlin/internal/settings"
	"github.com/6586x57890143/merlin/internal/voice"
)

// resolveSticky returns rc's ordered sticky messages, or nil if sticky
// reposting isn't enabled. Message text lives directly on the
// settings.RotationChannel record (set via /rotation configure sticky);
// there's no separate named-template table to resolve against.
//
// Deliberately untouched by the voice catalog: these are the operator's own
// words, not Merlin's, and every one of them is posted in order exactly as
// written. The catalog supplies only the notice she writes herself.
func resolveSticky(rc settings.RotationChannel) []string {
	if !rc.StickyEnabled {
		return nil
	}
	return rc.StickyMessages
}

// retentionNotice is the transparency message posted in every freshly
// rotated channel (spec.MD §6 step 7), genuinely useful to members and
// doubling as documentation of the retention policy if it's ever
// questioned. The wording comes from internal/voice, so it varies between
// rotations rather than reading like a form letter, but the two facts in it
// do not vary: every line in the catalog is required to carry both
// placeholders, and startup validation refuses to boot otherwise.
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
// That history is why the placeholder requirement is enforced by the build
// rather than left to whoever writes the next line.
func (p *Plugin) retentionNotice(ctx context.Context, rc settings.RotationChannel) string {
	cadence := humanDuration(time.Duration(rc.IntervalMinutes) * time.Minute)

	var line string
	if rc.RetentionHours == nil {
		line = p.voice.Line(ctx, rc.GuildID, voice.KeyRotationIntroForever, map[string]string{
			"cadence": cadence,
		})
	} else {
		line = p.voice.Line(ctx, rc.GuildID, voice.KeyRotationIntroKept, map[string]string{
			"cadence":   cadence,
			"retention": humanDuration(time.Duration(*rc.RetentionHours) * time.Hour),
		})
	}
	if line != "" {
		return line
	}

	// Unreachable given the catalog validates at startup and this, its only
	// caller, supplies exactly the placeholders the spec requires. Kept
	// anyway because of what is at stake: this notice is the server's
	// published retention policy, and returning nothing would quietly
	// remove it from a channel two thousand people read. A plain sentence
	// beats a silent gap.
	return plainRetentionNotice(rc, cadence)
}

// plainRetentionNotice is the no-personality version, reached only if the
// voice catalog produces nothing at all.
func plainRetentionNotice(rc settings.RotationChannel, cadence string) string {
	if rc.RetentionHours == nil {
		return fmt.Sprintf("this channel resets every %s. the previous channel is archived where only the moderators can reach it.", cadence)
	}
	return fmt.Sprintf("this channel resets every %s. anything archived is kept %s and then permanently deleted.",
		cadence, humanDuration(time.Duration(*rc.RetentionHours)*time.Hour))
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
