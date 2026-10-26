// Command icons draws every plugin's icon and the bullets a card's field
// labels wear: one glyph each, kept below as an 11 by 11 text grid so a
// change to it is reviewable as text, painted onto a tile in merlin's
// palette and written as a 128 px application emoji into
// internal/core/assets/emoji. Edit a glyph here and rerun; never edit
// the PNGs.
//
//	go run ./tools/icons
//
// The tile is pixel art on an exact grid, 32 cells of 4 px: an umber ground,
// a bronze frame with its corners cut, lit from the top left and shaded on
// the bottom right the way the mood badges are, and the glyph in cream at 2
// cells a pixel with a one-cell shadow under it.
package main

import (
	"fmt"
	"image"
	"image/color"
	"image/png"
	"log"
	"os"
	"path/filepath"
	"sort"
)

// glyphs are each plugin's icon, keyed by the plugin's Name(). 'w' is ink.
var glyphs = map[string][]string{
	"adminconfig": { // sliders
		".w...w...w.",
		".w...w...w.",
		"www..w...w.",
		".w...w...w.",
		".w..www..w.",
		".w...w...w.",
		".w...w..www",
		".w...w...w.",
		".w...w...w.",
		".w...w...w.",
		".w...w...w.",
	},
	"rotation": { // two arrows chasing round
		"...wwww.w..",
		"..w....ww..",
		".w....www..",
		"w..........",
		"w..........",
		"w.........w",
		"..........w",
		"..........w",
		"..www....w.",
		"..ww....w..",
		"..w.wwww...",
	},
	"roles": { // padlock
		"...wwwww...",
		"..w.....w..",
		"..w.....w..",
		"..w.....w..",
		"wwwwwwwwwww",
		"w.........w",
		"w...www...w",
		"w...www...w",
		"w....w....w",
		"w.........w",
		"wwwwwwwwwww",
	},
	"aimod": { // an eye, reading
		"...........",
		"...........",
		"....www....",
		"..ww...ww..",
		".w..www..w.",
		"w..ww.ww..w",
		".w..www..w.",
		"..ww...ww..",
		"....www....",
		"...........",
		"...........",
	},
	"rapsheet": { // clipboard
		"....www....",
		".ww.www.ww.",
		".w.......w.",
		".w.wwwww.w.",
		".w.......w.",
		".w.wwwww.w.",
		".w.......w.",
		".w.www...w.",
		".w.......w.",
		".w.......w.",
		".wwwwwwwww.",
	},
	"contest": { // trophy
		"..wwwwwww..",
		"www.....www",
		"w.w.....w.w",
		"w.w.....w.w",
		".ww.....ww.",
		"...w...w...",
		"....www....",
		".....w.....",
		".....w.....",
		"...wwwww...",
		"...wwwww...",
	},
	"statistics": { // bars
		"........www",
		"........www",
		"....www.www",
		"....www.www",
		"....www.www",
		"www.www.www",
		"www.www.www",
		"www.www.www",
		"www.www.www",
		"...........",
		"wwwwwwwwwww",
	},
	"fleet": { // birds flying in a v
		"...........",
		"....w.w....",
		".....w.....",
		"...........",
		"...........",
		"..w.w.w.w..",
		"...w...w...",
		"...........",
		"w.w.....w.w",
		".w.......w.",
		"...........",
	},
	"ping": { // a bolt
		"...........",
		"......www..",
		".....www...",
		"....www....",
		"...www.....",
		"..wwwwwww..",
		".....www...",
		"....www....",
		"...www.....",
		"..www......",
		"...........",
	},
	"support": { // a heart, for backing the birds
		"...........",
		"..ww...ww..",
		".wwww.wwww.",
		".wwwwwwwww.",
		".wwwwwwwww.",
		".wwwwwwwww.",
		"..wwwwwww..",
		"...wwwww...",
		"....www....",
		".....w.....",
		"...........",
	},
	"scheduler": { // clock
		"...wwwww...",
		".ww.....ww.",
		".w...w...w.",
		"w....w....w",
		"w....w....w",
		"w....www..w",
		"w.........w",
		"w.........w",
		".w.......w.",
		".ww.....ww.",
		"...wwwww...",
	},
	"help": { // an open book
		"...........",
		".www...www.",
		"w...w.w...w",
		"w.ww.w.ww.w",
		"w....w....w",
		"w.ww.w.ww.w",
		"w....w....w",
		"w....w....w",
		".www.w.www.",
		"....www....",
		"...........",
	},
	"perf": { // a gauge
		"...........",
		"....www....",
		"..ww...ww..",
		".w.......w.",
		"w.......w.w",
		"w......w..w",
		"w.....w...w",
		"w....w....w",
		"w...www...w",
		"wwwwwwwwwww",
		"...........",
	},
}

