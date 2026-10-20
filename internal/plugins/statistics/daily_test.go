package statistics

import (
	"context"
	"math"
	"strings"
	"testing"
	"time"
)

var dayOne = time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)

// TestDailyPointsMarkWhatTheyCannotSpeakFor: a day before counting began is
// absent rather than zero, a day cut by the window or by coverage is
// partial, and a quiet day inside coverage is a real zero. Getting this
// wrong draws a server that "went silent" on the days merlin was not there.
func TestDailyPointsMarkWhatTheyCannotSpeakFor(t *testing.T) {
	from := dayOne.Add(6 * time.Hour)
	to := dayOne.Add(4*24*time.Hour + 12*time.Hour)   // Sep 5 noon
	covered := dayOne.Add(24*time.Hour + 3*time.Hour) // Sep 2 03:00
	pts := dailyPoints([]DayStat{{Day: dayOne.Add(2 * 24 * time.Hour), Messages: 9, VoiceSeconds: 3600}}, from, to, covered)

	want := []dayState{dayNotCounted, dayPartial, dayCounted, dayCounted, dayPartial}
	if len(pts) != len(want) {
		t.Fatalf("got %d days, want %d", len(pts), len(want))
	}
	for i, w := range want {
		if pts[i].state != w {
			t.Errorf("%s: %s, want %s", pts[i].day.Format("Jan 2"), pts[i].state, w)
		}
	}
	if pts[2].messages != 9 || pts[2].voice != time.Hour || pts[3].messages != 0 {
		t.Fatalf("counts: %+v", pts)
	}
}

// TestTrailingAverageNeedsAWholeWeek: the average is undefined until seven
// whole counted days exist, and a partial day breaks it rather than
// dragging it down.
func TestTrailingAverageNeedsAWholeWeek(t *testing.T) {
	var stats []DayStat
	for i := range 10 {
		stats = append(stats, DayStat{Day: dayOne.AddDate(0, 0, i), Messages: 7 * (i + 1)})
	}
	pts := dailyPoints(stats, dayOne, dayOne.AddDate(0, 0, 9).Add(time.Hour), time.Time{})
	if !math.IsNaN(pts[5].avg) {
		t.Fatal("six days is not a week")
	}
	if pts[6].avg != 28 { // 7+14+...+49 over 7
		t.Fatalf("day 7 average %v", pts[6].avg)
	}
	if !math.IsNaN(pts[9].avg) {
		t.Fatal("a partial last day must not be averaged in")
	}
}

func TestSummarize(t *testing.T) {
	// Two whole weeks starting Tuesday Sep 1: 10 a day the first week, 20
	// the second, with Saturday of week two at 100.
	var stats []DayStat
	for i := range 14 {
		n := 10
		if i >= 7 {
			n = 20
		}
		if i == 11 {
			n = 100
		}
		stats = append(stats, DayStat{Day: dayOne.AddDate(0, 0, i), Messages: n})
	}
	pts := dailyPoints(stats, dayOne, dayOne.AddDate(0, 0, 14), time.Time{})
	s, ok := summarize(pts)
	if !ok || s.days != 14 || s.total != 7*10+6*20+100 {
		t.Fatalf("%+v", s)
	}
	if s.peak.messages != 100 || s.peak.day.Weekday() != time.Saturday || s.weekday != time.Saturday {
		t.Fatalf("peak %+v weekday %s", s.peak, s.weekday)
	}
	if s.quiet.messages != 10 || s.median != 15 {
		t.Fatalf("quiet %d median %v", s.quiet.messages, s.median)
	}
	if !s.trend || s.prior != 10 || s.trendText() != "+214%" {
		t.Fatalf("trend %v %v %q", s.recent, s.prior, s.trendText())
	}

	if _, ok := summarize(dailyPoints(nil, dayOne, dayOne.Add(time.Hour), time.Time{})); ok {
		t.Fatal("no whole day, no summary")
	}
	short, _ := summarize(pts[:5])
	if short.trend || short.trendText() != "needs 2 weeks" {
		t.Fatal("five days cannot be compared week on week")
	}
}

