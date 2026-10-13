package core

import (
	"bytes"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/snowflake/v2"
)

// The embed builders on disgo's types, for plugins answering through an
// Ix. Same palette, moods and brand assets as NewEmbed and friends, and the
// same rule that every attachment:// URL comes with its upload: both read
// it off the finished embed through referencedAssets.

// asset is one compiled-in image an embed can point at.
type asset struct {
	name string
	png  []byte
}

// referencedAssets is every brand image an embed with these URLs points at,
// in the order embedFiles always uploaded them: the avatar, the mood, the
// banner or rule. See embedFiles for why it is derived, never listed.
func referencedAssets(footerIcon, authorIcon, thumb, image string) []asset {
	var out []asset
	if footerIcon == avatarAttachmentURL || authorIcon == avatarAttachmentURL || thumb == avatarAttachmentURL || image == avatarAttachmentURL {
		out = append(out, asset{avatarAttachmentName, avatarPNG})
	}
	if thumb != "" {
		for _, a := range moodAssets {
			if thumb == "attachment://"+a.name {
				out = append(out, asset{a.name, a.png})
				break
			}
		}
	}
	switch image {
	case bannerAttachmentURL:
		out = append(out, asset{bannerAttachmentName, bannerPNG})
	case ruleAttachmentURL:
		out = append(out, asset{ruleAttachmentName, rulePNG})
	}
	return out
}

func (a asset) upload() *discord.File {
	return discord.NewFile(a.name, "", bytes.NewReader(a.png))
}

// Embed is NewEmbed on disgo's types: colour, title, description and
// fields, the rule as its image and the colour's mood as its thumbnail.
func Embed(color int, title, description string, fields ...discord.EmbedField) discord.Embed {
	e := discord.Embed{
		Title: title, Description: description, Color: color, Fields: fields,
		Image: &discord.EmbedResource{URL: ruleAttachmentURL},
	}
	SetMood(&e, moodForColor(color))
	return e
}

// LandmarkEmbed is NewLandmarkEmbed: the banner instead of the rule, and
// no thumbnail.
func LandmarkEmbed(color int, title, description string, fields ...discord.EmbedField) discord.Embed {
	e := Embed(color, title, description, fields...)
	e.Image = &discord.EmbedResource{URL: bannerAttachmentURL}
	SetMood(&e, MoodNone)
	return e
}

// SetMood is WithMood: the mood the colour implied, overridden.
func SetMood(e *discord.Embed, m Mood) {
	if url := moodAttachmentURL(m); url != "" {
		e.Thumbnail = &discord.EmbedResource{URL: url}
	} else {
		e.Thumbnail = nil
	}
}

// EmbedUploads is EmbedFiles: every upload e points at.
func EmbedUploads(e discord.Embed) []*discord.File {
	var footer, author, thumb, image string
	if e.Footer != nil {
		footer = e.Footer.IconURL
	}
	if e.Author != nil {
		author = e.Author.IconURL
	}
	if e.Thumbnail != nil {
		thumb = e.Thumbnail.URL
	}
	if e.Image != nil {
		image = e.Image.URL
	}
	var files []*discord.File
	for _, a := range referencedAssets(footer, author, thumb, image) {
		files = append(files, a.upload())
	}
	return files
}

// AvatarUpload is AvatarIcon on disgo's types.
func AvatarUpload() (string, *discord.File) {
	return avatarAttachmentURL, asset{avatarAttachmentName, avatarPNG}.upload()
}

// PluginUpload is PluginIcon on disgo's types: the CDN URL and no file once
// the emoji are synced, otherwise the PNG and its attachment:// URL.
func PluginUpload(plugin string) (string, *discord.File) {
	key := "plugin_" + plugin
	if r, ok := emojiFor(key); ok {
		return "https://cdn.discordapp.com/emojis/" + r.id + ".png", nil
	}
	png, err := emojiArt.ReadFile("assets/emoji/" + key + ".png")
	if err != nil {
		return "", nil
	}
	return "attachment://" + key + ".png", asset{key + ".png", png}.upload()
}

// PluginOptionEmoji is PluginComponentEmoji on disgo's types, nil until the
// emoji are synced.
func PluginOptionEmoji(plugin string) *discord.ComponentEmoji {
	r, ok := emojiFor("plugin_" + plugin)
	if !ok {
		return nil
	}
	id, err := snowflake.Parse(r.id)
	if err != nil {
		return nil
	}
	return &discord.ComponentEmoji{ID: id, Name: r.name}
}

// Inline is an embed field's Inline, which disgo takes as a pointer.
func Inline() *bool {
	t := true
	return &t
}
