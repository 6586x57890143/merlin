package statistics

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/image/draw"
	"golang.org/x/image/font"
	"golang.org/x/image/font/gofont/gobold"
	"golang.org/x/image/font/gofont/goregular"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"
)

// Layout, in logical units, drawn at scale. Discord shows an attachment at
// roughly 550px wide and everything past that is click-to-enlarge, which is
// the only size the small text is really read at, so the canvas is drawn at
// 2x and downscaled by the client rather than being sharp at neither size.
const (
	scale = 2

	cols     = 3
	cardW    = 296
	cardH    = 76
	gutter   = 12
	pad      = 24
	heatTop  = 104 // where the heatmap, or the grid without one, begins
	avatarPx = 48

	// The heatmap: GitHub's contribution graph, one cell per UTC day, weeks
	// as columns and weekdays as rows. cellPitch is GitHub's own 11px cell
	// in a 14px step; the pitch shrinks for a window too wide to fit and
	// the oldest weeks are dropped past cellPitchMin.
	cellPitch    = 14
	cellPitchMin = 4
	heatLabelW   = 26 // "Mon" in the rank face, plus a gap
	heatMonthH   = 14 // the month labels above the cells
	heatGap      = 16 // between the heatmap and the grid
	sectionH     = 36 // a section label and its rule, above the voice grid

	titleBase  = 34
	windowBase = 58
	totalsBase = 78
	ruleY      = 92
)

const (
	// fetchTTL bounds one avatar fetch. avatarWorkers is how many run at
	// once: two dozen sequential CDN round trips is the slowest part of the
	// whole command, and it happens inside an interaction budget.
	fetchTTL      = 8 * time.Second
	avatarWorkers = 8
)

// cdnBase is a var so a test can point the avatar fetch at a stub.
var cdnBase = "https://cdn.discordapp.com"

// Dark mode, off merlin's brand palette rather than a second set of colours
// that drifts from the embeds this report is posted next to. Every ink here
// was measured against cardColor rather than eyeballed: core.ColorInfo itself
// lands at 4.39:1 on this surface, which fails AA for text this size, so the
// channel line wears a lighter step of it.
var (
	bgColor    = rgb(0x17110D)
	cardColor  = rgb(0x241B14)
	ruleColor  = rgb(0x3A2C21)
	nameColor  = rgb(0xECD9AE) // 12.16:1
	countColor = rgb(0xC9B896) // 8.68:1
	chanColor  = rgb(0x9FA2D4) // 6.92:1
	mutedColor = rgb(0x9A8B7A) // 5.11:1

	// heatColors are GitHub's dark-theme greens, empty first. A reader
	// already knows what this graph means without a legend, which is the
	// reason to borrow the palette rather than derive one from the brand.
	heatColors = [5]color.RGBA{cardColor, rgb(0x0E4429), rgb(0x006D32), rgb(0x26A641), rgb(0x39D353)}
)

func rgb(v int) color.RGBA {
	return color.RGBA{R: uint8(v >> 16), G: uint8(v >> 8), B: uint8(v), A: 0xFF}
}

func px(n int) int { return n * scale }

// faces are the six text styles the card grid uses, parsed once per render.
type faces struct {
	title, name, rank, body font.Face
}

func newFaces() (faces, error) {
	var f faces
	var err error
	if f.title, err = face(gobold.TTF, 24); err != nil {
		return f, err
	}
	if f.name, err = face(gobold.TTF, 15); err != nil {
		return f, err
	}
	if f.rank, err = face(gobold.TTF, 11); err != nil {
		return f, err
	}
	if f.body, err = face(goregular.TTF, 12.5); err != nil {
		return f, err
	}
	return f, nil
}

