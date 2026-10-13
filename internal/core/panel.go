package core

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/bwmarrin/discordgo"
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
// barred line NewEmbed carries, here as the divider under the head. It is
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
	// rule, gone on the next render.
	Note string
	// Body is the page: TextDisplays, ActionsRows, Separators.
	Body []discordgo.MessageComponent
	// Footer is a closing subtext line.
	Footer string
	// Mood overrides the one Color implies (WithMood's counterpart).
	Mood Mood
	// Plugin is whose panel this is: its icon leads the title once the
	// emoji are synced. The thumbnail stays merlin's mood, which is the
	// art; the icon says which part of her this is.
	Plugin string
	// Icon replaces the mood thumbnail, for a page that is about the thing
	// it shows (a /help page wears its plugin's tile). IconFile is its
	// upload, nil when Icon is already a URL Discord can fetch.
	Icon     string
	IconFile *discordgo.File

	// Parts and IconUpload are Body and IconFile on disgo's types, for a
	// plugin answering through an Ix, which draws with Layout and Uploads.
	// Both shapes live here until the last plugin is off discordgo.
	Parts      []discord.ContainerSubComponent
	IconUpload *discord.File
}

// Components renders p as the message's single top-level container.
func (p Panel) Components() []discordgo.MessageComponent {
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
	parts := []discordgo.MessageComponent{
		discordgo.Section{
			Components: []discordgo.MessageComponent{discordgo.TextDisplay{Content: head}},
			Accessory:  discordgo.Thumbnail{Media: discordgo.UnfurledMediaItem{URL: p.thumbnail(mood)}},
		},
		discordgo.MediaGallery{Items: []discordgo.MediaGalleryItem{{Media: discordgo.UnfurledMediaItem{URL: ruleAttachmentURL}}}},
	}
	if p.Note != "" {
		parts = append(parts, discordgo.TextDisplay{Content: p.Note})
	}
	parts = append(parts, p.Body...)
	if p.Footer != "" {
		parts = append(parts, discordgo.TextDisplay{Content: "-# " + p.Footer})
	}
	color := p.Color
	return []discordgo.MessageComponent{discordgo.Container{AccentColor: &color, Components: parts}}
}

func (p Panel) thumbnail(mood Mood) string {
	if p.Icon != "" {
		return p.Icon
	}
	return moodAttachmentURL(mood)
}

// Files is what p points at: its thumbnail and the rule, never anything
// else, so an attachment:// URL can never be left without its upload.
func (p Panel) Files() []*discordgo.File {
	mood := p.Mood
	if mood == MoodNone {
		mood = moodForColor(p.Color)
	}
	thumb := moodFile(mood)
	if p.Icon != "" {
		thumb = p.IconFile
	}
	files := []*discordgo.File{{Name: ruleAttachmentName, ContentType: "image/png", Reader: bytes.NewReader(rulePNG)}}
	if thumb != nil {
		files = append(files, thumb)
	}
	return files
}

// Text is a TextDisplay, for building a Panel's Body.
func Text(s string) discordgo.TextDisplay { return discordgo.TextDisplay{Content: s} }

// Row is an ActionsRow, for building a Panel's Body.
func Row(c ...discordgo.MessageComponent) discordgo.ActionsRow {
	return discordgo.ActionsRow{Components: c}
}

// Gap is a small divider between a panel's sections.
func Gap() discordgo.Separator {
	return discordgo.Separator{}
}

// Readout lays facts out as a code block grid: the label column is the
// longest label plus two spaces, so values line up the same on every client,
// where how embed fields wrap is up to each one. Keep it to plain facts: a
// mention inside a code block renders as its raw snowflake, so who and where
// go on lines below the grid, not in it.
func Readout(rows [][2]string) string {
	width := 0
	for _, r := range rows {
		width = max(width, len(r[0]))
	}
	var b strings.Builder
	b.WriteString("```\n")
	for _, r := range rows {
		fmt.Fprintf(&b, "%-*s%s\n", width+2, r[0], r[1])
	}
	b.WriteString("```")
	return b.String()
}

