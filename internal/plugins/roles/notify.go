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
func (p *Plugin) notifyJailed(ctx context.Context, guildID, userID string, releaseAt *time.Time, reason string, sn sentence) {
	vars := map[string]string{"guild": p.guildName(guildID)}
	if releaseAt != nil {
		vars["until"] = relativeTimestamp(*releaseAt)
	}
	var fields []*discordgo.MessageEmbedField
	if reason != "" {
		fields = append(fields, &discordgo.MessageEmbedField{
			Name:  "Reason given",
			Value: core.TruncateEmbedField(reason),
		})
	}
	p.dm(ctx, guildID, userID, timedKey(sn.noticeKey, releaseAt), capitalize(sn.verb), core.ColorWarning, vars, fields...)
}

// notifyReleased tells userID their roles are back. sn is what they were
// released from, since coming home from the island is worded differently
// from getting out of the nest.
func (p *Plugin) notifyReleased(ctx context.Context, guildID, userID string, sn sentence) {
	p.dm(ctx, guildID, userID, sn.overKey, "Released", core.ColorSuccess,
		map[string]string{"guild": p.guildName(guildID)})
}

// notifyMoved tells userID they have been transferred into sentence to
// (script_vacation.go), and when it now ends. A nil releaseAt (a forever
// sentence, or a row that predates timed jails) gets the line that names no
// end rather than one promising a return.
func (p *Plugin) notifyMoved(ctx context.Context, guildID, userID string, releaseAt *time.Time, to sentence, reason string) {
	vars := map[string]string{"guild": p.guildName(guildID)}
	if releaseAt != nil {
		vars["until"] = relativeTimestamp(*releaseAt)
	}
	var fields []*discordgo.MessageEmbedField
	if reason != "" {
		fields = append(fields, &discordgo.MessageEmbedField{Name: "Reason given", Value: core.TruncateEmbedField(reason)})
	}
	p.dm(ctx, guildID, userID, timedKey(to.intoKey, releaseAt), "Moved to "+to.name, core.ColorWarning, vars, fields...)
}

// foreverKeys pairs every line that says when a sentence ends with the one
// said instead when it never does (see voice.KeyJailNoticeForever).
// TestEverySentenceLineHasAForeverTwin keeps it total over the sentence table.
var foreverKeys = map[voice.Key]voice.Key{
	voice.KeyJailNotice:               voice.KeyJailNoticeForever,
	voice.KeyJailAnnounce:             voice.KeyJailAnnounceForever,
	voice.KeyVacationNotice:           voice.KeyVacationNoticeForever,
	voice.KeyVacationAnnounce:         voice.KeyVacationAnnounceForever,
	voice.KeyVacationFromJail:         voice.KeyVacationFromJailForever,
	voice.KeyVacationFromJailAnnounce: voice.KeyVacationFromJailAnnounceForever,
	voice.KeyVacationToJail:           voice.KeyVacationToJailForever,
	voice.KeyVacationToJailAnnounce:   voice.KeyVacationToJailAnnounceForever,
}

// timedKey is key for a sentence ending at releaseAt, or its forever twin
// when there is no end. Callers set {until} only when releaseAt is non-nil.
func timedKey(key voice.Key, releaseAt *time.Time) voice.Key {
	if releaseAt == nil {
		return foreverKeys[key]
	}
	return key
}

// untilText renders a release instant for staff-facing text, or the honest
// words for a sentence with no end.
func untilText(t *time.Time) string {
	if t == nil {
		return "when a moderator decides"
	}
	return relativeTimestamp(*t)
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
	embed := core.NewEmbed(color, title, body, fields...)
	if _, err := p.ops(guildID).ChannelMessageSendComplex(ch.ID, &discordgo.MessageSend{
		Embed: embed,
		Files: core.EmbedFiles(embed),
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
