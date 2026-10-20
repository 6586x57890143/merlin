package statistics

import (
	"bytes"
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"image"
	"image/color"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/disgoorg/disgo/discord"
	"golang.org/x/image/draw"
	"golang.org/x/image/font/gofont/gobold"
	"golang.org/x/image/vector"

	"github.com/6586x57890143/merlin/internal/core"
)

// /statistics daily is the server's pulse on its own: chat messages per UTC
// day as the main chart, voice hours per day underneath as a smaller control
// on the same date axis, and the numbers behind both as a csv. It names
// nobody, so it sits behind statistics.view like channels and members rather
// than behind the operator check.
//
// Two charts rather than one with a second y-axis: messages and hours are
// different units on different scales, and a shared plot would let whichever
// axis was drawn last decide what looks busy.

const (
	dailyImageName = "daily.png"
	dailyCSVName   = "daily.csv"
	dayLen         = 24 * time.Hour
	// avgWindow is the trailing average drawn over the bars. A week, so the
	// weekday rhythm averages out and what is left is the trend.
	avgWindow = 7
)

// dayState says what a day's bar can be trusted for.
type dayState int

const (
	dayCounted    dayState = iota // a whole day, fully counted
	dayPartial                    // cut by the window or by when counting began
	dayNotCounted                 // before anything was counted: absent, not zero
)

func (s dayState) String() string {
	return [...]string{"counted", "partial", "not_counted"}[s]
}

type dayPoint struct {
	day      time.Time
	messages int
	voice    time.Duration
	state    dayState
	avg      float64 // trailing avgWindow-day message average; NaN where undefined
}

// dailyPoints lays every UTC day the window touches out in order, zero
// filled, since Days only returns days with something in them and a missing
// bar must read as a quiet day, not a skipped one. Days the buckets cannot
// speak for (before counting began, or pruned) are marked rather than shown
// as zeroes, which would read as the server having gone silent.
func dailyPoints(stats []DayStat, from, to, covered time.Time) []dayPoint {
	by := make(map[time.Time]DayStat, len(stats))
	for _, d := range stats {
		by[d.Day.UTC().Truncate(dayLen)] = d
	}
	var out []dayPoint
	for d := from.UTC().Truncate(dayLen); d.Before(to); d = d.Add(dayLen) {
		s := by[d]
		pt := dayPoint{day: d, messages: s.Messages, voice: time.Duration(s.VoiceSeconds) * time.Second, avg: math.NaN()}
		switch {
		case !covered.IsZero() && !d.Add(dayLen).After(covered):
			pt.state = dayNotCounted
		case d.Before(from) || d.Add(dayLen).After(to) || (!covered.IsZero() && d.Before(covered)):
			pt.state = dayPartial
		}
		out = append(out, pt)
	}
	// The average only runs over whole counted days: a partial or uncounted
	// day in the window would drag it down for a week afterwards.
	for i := range out {
		if i+1 < avgWindow {
			continue
		}
		sum, ok := 0, true
		for _, p := range out[i+1-avgWindow : i+1] {
			if p.state != dayCounted {
				ok = false
				break
			}
			sum += p.messages
		}
		if ok {
			out[i].avg = float64(sum) / avgWindow
		}
	}
	return out
}

// dailySummary is what the tiles and the embed say, over whole counted days
// only.
type dailySummary struct {
	days          int
	total         int
	voice         time.Duration
	mean, median  float64
	peak, quiet   dayPoint
	weekday       time.Weekday
	weekdayMean   float64
	recent, prior float64 // the last two whole weeks' averages
	trend         bool    // whether there were two weeks to compare
	activeDays    int     // days with at least one message
}

