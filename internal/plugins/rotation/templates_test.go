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

// disclosureRC builds a channel on a 1 day cadence with a 3 hour retention
// window, so "1 day" and "3 hours" are two distinct strings a notice either
// does or does not contain.
func disclosureRC(d settings.Disclosure, forever bool) settings.RotationChannel {
	rc := settings.RotationChannel{GuildID: "g1", IntervalMinutes: 24 * 60, Disclosure: d}
	if !forever {
		hours := 3
		rc.RetentionHours = &hours
	}
	return rc
}

// TestIntroKeySuppliesExactlyTheVarsItsKeyRequires is the structural
// guarantee behind the whole disclosure feature, and the reason introKey
// returns the key and its vars together instead of selecting in one place and
// populating in another.
//
// voice.Line does not error on a missing placeholder, it silently falls back
// to the compiled-in line, so a mismatch here would degrade every notice for
// that mode to the plainest possible wording and nothing would say why. This
// asserts the two sides agree for all eight combinations rather than trusting
// that whoever edits one switch remembers the other.
func TestIntroKeySuppliesExactlyTheVarsItsKeyRequires(t *testing.T) {
	for _, d := range []settings.Disclosure{
		settings.DisclosureFull, settings.DisclosureCadence,
		settings.DisclosureRetention, settings.DisclosureGeneric, "",
	} {
		for _, forever := range []bool{false, true} {
			key, vars := introKey(disclosureRC(d, forever))

			required := map[string]bool{}
			for _, name := range voice.RequiredVars(key) {
				required[name] = true
			}
			for _, name := range voice.RequiredVars(key) {
				if _, ok := vars[name]; !ok {
					t.Errorf("disclosure %q forever=%v: key %s requires {%s} but introKey supplied no value, so every line falls back",
						d, forever, key, name)
				}
			}
			for name := range vars {
				if !required[name] {
					t.Errorf("disclosure %q forever=%v: key %s does not declare {%s}, so supplying it is dead weight at best and a leak at worst",
						d, forever, key, name)
				}
			}
		}
	}
}

// TestDisclosureModesNeverPublishWhatTheySuppress is the safety-critical one.
//
// The whole point of the narrower modes is the *absence* of a fact, and an
// absence is exactly what a spot check misses: the wording varies per
// rotation, so a single tempting line that mentions the archive window in
// cadence mode would surface only in production, on whichever rotation
// happened to draw it. This walks every line of every key.
//
// The catalog's own required-placeholder validation already stops a
// {retention} substitution reaching a cadence-only line. What it cannot catch
// is prose, so this asserts on the rendered output instead.
func TestDisclosureModesNeverPublishWhatTheySuppress(t *testing.T) {
	const (
		cadenceText   = "1 day"
		retentionText = "3 hours"
	)

	cases := []struct {
		mode        settings.Disclosure
		forever     bool
		wantCadence bool
		wantReten   bool
	}{
		{settings.DisclosureFull, false, true, true},
		{settings.DisclosureFull, true, true, false},
		{settings.DisclosureCadence, false, true, false},
		{settings.DisclosureCadence, true, true, false},
		{settings.DisclosureRetention, false, false, true},
		{settings.DisclosureRetention, true, false, false},
		{settings.DisclosureGeneric, false, false, false},
		{settings.DisclosureGeneric, true, false, false},
		// An unset mode is today's behaviour, which is full disclosure.
		{"", false, true, true},
	}

	for _, c := range cases {
		rc := disclosureRC(c.mode, c.forever)
		for i := range maxCatalogLines {
			p := noticePluginPickingLine(t, i)
			notice := p.retentionNotice(context.Background(), rc)

			if got := strings.Contains(notice, cadenceText); got != c.wantCadence {
				t.Errorf("mode %q forever=%v line %d: states the rotation cadence = %v, want %v: %s",
					c.mode, c.forever, i, got, c.wantCadence, notice)
			}
			if got := strings.Contains(notice, retentionText); got != c.wantReten {
				t.Errorf("mode %q forever=%v line %d: states the retention window = %v, want %v: %s",
					c.mode, c.forever, i, got, c.wantReten, notice)
			}
			if strings.ContainsAny(notice, "{}") {
				t.Errorf("mode %q forever=%v line %d leaked a placeholder: %s", c.mode, c.forever, i, notice)
			}
		}
	}
}

