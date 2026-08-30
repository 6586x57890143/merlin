package lab

import (
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func testLab(t *testing.T) *Lab {
	t.Helper()
	l, err := New(slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// Pinned, so the listed fire instants are checkable arithmetic rather
	// than whatever the clock said.
	base := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	l.now = func() time.Time { return base }
	return l
}

func hours(n int) *int { return &n }

// The simulator's core claim: these are the instants the Scheduler will fire
// on. Walked through the real core.Schedule, so if that changes this moves
// with it.
func TestRotationListsTheRealSchedule(t *testing.T) {
	l := testLab(t)
	res := l.Rotation(RotationRequest{
		Interval: "90m", LeadMinutes: 10, Disclosure: "full",
		RetentionHours: hours(72), Fires: 3,
	})
	if res.Error != "" {
		t.Fatalf("unexpected error: %s", res.Error)
	}
	want := []string{
		"2026-03-01T13:30:00Z",
		"2026-03-01T15:00:00Z",
		"2026-03-01T16:30:00Z",
	}
	if len(res.Fires) != len(want) {
		t.Fatalf("got %d fires, want %d: %v", len(res.Fires), len(want), res.Fires)
	}
	for i, w := range want {
		if res.Fires[i] != w {
			t.Errorf("fire %d = %s, want %s", i, res.Fires[i], w)
		}
	}
	// "90m" is minute-precise and must not be rounded back to an hour, which
	// is the bug migration 0016 existed to fix.
	if res.Cadence != "90m" && res.Cadence != "1h30m" {
		t.Errorf("cadence = %q, want the interval as typed rather than truncated", res.Cadence)
	}
}

// Both member-facing messages come from the catalog, through the same
// functions the bot posts with.
func TestRotationRendersBothNotices(t *testing.T) {
	l := testLab(t)
	res := l.Rotation(RotationRequest{
		Interval: "24h", LeadMinutes: 10, Disclosure: "full", RetentionHours: hours(72),
	})
	if res.Error != "" {
		t.Fatalf("unexpected error: %s", res.Error)
	}
	if res.HeadsUp == "" {
		t.Error("no heads-up rendered for a channel with a notice lead")
	}
	if res.Intro == "" {
		t.Fatal("no intro notice rendered")
	}
	// full disclosure states both facts, and the catalog refuses to boot on a
	// line missing either, so both have to survive into the rendered text.
	// humanDuration prefers days when the interval divides evenly, so a 24h
	// cadence reads as "1 day" in the member-facing notice.
	if !strings.Contains(res.Intro, "1 day") {
		t.Errorf("full disclosure did not state the cadence: %q", res.Intro)
	}
	if !strings.Contains(res.Intro, "3 days") && !strings.Contains(res.Intro, "72 hours") {
		t.Errorf("full disclosure did not state the retention window: %q", res.Intro)
	}
}

// The disclosure modes are the reason this preview matters: what a channel is
// told is a policy decision, and the preview has to show the withholding as
// faithfully as the stating.
func TestRotationHonoursDisclosureModes(t *testing.T) {
	l := testLab(t)

	cadence := l.Rotation(RotationRequest{
		Interval: "24h", Disclosure: "cadence", RetentionHours: hours(72), LeadMinutes: 10,
	})
	if strings.Contains(cadence.Intro, "3 days") || strings.Contains(cadence.Intro, "72 hours") {
		t.Errorf("cadence disclosure leaked the retention window: %q", cadence.Intro)
	}
	if !strings.Contains(cadence.Intro, "1 day") {
		t.Errorf("cadence disclosure did not state the cadence: %q", cadence.Intro)
	}

	generic := l.Rotation(RotationRequest{
		Interval: "24h", Disclosure: "generic", RetentionHours: hours(72), LeadMinutes: 10,
	})
	for _, leak := range []string{"1 day", "24 hours", "3 days", "72 hours"} {
		if strings.Contains(generic.Intro, leak) {
			t.Errorf("generic disclosure leaked %q: %s", leak, generic.Intro)
		}
	}
	// A generic channel's heads-up carries no countdown either, since a
	// countdown is the rotation schedule.
	if strings.Contains(generic.HeadsUp, "10 minutes") {
		t.Errorf("generic heads-up carried a countdown: %q", generic.HeadsUp)
	}
}

// The page refuses exactly what the command refuses, in the same words,
// because it calls the same validator.
func TestRotationRefusesWhatTheCommandRefuses(t *testing.T) {
	l := testLab(t)

	// A lead at or beyond the interval would leave a permanent banner.
	if res := l.Rotation(RotationRequest{
		Interval: "1h", LeadMinutes: 60, Disclosure: "full",
	}); res.Error == "" {
		t.Error("a lead equal to the interval was accepted")
	}
	// Below the floor.
	if res := l.Rotation(RotationRequest{Interval: "5m", Disclosure: "full"}); res.Error == "" {
		t.Error("an interval below the floor was accepted")
	}
	// Not a duration at all.
	if res := l.Rotation(RotationRequest{Interval: "soon", Disclosure: "full"}); res.Error == "" {
		t.Error("a non-duration was accepted")
	}
	// An unknown disclosure mode.
	if res := l.Rotation(RotationRequest{Interval: "24h", Disclosure: "vague"}); res.Error == "" {
		t.Error("an unknown disclosure mode was accepted")
	}
	// A refusal fills in nothing else, so the page cannot render a preview of
	// a configuration that would never exist.
	res := l.Rotation(RotationRequest{Interval: "5m", Disclosure: "full"})
	if res.Intro != "" || len(res.Fires) != 0 {
		t.Errorf("a refused configuration still produced a preview: %+v", res)
	}
}

// Forever is a real setting, not a missing value, and the notice for it is a
// different key with different required placeholders.
func TestRotationHandlesForeverRetention(t *testing.T) {
	l := testLab(t)
	res := l.Rotation(RotationRequest{Interval: "24h", Disclosure: "full", RetentionHours: nil})
	if res.Error != "" {
		t.Fatalf("unexpected error: %s", res.Error)
	}
	if res.Intro == "" {
		t.Fatal("no intro rendered for forever retention")
	}
	var noted bool
	for _, n := range res.Notes {
		if strings.Contains(n, "forever") {
			noted = true
		}
	}
	if !noted {
		t.Errorf("forever retention was not called out in the notes: %v", res.Notes)
	}
}

// Keys is the catalog's contract, which is what somebody needs before writing
// a line rather than after being refused one.
func TestKeysCarryTheirRequiredPlaceholders(t *testing.T) {
	l := testLab(t)
	keys := l.Keys()
	if len(keys) == 0 {
		t.Fatal("no keys reported")
	}
	var found bool
	for _, k := range keys {
		if k.Key == "rotation.intro.full" {
			found = true
			if len(k.Required) != 2 {
				t.Errorf("rotation.intro.full requires %v, want cadence and retention", k.Required)
			}
		}
		if k.Required == nil {
			t.Errorf("key %q reported nil placeholders rather than an empty list", k.Key)
		}
	}
	if !found {
		t.Error("rotation.intro.full is missing from the key list")
	}
}

// Lint gives the same answer the bot's startup does, which is the whole point:
// reviewing a line should not need a Go toolchain.
func TestLintMatchesTheCatalogContract(t *testing.T) {
	l := testLab(t)

	if got := l.Lint("rotation.intro.full", "this channel resets every {cadence}, archives last {retention}."); len(got) != 0 {
		t.Errorf("a line meeting the contract was refused: %v", got)
	}
	// Missing a required placeholder: the failure the catalog exists to
	// prevent, since that notice is the server's published retention policy.
	if got := l.Lint("rotation.intro.full", "this channel resets every {cadence}."); len(got) == 0 {
		t.Error("a line missing {retention} was accepted")
	}
	// A placeholder outside the key's required set, which would publish a
	// deletion window a guild deliberately withheld.
	if got := l.Lint("rotation.intro.cadence", "resets every {cadence}, kept {retention}."); len(got) == 0 {
		t.Error("a line carrying an unlisted placeholder was accepted")
	}
	// Never nil, so the page can treat the result as a list without a guard.
	if l.Lint("rotation.intro.full", "{cadence} {retention}") == nil {
		t.Error("Lint returned nil rather than an empty list")
	}
}

// Rolling shows the spread of a key. Rolled against distinct guilds so the
// anti-repeat rule does not masquerade as variety.
func TestRollShowsVariation(t *testing.T) {
	l := testLab(t)
	lines := l.Roll("rotation.intro.full", map[string]string{
		"cadence": "24 hours", "retention": "3 days",
	}, 12)
	if len(lines) != 12 {
		t.Fatalf("got %d lines, want 12", len(lines))
	}
	seen := map[string]bool{}
	for _, s := range lines {
		if s == "" {
			t.Fatal("an empty line was rolled")
		}
		seen[s] = true
	}
	// The catalog requires at least eight lines per key, so twelve rolls
	// across distinct guilds must not collapse to one.
	if len(seen) < 3 {
		t.Errorf("12 rolls produced %d distinct lines: %v", len(seen), lines)
	}
}