func summarize(pts []dayPoint) (dailySummary, bool) {
	var s dailySummary
	var full []dayPoint
	for _, p := range pts {
		if p.state == dayCounted {
			full = append(full, p)
		}
	}
	if len(full) == 0 {
		return s, false
	}
	s.days = len(full)
	s.peak, s.quiet = full[0], full[0]
	counts := make([]int, 0, len(full))
	var wdSum, wdN [7]int
	for _, p := range full {
		s.total += p.messages
		s.voice += p.voice
		counts = append(counts, p.messages)
		if p.messages > s.peak.messages {
			s.peak = p
		}
		if p.messages < s.quiet.messages {
			s.quiet = p
		}
		if p.messages > 0 {
			s.activeDays++
		}
		wdSum[p.day.Weekday()] += p.messages
		wdN[p.day.Weekday()]++
	}
	s.mean = float64(s.total) / float64(s.days)
	slices.Sort(counts)
	if n := len(counts); n%2 == 1 {
		s.median = float64(counts[n/2])
	} else {
		s.median = float64(counts[n/2-1]+counts[n/2]) / 2
	}
	for wd := range 7 {
		if wdN[wd] > 0 {
			if m := float64(wdSum[wd]) / float64(wdN[wd]); m > s.weekdayMean {
				s.weekdayMean, s.weekday = m, time.Weekday(wd)
			}
		}
	}
	if n := len(full); n >= 2*avgWindow {
		for _, p := range full[n-avgWindow:] {
			s.recent += float64(p.messages)
		}
		for _, p := range full[n-2*avgWindow : n-avgWindow] {
			s.prior += float64(p.messages)
		}
		s.recent /= avgWindow
		s.prior /= avgWindow
		s.trend = true
	}
	return s, true
}

// trendText is the week-on-week change, or why there is none.
func (s dailySummary) trendText() string {
	switch {
	case !s.trend:
		return "needs 2 weeks"
	case s.prior == 0 && s.recent == 0:
		return "flat"
	case s.prior == 0:
		return "up from nothing"
	}
	return fmt.Sprintf("%+.0f%%", (s.recent/s.prior-1)*100)
}

func (p *Plugin) handleDaily(ctx context.Context, ix *core.Ix) {
	opts, err := parseOptions(ix.Args(), p.now())
	if err != nil {
		ix.RespondErr("That window doesn't work", err)
		return
	}
	if err := ix.Defer(); err != nil {
		return
	}
	stats, err := p.store.Days(ctx, ix.GuildID, opts.channelID, opts.from, opts.to)
	if err != nil {
		_ = ix.FollowUpErr("Could not read the statistics", err)
		return
	}
	pts := dailyPoints(stats, opts.from, opts.to, p.coveredFrom(ctx, ix.GuildID))
	sum, ok := summarize(pts)
	if !ok {
		_ = ix.FollowUpErr("Nothing to plot", errors.New("no whole day in that window was counted; `/statistics status` says since when, and `/statistics backfill` can fill in before that"))
		return
	}
	guild := p.guildName(ix.GuildID)
	scope := guild
	if opts.channelID != "" {
		scope = "#" + channelLabel(p.channelNames(ctx, ix.GuildID), opts.channelID)
	}

	embed := core.Embed(core.ColorInfo, "Daily activity", core.TruncateEmbedDescription(dailyMarkdown(sum, pts, scope)))
	files := []*discord.File{fileFrom(dailyCSVName, dailyCSV(pts))}
	if png, err := renderDaily(pts, sum, scope, opts.from, opts.to); err == nil {
		embed.Image = &discord.EmbedResource{URL: "attachment://" + dailyImageName}
		files = append([]*discord.File{fileFrom(dailyImageName, png)}, files...)
	} else {
		p.log.Error("statistics: render daily", "guild", ix.GuildID, "err", err)
	}
	if err := ix.FollowUpEmbedWithFiles(embed, files...); err != nil {
		_ = ix.FollowUpErr("Could not post the export", err)
	}
}

func (p *Plugin) channelNames(ctx context.Context, guildID string) map[string]string {
	names, _ := p.store.Channels(ctx, guildID)
	return names
}