// keys are the bullets a card's field labels wear (core.Bulleted), keyed by
// what they mean rather than the label, since many labels share one.
var keys = map[string][]string{
	"actor": { // a badge: who did it
		".....w.....",
		".....w.....",
		"....www....",
		"wwwwwwwwwww",
		".wwwwwwwww.",
		"..wwwwwww..",
		"...wwwww...",
		"..www.www..",
		"..ww...ww..",
		".ww.....ww.",
		".w.......w.",
	},
	"member": { // a bust
		"...........",
		"....www....",
		"...wwwww...",
		"...wwwww...",
		"...wwwww...",
		"....www....",
		"...........",
		"..wwwwwww..",
		".wwwwwwwww.",
		".wwwwwwwww.",
		"...........",
	},
	"channel": { // hash
		"...........",
		"...w...w...",
		"...w...w...",
		".wwwwwwwww.",
		"...w...w...",
		"...w...w...",
		"...w...w...",
		".wwwwwwwww.",
		"...w...w...",
		"...w...w...",
		"...........",
	},
	"role": { // a tag
		"...........",
		"wwwwwww....",
		"w......w...",
		"w.ww....w..",
		"w.ww.....w.",
		"w.........w",
		"w........w.",
		"w.......w..",
		"w......w...",
		"wwwwwww....",
		"...........",
	},
	"policy": { // a shield with a tick
		"wwwwwwwwwww",
		"w.........w",
		"w.........w",
		"w.......w.w",
		"w......w..w",
		"w.w...w...w",
		".w.w.w...w.",
		".w..w....w.",
		"..w.....w..",
		"...w...w...",
		"....www....",
	},
	"gauge": { // a dial: confidence, score
		"...........",
		"...........",
		"...wwwww...",
		"..w.....w..",
		".w......ww.",
		"w......w..w",
		"w.....w...w",
		"w....w....w",
		"w...www...w",
		"...........",
		"...........",
	},
	"time": { // a clock
		"...wwwww...",
		"..w.....w..",
		".w...w...w.",
		"w....w....w",
		"w....w....w",
		"w....www..w",
		"w.........w",
		"w.........w",
		".w.......w.",
		"..w.....w..",
		"...wwwww...",
	},
	"message": { // a speech bubble
		"...........",
		"wwwwwwwwwww",
		"w.........w",
		"w.wwwwwww.w",
		"w.........w",
		"w.wwwww...w",
		"w.........w",
		"wwwwwwwwwww",
		"..ww.......",
		"..w........",
		"...........",
	},
	"reason": { // quote marks
		"...........",
		"...........",
		".www...www.",
		".www...www.",
		".www...www.",
		"...w.....w.",
		"...w.....w.",
		"..w.....w..",
		".w.....w...",
		"...........",
		"...........",
	},
	"change": { // an arrow
		"...........",
		"......w....",
		"......ww...",
		"......www..",
		"wwwwwwwwww.",
		"wwwwwwwwwww",
		"wwwwwwwwww.",
		"......www..",
		"......ww...",
		"......w....",
		"...........",
	},
	"link": { // two links of a chain
		"...........",
		"...........",
		"...........",
		".www...www.",
		"w...w.w...w",
		"w..wwwww..w",
		"w...w.w...w",
		".www...www.",
		"...........",
		"...........",
		"...........",
	},
	"prize": { // a gift
		"..ww...ww..",
		"...w...w...",
		"....w.w....",
		"wwwwwwwwwww",
		"w....w....w",
		"wwwwwwwwwww",
		".w...w...w.",
		".w...w...w.",
		".w...w...w.",
		".w...w...w.",
		".wwwwwwwww.",
	},
	"status": { // a flag
		".ww........",
		".wwwwwwwww.",
		".wwwwwwwww.",
		".wwwwwwwww.",
		".wwwwwwwww.",
		".w.........",
		".w.........",
		".w.........",
		".w.........",
		".w.........",
		".w.........",
	},
	"dot": { // any other label
		"...........",
		"...........",
		"...........",
		".....w.....",
		"....www....",
		"...wwwww...",
		"....www....",
		".....w.....",
		"...........",
		"...........",
		"...........",
	},
}

// merlin's palette (internal/core/embeds.go): the ground is ColorDark, the
// frame ColorAccent, the glyph ColorLight. The bevel's light and dark are
// the frame's own colour lifted and dropped, so the tile reads as one piece
// of merlin's set rather than a borrowed icon.
var (
	ground = rgb(0x3D2817)
	frame  = rgb(0x8B5E34)
	lit    = rgb(0xB0814E)
	shade  = rgb(0x24170D)
	ink    = rgb(0xECD9AE)
)

