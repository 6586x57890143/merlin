package core

import (
	"bytes"
	_ "embed"
	"time"
	"unicode/utf8"

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

// DeferResponse acknowledges an interaction immediately, before doing the
// work it asked for. Discord gives a handler 3 seconds to respond at all,
// then permanently fails the interaction with a user-visible "the
// application did not respond" — even when the work itself went on to
// succeed. Any handler that runs a job, walks a guild's channels, or makes
// more than a REST call or two must defer first and finish with
// FollowUpOK/FollowUpErr, which have 15 minutes to land instead of 3
// seconds.
func DeferResponse(s *discordgo.Session, i *discordgo.InteractionCreate) error {
	return s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseDeferredChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{Flags: discordgo.MessageFlagsEphemeral},
	})
}

// FollowUpOK and FollowUpErr replace a DeferResponse placeholder with the
// real answer, matching RespondOK/RespondErr's styling exactly so a deferred
// command is indistinguishable from an immediate one once it lands.
func FollowUpOK(s *discordgo.Session, i *discordgo.InteractionCreate, title, msg string) error {
	return followUp(s, i, NewEmbed(ColorSuccess, title, msg))
}

func FollowUpErr(s *discordgo.Session, i *discordgo.InteractionCreate, title string, err error) error {
	return followUp(s, i, NewEmbed(ColorError, title, err.Error()))
}

func followUp(s *discordgo.Session, i *discordgo.InteractionCreate, embed *discordgo.MessageEmbed) error {
	_, err := s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{
		Embeds: &[]*discordgo.MessageEmbed{embed},
		Files:  []*discordgo.File{avatarFile()},
	})
	return err
}

// maxEmbedFieldValue is Discord's hard limit on one embed field's value.
// Exceeding it doesn't truncate server-side — it rejects the whole message,
// so a single over-long value (a guild's sticky messages, a long list) would
// take out the entire response.
const maxEmbedFieldValue = 1024

// TruncateEmbedField clips s to what Discord will accept in an embed field
// value, marking that it was cut rather than silently dropping the tail.
func TruncateEmbedField(s string) string {
	if len(s) <= maxEmbedFieldValue {
		return s
	}
	const ellipsis = "\n… (truncated)"
	// Cut on a rune boundary: slicing mid-rune yields invalid UTF-8, which
	// Discord rejects outright.
	cut := maxEmbedFieldValue - len(ellipsis)
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + ellipsis
}

// AvatarFile and BannerFile let callers outside this package (e.g. a DM sent
// via ChannelMessageSendComplex, which isn't an interaction response) attach
// the same brand images without duplicating the embed byte constants.
func AvatarFile() *discordgo.File { return avatarFile() }
func BannerFile() *discordgo.File { return bannerFile() }
