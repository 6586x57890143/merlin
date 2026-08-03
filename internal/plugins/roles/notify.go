package roles

import (
	"context"
	"fmt"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/6586x57890143/merlin/internal/core"
	"github.com/6586x57890143/merlin/internal/voice"
)

// Telling a member what happened to them.
//
// Everything in this file is best effort and must never affect the outcome
// of the action it describes. The jail already happened and is already
// recorded by the time any of this runs; a member with DMs closed, or a
// Discord hiccup, cannot be allowed to turn a successful jail into a failed
// one. Every path here logs and returns.
//
// The wording comes from internal/voice's plain register, not the playful
// one. The reader has just been punished and is having a bad minute, and a
// joke aimed at them is what turns a moderation action into a screenshot.

// notifyJailed tells userID they have been jailed in guildID, and when it
// ends. reason is attached as its own field when a mod gave one, rather
// than being folded into the sentence: an optional placeholder would make
// every line carrying it fall back on exactly the occasions it is missing.
func (p *Plugin) notifyJailed(ctx context.Context, guildID, userID string, releaseAt time.Time, reason string) {
	vars := map[string]string{
		"guild": p.guildName(guildID),
		"until": relativeTimestamp(releaseAt),
	}
	var fields []*discordgo.MessageEmbedField
	if reason != "" {
		fields = append(fields, &discordgo.MessageEmbedField{
			Name:  "Reason given",
			Value: core.TruncateEmbedField(reason),
		})
	}
	p.dm(ctx, guildID, userID, voice.KeyJailNotice, "Jailed", core.ColorWarning, vars, fields...)
}

// notifyReleased tells userID their roles are back.
func (p *Plugin) notifyReleased(ctx context.Context, guildID, userID string) {
	p.dm(ctx, guildID, userID, voice.KeyReleaseNotice, "Released", core.ColorSuccess,
		map[string]string{"guild": p.guildName(guildID)})
}

func (p *Plugin) dm(ctx context.Context, guildID, userID string, key voice.Key, title string, color int, vars map[string]string, fields ...*discordgo.MessageEmbedField) {
	body := p.voice.Line(ctx, guildID, key, vars)
	if body == "" {
		// Nothing renderable to say. Saying nothing is strictly better than
		// sending a member an embed with a visible placeholder in it.
		p.log.Error("roles: no line for member notice", "key", key, "guild", guildID)
		return
	}

	ch, err := p.ops(guildID).UserChannelCreate(userID)
	if err != nil {
		// Closed DMs are the ordinary case here, not an incident, so this
		// is Info rather than Error. It is still worth a line: "they were
		// never told" is a question mods do ask.
		p.log.Info("roles: could not open a DM to notify member", "guild", guildID, "user", userID, "err", err)
		return
	}
	if _, err := p.ops(guildID).ChannelMessageSendComplex(ch.ID, &discordgo.MessageSend{
		Embed: core.NewEmbed(color, title, body, fields...),
		Files: []*discordgo.File{core.AvatarFile()},
	}); err != nil {
		p.log.Info("roles: could not deliver member notice", "guild", guildID, "user", userID, "err", err)
	}
}

// guildName resolves guildID's display name for a DM, falling back to
// "this server" rather than failing the notice. A DM has to say which
// server it is about (most members are in many), but not knowing the name
// is a reason to be vague, not a reason to stay silent.
func (p *Plugin) guildName(guildID string) string {
	g, err := p.ops(guildID).Guild(guildID)
	if err != nil || g == nil || g.Name == "" {
		return "this server"
	}
	return g.Name
}

// relativeTimestamp renders t as Discord's own relative timestamp markup,
// which each reader sees rendered in their own locale and timezone ("in 3
// hours"). A jail that ends "at 02:00 UTC" is a small puzzle for someone
// who does not think in UTC, and this is a message they are reading while
// annoyed. Written out rather than taken from discordgo, which has no
// helper for this in the pinned version.
func relativeTimestamp(t time.Time) string {
	return fmt.Sprintf("<t:%d:R>", t.Unix())
}