const (
	size      = 128 // what Discord keeps an application emoji at
	cell      = 4
	tileCells = size / cell // 32
	glyphSide = 11
	glyphK    = 2 // cells per glyph pixel
	glyphOff  = (tileCells - glyphSide*glyphK) / 2
)

func main() {
	out := filepath.Join("internal", "core", "assets", "emoji")
	if err := os.MkdirAll(out, 0o755); err != nil {
		log.Fatal(err)
	}
	for prefix, set := range map[string]map[string][]string{"plugin_": glyphs, "key_": keys} {
		names := make([]string, 0, len(set))
		for name := range set {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			g := set[name]
			if err := balanced(g); err != nil {
				log.Fatalf("glyph %s%s: %v", prefix, name, err)
			}
			if err := write(filepath.Join(out, prefix+name+".png"), scale(tile(g), cell)); err != nil {
				log.Fatal(err)
			}
		}
	}
}

// balanced reports whether a glyph is 11 by 11 with equal empty space on
// opposite sides, which is what centres it on the tile.
func balanced(g []string) error {
	if len(g) != glyphSide {
		return fmt.Errorf("%d rows, want %d", len(g), glyphSide)
	}
	x0, x1, y0, y1 := glyphSide, -1, glyphSide, -1
	for y, row := range g {
		if len(row) != glyphSide {
			return fmt.Errorf("row %d %q is not %d wide", y, row, glyphSide)
		}
		for x := range glyphSide {
			switch row[x] {
			case 'w':
				x0, x1, y0, y1 = min(x0, x), max(x1, x), min(y0, y), max(y1, y)
			case '.':
			default:
				return fmt.Errorf("unknown ink %q at %d,%d", row[x], x, y)
			}
		}
	}
	last := glyphSide - 1
	if x0 != last-x1 || y0 != last-y1 {
		return fmt.Errorf("margins left %d right %d top %d bottom %d; it would sit off centre", x0, last-x1, y0, last-y1)
	}
	return nil
}

// tile is one plugin's icon at one pixel a cell.
func tile(g []string) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, tileCells, tileCells))
	last := tileCells - 1
	for y := range tileCells {
		for x := range tileCells {
			dx, dy := min(x, last-x), min(y, last-y)
			switch {
			case dx+dy < 2: // the cut corner
			case dx == 0 || dy == 0 || dx+dy == 2:
				img.SetNRGBA(x, y, frame)
			case (x == 1 || y == 1) && x < last-1 && y < last-1:
				img.SetNRGBA(x, y, lit)
			case x == last-1 || y == last-1:
				img.SetNRGBA(x, y, shade)
			default:
				img.SetNRGBA(x, y, ground)
			}
		}
	}
	// The shadow first, one cell down and right, then the glyph over it.
	for pass, c := range []color.NRGBA{shade, ink} {
		for gy, row := range g {
			for gx := range len(row) {
				if row[gx] != 'w' {
					continue
				}
				for ky := range glyphK {
					for kx := range glyphK {
						px := glyphOff + gx*glyphK + kx + 1 - pass
						py := glyphOff + gy*glyphK + ky + 1 - pass
						img.SetNRGBA(px, py, c)
					}
				}
			}
		}
	}
	return img
}

// scale upscales nearest-neighbour by k, so every cell stays a hard square.
func scale(src *image.NRGBA, k int) *image.NRGBA {
	b := src.Bounds()
	dst := image.NewNRGBA(image.Rect(0, 0, b.Dx()*k, b.Dy()*k))
	for y := range b.Dy() * k {
		for x := range b.Dx() * k {
			dst.SetNRGBA(x, y, src.NRGBAAt(x/k, y/k))
		}
	}
	return dst
}

// write saves img as an indexed PNG at best compression, which pixel art
// always fits in a fraction of a true colour file's size.
func write(path string, img *image.NRGBA) error {
	index := map[color.NRGBA]uint8{}
	var pal color.Palette
	b := img.Bounds()
	out := image.NewPaletted(b, nil)
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			c := img.NRGBAAt(x, y)
			i, ok := index[c]
			if !ok {
				i = uint8(len(pal))
				index[c] = i
				pal = append(pal, c)
			}
			out.SetColorIndex(x, y, i)
		}
	}
	out.Palette = pal
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	enc := png.Encoder{CompressionLevel: png.BestCompression}
	if err := enc.Encode(f, out); err != nil {
		_ = f.Close()
		return err
	}
	fmt.Println(filepath.ToSlash(path))
	return f.Close()
}

func rgb(v uint32) color.NRGBA {
	return color.NRGBA{uint8(v >> 16), uint8(v >> 8), uint8(v), 0xFF}
}
