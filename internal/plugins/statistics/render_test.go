package statistics

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
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
	old, oldEmoji := cdnBase, emojiBase
	cdnBase, emojiBase = srv.URL, srv.URL
	t.Cleanup(func() { cdnBase, emojiBase = old, oldEmoji })
	return srv.Client()
}

func samplePeople() []*person {
	return []*person{
		{id: "80351110224678912", name: "zoe", avatar: "abc", count: 42, voice: 90 * time.Minute,
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
	rep := report{people: samplePeople(), messages: 69, channels: 4}

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
	wantH := px(heatTop + gridHeight(4) + sectionH + gridHeight(1) + pad) // zoe is in voice too
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
	if cfg.Height != px(heatTop+40+pad) {
		t.Fatalf("the empty state is %d tall, want %d", cfg.Height, px(heatTop+40+pad))
	}
	// Nobody in voice: no section for it.
	one, err = renderPNG(client, report{people: samplePeople()[3:]}, "birdland", windowStart, windowStart.Add(time.Hour), 24)
	if err != nil {
		t.Fatal(err)
	}
	if cfg, err = png.DecodeConfig(bytes.NewReader(one)); err != nil || cfg.Height != px(heatTop+gridHeight(1)+pad) {
		t.Fatalf("a text-only report is %d tall, want %d (%v)", cfg.Height, px(heatTop+gridHeight(1)+pad), err)
	}
}

// TestRenderSurvivesADeadCDN: a card is about the name beside the picture, so
// an unreachable avatar host costs the circle, never the report.
func TestRenderSurvivesADeadCDN(t *testing.T) {
	old, oldEmoji := cdnBase, emojiBase
	cdnBase, emojiBase = "http://127.0.0.1:1", "http://127.0.0.1:1"
	defer func() { cdnBase, emojiBase = old, oldEmoji }()

	body, err := renderPNG(&http.Client{Timeout: time.Second}, report{people: samplePeople()}, "birdland", windowStart, windowStart.Add(time.Hour), 24)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := png.DecodeConfig(bytes.NewReader(body)); err != nil {
		t.Fatal(err)
	}
}

// TestDisplayNameSegments is the emoji and CJK case. Emoji become inline art
// keyed for the CDN; CJK, which neither the Go fonts nor the CDN can draw,
// is dropped rather than rendered as nothing with no explanation.
func TestDisplayNameSegments(t *testing.T) {
	f, err := newFaces()
	if err != nil {
		t.Fatal(err)
	}
	got := segments(f.name, "🦅 kestrel 隼")
	want := []seg{{emoji: "1f985"}, {text: " kestrel "}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("segments: %+v, want %+v", got, want)
	}
	if got := segments(f.name, "zoe"); !reflect.DeepEqual(got, []seg{{text: "zoe"}}) {
		t.Fatalf("an ordinary name was mangled: %+v", got)
	}
	// A name with nothing drawable left falls back to the id: ugly, and
	// unambiguous, which beats an empty card. All-emoji is drawable now.
	p := &person{id: "80351110224678912", name: "隼"}
	if got := displayName(f.name, p); plain(got) != p.id {
		t.Fatalf("displayName fallback: %+v", got)
	}
	if got := displayName(f.name, &person{id: "1", name: "🦅🦅"}); len(got) != 2 || got[0].emoji != "1f985" {
		t.Fatalf("an all-emoji name should draw as emoji: %+v", got)
	}
}

// TestTwemojiKeys pins the file-name convention against the sequences that
// trip it: variation selectors dropped, kept inside joiner sequences,
// keycaps, flags, skin tones, and a symbol the font can draw left as text.
func TestTwemojiKeys(t *testing.T) {
	f, err := newFaces()
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string][]seg{
		"\u2764\ufe0f":               {{emoji: "2764"}},
		"\U0001F468\u200d\U0001F4BB": {{emoji: "1f468-200d-1f4bb"}},
		"#\ufe0f\u20e3":              {{emoji: "23-20e3"}},
		"\U0001F1F3\U0001F1F4":       {{emoji: "1f1f3-1f1f4"}},
		"\U0001F44B\U0001F3FD hi":    {{emoji: "1f44b-1f3fd"}, {text: " hi"}},
		"(c) \u00a9 plain":           {{text: "(c) \u00a9 plain"}},
		"\u00a9\ufe0f":               {{emoji: "a9"}},
		"\U0001F427-general-chat":    {{emoji: "1f427"}, {text: "-general-chat"}},
	}
	for in, want := range cases {
		if got := segments(f.body, in); !reflect.DeepEqual(got, want) {
			t.Errorf("segments(%q) = %+v, want %+v", in, got, want)
		}
	}
}

// TestFitRichMeasures: truncation counts emoji at their drawn width and
// leaves room for the ellipsis.
func TestFitRichMeasures(t *testing.T) {
	f, err := newFaces()
	if err != nil {
		t.Fatal(err)
	}
	segs := segments(f.name, "🦅🦅🦅 a display name long enough that it has to be cut short somewhere")
	got := fitRich(f.name, segs, 120)
	if len(got) == 0 || richWidth(f.name, got) > 120 || got[len(got)-1].text != "..." {
		t.Fatalf("fitRich returned %+v at width %d", got, richWidth(f.name, got))
	}
	if short := segments(f.name, "zoe"); !reflect.DeepEqual(fitRich(f.name, short, 200), short) {
		t.Fatal("a string that fits must be left alone")
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
	got := totalsLine(report{people: samplePeople(), messages: 69, channels: 4})
	if got != "4 people, 69 messages, 4 channels" {
		t.Fatalf("totals line: %q", got)
	}
}

// TestHeatmap: a window over several days grows the canvas by the grid,
// the busiest day is the brightest cell, a day with nothing is the empty
// shade, and a day outside the window is not drawn.
func TestHeatmap(t *testing.T) {
	client := stubCDN(t)
	// windowStart is a Tuesday. Through the Sunday nineteen days on, so the
	// grid has a leading Monday outside the window and three columns.
	start, end := windowStart, windowStart.AddDate(0, 0, 19)
	day := func(n int) time.Time { return start.Truncate(24 * time.Hour).AddDate(0, 0, n) }
	rep := report{people: samplePeople()[:1], days: []DayStat{
		{Day: day(0), Messages: 100},
		{Day: day(1), Messages: 10, VoiceSeconds: 7200},
		{Day: day(2), Messages: 1},
	}}

	body, err := renderPNG(client, rep, "birdland", start, end, 24)
	if err != nil {
		t.Fatal(err)
	}
	img, err := png.Decode(bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if dir := os.Getenv("ACTIVITY_SAMPLE_DIR"); dir != "" {
		if err := os.WriteFile(filepath.Join(dir, "heatmap.png"), body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	hm := newHeatmap(rep.days, false, start, end, cardW)
	if hm.cols != 3 || hm.rows != 7 || hm.pitch != cellPitch {
		t.Fatalf("planned %dx%d at pitch %d, want 3x7 at %d", hm.cols, hm.rows, hm.pitch, cellPitch)
	}
	if want := px(heatTop + hm.height() + heatGap + gridHeight(1) + sectionH + gridHeight(1) + pad); img.Bounds().Dy() != want {
		t.Fatalf("canvas is %d tall, want %d", img.Bounds().Dy(), want)
	}

	at := func(col, row int) color.RGBA {
		x, y := pad+heatLabelW+col*hm.pitch+1, heatTop+heatMonthH+row*hm.pitch+1
		return color.RGBAModel.Convert(img.At(px(x), px(y))).(color.RGBA)
	}
	// Tuesday of week one is the busiest: messages at their peak, no voice
	// in a window that has some, so half the score and the third shade.
	if got := at(0, 1); got != heatColors[3] {
		t.Fatalf("busiest day drew %v, want %v", got, heatColors[3])
	}
	// Wednesday: a tenth of the messages and all of the voice, so a little
	// over half and the same shade; Thursday: next to nothing, the first.
	if got := at(0, 2); got != heatColors[3] {
		t.Fatalf("voice day drew %v, want %v", got, heatColors[3])
	}
	if got := at(0, 3); got != heatColors[1] {
		t.Fatalf("a quiet day drew %v, want %v", got, heatColors[1])
	}
	// Friday has no row at all and is the empty shade; the Monday before
	// the window is background.
	if got := at(0, 4); got != heatColors[0] {
		t.Fatalf("an empty day drew %v, want %v", got, heatColors[0])
	}
	if got := at(0, 0); got != bgColor {
		t.Fatalf("a day before the window drew %v, want background", got)
	}

	if dir := os.Getenv("ACTIVITY_SAMPLE_DIR"); dir != "" {
		var days []DayStat
		for n := range 60 {
			days = append(days, DayStat{Day: day(n - 40), Messages: (n * 37) % 90, VoiceSeconds: ((n * 53) % 7) * 1800})
		}
		wide := report{people: samplePeople(), messages: 2000, voice: 40 * time.Hour, channels: 4, days: days}
		body, err := renderPNG(client, wide, "birdland", day(-40), day(20), 24)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "heatmap-wide.png"), body, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	// Three years in a one-card canvas cannot fit at the minimum pitch, so
	// only the most recent weeks are kept and the width still holds.
	long := newHeatmap(nil, false, start.AddDate(-3, 0, 0), end, cardW)
	if long.pitch != cellPitchMin || long.cols*long.pitch > cardW-heatLabelW {
		t.Fatalf("a long window planned %d weeks at pitch %d", long.cols, long.pitch)
	}
	if last := long.first.AddDate(0, 0, 7*long.cols-1); last.Before(end.Truncate(24*time.Hour)) {
		t.Fatalf("the kept weeks end %s, before the window does", last)
	}
}

// TestHourlyHeatmap: a short window is one row per day and a cell per
// hour, the hours before the window on its first day are not drawn, and
// a window under two hours draws nothing.
func TestHourlyHeatmap(t *testing.T) {
	client := stubCDN(t)
	start, end := windowStart, windowStart.Add(28*time.Hour) // 14:00 Tue to 18:00 Wed
	rep := report{people: samplePeople()[:1], hourly: true, days: []DayStat{
		{Day: start, Messages: 50},
		{Day: start.Add(3 * time.Hour), Messages: 5, VoiceSeconds: 3600},
		{Day: start.Add(25 * time.Hour), Messages: 50, VoiceSeconds: 3600},
	}}
	body, err := renderPNG(client, rep, "birdland", start, end, 24)
	if err != nil {
		t.Fatal(err)
	}
	img, err := png.Decode(bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if dir := os.Getenv("ACTIVITY_SAMPLE_DIR"); dir != "" {
		if err := os.WriteFile(filepath.Join(dir, "heatmap-hourly.png"), body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	hm := newHeatmap(rep.days, true, start, end, cardW)
	if hm.cols != 24 || hm.rows != 2 || hm.pitch < cellPitchMin {
		t.Fatalf("planned %dx%d at pitch %d, want 24x2", hm.cols, hm.rows, hm.pitch)
	}
	at := func(col, row int) color.RGBA {
		x, y := pad+hm.labelW+col*hm.pitch+1, heatTop+heatMonthH+row*hm.pitch+1
		return color.RGBAModel.Convert(img.At(px(x), px(y))).(color.RGBA)
	}
	if got := at(13, 0); got != bgColor {
		t.Fatalf("an hour before the window drew %v, want background", got)
	}
	if got := at(14, 0); got != heatColors[3] {
		t.Fatalf("the first hour drew %v, want %v", got, heatColors[3])
	}
	if got := at(15, 1); got != heatColors[4] {
		t.Fatalf("the busiest hour drew %v, want %v", got, heatColors[4])
	}
	if got := at(16, 0); got != heatColors[0] {
		t.Fatalf("an empty hour drew %v, want %v", got, heatColors[0])
	}
	if got := at(18, 1); got != bgColor {
		t.Fatalf("an hour after the window drew %v, want background", got)
	}
	if none := newHeatmap(nil, true, start, start.Add(30*time.Minute), cardW); none.height() != 0 {
		t.Fatal("a window inside one hour should draw no grid")
	}
}

// TestVoiceListing: the people in voice are their own ranked grid under
// the chatters, and a member doing both is in both.
func TestVoiceListing(t *testing.T) {
	client := stubCDN(t)
	people := samplePeople()
	people = append(people, &person{id: "80351110224678916", name: "mic", voice: 5 * time.Hour, channels: map[string]bool{}})
	rep := report{people: rank(map[string]*person{"a": people[0], "b": people[1], "c": people[2], "d": people[3], "e": people[4]})}
	chat, voice := chatters(rep), voicers(rep)
	if len(chat) != 4 || len(voice) != 2 || voice[0].name != "mic" || voice[1].name != "zoe" {
		t.Fatalf("chatters %d, voicers %v", len(chat), voice)
	}
	body, err := renderPNG(client, rep, "birdland", windowStart, windowStart.Add(time.Hour), 24)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := png.DecodeConfig(bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if want := px(heatTop + gridHeight(4) + sectionH + gridHeight(2) + pad); cfg.Height != want {
		t.Fatalf("canvas is %d tall, want %d", cfg.Height, want)
	}
	if dir := os.Getenv("ACTIVITY_SAMPLE_DIR"); dir != "" {
		if err := os.WriteFile(filepath.Join(dir, "voice-listing.png"), body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestCardStats(t *testing.T) {
	if got := cardStats(&person{count: 3, voice: time.Hour}, true); got != iconVoice+" 1.0h   "+iconMessages+" 3 messages" {
		t.Fatalf("voice first: %q", got)
	}
	for _, tc := range []struct {
		p    person
		want string
	}{
		{person{count: 1}, iconMessages + " 1 message"},
		{person{count: 3, voice: 90 * time.Minute}, iconMessages + " 3 messages   " + iconVoice + " 1.5h"},
		{person{voice: 2 * time.Minute}, iconVoice + " <0.1h"},
	} {
		if got := cardStats(&tc.p, false); got != tc.want {
			t.Fatalf("cardStats = %q, want %q", got, tc.want)
		}
	}
}
