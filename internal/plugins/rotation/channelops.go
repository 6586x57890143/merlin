package rotation

import "github.com/bwmarrin/discordgo"

// DiscordChannelOps is the narrow slice of *discordgo.Session's REST methods
// the rotation state machine and sweep job need. Method signatures match
// discordgo's exactly (including the variadic RequestOption parameter), so
// *discordgo.Session satisfies this interface with no wrapper needed. Only
// tests provide a different (fake) implementation, mirroring the
// JobStateStore seam in internal/scheduler.
type DiscordChannelOps interface {
	Channel(channelID string, options ...discordgo.RequestOption) (*discordgo.Channel, error)
	GuildChannels(guildID string, options ...discordgo.RequestOption) ([]*discordgo.Channel, error)
	ThreadsActive(channelID string, options ...discordgo.RequestOption) (*discordgo.ThreadsList, error)
	GuildChannelCreateComplex(guildID string, data discordgo.GuildChannelCreateData, options ...discordgo.RequestOption) (*discordgo.Channel, error)
	ChannelEditComplex(channelID string, data *discordgo.ChannelEdit, options ...discordgo.RequestOption) (*discordgo.Channel, error)
	ChannelDelete(channelID string, options ...discordgo.RequestOption) (*discordgo.Channel, error)
	ChannelMessages(channelID string, limit int, beforeID, afterID, aroundID string, options ...discordgo.RequestOption) ([]*discordgo.Message, error)
	ChannelMessageSend(channelID, content string, options ...discordgo.RequestOption) (*discordgo.Message, error)
	ChannelMessageSendEmbed(channelID string, embed *discordgo.MessageEmbed, options ...discordgo.RequestOption) (*discordgo.Message, error)
	ChannelMessagePin(channelID, messageID string, options ...discordgo.RequestOption) error
	// User, called with "@me", resolves the bot's own user ID, needed so
	// newly-created channels that deny @everyone VIEW_CHANNEL (the hidden
	// staging channel, the archived channel) can also explicitly grant the
	// bot itself access; without it the bot would lock itself out of a
	// channel it just created, since its own role deliberately carries no
	// guild-wide Administrator bit to bypass a channel-level deny (least
	// privilege, spec.MD §4).
	User(userID string, options ...discordgo.RequestOption) (*discordgo.User, error)
}
