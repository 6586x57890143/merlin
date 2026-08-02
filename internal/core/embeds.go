package core

import (
	"bytes"
	_ "embed"
	"time"

	"github.com/bwmarrin/discordgo"
)

// Merlin's brand palette (spec.MD §4a), shared across every plugin's command
// responses so the bot's replies read consistently instead of each plugin
// picking its own colors. ColorSuccess/ColorError/ColorInfo/ColorWarning are
// the four RespondXxx-mapped semantic colors below; Primary/Accent/Light/
// Dark/Black are available for embeds that want brand color without a
// success/error/info/warning connotation (e.g. a plain informational embed
// that isn't a "list" in the ColorInfo sense).
const (
	ColorPrimary = 0x4B4C73
	ColorInfo    = 0x7A7DB8
	ColorAccent  = 0x8B5E34
	ColorLight   = 0xECD9AE
	ColorDark    = 0x3D2817
	ColorBlack   = 0x201812
	ColorSuccess = 0x7C8B5A
	ColorWarning = 0xD9A62E
	ColorError   = 0xA85234
)

//go:embed assets/merlin_avatar.png
var avatarPNG []byte

//go:embed assets/merlin_banner.png
var bannerPNG []byte

// Attachment names/URLs for the two brand images above. Both are embedded
// directly into the binary (go:embed) and sent as a message attachment on
// every embed response, referenced via Discord's attachment:// scheme —
// deliberately not an external image host, so branding never breaks due to
// link rot or an outage of something this bot doesn't control.
const (
	avatarAttachmentName = "merlin_avatar.png"
	avatarAttachmentURL  = "attachment://" + avatarAttachmentName
	bannerAttachmentName = "merlin_banner.png"
	bannerAttachmentURL  = "attachment://" + bannerAttachmentName
)

func avatarFile() *discordgo.File {
	return &discordgo.File{Name: avatarAttachmentName, ContentType: "image/png", Reader: bytes.NewReader(avatarPNG)}
}

func bannerFile() *discordgo.File {
	return &discordgo.File{Name: bannerAttachmentName, ContentType: "image/png", Reader: bytes.NewReader(bannerPNG)}
}

// NewEmbed builds a MessageEmbed with the given color/title/description and
// optional fields, plus Merlin's own consistent footer (brand icon + name)
// and a timestamp, so every plugin's responses read as one cohesive bot
// rather than a pile of ad hoc messages. Plugins should always use this (or
// RespondOK/Err/Info/Warn below) instead of constructing discordgo.MessageEmbed
// literals directly.
func NewEmbed(color int, title, description string, fields ...*discordgo.MessageEmbedField) *discordgo.MessageEmbed {
	return &discordgo.MessageEmbed{
		Title:       title,
		Description: description,
		Color:       color,
		Fields:      fields,
		Timestamp:   time.Now().UTC().Format(time.RFC3339),
		Footer:      &discordgo.MessageEmbedFooter{Text: "Merlin", IconURL: avatarAttachmentURL},
	}
}

// NewLandmarkEmbed is NewEmbed's richer sibling, reserved for the handful of
// moments that genuinely warrant visual weight — first-time setup, the
// onboarding DM — not every routine response (a banner image on every
// one-line confirmation would be noise, not polish). Adds Merlin's banner
// as the embed's large image; everything else (footer, timestamp) matches
// NewEmbed exactly.
func NewLandmarkEmbed(color int, title, description string, fields ...*discordgo.MessageEmbedField) *discordgo.MessageEmbed {
	e := NewEmbed(color, title, description, fields...)
	e.Image = &discordgo.MessageEmbedImage{URL: bannerAttachmentURL}
	return e
}

// RespondEmbed sends embed as an ephemeral interaction response, attaching
// the brand avatar file its footer icon references — the embed-based
// counterpart to respondEphemeral, exported so plugin handlers can use it
// directly instead of building their own InteractionResponse.
func RespondEmbed(s *discordgo.Session, i *discordgo.InteractionCreate, embed *discordgo.MessageEmbed) error {
	return s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Embeds: []*discordgo.MessageEmbed{embed},
			Files:  []*discordgo.File{avatarFile()},
			Flags:  discordgo.MessageFlagsEphemeral,
		},
	})
}

// RespondLandmarkEmbed is RespondEmbed's counterpart for a NewLandmarkEmbed
// — also attaches the banner file the embed's Image references, alongside
// the footer's avatar file.
func RespondLandmarkEmbed(s *discordgo.Session, i *discordgo.InteractionCreate, embed *discordgo.MessageEmbed) error {
	return s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Embeds: []*discordgo.MessageEmbed{embed},
			Files:  []*discordgo.File{avatarFile(), bannerFile()},
			Flags:  discordgo.MessageFlagsEphemeral,
		},
	})
}

// RespondLandmarkEmbedWithComponents combines RespondLandmarkEmbed's banner
// attachment with RespondEmbedWithComponents' components — for the rare
// response that's both a landmark moment and needs interactive controls
// (e.g. /config setup's first-run channel prompts).
func RespondLandmarkEmbedWithComponents(s *discordgo.Session, i *discordgo.InteractionCreate, embed *discordgo.MessageEmbed, components []discordgo.MessageComponent) error {
	return s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Embeds:     []*discordgo.MessageEmbed{embed},
			Components: components,
			Files:      []*discordgo.File{avatarFile(), bannerFile()},
			Flags:      discordgo.MessageFlagsEphemeral,
		},
	})
}

// RespondOK, RespondErr, RespondInfo, and RespondWarn are the response
// shapes every command handler in this bot needs, kept here rather than
// duplicated per plugin (adminconfig, rotation, scheduler all defined
// identical copies before this) — response styling is UI concern, not
// plugin business logic, so it lives in core and plugins just supply their
// own title/content.
func RespondOK(s *discordgo.Session, i *discordgo.InteractionCreate, title, msg string) {
	_ = RespondEmbed(s, i, NewEmbed(ColorSuccess, title, msg))
}

func RespondErr(s *discordgo.Session, i *discordgo.InteractionCreate, title string, err error) {
	_ = RespondEmbed(s, i, NewEmbed(ColorError, title, err.Error()))
}

func RespondInfo(s *discordgo.Session, i *discordgo.InteractionCreate, title, msg string) {
	_ = RespondEmbed(s, i, NewEmbed(ColorInfo, title, msg))
}

// RespondWarn is for a response that succeeded but the invoker should still
// notice something — e.g. a partial success, or a deprecated option.
func RespondWarn(s *discordgo.Session, i *discordgo.InteractionCreate, title, msg string) {
	_ = RespondEmbed(s, i, NewEmbed(ColorWarning, title, msg))
}

// AvatarFile and BannerFile let callers outside this package (e.g. a DM sent
// via ChannelMessageSendComplex, which isn't an interaction response) attach
// the same brand images without duplicating the embed byte constants.
func AvatarFile() *discordgo.File { return avatarFile() }
func BannerFile() *discordgo.File { return bannerFile() }