// MaxSelectValues is Discord's cap on a select's options and picks.
const MaxSelectValues = 25

// StringSelect is a select over fixed options. multi lets any number be
// picked, none included, which is what a set drawn with its current members
// preselected needs.
func StringSelect(customID, placeholder string, opts []discordgo.SelectMenuOption, multi, disabled bool) discordgo.SelectMenu {
	m := discordgo.SelectMenu{MenuType: discordgo.StringSelectMenu, CustomID: customID, Placeholder: placeholder, Options: opts[:min(len(opts), MaxSelectValues)], Disabled: disabled}
	if multi {
		zero := 0
		m.MinValues, m.MaxValues = &zero, len(m.Options)
	}
	return m
}

// EntitySelect is a user, role or channel picker drawn with ids
// preselected, so the set is what the member sees and edits as one; what
// changed comes back from SelectDiff. max is how many may be picked (1 for
// a single value). A list longer than the cap shows its first 25 and the
// rest are untouched by a submit, because SelectDiff only removes what was
// drawn.
func EntitySelect(kind discordgo.SelectMenuType, customID, placeholder string, ids []string, max int, disabled bool) discordgo.SelectMenu {
	dv := map[discordgo.SelectMenuType]discordgo.SelectMenuDefaultValueType{
		discordgo.UserSelectMenu:    discordgo.SelectMenuDefaultValueUser,
		discordgo.RoleSelectMenu:    discordgo.SelectMenuDefaultValueRole,
		discordgo.ChannelSelectMenu: discordgo.SelectMenuDefaultValueChannel,
	}[kind]
	m := discordgo.SelectMenu{MenuType: kind, CustomID: customID, Placeholder: placeholder, MaxValues: min(max, MaxSelectValues), Disabled: disabled}
	if max > 1 {
		zero := 0
		m.MinValues = &zero
	}
	for _, id := range ids[:min(len(ids), MaxSelectValues)] {
		if id != "" {
			m.DefaultValues = append(m.DefaultValues, discordgo.SelectMenuDefaultValue{ID: id, Type: dv})
		}
	}
	return m
}

// Clearable lets a single-value picker be emptied, which is how a member
// says "none" for a setting that may be unset.
func Clearable(m discordgo.SelectMenu) discordgo.SelectMenu {
	zero := 0
	m.MinValues = &zero
	return m
}

// ChannelSelect is EntitySelect for channels of the given types, one pick.
func ChannelSelect(customID, placeholder, current string, disabled bool, types ...discordgo.ChannelType) discordgo.SelectMenu {
	m := EntitySelect(discordgo.ChannelSelectMenu, customID, placeholder, []string{current}, 1, disabled)
	m.ChannelTypes = types
	return m
}

// Button is a panel button. A disabled one is still drawn, so a member who
// may not press it can see that it exists.
func Button(label, customID string, style discordgo.ButtonStyle, disabled bool) discordgo.Button {
	return discordgo.Button{Label: label, Style: style, CustomID: customID, Disabled: disabled}
}

// Switch is a section with a button beside its text: a setting, its state,
// and the one control that changes it.
func Switch(text string, b discordgo.Button) discordgo.Section {
	return discordgo.Section{Components: []discordgo.MessageComponent{Text(text)}, Accessory: b}
}

// Field is one text input in a modal, prefilled with value.
func Field(id, label, value, placeholder string, paragraph, required bool) discordgo.ActionsRow {
	style := discordgo.TextInputShort
	maxLen := 200
	if paragraph {
		style, maxLen = discordgo.TextInputParagraph, 4000
	}
	return Row(discordgo.TextInput{CustomID: id, Label: label, Value: value, Placeholder: placeholder, Style: style, Required: required, MaxLength: maxLen})
}