func TestDailyCSVLeavesUncountedDaysBlank(t *testing.T) {
	pts := dailyPoints([]DayStat{{Day: dayOne.AddDate(0, 0, 1), Messages: 1234, VoiceSeconds: 5400}},
		dayOne, dayOne.AddDate(0, 0, 2), dayOne.AddDate(0, 0, 1))
	got := strings.Split(strings.TrimSpace(string(dailyCSV(pts))), "\n")
	want := []string{
		"date_utc,weekday,messages,voice_minutes,messages_7d_avg,status",
		"2026-09-01,Tue,,,,not_counted",
		"2026-09-02,Wed,1234,90,,counted",
	}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("got\n%s", strings.Join(got, "\n"))
	}
}

func TestNiceScaleAndThousands(t *testing.T) {
	for _, c := range []struct{ top, step, ceil float64 }{
		{1154, 250, 1250}, {7, 2, 8}, {0, 1, 1}, {100, 20, 100}, {0.4, 0.1, 0.4},
	} {
		if step, ceil := niceScale(c.top, 5); math.Abs(step-c.step) > 1e-9 || math.Abs(ceil-c.ceil) > 1e-9 {
			t.Errorf("niceScale(%v) = %v, %v", c.top, step, ceil)
		}
	}
	for n, want := range map[int]string{0: "0", 999: "999", 1000: "1,000", 1234567: "1,234,567", -4321: "-4,321"} {
		if got := thousands(n); got != want {
			t.Errorf("thousands(%d) = %q", n, got)
		}
	}
}

// TestRenderDailyLongAndShortWindows: a window of a year draws days
// narrower than a unit, and one of two days barely has an axis; neither
// may fail the export.
func TestRenderDailyLongAndShortWindows(t *testing.T) {
	for _, span := range []int{2, 30, 365} {
		var stats []DayStat
		for i := range span {
			stats = append(stats, DayStat{Day: dayOne.AddDate(0, 0, i), Messages: i % 50, VoiceSeconds: i * 60})
		}
		to := dayOne.AddDate(0, 0, span)
		pts := dailyPoints(stats, dayOne, to, dayOne.AddDate(0, 0, 1))
		s, ok := summarize(pts)
		if !ok {
			t.Fatalf("%d days: no summary", span)
		}
		png, err := renderDaily(pts, s, "the birdhouse", dayOne, to)
		if err != nil || len(png) == 0 {
			t.Fatalf("%d days: %v", span, err)
		}
	}
}

func TestHandleDaily(t *testing.T) {
	p, store := seeded(t)
	var rows []Bucket
	for i := 1; i <= 14; i++ {
		rows = append(rows, Bucket{GuildID: guild, ChannelID: "c1", UserID: "u1", Hour: dayOne.AddDate(0, 0, i).Add(9 * time.Hour), Messages: i})
	}
	_ = store.AddMessages(context.Background(), rows)
	p.now = func() time.Time { return dayOne.AddDate(0, 0, 15).Add(6 * time.Hour) }

	// Names nobody, so an admin runs it without the operator.
	ix, w := run(t, admin, "daily", strArg("from", "2026-09-01"))
	p.handleDaily(context.Background(), ix)
	if !said(w, `filename="daily.png"`) || !said(w, `filename="daily.csv"`) {
		t.Fatalf("both exports attach: %v", w.Bodies)
	}
	if !said(w, "`105` messages over `14` whole days") || !said(w, "busiest `Tue 2026-09-15` with `14`") {
		t.Fatalf("summary: %v", w.Bodies)
	}
	// Sep 1 is cut by when counting began, Sep 16 by now.
	if !said(w, "2 days the window touches are left out") {
		t.Fatalf("left-out days are owned up to: %v", w.Bodies)
	}

	ix, w = run(t, admin, "daily", strArg("from", "2020-01-01"), strArg("to", "2020-01-03"))
	p.handleDaily(context.Background(), ix)
	if !said(w, "Nothing to plot") {
		t.Fatalf("a window before counting says so: %v", w.Bodies)
	}
	ix, w = run(t, admin, "daily", strArg("from", "nonsense"))
	p.handleDaily(context.Background(), ix)
	if !said(w, "That window doesn't work") {
		t.Fatal("a bad date is refused before deferring")
	}
}