// renderPNG draws the report and returns the encoded png, ready to attach.
// Nothing touches the filesystem: the image exists for as long as it takes to
// upload it, which is the same promise the rest of this plugin makes.
func renderPNG(client *http.Client, rep report, guild string, start, end time.Time, top int) ([]byte, error) {
	chat, voice := capped(chatters(rep), top), capped(voicers(rep), top)

	f, err := newFaces()
	if err != nil {
		return nil, err
	}

	used := min(cols, max(1, len(chat), len(voice)))
	w := pad*2 + used*cardW + (used-1)*gutter
	inner := w - pad*2
	hm := newHeatmap(rep.days, rep.hourly, start, end, inner)
	gridTop := heatTop + hm.height()
	if hm.height() > 0 {
		gridTop += heatGap
	}
	// The empty state is one line of text where the grid would be, rather
	// than an empty grid, which reads as a rendering fault.
	h := gridTop + 40
	if len(chat) > 0 {
		h = gridTop + gridHeight(len(chat))
	}
	voiceTop := h + sectionH
	if len(voice) > 0 {
		h = voiceTop + gridHeight(len(voice))
	}
	h += pad

	img := image.NewRGBA(image.Rect(0, 0, px(w), px(h)))
	draw.Draw(img, img.Bounds(), &image.Uniform{bgColor}, image.Point{}, draw.Src)

	g := newGlyphs(client)
	drawRich(img, f.title, nameColor, pad, titleBase, fitRich(f.title, segments(f.title, "who was active in "+guild), inner), g)
	text(img, f.body, mutedColor, pad, windowBase, truncate(f.body, fmt.Sprintf("%s to %s utc, over %s",
		start.Format("2006-01-02 15:04"), end.Format("2006-01-02 15:04"), humanSpan(end.Sub(start))), inner))
	text(img, f.body, countColor, pad, totalsBase, truncate(f.body, totalsLine(rep), inner))
	draw.Draw(img, image.Rect(px(pad), px(ruleY), px(w-pad), px(ruleY)+scale), &image.Uniform{ruleColor}, image.Point{}, draw.Src)
	hm.draw(img, f, pad, heatTop)

	if len(chat) == 0 {
		text(img, f.body, mutedColor, pad, gridTop+24, "nobody chatted in that window.")
	}
	grid(img, f, g, client, chat, gridTop, false)
	if len(voice) > 0 {
		text(img, f.name, nameColor, pad, voiceTop-10, "in voice")
		draw.Draw(img, image.Rect(px(pad), px(voiceTop-4), px(w-pad), px(voiceTop-4)+scale), &image.Uniform{ruleColor}, image.Point{}, draw.Src)
		grid(img, f, g, client, voice, voiceTop+6, true)
	}
	return encode(img)
}

// gridHeight is the height of n cards in cols columns.
func gridHeight(n int) int {
	rows := (n + cols - 1) / cols
	return rows*cardH + (rows-1)*gutter
}

// grid draws one ranked listing of cards from top down.
func grid(img *image.RGBA, f faces, g *glyphs, client *http.Client, people []*person, top int, voiceFirst bool) {
	avatars := fetchAvatars(client, people)
	for i, p := range people {
		x := pad + (i%cols)*(cardW+gutter)
		y := top + (i/cols)*(cardH+gutter)
		card(img, f, g, p, avatars[i], i+1, x, y, voiceFirst)
	}
}

func totalsLine(rep report) string {
	line := fmt.Sprintf("%d people, %d messages, ", len(rep.people), rep.messages)
	if rep.voice > 0 {
		line += hours(rep.voice) + " in voice, "
	}
	line += fmt.Sprintf("%d channels", rep.channels)
	if rep.partial() {
		line += ", counted from " + rep.coveredFrom.Format("2006-01-02")
	}
	return line
}

