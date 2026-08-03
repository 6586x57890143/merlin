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
// every embed response, referenced via Discord's attachment:// scheme.
// Deliberately not an external image host, so branding never breaks due to
// link rot or an outage of something this bot doesn't control.
const (
	avatarAttachmentName = "merlin_avatar.png"
	avatarAttachmentURL  = "attachment://" + avatarAttachmentName
	bannerAttachmentName = "merlin_banner.png"
	bannerAttachmentURL  = "attachment://" + bannerAttachmentName
)

// Merlin's moods. One drawing of her per kind of thing a message can be,
// shown as the embed's thumbnail.
//
// The thumbnail slot rather than the author icon: Discord renders the
// author icon at around 24px and crops it to a circle, which turns a
// detailed square sprite into a smudge with its corners missing. The
// thumbnail renders near 80px, uncropped.
//
// Embedded into the binary and referenced via attachment:// for the same
// reason as the avatar and banner above: no external image host, so
// branding cannot break because something this bot does not control had an
// outage.
//
//go:embed assets/merlin_ok.png
var moodOKPNG []byte

//go:embed assets/merlin_error.png
var moodErrorPNG []byte

//go:embed assets/merlin_warn.png
var moodWarnPNG []byte

//go:embed assets/merlin_info.png
var moodInfoPNG []byte

//go:embed assets/merlin_notice.png
var moodNoticePNG []byte

//go:embed assets/merlin_idle.png
var moodIdlePNG []byte

// Mood is which drawing of Merlin a message carries.
type Mood int

const (
	// MoodNone means no thumbnail at all, for embeds whose visual weight
	// comes from somewhere else (the setup wizard's banner).
	MoodNone Mood = iota
	MoodOK
	MoodError
	MoodWarn
	MoodInfo
	// MoodNotice is for things Merlin is telling you rather than answering:
	// the rotation notice, the DMs a jailed member gets.
	MoodNotice
	// MoodIdle is for deliberately-stopped states, pause and dry-run. A
	// paused bot is not broken, and showing it the error face would say the
	// opposite of what an operator needs to read.
	MoodIdle
)

var moodAssets = map[Mood]struct {
	name string
	png  []byte
}{
	MoodOK:     {"merlin_ok.png", moodOKPNG},
	MoodError:  {"merlin_error.png", moodErrorPNG},
	MoodWarn:   {"merlin_warn.png", moodWarnPNG},
	MoodInfo:   {"merlin_info.png", moodInfoPNG},
	MoodNotice: {"merlin_notice.png", moodNoticePNG},
	MoodIdle:   {"merlin_idle.png", moodIdlePNG},
}

// moodForColor maps an embed's colour to a mood.
//
// Deriving it from the colour rather than taking it as a parameter is what
// lets every existing RespondOK/Err/Info/Warn call site pick up an icon
// without being touched. The colour already encodes exactly this
// distinction; asking a hundred call sites to repeat it in a second
// argument would only create opportunities for the two to disagree.
func moodForColor(color int) Mood {
	switch color {
	case ColorSuccess:
		return MoodOK
	case ColorError:
		return MoodError
	case ColorWarning:
		return MoodWarn
	case ColorPrimary:
		return MoodNotice
	default:
		return MoodInfo
	}
}

func moodAttachmentURL(m Mood) string {
	a, ok := moodAssets[m]
	if !ok {
		return ""
	}
	return "attachment://" + a.name
}

func moodFile(m Mood) *discordgo.File {
	a, ok := moodAssets[m]
	if !ok {
		return nil
	}
	return &discordgo.File{Name: a.name, ContentType: "image/png", Reader: bytes.NewReader(a.png)}
}

// WithMood overrides the mood an embed's colour implied, for the few states
// the palette cannot express on its own. Pause and dry-run are the reason
// it exists: both are reported as ordinary informational responses, and
// both should show Merlin asleep rather than attentive.
func WithMood(embed *discordgo.MessageEmbed, m Mood) *discordgo.MessageEmbed {
	if url := moodAttachmentURL(m); url != "" {
		embed.Thumbnail = &discordgo.MessageEmbedThumbnail{URL: url}
	} else {
		embed.Thumbnail = nil
	}
	return embed
}

// embedFiles returns every attachment an embed references.
//
// Derived from the embed rather than tracked alongside it, because the two
// getting out of step is exactly the bug that shows a broken-image icon to
// a whole server: an attachment:// URL with no matching upload renders as a
// blank frame, and nothing about the code that built the embed would look
// wrong. Reading the URLs back off the finished embed means they cannot
// disagree.
func embedFiles(embed *discordgo.MessageEmbed) []*discordgo.File {
	files := []*discordgo.File{avatarFile()}
	if embed == nil {
		return files
	}
	if embed.Thumbnail != nil {
		for m, a := range moodAssets {
			if embed.Thumbnail.URL == "attachment://"+a.name {
				files = append(files, moodFile(m))
				break
			}
		}
	}
	if embed.Image != nil && embed.Image.URL == bannerAttachmentURL {
		files = append(files, bannerFile())
	}
	return files
}

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
	e := &discordgo.MessageEmbed{
		Title:       title,
		Description: description,
		Color:       color,
		Fields:      fields,
		Timestamp:   time.Now().UTC().Format(time.RFC3339),
		Footer:      &discordgo.MessageEmbedFooter{Text: "Merlin", IconURL: avatarAttachmentURL},
	}
	return WithMood(e, moodForColor(color))
}