func dailyMarkdown(s dailySummary, pts []dayPoint, scope string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "**%s**, `%s` to `%s` utc\n\n", escape(scope), pts[0].day.Format("2006-01-02"), pts[len(pts)-1].day.Format("2006-01-02"))
	fmt.Fprintf(&b, "`%s` messages over `%d` whole days, `%s` a day on average (median `%s`)\n",
		thousands(s.total), s.days, thousands(int(math.Round(s.mean))), thousands(int(math.Round(s.median))))
	fmt.Fprintf(&b, "busiest `%s` with `%s`, quietest `%s` with `%s`\n",
		s.peak.day.Format("Mon 2006-01-02"), thousands(s.peak.messages), s.quiet.day.Format("Mon 2006-01-02"), thousands(s.quiet.messages))
	fmt.Fprintf(&b, "busiest weekday `%s` (`%s` on average), something said on `%d` of `%d` days\n",
		s.weekday, thousands(int(math.Round(s.weekdayMean))), s.activeDays, s.days)
	if s.trend {
		fmt.Fprintf(&b, "last 7 days `%s`/day against `%s`/day the week before: `%s`\n",
			thousands(int(math.Round(s.recent))), thousands(int(math.Round(s.prior))), s.trendText())
	}
	fmt.Fprintf(&b, "voice: `%s` in total\n", hours(s.voice))
	if n := len(pts) - s.days; n > 0 {
		fmt.Fprintf(&b, "\n-# %s the window touches %s left out of the figures: cut short, or from before counting began.\n",
			plural(n, "day"), map[bool]string{true: "is", false: "are"}[n == 1])
	}
	b.WriteString("-# every day, with voice minutes, is in `" + dailyCSVName + "`.")
	return b.String()
}

// dailyCSV is the export: one row per UTC day the window touches, including
// the ones the figures leave out, labelled, so a spreadsheet can make its
// own choice about them.
func dailyCSV(pts []dayPoint) []byte {
	var buf bytes.Buffer
	w := csv.NewWriter(&buf)
	_ = w.Write([]string{"date_utc", "weekday", "messages", "voice_minutes", "messages_7d_avg", "status"})
	for _, p := range pts {
		avg := ""
		if !math.IsNaN(p.avg) {
			avg = strconv.FormatFloat(p.avg, 'f', 1, 64)
		}
		msgs, mins := strconv.Itoa(p.messages), strconv.FormatFloat(p.voice.Minutes(), 'f', 0, 64)
		if p.state == dayNotCounted {
			msgs, mins = "", ""
		}
		_ = w.Write([]string{p.day.Format("2006-01-02"), p.day.Weekday().String()[:3], msgs, mins, avg, p.state.String()})
	}
	w.Flush()
	return buf.Bytes()
}

// thousands groups digits, since "12843" is read slower than "12,843".
func thousands(n int) string {
	s := strconv.Itoa(max(n, -n))
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	if n < 0 {
		return "-" + s
	}
	return s
}

// The infographic, in logical units like render.go's report.
const (
	dailyW      = 960
	tileTop     = 104
	tileH       = 74
	chatTop     = 208 // the chat chart's legend row
	chatH       = 280
	voiceGap    = 52 // between the chat chart's date labels and the voice heading
	voiceH      = 96
	axisLabelW  = 52
	dateLabelH  = 20
	legendRowH  = 22
	minLabelGap = 64 // between two date labels
)

var (
	chatBar    = rgb(0x26A641) // heatColors[3]: the report's own green
	chatPeak   = rgb(0x39D353)
	chatDim    = rgb(0x0E4429) // a partial day
	voiceBar   = rgb(0x7C80C4) // a quieter step of chanColor: the control, not the subject
	voiceDim   = rgb(0x34365A)
	avgLine    = nameColor
	gridColor  = ruleColor
	tileColor  = cardColor
	notCounted = heatOutside
)