func encode(img image.Image) ([]byte, error) {
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func card(dst *image.RGBA, f faces, g *glyphs, p *person, pic image.Image, rank, x, y int, voiceFirst bool) {
	draw.Draw(dst, image.Rect(px(x), px(y), px(x+cardW), px(y+cardH)), &image.Uniform{cardColor}, image.Point{}, draw.Src)

	drawAvatar(dst, pic, image.Rect(px(x+10), px(y+14), px(x+10+avatarPx), px(y+14+avatarPx)))

	// The rank sits top right so the image and the markdown list agree on who
	// is who; the name is measured against what it leaves behind.
	rankStr := fmt.Sprintf("%d.", rank)
	rankW := measure(f.rank, rankStr)
	text(dst, f.rank, mutedColor, x+cardW-12-rankW, y+26, rankStr)

	tx := x + 10 + avatarPx + 12
	nameW := x + cardW - 16 - rankW - tx
	bodyW := x + cardW - 12 - tx
	drawRich(dst, f.name, nameColor, tx, y+30, fitRich(f.name, displayName(f.name, p), nameW), g)
	drawRich(dst, f.body, countColor, tx, y+48, fitRich(f.body, segments(f.body, cardStats(p, voiceFirst)), bodyW), g)
	drawRich(dst, f.body, chanColor, tx, y+64, fitRich(f.body, segments(f.body, channelList(p.channels)), bodyW), g)
}

// cardStats is the two counts as the card shows them, icons and all: the
// emoji go through the same Twemoji path a name's do.
func cardStats(p *person, voiceFirst bool) string {
	var parts []string
	if p.count > 0 {
		parts = append(parts, iconMessages+" "+plural(p.count, "message"))
	}
	if p.voice > 0 {
		parts = append(parts, iconVoice+" "+hours(p.voice))
	}
	if voiceFirst && len(parts) == 2 {
		parts[0], parts[1] = parts[1], parts[0]
	}
	return strings.Join(parts, "   ")
}

// heatmap is the activity grid laid out for one canvas width, in one of
// two shapes. Daily is GitHub's: weekday rows, one column per week, one
// cell per UTC day. Hourly, for a window of hourlyHeatMax or less, is one
// row per day and twenty four hour columns, one cell per hour, since a
// handful of day cells says nothing a totals line does not and the hours
// show when the server is awake.
type heatmap struct {
	cells  map[time.Time]DayStat // by cell start
	hourly bool
	unit   time.Duration // a cell's span
	first  time.Time     // daily: the Monday of column 0; hourly: the day of row 0
	start  time.Time     // window bounds, at cell resolution, inclusive
	end    time.Time
	cols   int
	rows   int
	pitch  int
	labelW int
	maxMsg int
	maxSec int
}

// newHeatmap plans the grid. Nothing is drawn for a window under two hours,
// which is one cell either way.
func newHeatmap(days []DayStat, hourly bool, start, end time.Time, inner int) heatmap {
	h := heatmap{cells: map[time.Time]DayStat{}, hourly: hourly, unit: 24 * time.Hour, labelW: heatLabelW}
	if hourly {
		h.unit, h.labelW = time.Hour, heatLabelW+20
	}
	h.start = start.UTC().Truncate(h.unit)
	h.end = end.UTC().Add(-time.Nanosecond).Truncate(h.unit)
	if !h.end.After(h.start) {
		return h
	}
	for _, d := range days {
		h.cells[d.Day.UTC().Truncate(h.unit)] = d
		h.maxMsg = max(h.maxMsg, d.Messages)
		h.maxSec = max(h.maxSec, d.VoiceSeconds)
	}
	if hourly {
		h.first = h.start.Truncate(24 * time.Hour)
		h.cols = 24
		h.rows = int(h.end.Sub(h.first).Hours()/24) + 1
		h.pitch = min(cellPitch, max(cellPitchMin, (inner-h.labelW)/24))
		return h
	}
	h.first = mondayOf(h.start)
	h.rows = 7
	h.cols = int(h.end.Sub(h.first).Hours()/(24*7)) + 1
	h.pitch = min(cellPitch, max(cellPitchMin, (inner-h.labelW)/max(1, h.cols)))
	// ponytail: a window wider than the canvas at the minimum pitch shows
	// only its most recent weeks; scale the cells if that ever matters.
	if fit := (inner - h.labelW) / h.pitch; h.cols > fit {
		h.first = h.first.AddDate(0, 0, 7*(h.cols-fit))
		h.cols = fit
	}
	return h
}

func mondayOf(t time.Time) time.Time {
	back := (int(t.Weekday()) + 6) % 7
	return t.AddDate(0, 0, -back)
}

// cell is the instant a cell stands for.
func (h heatmap) cell(col, row int) time.Time {
	if h.hourly {
		return h.first.AddDate(0, 0, row).Add(time.Duration(col) * time.Hour)
	}
	return h.first.AddDate(0, 0, 7*col+row)
}

func (h heatmap) height() int {
	if h.cols == 0 {
		return 0
	}
	return heatMonthH + h.rows*h.pitch
}

// level is the cell's shade for one day: nothing, or a quartile of the
// day's activity against the window's busiest. Messages and voice are each
// normalised to their own peak and averaged, so a voice-heavy server and a
// text-heavy one both light up; a metric the window has none of is left
// out rather than halving every score.
func (h heatmap) level(day time.Time) int {
	d, ok := h.cells[day]
	if !ok {
		return 0
	}
	var score float64
	var n int
	if h.maxMsg > 0 {
		score += float64(d.Messages) / float64(h.maxMsg)
		n++
	}
	if h.maxSec > 0 {
		score += float64(d.VoiceSeconds) / float64(h.maxSec)
		n++
	}
	if n == 0 || score <= 0 {
		return 0
	}
	return 1 + min(3, int(score/float64(n)*4))
}

func (h heatmap) draw(dst *image.RGBA, f faces, x, y int) {
	if h.cols == 0 {
		return
	}
	cell := max(2, h.pitch*11/cellPitch)
	cellsX, cellsY := x+h.labelW, y+heatMonthH
	if h.pitch >= 8 {
		h.labels(dst, f, x, y, cellsX, cellsY, cell)
	}
	for col := range h.cols {
		for row := range h.rows {
			at := h.cell(col, row)
			if at.Before(h.start) || at.After(h.end) {
				continue
			}
			cx, cy := cellsX+col*h.pitch, cellsY+row*h.pitch
			draw.Draw(dst, image.Rect(px(cx), px(cy), px(cx+cell), px(cy+cell)),
				&image.Uniform{heatColors[h.level(at)]}, image.Point{}, draw.Src)
		}
	}
}

// labels draws the axis words: weekdays and months for the daily grid,
// dates and hours for the hourly one. Skipped entirely once the cells are
// too small to leave room for them.
func (h heatmap) labels(dst *image.RGBA, f faces, x, y, cellsX, cellsY, cell int) {
	rowLabel := func(row int, s string) { text(dst, f.rank, mutedColor, x, cellsY+row*h.pitch+cell-1, s) }
	colLabel := func(col int, s string) { text(dst, f.rank, mutedColor, cellsX+col*h.pitch, y+heatMonthH-4, s) }
	if h.hourly {
		// Every row is a date, every other one when there are many or the
		// rows are too tight for the face.
		step := 1
		if h.rows > 7 || h.pitch < 12 {
			step = 2
		}
		for row := 0; row < h.rows; row += step {
			rowLabel(row, h.cell(0, row).Format("Jan 2"))
		}
		for _, hr := range []int{0, 6, 12, 18} {
			colLabel(hr, fmt.Sprintf("%dh", hr))
		}
		return
	}
	for row, label := range []string{"Mon", "", "Wed", "", "Fri", "", ""} {
		if label != "" {
			rowLabel(row, label)
		}
	}
	// A month label on the first column that reaches into each month. Two
	// closer than a word's width keep the later one, so a window opening
	// on the last days of a month is labelled with the month it is mostly
	// in.
	var labels []int
	for col := range h.cols {
		m := h.first.AddDate(0, 0, 7*col+6).Month()
		if col == 0 || m != h.first.AddDate(0, 0, 7*col-1).Month() {
			if n := len(labels); n > 0 && col-labels[n-1] < 3 {
				labels = labels[:n-1]
			}
			labels = append(labels, col)
		}
	}
	for _, col := range labels {
		colLabel(col, h.first.AddDate(0, 0, 7*col+6).Month().String()[:3])
	}
}

// displayName is the name as it can actually be drawn: the face's text and
// the CDN's emoji, with what is neither (the Go fonts carry no CJK) dropped.
// A rune the face cannot advance renders as nothing at all, so without this
// a member called "隼" would get a card with a blank line where their name
// should be and nothing anywhere saying why. A name with nothing drawable
// left falls back to the id, which is ugly and unambiguous. The markdown
// keeps the real name: Discord renders it fine.
func displayName(f font.Face, p *person) []seg {
	segs := segments(f, p.name)
	if plain(segs) == "" && !hasEmoji(segs) {
		return segments(f, p.id)
	}
	return segs
}

func hasEmoji(segs []seg) bool {
	for _, s := range segs {
		if s.emoji != "" {
			return true
		}
	}
	return false
}

// fetchAvatars pulls every shown member's picture at once, in index order, so
// a slow CDN costs one round trip rather than one per person.
func fetchAvatars(client *http.Client, people []*person) []image.Image {
	out := make([]image.Image, len(people))
	sem := make(chan struct{}, avatarWorkers)
	var wg sync.WaitGroup
	for i, p := range people {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			// A failed fetch leaves a nil, which drawAvatar renders as the
			// plain circle. One dead CDN link must not cost the whole card.
			img, err := fetchAvatar(client, p)
			if err == nil {
				out[i] = img
			}
		}()
	}
	wg.Wait()
	return out
}

