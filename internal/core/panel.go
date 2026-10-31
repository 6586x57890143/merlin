package core

import (
	_ "embed"
	"strings"
	"unicode/utf8"

	"github.com/disgoorg/disgo/discord"
)

// Panel is a settings surface: one Components V2 container that a plugin's
// whole configuration is read and changed from, in place of a tree of
// subcommands that each answered with an embed.
//
// The layout is the one skua settled on, kept because it reads on a phone:
// the title and one subtext line sit beside the mood thumbnail, and
// everything else goes below at full width, since beside a thumbnail a phone
// gives text a narrow column. What is merlin's own is the art: the mood is
// still derived from the colour (moodForColor), and the rule is the same
// barred line Embed carries, here closing the page above the footer. It is
// there for its width as much as its look, exactly as on an embed: a
// container is sized to its content, so a panel whose page is two short lines
// would otherwise render narrower than the one before it and every click
// would make the controls jump.
//
// A panel holds no session. Everything it shows is re-read from settings on
// every render and anything a control needs to know rides in its CustomID, so
// a panel opened last week still works and two admins can drive one at once,
// the same choice /config setup made.
type Panel struct {
	Color int
	Title string
	// Line is the subtext beside the thumbnail: what this panel is for.
	Line string
	// Note says what the last click did ("✓ ...", "✗ ..."). Shown under the
	// head, gone on the next render.
	Note string
	// Parts is the page: text, rows of controls, separators.
	Parts []discord.ContainerSubComponent
	// Footer is a closing subtext line.
	Footer string
	// Mood overrides the one Color implies (SetMood's counterpart).
	Mood Mood
	// Plugin is whose panel this is: its icon leads the title once the
	// emoji are synced. The thumbnail stays merlin's mood, which is the
	// art; the icon says which part of her this is.
	Plugin string
	// Icon replaces the mood thumbnail, for a page that is about the thing
	// it shows (a /help page wears its plugin's tile). IconUpload is its
	// upload, nil when Icon is already a URL Discord can fetch.
	Icon       string
	IconUpload *discord.File
	// Hero leads the panel with the supporter campaign's banner (merlin in
	// flight at dusk) at full width, the head under it with no thumbnail
	// beside: for a card whose art is the point, not a page somebody works
	// in.
	Hero bool
}

// The supporter campaign's banner, which a Hero panel leads with: drawn
// for it in the perk tiles' dusk palette, halved to 1024x384 and stripped
// of metadata. Discord shows it at about 550 px, so this is still sharp on
// a dense screen at a quarter of the original's weight.
//
//go:embed assets/merlin_supporter_hero.png
var heroPNG []byte

const (
	heroAttachmentName = "merlin_supporter_hero.png"
	heroAttachmentURL  = "attachment://" + heroAttachmentName
)

// Layout renders p as the message's single top-level container.
func (p Panel) Layout() []discord.LayoutComponent {
	mood := p.Mood
	if mood == MoodNone {
		mood = moodForColor(p.Color)
	}
	head := "## " + p.Title
	if e := PluginEmoji(p.Plugin); e != "" {
		head = "## " + e + " " + p.Title
	}
	if p.Line != "" {
		head += "\n-# " + p.Line
	}
	parts := []discord.ContainerSubComponent{
		discord.SectionComponent{
			Components: []discord.SectionSubComponent{discord.TextDisplayComponent{Content: head}},
			Accessory:  discord.ThumbnailComponent{Media: discord.UnfurledMediaItem{URL: p.thumbnail(mood)}},
		},
	}
	if p.Hero {
		parts = []discord.ContainerSubComponent{
			discord.MediaGalleryComponent{Items: []discord.MediaGalleryItem{{Media: discord.UnfurledMediaItem{URL: heroAttachmentURL}, Description: "merlin in flight over a dusk sky"}}},
			discord.TextDisplayComponent{Content: head},
		}
	}
	if p.Note != "" {
		parts = append(parts, discord.TextDisplayComponent{Content: p.Note})
	}
	parts = append(parts, p.Parts...)
	// The rule closes the page and divides off the footer, never splits the
	// head from what it heads.
	parts = append(parts, discord.MediaGalleryComponent{Items: []discord.MediaGalleryItem{{Media: discord.UnfurledMediaItem{URL: ruleAttachmentURL}}}})
	if p.Footer != "" {
		parts = append(parts, discord.TextDisplayComponent{Content: "-# " + p.Footer})
	}
	return []discord.LayoutComponent{discord.ContainerComponent{AccentColor: p.Color, Components: parts}}
}

