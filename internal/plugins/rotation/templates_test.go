package rotation

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/6586x57890143/merlin/internal/settings"
	"github.com/6586x57890143/merlin/internal/voice"
)

// noticePluginPickingLine builds a Plugin whose voice always selects line
// index i, so a test can walk the whole catalog instead of sampling
// whichever line the RNG happened to hand it. A notice that is accurate
// four times out of five is not accurate.
func noticePluginPickingLine(t *testing.T, i int) *Plugin {
	t.Helper()
	sp, err := voice.New(slog.New(slog.NewTextHandler(io.Discard, nil)),
		voice.WithRand(func(n int) int { return i % n }))
	if err != nil {
		t.Fatalf("voice.New: %v", err)
	}
	return &Plugin{voice: sp}
}

// maxCatalogLines is comfortably above the largest key's line count, so
// walking 0..maxCatalogLines with a modulo picker covers every line of
// every key regardless of how many there are.
const maxCatalogLines = 24

// TestRetentionNoticeDescribesRetentionNotInterval is a regression for a
// false public statement. The notice used to be handed rc.IntervalMinutes and
// tell members "nothing posted here roosts longer than [interval]", the
// rotation cadence presented as the deletion policy, when the two are
// independent settings. This bot's whole justification is being able to point
// at what it actually does, so an inaccurate retention claim is worse than
// none at all.
//
// Now that the wording varies, this checks every line the catalog could
// possibly produce, which is the assertion that actually matters: one
// tempting rewrite that drops the retention window would otherwise show up
// only in production, on whichever rotation happened to draw it.
func TestRetentionNoticeDescribesRetentionNotInterval(t *testing.T) {
	hours := 3
	rc := settings.RotationChannel{GuildID: "g1", IntervalMinutes: 24 * 60, RetentionHours: &hours}

	for i := range maxCatalogLines {
		p := noticePluginPickingLine(t, i)
		notice := p.retentionNotice(context.Background(), rc)
		if !strings.Contains(notice, "3 hours") {
			t.Fatalf("line %d does not state the actual 3-hour retention window: %s", i, notice)
		}
		if !strings.Contains(notice, "1 day") {
			t.Fatalf("line %d does not state the 1-day rotation cadence: %s", i, notice)
		}
	}
}

// TestRetentionNoticeOnKeepForeverPromisesNoDeletion covers the worst case of
// the old behavior: retention unset means archives are kept indefinitely, but
// the notice still announced a deletion window derived from the interval,
// telling members their content would be erased when nothing would erase it.
//
// Checked across the whole catalog, because "kept forever" and "deleted
// eventually" are separate sets of lines and the risk is somebody writing a
// nicely-worded deletion promise into the wrong one.
func TestRetentionNoticeOnKeepForeverPromisesNoDeletion(t *testing.T) {
	rc := settings.RotationChannel{GuildID: "g1", IntervalMinutes: 24 * 60, RetentionHours: nil}

	for i := range maxCatalogLines {
		p := noticePluginPickingLine(t, i)
		notice := p.retentionNotice(context.Background(), rc)
		for _, claim := range []string{"gone for good", "deletes it", "permanently deleted", "nothing left to find"} {
			if strings.Contains(notice, claim) {
				t.Fatalf("keep-forever line %d promises deletion (%q): %s", i, claim, notice)
			}
		}
		if !strings.Contains(notice, "1 day") {
			t.Fatalf("keep-forever line %d does not state the rotation cadence: %s", i, notice)
		}
	}
}

// TestRotationSummaryNeverPrintsAPointer is the audit-trail regression. The
// audit line interpolated the *int RetentionHours with %v, so a real record
// in production reads "retention=0x55c4509432e8", the one irreversible
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

// The retention notice is the single most-read thing Merlin posts: it lands
// in the busiest channel in the server on every rotation. It has to look
// like the same bot as everything else, which means going through
// core.NewEmbed and carrying the brand file its footer icon references. An
// embed whose footer points at an attachment that was never uploaded shows
// a broken icon to the whole server.
func TestRetentionNoticeIsBrandedAndCarriesItsIcon(t *testing.T) {
	ops, _, _, p, rc := setupRotation(t, finiteRetentionRC())

	if err := p.rotate(context.Background(), "g1", rc); err != nil {
		t.Fatalf("rotate: %v", err)
	}

	if len(ops.complexSends) != 1 {
		t.Fatalf("complex sends = %d, want 1; the notice did not go out as an embed plus attachment", len(ops.complexSends))
	}
	sent := ops.complexSends[0]
	if sent.Embed == nil {
		t.Fatal("the notice was not sent as an embed")
	}
	if sent.Embed.Footer == nil || sent.Embed.Footer.Text != "Merlin" {
		t.Error("the notice carries no Merlin footer, so it reads as a different bot than every other message")
	}
	if sent.Embed.Color == 0 {
		t.Error("the notice has no colour, the one visual cue that ties it to the rest of the bot")
	}
	if !strings.Contains(sent.Embed.Description, "1 day") {
		t.Errorf("the notice lost its cadence disclosure: %q", sent.Embed.Description)
	}

	// The footer icon is an attachment:// reference, so the file has to
	// travel in the same request or it renders broken.
	if len(sent.Files) != 1 {
		t.Fatalf("attached files = %d, want 1; the footer icon would render broken", len(sent.Files))
	}
	if !strings.Contains(sent.Embed.Footer.IconURL, sent.Files[0].Name) {
		t.Errorf("footer icon %q does not match the attached file %q", sent.Embed.Footer.IconURL, sent.Files[0].Name)
	}
}
