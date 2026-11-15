package statistics

import (
	"bytes"
	"context"
	"encoding/csv"
	"fmt"
	"image"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/disgoorg/disgo/discord"
	"golang.org/x/image/draw"

	"github.com/6586x57890143/merlin/internal/core"
)

// /statistics members is the server's population: how many people were in
// it at the end of each day, and the arrivals and departures that moved it.
//
// Nothing stores the population itself. It is worked back from Discord's
// member count now: the count at the end of a day is today's, less every
// arrival since, plus every departure. That is exact as far as the gateway
// heard everything, and a join or leave merlin missed while offline shifts
// every day before it by one, which the footer owns up to.

const (
	membersImageName = "members.png"
	membersCSVName   = "members.csv"
	popTop           = 112 // the population chart's legend row
	popH             = 220
	flowGap          = 44 // between one chart's date labels and the next legend
	flowH            = 96
)

var (
	popLine    = nameColor
	leftBar    = rgb(0xC9605A) // departures: a muted red beside the report's green
	leftDim    = rgb(0x4F2724)
	joinedDim  = chatDim
	joinedBar  = chatBar
	popUnknown = notCounted
)

type memberPoint struct {
	dayPoint
	joined, departed int
	pop              float64 // members at the end of the day; NaN where it cannot be worked out
}

// memberPoints lays the window out a day at a time. window is what the bars
// and totals count; since runs from the window's start to now and is what
// the population is walked back through, so a window that ended last month
// still lands on the right numbers. members is Discord's count now, zero
// when unknown, which leaves every population NaN.
func memberPoints(window, since []MemberDay, from, to, covered time.Time, members int) []memberPoint {
	by := make(map[time.Time]MemberDay, len(window))
	for _, d := range window {
		by[d.Day.UTC().Truncate(dayLen)] = d
	}
	total := 0
	for _, d := range since {
		total += d.Joined - d.Departed
	}
	axis := dayAxis(from, to, covered)
	out := make([]memberPoint, len(axis))
	cum, j := 0, 0 // net change from the window's start through the day in hand
	for i, a := range axis {
		for j < len(since) && !since[j].Day.UTC().Truncate(dayLen).After(a.day) {
			cum += since[j].Joined - since[j].Departed
			j++
		}
		w := by[a.day]
		pt := memberPoint{dayPoint: a, joined: w.Joined, departed: w.Departed, pop: math.NaN()}
		if members > 0 && a.state != dayNotCounted {
			pt.pop = float64(members - (total - cum))
		}
		out[i] = pt
	}
	return out
}

// memberCoverage is when the member buckets begin to speak for a day: when
// live counting began (backfill reads message history, which has no joins
// in it), or the retention cutoff once that has passed it.
func (p *Plugin) memberCoverage(ctx context.Context, guildID string, now time.Time) time.Time {
	cfg, err := p.store.Config(ctx, guildID)
	if err != nil || cfg.LiveSince.IsZero() {
		return time.Time{}
	}
	if cut := now.AddDate(0, 0, -cfg.RetentionDays).Truncate(time.Hour); cut.After(cfg.LiveSince) {
		return cut
	}
	return cfg.LiveSince
}

func (p *Plugin) handleMembers(ctx context.Context, ix *core.Ix) {
	now := p.now()
	opts, err := parseOptions(ix.Args(), now)
	if err != nil {
		ix.RespondErr("That window doesn't work", err)
		return
	}
	if err := ix.Defer(); err != nil {
		return
	}
	window, err := p.store.MemberDays(ctx, ix.GuildID, opts.from, opts.to)
	if err != nil {
		_ = ix.FollowUpErr("Could not read the statistics", err)
		return
	}
	end := opts.to
	if now.After(end) {
		end = now
	}
	since, err := p.store.MemberDays(ctx, ix.GuildID, opts.from, end)
	if err != nil {
		_ = ix.FollowUpErr("Could not read the statistics", err)
		return
	}
	guild, count := p.guildInfo(ix.GuildID)
	pts := memberPoints(window, since, opts.from, opts.to, p.memberCoverage(ctx, ix.GuildID, now), count)

	embed := core.Embed(core.ColorInfo, "Members", core.TruncateEmbedDescription(membersMarkdown(guild, opts.from, opts.to, pts, count)))
	var files []*discord.File
	if anyCounted(pts) {
		files = append(files, fileFrom(membersCSVName, membersCSV(pts)))
		if png, err := renderMembers(p.client, pts, guild, opts.from, opts.to, count); err == nil {
			embed.Image = &discord.EmbedResource{URL: "attachment://" + membersImageName}
			files = append([]*discord.File{fileFrom(membersImageName, png)}, files...)
		} else {
			p.log.Error("statistics: render members", "guild", ix.GuildID, "err", err)
		}
	}
	if err := ix.FollowUpEmbedWithFiles(embed, files...); err != nil {
		_ = ix.FollowUpErr("Could not post the export", err)
	}
}

