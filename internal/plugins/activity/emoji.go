package activity

import (
	"fmt"
	"image"
	"image/color"
	"net/http"
	"strings"
	"unicode"

	"golang.org/x/image/draw"
	"golang.org/x/image/font"
	"golang.org/x/image/math/fixed"
)

// emojiBase is where emoji art comes from. The Go fonts carry no emoji and
// x/image/font cannot rasterise a colour font anyway, so each one is fetched
// as Twemoji's 72px png, the way avatars already are, and drawn inline.
// Pinned, because a file name convention that moves under a deployed bot
// is a card with a hole in it. A var so a test can stub it.
var emojiBase = "https://cdn.jsdelivr.net/gh/jdecked/twemoji@16.0.1/assets/72x72"

// seg is one run of a string as it will be drawn: text the face can shape,
// or one emoji sequence, keyed by its Twemoji file name ("1f427",
// "1f468-200d-1f4bb", "23-20e3").
type seg struct {
	text  string
	emoji string
}

// segments splits s into drawable text and emoji, dropping what is neither
// (the Go fonts have no CJK). An emoji is a rune in the emoji blocks that the
// face cannot draw, or any rune followed by U+FE0F asking for the emoji
// presentation, plus whatever modifiers, joiners and tags hang off it.
func segments(f font.Face, s string) []seg {
	var out []seg
	var text strings.Builder
	flush := func() {
		if text.Len() > 0 {
			out = append(out, seg{text: text.String()})
			text.Reset()
		}
	}
	runes := []rune(s)
	for i := 0; i < len(runes); i++ {
		r := runes[i]
		if key, n := emojiAt(f, runes[i:]); n > 0 {
			flush()
			out = append(out, seg{emoji: key})
			i += n - 1
			continue
		}
		switch {
		case unicode.IsSpace(r):
			text.WriteRune(' ')
		case drawable(f, r):
			text.WriteRune(r)
		}
	}
	flush()
	return out
}

func drawable(f font.Face, r rune) bool {
	_, ok := f.GlyphAdvance(r)
	return ok
}

// emojiAt reports the Twemoji key of the emoji sequence at the head of
// runes and how many runes it spans, or n == 0 when there is none.
func emojiAt(f font.Face, runes []rune) (key string, n int) {
	r := runes[0]
	next := func(i int) rune {
		if i < len(runes) {
			return runes[i]
		}
		return 0
	}
	keycap := strings.ContainsRune("#*0123456789", r) && (next(1) == 0x20E3 || next(1) == 0xFE0F && next(2) == 0x20E3)
	pictographic := emojiBlock(r) && (!drawable(f, r) || next(1) == 0xFE0F)
	if !keycap && !pictographic {
		return "", 0
	}
	n = 1
	if regional(r) && regional(next(1)) {
		n = 2 // a flag
	}
	for n < len(runes) {
		switch c := runes[n]; {
		case c == 0xFE0F || c == 0x20E3 || c >= 0x1F3FB && c <= 0x1F3FF || c >= 0xE0020 && c <= 0xE007F:
			n++
		case c == 0x200D && n+1 < len(runes):
			n += 2
		default:
			return twemojiKey(runes[:n]), n
		}
	}
	return twemojiKey(runes[:n]), n
}

// twemojiKey is Twemoji's file name for a sequence: the code points in hex
// joined by dashes, with U+FE0F dropped unless a joiner is present, which is
// the convention their own lookup uses.
func twemojiKey(runes []rune) string {
	var zwj bool
	for _, r := range runes {
		zwj = zwj || r == 0x200D
	}
	parts := make([]string, 0, len(runes))
	for _, r := range runes {
		if r == 0xFE0F && !zwj {
			continue
		}
		parts = append(parts, fmt.Sprintf("%x", r))
	}
	return strings.Join(parts, "-")
}

func regional(r rune) bool { return r >= 0x1F1E6 && r <= 0x1F1FF }