// TestPlainRetentionNoticeRespectsTheDisclosureMode covers the backstop.
//
// plainRetentionNotice is reached only when the catalog produces nothing at
// all, which is the moment nobody is watching. If it ignored the mode it
// would publish the cadence and the deletion window of a guild that had
// explicitly chosen to publish neither, and it would do so silently and in
// public. That makes the fallback worth its own test rather than trusting it
// to be unreachable.
func TestPlainRetentionNoticeRespectsTheDisclosureMode(t *testing.T) {
	cases := []struct {
		mode        settings.Disclosure
		forever     bool
		wantCadence bool
		wantReten   bool
	}{
		{settings.DisclosureFull, false, true, true},
		{settings.DisclosureCadence, false, true, false},
		{settings.DisclosureRetention, false, false, true},
		{settings.DisclosureRetention, true, false, false},
		{settings.DisclosureGeneric, false, false, false},
	}

	for _, c := range cases {
		got := plainRetentionNotice(disclosureRC(c.mode, c.forever))
		if got == "" {
			t.Errorf("mode %q forever=%v: the fallback said nothing at all", c.mode, c.forever)
		}
		if has := strings.Contains(got, "1 day"); has != c.wantCadence {
			t.Errorf("fallback mode %q forever=%v: states cadence = %v, want %v: %s", c.mode, c.forever, has, c.wantCadence, got)
		}
		if has := strings.Contains(got, "3 hours"); has != c.wantReten {
			t.Errorf("fallback mode %q forever=%v: states retention = %v, want %v: %s", c.mode, c.forever, has, c.wantReten, got)
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

// The retention notice is the single most-read thing merlin posts: it lands
// in the busiest channel in the server on every rotation. It has to look
// like the same bot as everything else, which means going through
// core.NewEmbed and carrying every file it references. An embed pointing at
// an attachment that was never uploaded shows a broken frame to the whole
// server.
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
	// No footer and no timestamp: both only repeated what Discord already
	// draws above the message. The colour and the face are what tie the
	// notice to the rest of the bot now.
	if sent.Embed.Footer != nil {
		t.Errorf("the notice grew a footer back: %+v", sent.Embed.Footer)
	}
	if sent.Embed.Timestamp != "" {
		t.Errorf("the notice grew a timestamp back: %q", sent.Embed.Timestamp)
	}
	if sent.Embed.Color == 0 {
		t.Error("the notice has no colour, the one visual cue that ties it to the rest of the bot")
	}
	if !strings.Contains(sent.Embed.Description, "1 day") {
		t.Errorf("the notice lost its cadence disclosure: %q", sent.Embed.Description)
	}

	// Every attachment:// URL in the embed has to have a matching upload in
	// the same request. Discord renders a reference to a file that was not
	// sent as a broken frame, and nothing about the code that built the
	// embed would look wrong, so this asserts on the relationship rather
	// than on a file count that changes whenever the design does.
	attached := map[string]bool{}
	for _, f := range sent.Files {
		attached[f.Name] = true
	}
	var referenced []string
	if sent.Embed.Footer != nil {
		referenced = append(referenced, sent.Embed.Footer.IconURL)
	}
	if sent.Embed.Thumbnail != nil {
		referenced = append(referenced, sent.Embed.Thumbnail.URL)
	}
	if sent.Embed.Image != nil {
		referenced = append(referenced, sent.Embed.Image.URL)
	}
	for _, url := range referenced {
		name, ok := strings.CutPrefix(url, "attachment://")
		if !ok {
			continue
		}
		if !attached[name] {
			t.Errorf("embed references %q but that file was never uploaded, so it renders as a broken image", url)
		}
	}

	// And the notice should carry a face, which is now the only brand mark
	// on it.
	if sent.Embed.Thumbnail == nil {
		t.Error("the notice has no mood thumbnail")
	}
}
