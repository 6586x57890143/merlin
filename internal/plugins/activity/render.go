package activity

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
	"unicode"

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
	gridTop  = 104
	avatarPx = 48

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
	people := rep.people
	if top > 0 && len(people) > top {
		people = people[:top]
	}

	f, err := newFaces()
	if err != nil {
		return nil, err
	}

	used := min(cols, max(1, len(people)))
	rows := (len(people) + cols - 1) / cols
	w := pad*2 + used*cardW + (used-1)*gutter
	// The empty state is one line of text where the grid would be, rather
	// than an empty grid, which reads as a rendering fault.
	h := gridTop + 40 + pad
	if rows > 0 {
		h = gridTop + rows*cardH + (rows-1)*gutter + pad
	}

	img := image.NewRGBA(image.Rect(0, 0, px(w), px(h)))
	draw.Draw(img, img.Bounds(), &image.Uniform{bgColor}, image.Point{}, draw.Src)

	inner := w - pad*2
	text(img, f.title, nameColor, pad, titleBase, truncate(f.title, "who was active in "+guild, inner))
	text(img, f.body, mutedColor, pad, windowBase, fmt.Sprintf("%s to %s utc, over %s",
		start.Format("2006-01-02 15:04"), end.Format("2006-01-02 15:04"), humanSpan(end.Sub(start))))
	text(img, f.body, countColor, pad, totalsBase, truncate(f.body, totalsLine(rep), inner))
	draw.Draw(img, image.Rect(px(pad), px(ruleY), px(w-pad), px(ruleY)+scale), &image.Uniform{ruleColor}, image.Point{}, draw.Src)

	if len(people) == 0 {
		text(img, f.body, mutedColor, pad, gridTop+24, "nobody chatted in that window.")
		return encode(img)
	}

	avatars := fetchAvatars(client, people)
	for i, p := range people {
		x := pad + (i%cols)*(cardW+gutter)
		y := gridTop + (i/cols)*(cardH+gutter)
		card(img, f, p, avatars[i], i+1, x, y)
	}
	return encode(img)
}

func totalsLine(rep report) string {
	line := fmt.Sprintf("%d people, %d messages, %d of %d channels",
		len(rep.people), rep.messages, rep.busy, rep.looked+rep.skipped)
	if rep.truncated() {
		// Named, not just flagged. The png is the half that gets saved and
		// passed around, so it has to carry why its numbers are a floor.
		if rep.stoppedBy == stopTime {
			line += ", stopped early: out of time"
		} else {
			line += ", stopped early: page ceiling"
		}
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

func card(dst *image.RGBA, f faces, p *person, pic image.Image, rank, x, y int) {
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
	text(dst, f.name, nameColor, tx, y+30, truncate(f.name, displayName(f.name, p), nameW))
	text(dst, f.body, countColor, tx, y+48, plural(p.count, "message"))
	text(dst, f.body, chanColor, tx, y+64, truncate(f.body, channelList(p.channels), bodyW))
}

// displayName is the name as it can actually be drawn. The Go fonts carry no
// CJK and no emoji, and a rune the face cannot advance renders as nothing at
// all, so a member called "🦅" would get a card with a blank line where their
// name should be and nothing anywhere saying why. Dropping what cannot be
// drawn at least leaves the readable part, and a name that is entirely
// undrawable falls back to the id, which is ugly and unambiguous. The
// markdown keeps the real name: Discord renders it fine.
func displayName(f font.Face, p *person) string {
	out := sanitizeName(f, p.name)
	if out == "" {
		return p.id
	}
	return out
}

func sanitizeName(f font.Face, name string) string {
	var b strings.Builder
	for _, r := range name {
		if unicode.IsSpace(r) {
			b.WriteRune(' ')
			continue
		}
		if _, ok := f.GlyphAdvance(r); ok {
			b.WriteRune(r)
		}
	}
	return strings.TrimSpace(strings.Join(strings.Fields(b.String()), " "))
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
