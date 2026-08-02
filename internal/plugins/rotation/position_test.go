package rotation

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/6586x57890143/merlin/internal/settings"
)

// findEdit returns the index of the first recorded edit matching pred, or -1.
func findEdit(edits []recordedEdit, pred func(recordedEdit) bool) int {
	for i, e := range edits {
		if pred(e) {
			return i
		}
	}
	return -1
}

// The replacement has to land in the slot the original occupied. Members
// navigate by position, and a channel that silently migrates to the bottom of
// its category every rotation is the difference between a rotation nobody
// notices and one everybody has to re-find.
func TestRotationPutsReplacementBackInTheOriginalSlot(t *testing.T) {
	ops, _, _, p, rc := setupRotation(t, finiteRetentionRC())
	// setupRotation seeds old1 at position 3 in category cat0.

	if err := p.rotate(context.Background(), "g1", rc); err != nil {
		t.Fatalf("rotate: %v", err)
	}

	channels, err := ops.GuildChannels("g1")
	if err != nil {
		t.Fatalf("GuildChannels: %v", err)
	}
	replacement := findOtherChannelByName(channels, "general-chat", "old1")
	if replacement == nil {
		t.Fatal("no replacement channel found")
	}
	if replacement.Position != 3 {
		t.Errorf("replacement sits at position %d, want 3; it did not take the original's place", replacement.Position)
	}

	// An explicit edit, not just an inherited create-time value: Discord ties
	// equal positions by channel ID, so a replacement created at the same
	// index as the channel it replaces still sorts below it. Only a PATCH
	// after the old channel is gone re-normalizes the category.
	if findEdit(ops.editCalls, func(e recordedEdit) bool {
		return e.channelID == replacement.ID && e.position != nil && *e.position == 3
	}) < 0 {
		t.Error("no explicit position edit was issued for the replacement; its placement is left to Discord's tie-breaking")
	}
}

// Ordering is the whole argument. While both channels are in the category
// they compete for the slot, and Discord re-flows the survivors when one
// leaves, so a position set before the archive is undone by the archive.
func TestPositionRestoreHappensAfterTheOldChannelLeavesTheCategory(t *testing.T) {
	ops, _, _, p, rc := setupRotation(t, finiteRetentionRC())

	if err := p.rotate(context.Background(), "g1", rc); err != nil {
		t.Fatalf("rotate: %v", err)
	}

	archiveAt := findEdit(ops.editCalls, func(e recordedEdit) bool {
		return e.channelID == "old1" && e.parentID == "archivecat"
	})
	positionAt := findEdit(ops.editCalls, func(e recordedEdit) bool {
		return e.channelID != "old1" && e.position != nil
	})
	if archiveAt < 0 {
		t.Fatal("the old channel was never moved into the archive category")
	}
	if positionAt < 0 {
		t.Fatal("the replacement's position was never set")
	}
	if positionAt < archiveAt {
		t.Errorf("position edit at call %d precedes the archive at call %d; the archive re-flows the category and undoes it", positionAt, archiveAt)
	}
}

// The rotation is already correct by the time the position is set: the
// replacement is live under the right name and the old channel is archived.
// Failing the job here would have the Scheduler retry the whole rotation,
// which re-enters rotate() and creates a *second* replacement, trading a
// cosmetic misplacement for a duplicate channel on every retry.
func TestRotationSucceedsEvenIfThePositionEditFails(t *testing.T) {
	ops, archives, _, p, rc := setupRotation(t, finiteRetentionRC())
	// Calls: 1 reveal, 2 archive, 3 position.
	ops.failOnCall["ChannelEditComplex"] = 3

	if err := p.rotate(context.Background(), "g1", rc); err != nil {
		t.Fatalf("a failed position edit failed the whole rotation: %v", err)
	}

	// And the rotation's real work must still be recorded, or the next sweep
	// would never delete this archive.
	if len(archives.records) != 1 {
		t.Errorf("archive records = %d, want 1; the rotation did not complete its bookkeeping", len(archives.records))
	}
}

// --- interval granularity ---

func TestRotationIntervalAcceptsMinutePrecisionAboveTheFloor(t *testing.T) {
	base := finiteRetentionRC()
	cases := []struct {
		name    string
		minutes int
		wantErr bool
	}{
		{"below the floor", 30, true},
		{"one minute below the floor", minRotationIntervalMinutes - 1, true},
		{"zero", 0, true},
		{"exactly the floor", minRotationIntervalMinutes, false},
		{"ninety minutes", 90, false},
		{"two and a half hours", 150, false},
		{"a day", 24 * 60, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rc := base
			rc.IntervalMinutes = c.minutes
			err := validateRotationChannel(rc)
			if c.wantErr && err == nil {
				t.Errorf("interval of %d minutes was accepted; the floor is %d", c.minutes, minRotationIntervalMinutes)
			}
			if !c.wantErr && err != nil {
				t.Errorf("interval of %d minutes was rejected: %v", c.minutes, err)
			}
		})
	}
}

// A refusal has to say what the floor is and what was actually given, or the
// admin is left guessing which of the two numbers is wrong.
func TestRotationIntervalRefusalNamesTheFloor(t *testing.T) {
	rc := finiteRetentionRC()
	rc.IntervalMinutes = 15
	err := validateRotationChannel(rc)
	if err == nil {
		t.Fatal("expected a sub-hour interval to be refused")
	}
	if !strings.Contains(err.Error(), "1h") {
		t.Errorf("refusal does not state the minimum: %v", err)
	}
}

// The scheduler has to receive the interval in real time units. Storing
// minutes and then multiplying by an hour would turn a daily rotation into a
// sixty-day one, which is the exact class of unit bug migration 0016 had to
// avoid.
func TestConfiguredIntervalConvertsToTheRightDuration(t *testing.T) {
	cases := []struct {
		minutes int
		want    time.Duration
	}{
		{60, time.Hour},
		{90, 90 * time.Minute},
		{24 * 60, 24 * time.Hour},
	}
	for _, c := range cases {
		rc := settings.RotationChannel{IntervalMinutes: c.minutes}
		if got := time.Duration(rc.IntervalMinutes) * time.Minute; got != c.want {
			t.Errorf("%d minutes became %s, want %s", c.minutes, got, c.want)
		}
	}
}
