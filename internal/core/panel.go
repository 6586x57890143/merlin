package core

import (
	"strings"

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
	// Hero leads the panel with merlin's banner at full width, the head
	// under it with no thumbnail beside: for a card whose art is the point
	// (the supporter advert), not a page somebody works in.
	Hero bool
}

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
			discord.MediaGalleryComponent{Items: []discord.MediaGalleryItem{{Media: discord.UnfurledMediaItem{URL: bannerAttachmentURL}, Description: "merlin in flight"}}},
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
		return append(files, asset{bannerAttachmentName, bannerPNG}.upload())
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

// Readout lays facts out one a line, the label in bold: the fact lines of
// the shared design guide. Plain lines rather than a code block grid, which
// read as a terminal, cut off on phones and showed a mention as its raw
// snowflake; a code block is for what somebody copies.
func Readout(rows [][2]string) string {
	lines := make([]string, 0, len(rows))
	for _, r := range rows {
		lines = append(lines, "**"+r[0]+"** "+r[1])
	}
	return strings.Join(lines, "\n")
}

// MaxSelectValues is Discord's cap on a select's options and picks.
const MaxSelectValues = 25

// MoodURL is the attachment:// URL of merlin's drawing for m, or "" for
// MoodNone, for a card that sets its own thumbnail (the audit log's).
func MoodURL(m Mood) string { return moodAttachmentURL(m) }

// RuleURL is the rule's attachment:// URL, for the same.
const RuleURL = ruleAttachmentURL