func renderDaily(pts []dayPoint, s dailySummary, scope string, start, end time.Time) ([]byte, error) {
	f, err := newFaces()
	if err != nil {
		return nil, err
	}
	big, err := face(gobold.TTF, 20)
	if err != nil {
		return nil, err
	}
	inner := dailyW - pad*2
	voiceTop := chatTop + legendRowH + chatH + dateLabelH + voiceGap
	h := voiceTop + legendRowH + voiceH + dateLabelH + pad + 18

	img := image.NewRGBA(image.Rect(0, 0, px(dailyW), px(h)))
	draw.Draw(img, img.Bounds(), &image.Uniform{bgColor}, image.Point{}, draw.Src)

	text(img, f.title, nameColor, pad, titleBase, truncate(f.title, "how active "+scope+" is, day by day", inner))
	text(img, f.body, mutedColor, pad, windowBase, truncate(f.body, fmt.Sprintf("%s to %s utc, over %s",
		start.Format("2006-01-02 15:04"), end.Format("2006-01-02 15:04"), humanSpan(end.Sub(start))), inner))
	text(img, f.body, countColor, pad, totalsBase, truncate(f.body, fmt.Sprintf("%s messages and %s in voice over %s",
		thousands(s.total), hours(s.voice), plural(s.days, "whole day")), inner))
	fill(img, pad, ruleY, inner, 1, ruleColor)

	tiles := [][3]string{
		{"per day, average", thousands(int(math.Round(s.mean))), "median " + thousands(int(math.Round(s.median)))},
		{"busiest day", thousands(s.peak.messages), s.peak.day.Format("Mon Jan 2")},
		{"quietest day", thousands(s.quiet.messages), s.quiet.day.Format("Mon Jan 2")},
		{"busiest weekday", s.weekday.String()[:3], thousands(int(math.Round(s.weekdayMean))) + " on average"},
		{"last 7 days", s.trendText(), trendSub(s)},
		{"days with chat", fmt.Sprintf("%d/%d", s.activeDays, s.days), fmt.Sprintf("%.0f%% of days", 100*float64(s.activeDays)/float64(s.days))},
	}
	tw := (inner - (len(tiles)-1)*gutter) / len(tiles)
	for i, t := range tiles {
		x := pad + i*(tw+gutter)
		fill(img, x, tileTop, tw, tileH, tileColor)
		text(img, f.rank, mutedColor, x+12, tileTop+20, truncate(f.rank, t[0], tw-24))
		text(img, big, nameColor, x+12, tileTop+46, truncate(big, t[1], tw-24))
		text(img, f.body, countColor, x+12, tileTop+64, truncate(f.body, t[2], tw-24))
	}

	msgs := make([]float64, len(pts))
	voice := make([]float64, len(pts))
	for i, p := range pts {
		msgs[i], voice[i] = float64(p.messages), p.voice.Hours()
	}
	chat := barChart{f: f, pts: pts, values: msgs, top: chatTop, h: chatH, bar: chatBar, dim: chatDim,
		label: func(v float64) string { return thousands(int(math.Round(v))) }}
	chat.legend(img, "messages per day", true)
	s.peakLabel(img, f, chat, chat.draw(img, true))

	vc := barChart{f: f, pts: pts, values: voice, top: voiceTop, h: voiceH, bar: voiceBar, dim: voiceDim,
		label: func(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) + "h" }}
	vc.legend(img, "voice, hours per day (for comparison)", false)
	vc.draw(img, false)

	text(img, f.rank, mutedColor, pad, h-pad+4, truncate(f.rank,
		"utc days. dim bars are cut short by the window or by when counting began, and are left out of the figures; shaded columns were not counted at all.", inner))
	return encode(img)
}

func trendSub(s dailySummary) string {
	if !s.trend {
		return "against the week before"
	}
	return fmt.Sprintf("%s/day, was %s", thousands(int(math.Round(s.recent))), thousands(int(math.Round(s.prior))))
}

// barChart is one panel: a y-axis of round numbers, a bar per day, the
// date axis under it.
type barChart struct {
	f      faces
	pts    []dayPoint
	values []float64
	top, h int // the legend row starts at top; the plot sits under it
	bar    color.RGBA
	dim    color.RGBA
	label  func(float64) string
}

func (c barChart) plot() (x, y, w, h int) {
	return pad + axisLabelW, c.top + legendRowH, dailyW - pad*2 - axisLabelW, c.h
}

// slot is one day's width, in device pixels, as a float: a long window has
// days narrower than a logical unit.
func (c barChart) slot() float64 {
	_, _, w, _ := c.plot()
	return float64(px(w)) / float64(len(c.pts))
}

