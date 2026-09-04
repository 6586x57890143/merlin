package activity

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// stubCDN serves a small gradient in place of Discord's avatar CDN, so the
// real fetch, decode, scale and circle mask path runs with no network.
func stubCDN(t *testing.T) *http.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		img := image.NewRGBA(image.Rect(0, 0, 64, 64))
		for x := range 64 {
			for y := range 64 {
				img.Set(x, y, color.RGBA{R: uint8(x * 4), G: 0x66, B: uint8(y * 4), A: 0xFF})
			}
		}
		var buf bytes.Buffer
		_ = png.Encode(&buf, img)
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(buf.Bytes())
	}))
	t.Cleanup(srv.Close)
	old := cdnBase
	cdnBase = srv.URL
	t.Cleanup(func() { cdnBase = old })
	return srv.Client()
}

func samplePeople() []*person {
	return []*person{
		{id: "80351110224678912", name: "zoe", avatar: "abc", count: 42,
			channels: map[string]bool{"general": true, "media": true}},
		{id: "80351110224678913", name: "a display name long enough that it has to be cut short somewhere", count: 17,
			channels: map[string]bool{"general": true, "media": true, "off-topic": true, "art": true}},
		{id: "80351110224678914", name: "🦅 kestrel 隼", avatar: "def", count: 9,
			channels: map[string]bool{"art": true}},
		{id: "80351110224678915", name: "abe", count: 1, channels: map[string]bool{"media": true}},
	}
}

// TestRenderPNG draws a full report against the stub cdn and checks the
// canvas geometry, which is the part a layout change breaks silently.
func TestRenderPNG(t *testing.T) {
	client := stubCDN(t)
	rep := report{people: samplePeople(), messages: 69, busy: 4, looked: 9, skipped: 1}

	body, err := renderPNG(client, rep, "birdland", windowStart, windowStart.Add(4*time.Hour), 24)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := png.DecodeConfig(bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	// Four people is two rows of a three wide grid.
	wantW := px(pad*2 + cols*cardW + (cols-1)*gutter)
	wantH := px(gridTop + 2*cardH + gutter + pad)
	if cfg.Width != wantW || cfg.Height != wantH {
		t.Fatalf("canvas %dx%d, want %dx%d", cfg.Width, cfg.Height, wantW, wantH)
	}

	// Kept for eyeballing: the checks above cover geometry, not whether it
	// reads well, and that is the half a person has to look at.
	if dir := os.Getenv("ACTIVITY_SAMPLE_DIR"); dir != "" {
		if err := os.WriteFile(filepath.Join(dir, "activity.png"), body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// TestRenderNarrowAndEmpty: one person must not pad out to three columns of
// dead space, and nobody at all must not draw an empty grid.
func TestRenderNarrowAndEmpty(t *testing.T) {
	client := stubCDN(t)

	one, err := renderPNG(client, report{people: samplePeople()[:1], messages: 42}, "birdland", windowStart, windowStart.Add(time.Hour), 24)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := png.DecodeConfig(bytes.NewReader(one))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Width != px(pad*2+cardW) {
		t.Fatalf("a one person report is %d wide, want %d", cfg.Width, px(pad*2+cardW))
	}

	empty, err := renderPNG(client, report{}, "birdland", windowStart, windowStart.Add(time.Hour), 24)
	if err != nil {
		t.Fatal(err)
	}
	if cfg, err = png.DecodeConfig(bytes.NewReader(empty)); err != nil {
		t.Fatal(err)
	}
	if cfg.Height != px(gridTop+40+pad) {
		t.Fatalf("the empty state is %d tall, want %d", cfg.Height, px(gridTop+40+pad))
	}
}

// TestRenderSurvivesADeadCDN: a card is about the name beside the picture, so
// an unreachable avatar host costs the circle, never the report.
func TestRenderSurvivesADeadCDN(t *testing.T) {
	old := cdnBase
	cdnBase = "http://127.0.0.1:1"
	defer func() { cdnBase = old }()

	body, err := renderPNG(&http.Client{Timeout: time.Second}, report{people: samplePeople()}, "birdland", windowStart, windowStart.Add(time.Hour), 24)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := png.DecodeConfig(bytes.NewReader(body)); err != nil {
		t.Fatal(err)
	}
}

// TestSanitizeName is the emoji and CJK case. The Go fonts carry neither, and
// an undrawable rune renders as nothing at all, so a card would show a blank
// line where a name should be with nothing saying why.
func TestSanitizeName(t *testing.T) {
	f, err := newFaces()
	if err != nil {
		t.Fatal(err)
	}
	if got := sanitizeName(f.name, "🦅 kestrel 隼"); got != "kestrel" {
		t.Fatalf("sanitizeName kept something undrawable: %q", got)
	}
	if got := sanitizeName(f.name, "zoe"); got != "zoe" {
		t.Fatalf("sanitizeName mangled an ordinary name: %q", got)
	}
	// A name with nothing drawable left falls back to the id: ugly, and
	// unambiguous, which beats an empty card.
	p := &person{id: "80351110224678912", name: "🦅🦅"}
	if got := displayName(f.name, p); got != p.id {
		t.Fatalf("displayName fallback: %q", got)
	}
}

func TestTruncateMeasures(t *testing.T) {
	f, err := newFaces()
	if err != nil {
		t.Fatal(err)
	}
	long := "a display name long enough that it has to be cut short somewhere"
	got := truncate(f.name, long, 120)
	if got == long || len(got) == 0 {
		t.Fatalf("truncate returned %q", got)
	}
	if measure(f.name, got) > 120 {
		t.Fatalf("%q still overruns its box", got)
	}
	if truncate(f.name, "zoe", 200) != "zoe" {
		t.Fatal("a string that fits must be left alone")
	}
}

func TestPlural(t *testing.T) {
	if plural(1, "message") != "1 message" || plural(2, "message") != "2 messages" {
		t.Fatal("plural is wrong")
	}
}

func TestDefaultAvatarIndex(t *testing.T) {
	if got := defaultAvatarIndex("80351110224678912"); got < 0 || got > 5 {
		t.Fatalf("default avatar index out of range: %d", got)
	}
	if defaultAvatarIndex("not-a-snowflake") != 0 {
		t.Fatal("an unparseable id should fall back to the first default avatar")
	}
}

func TestTotalsLine(t *testing.T) {
	got := totalsLine(report{people: samplePeople(), messages: 69, busy: 4, looked: 9, skipped: 1, truncated: true})
	if got != "4 people, 69 messages, 4 of 10 channels, stopped early" {
		t.Fatalf("totals line: %q", got)
	}
}