func anyCounted(pts []memberPoint) bool {
	for _, p := range pts {
		if p.state != dayNotCounted {
			return true
		}
	}
	return false
}

// popRange is the first and last day with a population worked out.
func popRange(pts []memberPoint) (first, last memberPoint, ok bool) {
	for _, p := range pts {
		if math.IsNaN(p.pop) {
			continue
		}
		if !ok {
			first, ok = p, true
		}
		last = p
	}
	return first, last, ok
}

func membersMarkdown(guild string, from, to time.Time, pts []memberPoint, count int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "**%s**, `%s` to `%s` utc\n\n", escape(guild), from.Format("2006-01-02 15:04"), to.Format("2006-01-02 15:04"))
	joined, departed := 0, 0
	var mostIn, mostOut memberPoint
	for _, p := range pts {
		joined += p.joined
		departed += p.departed
		if p.joined > mostIn.joined {
			mostIn = p
		}
		if p.departed > mostOut.departed {
			mostOut = p
		}
	}
	if joined+departed == 0 {
		b.WriteString("no joins or departures were counted in that window.\n")
	} else {
		fmt.Fprintf(&b, "`%d` joined, `%d` left, net `%+d`\n", joined, departed, joined-departed)
	}
	if first, last, ok := popRange(pts); ok {
		fmt.Fprintf(&b, "`%s` members at the end of `%s`, `%s` at the end of `%s`; `%s` now\n",
			thousands(int(first.pop)), first.day.Format("2006-01-02"), thousands(int(last.pop)), last.day.Format("2006-01-02"), thousands(count))
	}
	if mostIn.joined > 0 {
		fmt.Fprintf(&b, "most arrivals `%s` with `%d`", mostIn.day.Format("Mon 2006-01-02"), mostIn.joined)
		if mostOut.departed > 0 {
			fmt.Fprintf(&b, ", most departures `%s` with `%d`", mostOut.day.Format("Mon 2006-01-02"), mostOut.departed)
		}
		b.WriteString("\n")
	}
	if joined+departed > 0 {
		b.WriteString("\n")
		// Newest first, since the question is usually "what happened this
		// week" and the tail of a long window is the part that fits.
		shown := 0
		for n := len(pts) - 1; n >= 0 && shown < core.PageSize; n-- {
			if d := pts[n]; d.joined+d.departed > 0 {
				fmt.Fprintf(&b, "`%s` `+%d` `-%d`\n", d.day.Format("2006-01-02"), d.joined, d.departed)
				shown++
			}
		}
	}
	if anyCounted(pts) {
		b.WriteString("\n-# every day is in `" + membersCSVName + "`.")
	}
	return b.String()
}

func membersCSV(pts []memberPoint) []byte {
	var buf bytes.Buffer
	w := csv.NewWriter(&buf)
	_ = w.Write([]string{"date_utc", "weekday", "joined", "departed", "net", "members_end_of_day", "status"})
	for _, p := range pts {
		in, out, net, pop := strconv.Itoa(p.joined), strconv.Itoa(p.departed), strconv.Itoa(p.joined-p.departed), ""
		if p.state == dayNotCounted {
			in, out, net = "", "", ""
		}
		if !math.IsNaN(p.pop) {
			pop = strconv.Itoa(int(p.pop))
		}
		_ = w.Write([]string{p.day.Format("2006-01-02"), p.day.Weekday().String()[:3], in, out, net, pop, p.state.String()})
	}
	w.Flush()
	return buf.Bytes()
}

