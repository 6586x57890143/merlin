package core

import (
	"github.com/disgoorg/disgo/discord"
)

// Layout is Components on disgo's types: p, built from Parts, as the
// message's single top-level container. Same head, thumbnail, rule, note
// and footer, drawn the same way.
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
		discord.MediaGalleryComponent{Items: []discord.MediaGalleryItem{{Media: discord.UnfurledMediaItem{URL: ruleAttachmentURL}}}},
	}
	if p.Note != "" {
		parts = append(parts, discord.TextDisplayComponent{Content: p.Note})
	}
	parts = append(parts, p.Parts...)
	if p.Footer != "" {
		parts = append(parts, discord.TextDisplayComponent{Content: "-# " + p.Footer})
	}
	return []discord.LayoutComponent{discord.ContainerComponent{AccentColor: p.Color, Components: parts}}
}

// Uploads is Files on disgo's types: the rule and the thumbnail, nothing
// else.
func (p Panel) Uploads() []*discord.File {
	mood := p.Mood
	if mood == MoodNone {
		mood = moodForColor(p.Color)
	}
	files := []*discord.File{asset{ruleAttachmentName, rulePNG}.upload()}
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
