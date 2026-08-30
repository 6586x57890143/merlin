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
// words, not merlin's, and every one of them is posted in order exactly as
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
	return RetentionNotice(ctx, p.voice, rc)
}

// HeadsUpNotice renders the pre-rotation warning for one slot, given a
// speaker and how long is left.
//
// Exported alongside RetentionNotice and for the same reason: these are the
// two messages a member actually sees in a rotating channel, minutes apart,
// and cmd/lab previews both by calling this rather than by reimplementing the
// selection. Returns "" when the catalog has nothing to say, which the caller
// treats as "post nothing" rather than posting visible braces.
func HeadsUpNotice(ctx context.Context, speaker voice.Source, rc settings.RotationChannel, remaining time.Duration) string {
	// A channel on generic disclosure gets the same courtesy warning without
	// the countdown. Naming the remaining minutes would hand over the
	// rotation schedule the guild chose not to publish, and doing it in the
	// message that immediately precedes the deliberately vague intro notice
	// would make the setting pointless. Turning the heads-up off entirely is
	// still a separate decision: notice_lead_minutes = 0.
	if rc.Disclosure.Resolve() == settings.DisclosureGeneric {
		return speaker.Line(ctx, rc.GuildID, voice.KeyRotationHeadsUpGeneric, nil)
	}
	return speaker.Line(ctx, rc.GuildID, voice.KeyRotationHeadsUp, map[string]string{
		// humanDuration, not core.FormatDuration: this is a member-facing
		// message and its sibling intro notice in the same channel says
		// "18 hours", so saying "18h" here would have the same bot describe
		// the same window two different ways minutes apart.
		"when": humanDuration(remaining.Round(time.Minute)),
	})
}

// RetentionNotice renders the notice for one rotation slot, given a speaker.
//
// Exported and taking voice.Source rather than living only as the method
// above, so cmd/lab can preview the real notice in a browser instead of
// reimplementing this selection in JavaScript. That distinction is the whole
// reason the preview is worth having: a reimplementation drifts, and what it
// would drift on is a guild's published retention policy. The method is a
// one-line delegation for the same reason, so there is exactly one path and
// the preview cannot diverge from what actually gets posted.
//
// It needs nothing from Plugin but the speaker, which is what made the split
// free.
func RetentionNotice(ctx context.Context, speaker voice.Source, rc settings.RotationChannel) string {
	key, vars := introKey(rc)
	if line := speaker.Line(ctx, rc.GuildID, key, vars); line != "" {
		return line
	}

	// Unreachable given the catalog validates at startup and introKey
	// supplies exactly the placeholders each key's spec requires. Kept
	// anyway because of what is at stake: this notice is the server's
	// published retention policy, and returning nothing would quietly
	// remove it from a channel two thousand people read. A plain sentence
	// beats a silent gap.
	return plainRetentionNotice(rc)
}

// introKey picks the notice for rc's disclosure mode and builds exactly the
// placeholders that key requires.
//
// The key and its variables are returned together, deliberately, rather than
// selected in one place and populated in another. Every one of these six keys
// requires a different subset of {cadence}/{retention}, and voice.Line
// silently falls back when a required placeholder is missing, so a mapping
// split across two switch statements would degrade to the compiled-in
// fallback the first time somebody edited one of them.
//
// The empty disclosure value resolves to full, matching what every channel
// did before migration 0018, and any value that is neither empty nor one of
// the four known modes falls through to full as well. That is the right
// direction for an unreadable setting: over-disclosing is a wording problem,
// while under-disclosing on a corrupt row would silently retract a retention
// promise the guild believes it is still making.
func introKey(rc settings.RotationChannel) (voice.Key, map[string]string) {
	cadence := humanDuration(time.Duration(rc.IntervalMinutes) * time.Minute)
	forever := rc.RetentionHours == nil
	var retention string
	if !forever {
		retention = humanDuration(time.Duration(*rc.RetentionHours) * time.Hour)
	}

	switch rc.Disclosure.Resolve() {
	case settings.DisclosureCadence:
		return voice.KeyRotationIntroCadence, map[string]string{"cadence": cadence}
	case settings.DisclosureRetention:
		if forever {
			return voice.KeyRotationIntroRetentionForever, nil
		}
		return voice.KeyRotationIntroRetention, map[string]string{"retention": retention}
	case settings.DisclosureGeneric:
		return voice.KeyRotationIntroGeneric, nil
	default:
		if forever {
			return voice.KeyRotationIntroFullForever, map[string]string{"cadence": cadence}
		}
		return voice.KeyRotationIntroFull, map[string]string{"cadence": cadence, "retention": retention}
	}
}