func (p Panel) thumbnail(mood Mood) string {
	if p.Icon != "" {
		return p.Icon
	}
	return moodAttachmentURL(mood)
}

// Uploads is what p points at: its thumbnail and the rule, never anything
// else, so an attachment:// URL can never be left without its upload.
func (p Panel) Uploads() []*discord.File {
	mood := p.Mood
	if mood == MoodNone {
		mood = moodForColor(p.Color)
	}
	files := []*discord.File{asset{ruleAttachmentName, rulePNG}.upload()}
	if p.Hero {
		return append(files, asset{heroAttachmentName, heroPNG}.upload())
	}
	if p.Icon != "" {
		if p.IconUpload != nil {
			files = append(files, p.IconUpload)
		}
		return files
	}
	if a, ok := moodAssets[mood]; ok {
		files = append(files, asset{a.name, a.png}.upload())
	}
	return files
}

// readoutWidth holds a grid's value column to 28, so a line stays inside
// the 40 or so columns a phone shows of a code block.
const readoutWidth = 28

// Readout lays facts out as a code block grid: the label column is the
// longest label plus two, so values line up the same on every client, and a
// value too long for its column wraps after a comma, staying in it. Discord
// gives Components V2 no field layout, and a monospace grid is the one that
// holds. A code block shows markup as typed, so a row whose value is a
// mention, a timestamp or a link goes under the grid as a bold-label line,
// where it renders.
func Readout(rows [][2]string) string {
	var grid, below []string
	width := 0
	for _, r := range rows {
		if !hasMarkup(r[1]) {
			width = max(width, utf8.RuneCountInString(r[0]))
		}
	}
	for _, r := range rows {
		if hasMarkup(r[1]) {
			below = append(below, "**"+r[0]+"** "+r[1])
			continue
		}
		for n, line := range wrapList(r[1], readoutWidth) {
			label := ""
			if n == 0 {
				label = r[0]
			}
			grid = append(grid, strings.TrimRight(label+strings.Repeat(" ", width+2-utf8.RuneCountInString(label))+line, " "))
		}
	}
	if len(grid) > 0 {
		below = append([]string{"```\n" + strings.Join(grid, "\n") + "\n```"}, below...)
	}
	return strings.Join(below, "\n")
}

// hasMarkup is whether v holds something a code block would show as typed:
// a mention, emoji or timestamp (<...>) or a masked link.
func hasMarkup(v string) bool { return strings.Contains(v, "<") || strings.Contains(v, "](") }

// wrapList breaks a comma separated list into lines of at most width,
// breaking only after a comma. A single item longer than width stays whole.
func wrapList(s string, width int) []string {
	var lines []string
	line := ""
	for n, item := range strings.Split(s, ", ") {
		if n > 0 {
			item = ", " + item
		}
		if line != "" && utf8.RuneCountInString(line+item) > width {
			lines = append(lines, line+",")
			item = strings.TrimPrefix(item, ", ")
			line = ""
		}
		line += item
	}
	return append(lines, line)
}

// MaxSelectValues is Discord's cap on a select's options and picks.
const MaxSelectValues = 25

// MoodURL is the attachment:// URL of merlin's drawing for m, or "" for
// MoodNone, for a card that sets its own thumbnail (the audit log's).
func MoodURL(m Mood) string { return moodAttachmentURL(m) }

// RuleURL is the rule's attachment:// URL, for the same.
const RuleURL = ruleAttachmentURL
