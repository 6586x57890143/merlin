package core

import "github.com/bwmarrin/discordgo"

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

// NewEmbed builds a MessageEmbed with the given color/title/description and
// optional fields. Plugins should use this instead of constructing
// discordgo.MessageEmbed literals directly, so every command response shares
// one look.
func NewEmbed(color int, title, description string, fields ...*discordgo.MessageEmbedField) *discordgo.MessageEmbed {
	return &discordgo.MessageEmbed{
		Title:       title,
		Description: description,
		Color:       color,
		Fields:      fields,
	}
}

// RespondEmbed sends embed as an ephemeral interaction response — the
// embed-based counterpart to respondEphemeral, exported so plugin handlers
// can use it directly instead of building their own InteractionResponse.
func RespondEmbed(s *discordgo.Session, i *discordgo.InteractionCreate, embed *discordgo.MessageEmbed) error {
	return s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Embeds: []*discordgo.MessageEmbed{embed},
			Flags:  discordgo.MessageFlagsEphemeral,
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