// renderMembers draws the population line over the arrivals and departures
// that moved it, all three on one date axis.
func renderMembers(client *http.Client, pts []memberPoint, guild string, start, end time.Time, count int) ([]byte, error) {
	f, err := newFaces()
	if err != nil {
		return nil, err
	}
	inner := dailyW - pad*2
	joinTop := popTop + legendRowH + popH + dateLabelH + flowGap
	leftTop := joinTop + legendRowH + flowH + dateLabelH + flowGap
	h := leftTop + legendRowH + flowH + dateLabelH + pad + 18

	img := image.NewRGBA(image.Rect(0, 0, px(dailyW), px(h)))
	draw.Draw(img, img.Bounds(), &image.Uniform{bgColor}, image.Point{}, draw.Src)

	joined, departed := 0, 0
	days := make([]dayPoint, len(pts))
	pop, in, out := make([]float64, len(pts)), make([]float64, len(pts)), make([]float64, len(pts))
	for i, p := range pts {
		days[i], pop[i], in[i], out[i] = p.dayPoint, p.pop, float64(p.joined), float64(p.departed)
		joined += p.joined
		departed += p.departed
	}
	drawRich(img, f.title, nameColor, pad, titleBase,
		fitRich(f.title, segments(f.title, "who comes and goes in "+guild), inner), newGlyphs(client))
	text(img, f.body, mutedColor, pad, windowBase, truncate(f.body, fmt.Sprintf("%s to %s utc, over %s",
		start.Format("2006-01-02 15:04"), end.Format("2006-01-02 15:04"), humanSpan(end.Sub(start))), inner))
	totals := fmt.Sprintf("%s joined, %s left, net %+d", thousands(joined), thousands(departed), joined-departed)
	if count > 0 {
		totals += ", " + thousands(count) + " members now"
	}
	text(img, f.body, countColor, pad, totalsBase, truncate(f.body, totals, inner))
	fill(img, pad, ruleY, inner, 1, ruleColor)

	label := func(v float64) string { return thousands(int(math.Round(v))) }
	pc := barChart{f: f, pts: days, values: pop, top: popTop, h: popH, bar: popLine, label: label}
	pc.legend(img, "members at the end of each day", false)
	pc.drawLine(img)

	jc := barChart{f: f, pts: days, values: in, top: joinTop, h: flowH, bar: joinedBar, dim: joinedDim, label: label}
	jc.legend(img, "joined per day", false)
	jc.draw(img, false)

	lc := barChart{f: f, pts: days, values: out, top: leftTop, h: flowH, bar: leftBar, dim: leftDim, label: label}
	lc.legend(img, "left per day", false)
	lc.draw(img, false)

	text(img, f.rank, mutedColor, pad, h-pad+4, truncate(f.rank,
		"utc days. members are worked back from discord's count now, so a join or leave missed while merlin was offline shifts the days before it. dim bars are cut short.", inner))
	return encode(img)
}

// drawLine is draw for a level rather than a count: the axis hugs the
// values instead of starting at zero, where a server of two thousand gaining
// thirty would be a flat line along the top.
func (c barChart) drawLine(dst *image.RGBA) {
	x0, y0, w, h := c.plot()
	lo, hi := math.Inf(1), math.Inf(-1)
	for _, v := range c.values {
		if !math.IsNaN(v) {
			lo, hi = min(lo, v), max(hi, v)
		}
	}
	slot := c.slot()
	for i, v := range c.values {
		if math.IsNaN(v) {
			l := int(math.Round(float64(px(x0)) + float64(i)*slot))
			r := int(math.Round(float64(px(x0)) + float64(i+1)*slot))
			draw.Draw(dst, image.Rect(l, px(y0), r, px(y0+h)), &image.Uniform{popUnknown}, image.Point{}, draw.Src)
		}
	}
	if math.IsInf(lo, 1) {
		c.dates(dst)
		return
	}
	ticks := max(2, h/56)
	// A span of at least one member per tick, so a flat week is not labelled
	// in fractions of a person.
	step, _ := niceScale(max(hi-lo, float64(ticks)), ticks)
	floor := math.Floor(lo/step) * step
	ceil := floor + step
	for ceil < hi {
		ceil += step
	}
	for v := floor; v <= ceil+step/2; v += step {
		y := y0 + h - int(math.Round((v-floor)/(ceil-floor)*float64(h)))
		fill(dst, x0, y, w, 1, gridColor)
		s := c.label(v)
		text(dst, c.f.rank, mutedColor, x0-8-measure(c.f.rank, s), y+4, s)
	}
	c.stroke(dst, c.values, floor, ceil, c.bar)
	c.dates(dst)
}
