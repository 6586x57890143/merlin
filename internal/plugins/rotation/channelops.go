package rotation

import "github.com/bwmarrin/discordgo"

// DiscordChannelOps is the narrow slice of *discordgo.Session's REST methods
// the rotation state machine and sweep job need. Method signatures match
// discordgo's exactly (including the variadic RequestOption parameter), so
// *discordgo.Session satisfies this interface with no wrapper needed — only
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
}