// drawAvatar masks the picture to a circle, the way Discord draws it. A nil
// picture leaves the filled circle underneath rather than a hole.
func drawAvatar(dst *image.RGBA, src image.Image, r image.Rectangle) {
	mask := &circle{p: image.Pt(r.Min.X+r.Dx()/2, r.Min.Y+r.Dy()/2), r: r.Dx() / 2}
	draw.DrawMask(dst, r, &image.Uniform{ruleColor}, image.Point{}, mask, r.Min, draw.Over)
	if src == nil {
		return
	}
	scaled := image.NewRGBA(r)
	draw.CatmullRom.Scale(scaled, r, src, src.Bounds(), draw.Src, nil)
	draw.DrawMask(dst, r, scaled, r.Min, mask, r.Min, draw.Over)
}

func fetchAvatar(client *http.Client, p *person) (image.Image, error) {
	// The .png form renders an animated avatar as its first frame, so there
	// is no need to branch on the a_ prefix. A member with no avatar set
	// falls back to the same default sprite Discord shows in the client.
	url := fmt.Sprintf("%s/embed/avatars/%d.png", cdnBase, defaultAvatarIndex(p.id))
	if p.avatar != "" {
		url = fmt.Sprintf("%s/avatars/%s/%s.png?size=128", cdnBase, p.id, p.avatar)
	}
	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("avatar: %s", resp.Status)
	}
	img, _, err := image.Decode(resp.Body)
	return img, err
}

