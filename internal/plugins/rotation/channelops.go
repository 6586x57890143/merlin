package rotation

import (
	"fmt"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/snowflake/v2"
)

// DiscordChannelOps is what rotation does in Discord, on disgo's types.
// discordguard.GuildOps satisfies it, so every write is gated; tests use a
// fake.
type DiscordChannelOps interface {
	GetChannel(channelID snowflake.ID) (discord.Channel, error)
	GetGuildChannels(guildID snowflake.ID) ([]discord.GuildChannel, error)
	GetActiveGuildThreads(guildID snowflake.ID) (*discord.GuildActiveThreads, error)
	CreateGuildChannel(guildID snowflake.ID, c discord.GuildChannelCreate) (discord.GuildChannel, error)
	// UpdateChannel is always sent a GuildTextChannelUpdate. Name, position,
	// parent and overwrites mean the same on every channel type, so one
	// payload serves the category and its channels alike.
	UpdateChannel(channelID snowflake.ID, u discord.ChannelUpdate) (discord.Channel, error)
	DeleteChannel(channelID snowflake.ID) error
	GetMessages(channelID, before snowflake.ID, limit int) ([]discord.Message, error)
	CreateMessage(channelID snowflake.ID, m discord.MessageCreate) (*discord.Message, error)
	PinMessage(channelID, messageID snowflake.ID) error
}

// guildChannel is channelID read as a guild channel.
func guildChannel(ops DiscordChannelOps, channelID snowflake.ID) (discord.GuildChannel, error) {
	c, err := ops.GetChannel(channelID)
	if err != nil {
		return nil, err
	}
	gc, ok := c.(discord.GuildChannel)
	if !ok {
		return nil, fmt.Errorf("channel %s is not in a server", channelID)
	}
	return gc, nil
}

// parent is ch's category, 0 for none.
func parent(ch discord.GuildChannel) snowflake.ID {
	if p := ch.ParentID(); p != nil {
		return *p
	}
	return 0
}