func (c barChart) legend(dst *image.RGBA, name string, withAvg bool) {
	x := pad + axisLabelW
	y := c.top + 12
	fill(dst, x, y-9, 10, 10, c.bar)
	x += 16
	text(dst, c.f.body, countColor, x, y, name)
	if withAvg {
		x += measure(c.f.body, name) + 24
		fill(dst, x, y-5, 18, 2, avgLine)
		text(dst, c.f.body, countColor, x+24, y, fmt.Sprintf("%d day average", avgWindow))
	}
}

// draw paints the panel and returns the y-axis ceiling it used.
func (c barChart) draw(dst *image.RGBA, withAvg bool) float64 {
	x0, y0, w, h := c.plot()
	top := 0.0
	for i, v := range c.values {
		if c.pts[i].state != dayNotCounted {
			top = max(top, v)
		}
	}
	step, ceil := niceScale(top, max(2, h/56))
	// The grid first, so bars sit on it rather than under it.
	for v := 0.0; v <= ceil+step/2; v += step {
		y := y0 + h - int(math.Round(v/ceil*float64(h)))
		fill(dst, x0, y, w, 1, gridColor)
		s := c.label(v)
		text(dst, c.f.rank, mutedColor, x0-8-measure(c.f.rank, s), y+4, s)
	}

	slot := c.slot()
	gap := 0.0
	if slot >= 6 {
		gap = max(2, slot*0.2)
	}
	base := px(y0 + h)
	band := 0 // leading days not counted; they only ever lead, since coverage is a start
	for i, p := range c.pts {
		l := int(math.Round(float64(px(x0)) + float64(i)*slot))
		r := max(l+1, int(math.Round(float64(px(x0))+float64(i+1)*slot-gap)))
		if p.state == dayNotCounted {
			// One continuous band, no gaps, so it cannot read as faint bars.
			full := int(math.Round(float64(px(x0)) + float64(i+1)*slot))
			draw.Draw(dst, image.Rect(l, px(y0), full, base), &image.Uniform{notCounted}, image.Point{}, draw.Src)
			band = max(band, i+1)
			continue
		}
		bh := int(math.Round(c.values[i] / ceil * float64(px(h))))
		if bh == 0 && c.values[i] > 0 {
			bh = 1 // a day with anything in it is never drawn as nothing
		}
		col := c.bar
		if p.state == dayPartial {
			col = c.dim
		}
		draw.Draw(dst, image.Rect(l, base-bh, r, base), &image.Uniform{col}, image.Point{}, draw.Src)
	}
	if lbl := "not counted"; band > 0 && float64(band)*slot/scale >= float64(measure(c.f.rank, lbl)+16) {
		text(dst, c.f.rank, mutedColor, x0+int(float64(band)*slot/scale)/2-measure(c.f.rank, lbl)/2, y0+h/2, lbl)
	}
	if withAvg {
		c.drawAvg(dst, ceil)
	}
	c.dates(dst)
	return ceil
}

// drawAvg strokes the trailing average as an antialiased 2 unit line, broken
// wherever it is undefined rather than bridged across a gap.
func (c barChart) drawAvg(dst *image.RGBA, ceil float64) {
	x0, y0, _, h := c.plot()
	slot := c.slot()
	at := func(i int) (float32, float32) {
		return float32(float64(px(x0)) + (float64(i)+0.5)*slot),
			float32(float64(px(y0+h)) - c.pts[i].avg/ceil*float64(px(h)))
	}
	r := vector.NewRasterizer(dst.Bounds().Dx(), dst.Bounds().Dy())
	half := float32(scale) // a 2 unit stroke
	for i := 1; i < len(c.pts); i++ {
		if math.IsNaN(c.pts[i-1].avg) || math.IsNaN(c.pts[i].avg) {
			continue
		}
		ax, ay := at(i - 1)
		bx, by := at(i)
		dx, dy := bx-ax, by-ay
		n := float32(math.Hypot(float64(dx), float64(dy)))
		nx, ny := -dy/n*half, dx/n*half
		// Each segment overshoots by half a stroke so the joins have no notch.
		ex, ey := dx/n*half/2, dy/n*half/2
		r.MoveTo(ax+nx-ex, ay+ny-ey)
		r.LineTo(bx+nx+ex, by+ny+ey)
		r.LineTo(bx-nx+ex, by-ny+ey)
		r.LineTo(ax-nx-ex, ay-ny-ey)
		r.ClosePath()
	}
	r.Draw(dst, dst.Bounds(), &image.Uniform{avgLine}, image.Point{})
}