func emojiBlock(r rune) bool {
	switch {
	case r >= 0x1F000, // pictographs, flags, transport, supplemental symbols and everything after
		r >= 0x2600 && r <= 0x27BF, // misc symbols, dingbats
		r >= 0x2300 && r <= 0x23FF, // misc technical (watches, hourglasses)
		r >= 0x2B00 && r <= 0x2BFF, // arrows and shapes
		r >= 0x2194 && r <= 0x21AA,
		r >= 0x25AA && r <= 0x25FE:
		return true
	}
	switch r {
	case 0x00A9, 0x00AE, 0x203C, 0x2049, 0x2122, 0x2139, 0x24C2, 0x2934, 0x2935, 0x3030, 0x303D, 0x3297, 0x3299:
		return true
	}
	return false
}

// glyphs fetches and caches emoji art for one render.
type glyphs struct {
	client *http.Client
	cache  map[string]image.Image
}

func newGlyphs(client *http.Client) *glyphs {
	return &glyphs{client: client, cache: map[string]image.Image{}}
}

// get returns the art for a key, or nil when the CDN could not supply it,
// in which case the emoji draws as empty space rather than a broken frame.
func (g *glyphs) get(key string) image.Image {
	if img, ok := g.cache[key]; ok {
		return img
	}
	var img image.Image
	if resp, err := g.client.Get(emojiBase + "/" + key + ".png"); err == nil {
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode == http.StatusOK {
			if decoded, _, err := image.Decode(resp.Body); err == nil {
				img = decoded
			}
		}
	}
	g.cache[key] = img
	return img
}

// emojiSide is the square an emoji occupies for a face, in device pixels:
// the face's full line height, which is how a client sizes them too.
func emojiSide(f font.Face) int { return f.Metrics().Height.Ceil() }

// richWidth is the drawn width of segments in logical units.
func richWidth(f font.Face, segs []seg) int {
	w := fixed.Int26_6(0)
	for _, s := range segs {
		if s.emoji != "" {
			w += fixed.I(emojiSide(f))
			continue
		}
		w += font.MeasureString(f, s.text)
	}
	return w.Ceil() / scale
}

// drawRich draws segments at logical coordinates, baseline at y. Text and
// emoji advance together, so an emoji mid-name sits where the name has it.
func drawRich(dst *image.RGBA, f font.Face, col color.Color, x, y int, segs []seg, g *glyphs) {
	d := font.Drawer{Dst: dst, Src: &image.Uniform{col}, Face: f, Dot: fixed.P(px(x), px(y))}
	for _, s := range segs {
		if s.emoji == "" {
			d.DrawString(s.text)
			continue
		}
		side := emojiSide(f)
		if art := g.get(s.emoji); art != nil {
			top := d.Dot.Y.Ceil() - f.Metrics().Ascent.Ceil()
			r := image.Rect(d.Dot.X.Ceil(), top, d.Dot.X.Ceil()+side, top+side)
			scaled := image.NewRGBA(r)
			draw.CatmullRom.Scale(scaled, r, art, art.Bounds(), draw.Src, nil)
			draw.Draw(dst, r, scaled, r.Min, draw.Over)
		}
		d.Dot.X += fixed.I(side)
	}
}

// fitRich cuts segments to maxW logical units with a trailing ellipsis,
// measuring rather than counting runes, since names are other people's.
func fitRich(f font.Face, segs []seg, maxW int) []seg {
	if richWidth(f, segs) <= maxW {
		return segs
	}
	dots := seg{text: "..."}
	for len(segs) > 0 {
		last := segs[len(segs)-1]
		if last.emoji != "" || len(last.text) <= 1 {
			segs = segs[:len(segs)-1]
		} else {
			r := []rune(last.text)
			segs[len(segs)-1] = seg{text: string(r[:len(r)-1])}
		}
		if richWidth(f, append(segs[:len(segs):len(segs)], dots)) <= maxW {
			return append(segs, dots)
		}
	}
	return nil
}

// plain is the text of segments with emoji left out, for the callers that
// only need something to compare or log.
func plain(segs []seg) string {
	var b strings.Builder
	for _, s := range segs {
		b.WriteString(s.text)
	}
	return strings.TrimSpace(strings.Join(strings.Fields(b.String()), " "))
}