// plainRetentionNotice is the no-personality version, reached only if the
// voice catalog produces nothing at all.
//
// It respects the disclosure mode too. That is the whole reason this is a
// switch rather than the two-line function it used to be: a catalog failure
// falling back to the fully-disclosing sentence would publish the cadence and
// the deletion window of a guild that had explicitly chosen to publish
// neither, and it would do it at the one moment nobody is watching the logs.
func plainRetentionNotice(rc settings.RotationChannel) string {
	cadence := humanDuration(time.Duration(rc.IntervalMinutes) * time.Minute)
	forever := rc.RetentionHours == nil
	var retention string
	if !forever {
		retention = humanDuration(time.Duration(*rc.RetentionHours) * time.Hour)
	}

	switch rc.Disclosure.Resolve() {
	case settings.DisclosureCadence:
		return fmt.Sprintf("this channel resets every %s.", cadence)
	case settings.DisclosureRetention:
		if forever {
			return "the previous channel is archived where only the moderators can reach it, and nothing on it is deleted."
		}
		return fmt.Sprintf("anything archived from this channel is kept %s and then permanently deleted.", retention)
	case settings.DisclosureGeneric:
		return "this channel has rotated. this is the new one."
	default:
		if forever {
			return fmt.Sprintf("this channel resets every %s. the previous channel is archived where only the moderators can reach it.", cadence)
		}
		return fmt.Sprintf("this channel resets every %s. anything archived is kept %s and then permanently deleted.",
			cadence, retention)
	}
}

// humanDuration renders d as a member-facing phrase ("3 days", "18 hours"),
// the prose counterpart to core.FormatDuration's compact "3d"/"18h" used
// in command output, picking the same unit (whole days if it divides
// evenly, otherwise hours) so both ends of this bot describe a given
// interval/retention window the same way.
//
// The split is deliberate and worth keeping: core.FormatDuration is the
// admin-surface formatter, read at a glance in a table by someone who wants
// density, and humanDuration is the member-surface one, read mid-sentence in
// a chat channel. Every member-facing duration goes through this function,
// including the heads-up countdown, so the two messages rotation posts into
// the same channel cannot describe time two different ways.
func humanDuration(d time.Duration) string {
	switch {
	case d >= 24*time.Hour && d%(24*time.Hour) == 0:
		if days := int(d / (24 * time.Hour)); days == 1 {
			return "1 day"
		} else {
			return fmt.Sprintf("%d days", days)
		}
	case d >= time.Hour && d%time.Hour == 0:
		if hours := int(d / time.Hour); hours == 1 {
			return "1 hour"
		} else {
			return fmt.Sprintf("%d hours", hours)
		}
	default:
		// Sub-hour and part-hour values are reachable now that intervals
		// are minute-precise. Truncating to whole hours here would tell
		// members a 90-minute channel resets "every 1 hour", which is both
		// wrong and wrong in the direction that makes the notice look like
		// it is describing a different channel.
		if minutes := int(d / time.Minute); minutes == 1 {
			return "1 minute"
		} else {
			return fmt.Sprintf("%d minutes", minutes)
		}
	}
}
