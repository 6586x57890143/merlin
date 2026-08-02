package rotation

import (
	"strings"
	"testing"

	"github.com/6586x57890143/merlin/internal/settings"
)

// TestRetentionNoticeDescribesRetentionNotInterval is a regression for a
// false public statement. The notice used to be handed rc.IntervalMinutes and
// tell members "nothing posted here roosts longer than [interval]" — the
// rotation cadence presented as the deletion policy, when the two are
// independent settings. This bot's whole justification is being able to point
// at what it actually does, so an inaccurate retention claim is worse than
// none at all.
func TestRetentionNoticeDescribesRetentionNotInterval(t *testing.T) {
	hours := 3
	rc := settings.RotationChannel{IntervalMinutes: 24 * 60, RetentionHours: &hours}

	notice := retentionNotice(rc)
	if !strings.Contains(notice, "3 hours") {
		t.Fatalf("notice must state the actual 3-hour retention window, got: %s", notice)
	}
	if !strings.Contains(notice, "1 day") {
		t.Fatalf("notice should still state the 1-day rotation cadence, got: %s", notice)
	}
}

// TestRetentionNoticeOnKeepForeverPromisesNoDeletion covers the worst case of
// the old behavior: retention unset means archives are kept indefinitely, but
// the notice still announced a deletion window derived from the interval —
// telling members their content would be erased when nothing would erase it.
func TestRetentionNoticeOnKeepForeverPromisesNoDeletion(t *testing.T) {
	notice := retentionNotice(settings.RotationChannel{IntervalMinutes: 24 * 60, RetentionHours: nil})

	for _, claim := range []string{"gone for good", "roosts more than"} {
		if strings.Contains(notice, claim) {
			t.Fatalf("keep-forever notice must not promise deletion (%q), got: %s", claim, notice)
		}
	}
	if !strings.Contains(notice, "1 day") {
		t.Fatalf("notice should still state the rotation cadence, got: %s", notice)
	}
}

// TestRotationSummaryNeverPrintsAPointer is the audit-trail regression. The
// audit line interpolated the *int RetentionHours with %v, so a real record
// in production reads "retention=0x55c4509432e8" — the one irreversible
// setting in this plugin was unreadable in the exact place someone looks
// after a channel has been permanently deleted.
func TestRotationSummaryNeverPrintsAPointer(t *testing.T) {
	hours := 168
	got := rotationSummary(settings.RotationChannel{
		IntervalMinutes: 24 * 60, RetentionHours: &hours, ArchiveVisibility: "mod_only",
	})

	if strings.Contains(got, "0x") {
		t.Fatalf("audit summary leaked a pointer address: %s", got)
	}
	if !strings.Contains(got, "retention=7d") {
		t.Fatalf("expected the retention window rendered as a duration, got: %s", got)
	}
	if !strings.Contains(got, "interval=1d") {
		t.Fatalf("expected the interval rendered as a duration, got: %s", got)
	}
}

// TestFormatRetentionDistinguishesForever: "kept forever" and "kept for some
// number of hours" must never look alike in an audit record.
func TestFormatRetentionDistinguishesForever(t *testing.T) {
	if got := formatRetention(nil); got != "forever" {
		t.Errorf("formatRetention(nil) = %q, want %q", got, "forever")
	}
	three := 3
	if got := formatRetention(&three); got != "3h" {
		t.Errorf("formatRetention(3) = %q, want %q", got, "3h")
	}
	week := 168
	if got := formatRetention(&week); got != "7d" {
		t.Errorf("formatRetention(168) = %q, want %q", got, "7d")
	}
}
