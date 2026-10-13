package statistics

import "github.com/bwmarrin/discordgo"

// The tests predate disgo and their fixtures' IDs ("g1", "u1") are not
// snowflakes, so they build discordgo values and come in here, read as
// HandleMessage and HandleVoiceState read disgo's; intake_test checks
// those against Discord's JSON.
func (p *Plugin) messageGo(m *discordgo.Message) {
	if m == nil {
		return
	}
	in := post{guildID: m.GuildID, channelID: m.ChannelID, at: m.Timestamp, skip: m.Author == nil || m.WebhookID != ""}
	if m.Author != nil {
		in.authorID, in.avatar, in.skip = m.Author.ID, m.Author.Avatar, in.skip || m.Author.Bot
		in.name = m.Author.Username
		if m.Author.GlobalName != "" {
			in.name = m.Author.GlobalName
		}
	}
	if m.Member != nil && m.Member.Nick != "" {
		in.name = m.Member.Nick
	}
	p.count1(in)
}

func (p *Plugin) voiceGo(vs *discordgo.VoiceStateUpdate) {
	if vs == nil || vs.VoiceState == nil {
		return
	}
	in := move{guildID: vs.GuildID, userID: vs.UserID, channelID: vs.ChannelID}
	if vs.BeforeUpdate != nil {
		in.before = &vs.BeforeUpdate.ChannelID
	}
	if m := vs.Member; m != nil && m.User != nil {
		in.bot, in.named, in.avatar, in.name = m.User.Bot, true, m.User.Avatar, m.User.Username
		if m.User.GlobalName != "" {
			in.name = m.User.GlobalName
		}
		if m.Nick != "" {
			in.name = m.Nick
		}
	}
	p.voice1(in)
}