// defaultAvatarIndex mirrors Discord's own rule for accounts on the current
// username system: derived from the id, not from the legacy discriminator.
func defaultAvatarIndex(id string) int {
	n := int64(0)
	for _, c := range id {
		if c < '0' || c > '9' {
			return 0
		}
		n = n*10 + int64(c-'0')
	}
	return int((n >> 22) % 6)
}

// circle is an alpha mask, so an avatar lands as a circle the way Discord
// draws it rather than a square that reads as a different product.
type circle struct {
	p image.Point
	r int
}

func (c *circle) ColorModel() color.Model { return color.AlphaModel }
func (c *circle) Bounds() image.Rectangle {
	return image.Rect(c.p.X-c.r, c.p.Y-c.r, c.p.X+c.r, c.p.Y+c.r)
}
func (c *circle) At(x, y int) color.Color {
	dx, dy := x-c.p.X, y-c.p.Y
	if dx*dx+dy*dy <= c.r*c.r {
		return color.Alpha{A: 255}
	}
	return color.Alpha{}
}

func face(ttf []byte, size float64) (font.Face, error) {
	f, err := opentype.Parse(ttf)
	if err != nil {
		return nil, err
	}
	return opentype.NewFace(f, &opentype.FaceOptions{Size: size * scale, DPI: 72, Hinting: font.HintingFull})
}

// text draws at logical coordinates; the faces are already built at scale.
func text(dst *image.RGBA, f font.Face, col color.Color, x, y int, s string) {
	d := font.Drawer{Dst: dst, Src: &image.Uniform{col}, Face: f, Dot: fixed.P(px(x), px(y))}
	d.DrawString(s)
}

// measure returns a string's width in logical units.
func measure(f font.Face, s string) int {
	return font.MeasureString(f, s).Ceil() / scale
}

// truncate cuts a string to fit maxW logical units. Names and server names
// are chosen by other people and some are very long, so this measures rather
// than guessing a rune count.
func truncate(f font.Face, s string, maxW int) string {
	if measure(f, s) <= maxW {
		return s
	}
	runes := []rune(s)
	for len(runes) > 1 {
		runes = runes[:len(runes)-1]
		if measure(f, string(runes)+"...") <= maxW {
			return string(runes) + "..."
		}
	}
	return ""
}

// plural keeps a card from reading "1 messages", which is the sort of thing
// that makes a report look machine generated.
func plural(n int, word string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, word)
	}
	return fmt.Sprintf("%d %ss", n, word)
}