// NewLandmarkEmbed is NewEmbed's richer sibling, reserved for the handful of
// moments that genuinely warrant visual weight (first-time setup, the
// onboarding DM), not every routine response (a banner image on every
// one-line confirmation would be noise, not polish). Adds Merlin's banner
// as the embed's large image; everything else (footer, timestamp) matches
// NewEmbed exactly.
func NewLandmarkEmbed(color int, title, description string, fields ...*discordgo.MessageEmbedField) *discordgo.MessageEmbed {
	e := NewEmbed(color, title, description, fields...)
	e.Image = &discordgo.MessageEmbedImage{URL: bannerAttachmentURL}
	// No mood thumbnail here. The banner already carries the visual weight,
	// and an embed with both reads as cluttered rather than considered.
	return WithMood(e, MoodNone)
}

// RespondEmbed sends embed as an ephemeral interaction response, attaching
// the brand avatar file its footer icon references, the embed-based
// counterpart to respondEphemeral, exported so plugin handlers can use it
// directly instead of building their own InteractionResponse.
func RespondEmbed(s *discordgo.Session, i *discordgo.InteractionCreate, embed *discordgo.MessageEmbed) error {
	return s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Embeds: []*discordgo.MessageEmbed{embed},
			Files:  embedFiles(embed),
			Flags:  discordgo.MessageFlagsEphemeral,
		},
	})
}

// RespondLandmarkEmbed is RespondEmbed's counterpart for a NewLandmarkEmbed:
// it also attaches the banner file the embed's Image references, alongside
// the footer's avatar file.
func RespondLandmarkEmbed(s *discordgo.Session, i *discordgo.InteractionCreate, embed *discordgo.MessageEmbed) error {
	return s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Embeds: []*discordgo.MessageEmbed{embed},
			Files:  embedFiles(embed),
			Flags:  discordgo.MessageFlagsEphemeral,
		},
	})
}

// RespondLandmarkEmbedWithComponents combines RespondLandmarkEmbed's banner
// attachment with RespondEmbedWithComponents' components, for the rare
// response that's both a landmark moment and needs interactive controls
// (e.g. /config setup's first-run channel prompts).
func RespondLandmarkEmbedWithComponents(s *discordgo.Session, i *discordgo.InteractionCreate, embed *discordgo.MessageEmbed, components []discordgo.MessageComponent) error {
	return s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Embeds:     []*discordgo.MessageEmbed{embed},
			Components: components,
			Files:      embedFiles(embed),
			Flags:      discordgo.MessageFlagsEphemeral,
		},
	})
}

// RespondOK, RespondErr, RespondInfo, and RespondWarn are the response
// shapes every command handler in this bot needs, kept here rather than
// duplicated per plugin (adminconfig, rotation, scheduler all defined
// identical copies before this). Response styling is a UI concern, not
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
// notice something, e.g. a partial success, or a deprecated option.
func RespondWarn(s *discordgo.Session, i *discordgo.InteractionCreate, title, msg string) {
	_ = RespondEmbed(s, i, NewEmbed(ColorWarning, title, msg))
}

// DeferResponse acknowledges an interaction immediately, before doing the
// work it asked for. Discord gives a handler 3 seconds to respond at all,
// then permanently fails the interaction with a user-visible "the
// application did not respond", even when the work itself went on to
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
		Files:  embedFiles(embed),
	})
	return err
}

// maxEmbedFieldValue is Discord's hard limit on one embed field's value.
// Exceeding it doesn't truncate server-side: it rejects the whole message,
// so a single over-long value (a guild's sticky messages, a long list) would
// take out the entire response.
const maxEmbedFieldValue = 1024

// TruncateEmbedField clips s to what Discord will accept in an embed field
// value, marking that it was cut rather than silently dropping the tail.
func TruncateEmbedField(s string) string {
	if len(s) <= maxEmbedFieldValue {
		return s
	}
	const ellipsis = "\n... (truncated)"
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

// EmbedFiles returns every attachment embed references, for senders outside
// an interaction response (a DM, a channel post).
//
// Prefer this over listing files by hand. NewEmbed now attaches a mood
// thumbnail by attachment:// URL, so a caller that hand-lists only the
// avatar ends up sending an embed that references an image it did not
// upload, which Discord renders as a broken frame. Asking the embed what it
// needs cannot go out of step with what the embed actually has.
func EmbedFiles(embed *discordgo.MessageEmbed) []*discordgo.File { return embedFiles(embed) }
