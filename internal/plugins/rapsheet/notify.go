package rapsheet

import (
	"context"
	"fmt"
	"time"

	"github.com/disgoorg/disgo/discord"

	"github.com/6586x57890143/merlin/internal/core"
	"github.com/6586x57890143/merlin/internal/voice"
)

// Telling a member what happened to them.
//
// The same contract as roles/notify.go, and the same reasons: everything
// here is best effort and never affects the outcome of the action it
// describes, and the wording is the plain register because the reader has
// just been punished. The reason a moderator gave rides as its own field
// rather than inside the sentence, so a line never falls back for want of
// an optional placeholder.

// notifyWarned tells userID they have been warned, and for what.
func (p *Plugin) notifyWarned(ctx context.Context, guildID, userID string, category Category, reason string) {
	p.dm(ctx, guildID, userID, voice.KeyWarnNotice, "Warning", core.ColorWarning,
		map[string]string{"guild": p.guildName(guildID)},
		append(reasonFields(category, reason), nextStepField())...)
}

// reasonFields is the "what for" block every notice carries.
func reasonFields(category Category, reason string) []discord.EmbedField {
	fields := []discord.EmbedField{{
		Name:   "Category",
		Value:  categoryLabel(category),
		Inline: core.Inline(),
	}}
	if reason != "" {
		fields = append(fields, discord.EmbedField{
			Name:  "Reason given",
			Value: core.TruncateEmbedField(reason),
		})
	}
	return fields
}

// nextStepField is the "what now" line for a member who is still in the
// server: where to take it up, and where to see their own record. A ban
// gets none, because a banned member can reach neither.
func nextStepField() discord.EmbedField {
	return discord.EmbedField{
		Name:  "What now",
		Value: "If you think this was a mistake, a moderator in the server can look at it. `/rapsheet me` there shows everything on your record.",
	}
}

func (p *Plugin) dm(ctx context.Context, guildID, userID string, key voice.Key, title string, color int, vars map[string]string, fields ...discord.EmbedField) {
	body := p.speaker.Line(ctx, guildID, key, vars)
	if body == "" {
		// Nothing renderable to say. Saying nothing is strictly better than
		// sending a member an embed with a visible placeholder in it.
		p.log.Error("rapsheet: no line for member notice", "key", key, "guild", guildID)
		return
	}
	ch, err := p.ops(guildID).CreateDMChannel(core.ID(userID))
	if err != nil {
		// Closed DMs are the ordinary case here, not an incident.
		p.log.Info("rapsheet: could not open a DM to notify member", "guild", guildID, "user", userID, "err", err)
		return
	}
	embed := core.Embed(color, title, body, fields...)
	if _, err := p.ops(guildID).CreateMessage(ch.ID(), discord.MessageCreate{
		Embeds: []discord.Embed{core.Bulleted(embed)},
		Files:  core.EmbedUploads(embed),
	}); err != nil {
		p.log.Info("rapsheet: could not deliver member notice", "guild", guildID, "user", userID, "err", err)
	}
}

// guildName resolves guildID's display name for a DM, falling back to
// "this server" rather than failing the notice.
func (p *Plugin) guildName(guildID string) string {
	g, err := p.ops(guildID).GetGuild(core.ID(guildID))
	if err != nil || g == nil || g.Name == "" {
		return "this server"
	}
	return g.Name
}

// relativeTimestamp renders t as Discord's own relative timestamp markup,
// which each reader sees in their own locale and timezone ("in 3 hours").
func relativeTimestamp(t time.Time) string {
	return fmt.Sprintf("<t:%d:R>", t.Unix())
}

// absoluteTimestamp renders t as a short date-time in the reader's zone.
func absoluteTimestamp(t time.Time) string {
	return fmt.Sprintf("<t:%d:f>", t.Unix())
}
