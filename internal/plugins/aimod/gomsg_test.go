package aimod

import (
	"time"

	"github.com/bwmarrin/discordgo"
)

// goMessage reads m as gatewayMessage reads a disgo one. The tests predate
// disgo and their fixtures' IDs ("g1", "u1") are not snowflakes, so they
// build discordgo messages and come in here; gatewayMessage has its own
// test against Discord's JSON.
func goMessage(m *discordgo.Message) message {
	out := message{
		ID: m.ID, GuildID: m.GuildID, ChannelID: m.ChannelID,
		Webhook: m.WebhookID != "", Edited: m.EditedTimestamp != nil, Content: m.Content,
	}
	if m.Author != nil {
		out.AuthorID, out.Bot = m.Author.ID, m.Author.Bot
	}
	if m.Member != nil {
		out.RoleIDs = m.Member.Roles
	}
	for _, s := range m.MessageSnapshots {
		if s.Message != nil {
			out.Snapshots = append(out.Snapshots, s.Message.Content)
		}
	}
	if m.ReferencedMessage != nil {
		out.ReplyTo = m.ReferencedMessage.Content
	}
	return out
}

func (p *Plugin) handleGo(m *discordgo.Message) {
	if m != nil {
		p.handle(goMessage(m))
	}
}

func (p *Plugin) handleEditGo(m *discordgo.Message) {
	if m != nil && m.EditedTimestamp != nil {
		p.handle(goMessage(m))
	}
}

func (p *Plugin) shouldSkipGo(cfg Config, m *discordgo.Message, now time.Time) skipReason {
	return p.shouldSkip(cfg, goMessage(m), now)
}

func messageTextGo(m *discordgo.Message) (string, bool) { return messageText(goMessage(m)) }

func replyContextGo(m *discordgo.Message) string { return replyContext(goMessage(m)) }
