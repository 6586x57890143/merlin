package roles

import (
	"context"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/6586x57890143/merlin/internal/core"
	"github.com/6586x57890143/merlin/internal/voice"
)

// Telling the channel what just happened, not just the member concerned.
//
// notify.go's DM is read by the person it happened to, and stays in the
// plain register for exactly that reason. This is read by whoever else is
// sitting in the channel the command was run in: an audience, not the
// target, so it runs in Merlin's ordinary playful voice (see
// voice.KeyJailAnnounce/KeyReleaseAnnounce and PERSONA.md).
//
// Both are best effort, same policy as notify.go: the jail or release
// already happened and is already recorded by the time either of these
// runs, so a failed channel post must never be mistaken for a failed
// moderation action.

// announceJail posts a public notice into channelID naming jailedIDs and
// when they're back, in Merlin's playful voice. duration is rendered as the
// same Discord relative timestamp notifyJailed's DM uses for {until}, not a
// raw span, so a reader sees "in 3 hours" whether they got it by DM or read
// it in the channel. reason, when given, is attached as its own embed field
// rather than folded into the line, matching notifyJailed's own reasoning:
// an optional placeholder would make every line carrying it fall back on
// exactly the occasions it is missing.
func (p *Plugin) announceJail(ctx context.Context, guildID, channelID string, jailedIDs []string, duration time.Duration, reason string) {
	if len(jailedIDs) == 0 {
		return
	}
	vars := map[string]string{
		"members": mentionList(jailedIDs),
		"until":   relativeTimestamp(p.now().Add(duration)),
	}
	var fields []*discordgo.MessageEmbedField
	if reason != "" {
		fields = append(fields, &discordgo.MessageEmbedField{
			Name:  "Reason given",
			Value: core.TruncateEmbedField(reason),
		})
	}
	p.announce(ctx, guildID, channelID, voice.KeyJailAnnounce, core.ColorWarning, vars, fields...)
}

// announceRelease posts a public notice that releasedIDs are out, in
// Merlin's ordinary gentle register: no dunking on the way out, matching
// moderation.release's warmth rather than jail's spectacle.
func (p *Plugin) announceRelease(ctx context.Context, guildID, channelID string, releasedIDs []string) {
	if len(releasedIDs) == 0 {
		return
	}
	p.announce(ctx, guildID, channelID, voice.KeyReleaseAnnounce, core.ColorSuccess,
		map[string]string{"members": mentionList(releasedIDs)})
}

func (p *Plugin) announce(ctx context.Context, guildID, channelID string, key voice.Key, color int, vars map[string]string, fields ...*discordgo.MessageEmbedField) {
	body := p.voice.Line(ctx, guildID, key, vars)
	if body == "" {
		// Nothing renderable to say. Saying nothing beats posting an embed
		// with a visible placeholder in it.
		p.log.Error("roles: no line for channel announcement", "key", key, "guild", guildID)
		return
	}
	embed := core.NewEmbed(color, "", body, fields...)
	if _, err := p.ops(guildID).ChannelMessageSendComplex(channelID, &discordgo.MessageSend{
		Embed: embed,
		Files: core.EmbedFiles(embed),
	}); err != nil {
		p.log.Warn("roles: failed to post channel announcement", "guild", guildID, "channel", channelID, "key", key, "err", err)
	}
}