// dates labels the x axis at a stride that keeps labels apart: every day
// when there is room, then Mondays, then the first of the month.
func (c barChart) dates(dst *image.RGBA) {
	x0, y0, _, h := c.plot()
	slotUnits := c.slot() / scale
	need := int(math.Ceil(minLabelGap / slotUnits))
	label := func(i int) bool { return i%max(1, need) == 0 }
	switch {
	case need > 31:
		months := (need + 29) / 30
		label = func(i int) bool { d := c.pts[i].day; return d.Day() == 1 && int(d.Month())%months == 0 }
	case need > 7:
		weeks := (need + 6) / 7
		label = func(i int) bool { d := c.pts[i].day; return d.Weekday() == time.Monday && d.YearDay()/7%weeks == 0 }
	case need > 1:
		label = func(i int) bool { return c.pts[i].day.Weekday() == time.Monday || need < 7 && i%need == 0 }
	}
	last := -minLabelGap
	for i, p := range c.pts {
		if !label(i) {
			continue
		}
		x := x0 + int((float64(i)+0.5)*slotUnits)
		s := p.day.Format("Jan 2")
		if x-measure(c.f.rank, s)/2 < last+8 {
			continue
		}
		fill(dst, x, y0+h, 1, 4, mutedColor)
		text(dst, c.f.rank, mutedColor, x-measure(c.f.rank, s)/2, y0+h+16, s)
		last = x + measure(c.f.rank, s)/2
	}
}

// peakLabel names the busiest day above its bar, the one number on the
// chart worth reading without the axis.
func (s dailySummary) peakLabel(dst *image.RGBA, f faces, c barChart, ceil float64) {
	i := slices.IndexFunc(c.pts, func(p dayPoint) bool { return p.day.Equal(s.peak.day) })
	if i < 0 || s.peak.messages == 0 {
		return
	}
	x0, y0, w, h := c.plot()
	l := float64(px(x0)) + float64(i)*c.slot()
	r := l + c.slot()
	if c.slot() >= 6 {
		r -= max(2, c.slot()*0.2)
	}
	draw.Draw(dst, image.Rect(int(math.Round(l)), px(y0+h)-int(math.Round(float64(s.peak.messages)/ceil*float64(px(h)))), max(int(math.Round(l))+1, int(math.Round(r))), px(y0+h)),
		&image.Uniform{chatPeak}, image.Point{}, draw.Src)
	lbl := thousands(s.peak.messages) + " on " + s.peak.day.Format("Jan 2")
	lw := measure(f.rank, lbl)
	cx := int((l+r)/2) / scale
	x := min(max(x0, cx-lw/2), x0+w-lw)
	y := y0 + h - int(float64(s.peak.messages)/ceil*float64(h)) - 6
	text(dst, f.rank, nameColor, x, max(y, y0+10), lbl)
}

// niceScale picks a tick step of 1, 2, 2.5 or 5 times a power of ten giving
// at most ticks intervals, and the ceiling it rounds top up to.
func niceScale(top float64, ticks int) (step, ceil float64) {
	if top <= 0 {
		return 1, 1
	}
	raw := top / float64(ticks)
	mag := math.Pow(10, math.Floor(math.Log10(raw)))
	for _, m := range []float64{1, 2, 2.5, 5, 10} {
		if step = m * mag; step >= raw {
			break
		}
	}
	return step, math.Ceil(top/step-1e-9) * step
}

func fill(dst *image.RGBA, x, y, w, h int, c color.Color) {
	draw.Draw(dst, image.Rect(px(x), px(y), px(x+w), px(y+h)), &image.Uniform{c}, image.Point{}, draw.Src)
}
