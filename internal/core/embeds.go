package core

import (
	"bytes"
	_ "embed"
	"unicode/utf8"

	"github.com/disgoorg/disgo/discord"
)

// merlin's brand palette (spec.MD §4a), shared across every plugin's command
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

// rulePNG is the thin separator every Embed carries as its image: a small
// falcon over a barred line that fades out at both ends, transparent so it
// sits on either theme. It is there for its width as much as its look.
// Discord sizes an embed to its content, so a one-line confirmation and a
// field-heavy audit entry used to render as boxes of every width down one
// channel; an image as wide as the embed allows pins them all to the same
// width. Drawn at 4x in pixel art to match the moods, 1040x32 and under a
// kilobyte, so uploading it with every message costs nothing worth counting.
//
//go:embed assets/merlin_rule.png
var rulePNG []byte

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
	ruleAttachmentName   = "merlin_rule.png"
	ruleAttachmentURL    = "attachment://" + ruleAttachmentName
)

// merlin's moods. One drawing of her per kind of thing a message can be,
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

// The tip jar's three faces, cut from assets/source/merlin_tipjar_crying_source.png:
// crying over an empty jar, sweating over a nearly empty one, and dry-eyed
// over a full one.
//
//go:embed assets/merlin_tipjar_empty.png
var moodTipJarEmptyPNG []byte

//go:embed assets/merlin_tipjar_low.png
var moodTipJarLowPNG []byte

//go:embed assets/merlin_tipjar_full.png
var moodTipJarFullPNG []byte

//go:embed assets/merlin_supporter.png
var moodSupporterPNG []byte

// Mood is which drawing of merlin a message carries.
type Mood int

const (
	// MoodNone means no thumbnail at all, for embeds whose visual weight
	// comes from somewhere else (the setup wizard's banner).
	MoodNone Mood = iota
	MoodOK
	MoodError
	MoodWarn
	MoodInfo
	// MoodNotice is for things merlin is telling you rather than answering:
	// the rotation notice, the DMs a jailed member gets.
	MoodNotice
	// MoodIdle is for deliberately-stopped states, pause and dry-run. A
	// paused bot is not broken, and showing it the error face would say the
	// opposite of what an operator needs to read.
	MoodIdle
	// MoodTipJarEmpty, MoodTipJarLow and MoodTipJarFull belong to
	// /aimod funding show alone, set via WithMood from how much scanning
	// credit is left. No colour maps to them.
	MoodTipJarEmpty
	MoodTipJarLow
	MoodTipJarFull
	// MoodSupporter, merlin with a winged heart in her beak, is the thank
	// you card for a new supporter (roles). No colour maps to it.
	MoodSupporter
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

	MoodTipJarEmpty: {"merlin_tipjar_empty.png", moodTipJarEmptyPNG},
	MoodTipJarLow:   {"merlin_tipjar_low.png", moodTipJarLowPNG},
	MoodTipJarFull:  {"merlin_tipjar_full.png", moodTipJarFullPNG},

	MoodSupporter: {"merlin_supporter.png", moodSupporterPNG},
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

// asset is one compiled-in image an embed can point at.
type asset struct {
	name string
	png  []byte
}

func (a asset) upload() *discord.File {
	return discord.NewFile(a.name, "", bytes.NewReader(a.png))
}

// referencedAssets is every brand image an embed with these URLs points at:
// the avatar, the mood, the banner or rule. Each is listed once however many
// slots reference it, since Discord rejects a message carrying the same
// filename twice.
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

// SetMood overrides the mood an embed's colour implied, for the few states
// the palette cannot express on its own. Pause and dry-run are the reason
// it exists: both are reported as ordinary informational responses, and
// both should show merlin asleep rather than attentive.
func SetMood(e *discord.Embed, m Mood) {
	if url := moodAttachmentURL(m); url != "" {
		e.Thumbnail = &discord.EmbedResource{URL: url}
	} else {
		e.Thumbnail = nil
	}
}

// EmbedUploads is every upload e points at, for sending with it.
//
// Derived from the embed rather than tracked alongside it, because the two
// getting out of step is exactly the bug that shows a broken-image icon to
// a whole server: an attachment:// URL with no matching upload renders as a
// blank frame, and nothing about the code that built the embed would look
// wrong. Reading the URLs back off the finished embed means they cannot
// disagree. The avatar goes only where something points at it.
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

// AvatarUpload is merlin's avatar for a thumbnail: its attachment:// URL
// and the upload it names.
func AvatarUpload() (string, *discord.File) {
	return avatarAttachmentURL, asset{avatarAttachmentName, avatarPNG}.upload()
}

// Embed builds an embed with the given colour, title, description and
// fields, the rule as its image and the colour's mood as its thumbnail.
// Plugins use this (or the Ix Respond helpers) rather than embed literals,
// so every reply reads as one bot.
//
// Deliberately no footer and no timestamp. Both used to be set, and together
// they rendered as a second "merlin, today at 14:32" line directly underneath
// the one Discord already draws above every message this bot sends. It said
// nothing the client had not just said, in a smaller font, and the identity
// it was there to establish now comes from the mood thumbnail and the palette
// instead. An embed footer should carry something the reader would otherwise
// not know; repeating the author and the clock is furniture.
//
// A caller that sets its own image (a chart) replaces the rule, and the
// width it pins comes along with theirs.
func Embed(color int, title, description string, fields ...discord.EmbedField) discord.Embed {
	e := discord.Embed{
		Title: title, Description: description, Color: color, Fields: fields,
		Image: &discord.EmbedResource{URL: ruleAttachmentURL},
	}
	SetMood(&e, moodForColor(color))
	return e
}

// LandmarkEmbed is Embed's richer sibling, reserved for the handful of
// moments that genuinely warrant visual weight (first-time setup, the
// onboarding DM), not every routine response. merlin's banner is its image,
// and it has no mood thumbnail: the banner already carries the weight, and
// an embed with both reads as cluttered rather than considered.
func LandmarkEmbed(color int, title, description string, fields ...discord.EmbedField) discord.Embed {
	e := Embed(color, title, description, fields...)
	e.Image = &discord.EmbedResource{URL: bannerAttachmentURL}
	SetMood(&e, MoodNone)
	return e
}

// Inline is an embed field's Inline, which disgo takes as a pointer.
func Inline() *bool {
	t := true
	return &t
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

// maxEmbedDescriptionLen is Discord's hard limit on an embed description.
// Same failure mode as a field: over-limit rejects the whole message rather
// than trimming.
const maxEmbedDescriptionLen = 4096

// TruncateEmbedDescription is TruncateEmbedField's counterpart for the
// description, which is where any embed built as one prose body ends up.
//
// /config status is the motivating case and shows why this is a correctness
// fix rather than tidiness: it builds one description out of a database error
// string, a scheduler error string, and a per-channel "couldn't verify: %v",
// none of which have a bounded length, and none of which it authored. The
// message failing outright at 4096 bytes would mean the health command breaks
// precisely when something is unhealthy enough to produce a long error, which
// is the one moment it has to work.
func TruncateEmbedDescription(s string) string {
	if len(s) <= maxEmbedDescriptionLen {
		return s
	}
	const ellipsis = "\n... (truncated)"
	// Cut on a rune boundary: slicing mid-rune yields invalid UTF-8, which
	// Discord rejects outright.
	cut := maxEmbedDescriptionLen - len(ellipsis)
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + ellipsis
}
