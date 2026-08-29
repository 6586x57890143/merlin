package roles

import (
	"context"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/bwmarrin/discordgo"

	"github.com/6586x57890143/merlin/internal/voice"
)

// Telling the channel what just happened, not just the member concerned.
//
// notify.go's DM is read by the person it happened to, and stays in the
// plain register for exactly that reason. This is read by whoever else is
// sitting in the channel the command was run in: an audience, not the
// target, so it runs in merlin's ordinary playful voice (see
// voice.KeyJailAnnounce/KeyReleaseAnnounce and PERSONA.md).
//
// A plain message, not an embed: this is a chat aside, not a status report,
// and Discord's own "-# " subtext markdown already gives the secondary
// detail (the reason, when a mod gave one) the same visually-set-apart
// treatment an embed field used to, without the extra chrome.
//
// Both announceJail and announceRelease, and the echo in
// announceDestinations, are best effort, same policy as notify.go: the jail
// or release already happened and is already recorded by the time either
// of these runs, so a failed channel post must never be mistaken for a
// failed moderation action.

// maxAnnounceReasonLen bounds the mod-given reason folded into a jail
// announcement's subtext line. Discord places no length limit on the
// /roles jail "reason" option itself, so without this a long paste would
// eat into the plain message's 2000-byte content ceiling; a jail
// announcement's reason is meant to be a short annotation, not the whole
// message.
const maxAnnounceReasonLen = 300

// truncateReason clips s to maxAnnounceReasonLen, marking that it was cut
// rather than silently dropping the tail. Mirrors core.TruncateEmbedField's
// shape, but that helper is sized for an embed field (1024 bytes) and this
// is a single subtext line in a plain message, a different, smaller budget.
func truncateReason(s string) string {
	if len(s) <= maxAnnounceReasonLen {
		return s
	}
	const ellipsis = " (truncated)"
	cut := maxAnnounceReasonLen - len(ellipsis)
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + ellipsis
}

// announceJail posts a public notice naming jailedIDs and when they're
// back, in merlin's playful voice, into the channel the command was run in
// and the guild's configured announcement channel, if it has one
// (announceDestinations).
// duration is rendered as the same Discord relative timestamp notifyJailed's
// DM uses for {until}, not a raw span, so a reader sees "in 3 hours" whether
// they got it by DM or read it in a channel, and bold-wrapped for emphasis:
// <@id> mentions already render with their own pill styling from Discord
// itself, so only the timestamp needs it. reason, when given, becomes its
// own "-# " subtext line rather than being folded into the sentence,
// matching notifyJailed's own reasoning: an optional placeholder would make
// every catalog line carrying it fall back on exactly the occasions it is
// missing.
func (p *Plugin) announceJail(ctx context.Context, guildID, invokingChannelID string, jailedIDs []string, duration time.Duration, reason string) {
	if len(jailedIDs) == 0 {
		return
	}
	vars := map[string]string{
		"members": mentionList(jailedIDs),
		"until":   "**" + relativeTimestamp(p.now().Add(duration)) + "**",
	}
	body := p.voice.Line(ctx, guildID, voice.KeyJailAnnounce, vars)
	if body == "" {
		// Nothing renderable to say. Saying nothing beats posting a message
		// with a visible placeholder in it.
		p.log.Error("roles: no line for jail announcement", "guild", guildID)
		return
	}
	if reason != "" {
		// A subtext line has to stay one line: a reason containing a
		// newline would otherwise start a second, unprefixed line that
		// renders as ordinary text glued onto the fine print.
		clean := strings.Join(strings.Fields(reason), " ")
		body += "\n-# reason: " + truncateReason(clean)
	}
	p.broadcast(guildID, invokingChannelID, body)
}

// announceRelease posts a public notice that releasedIDs are out, in
// merlin's ordinary gentle register: no dunking on the way out, matching
// moderation.release's warmth rather than jail's spectacle. Same
// destinations as announceJail: the channel the command was run in, plus
// the guild's configured announcement channel, since someone waiting there
// benefits from knowing people do get let out.
func (p *Plugin) announceRelease(ctx context.Context, guildID, invokingChannelID string, releasedIDs []string) {
	if len(releasedIDs) == 0 {
		return
	}
	body := p.voice.Line(ctx, guildID, voice.KeyReleaseAnnounce, map[string]string{"members": mentionList(releasedIDs)})
	if body == "" {
		p.log.Error("roles: no line for release announcement", "guild", guildID)
		return
	}
	p.broadcast(guildID, invokingChannelID, body)
}

// broadcast sends content, unchanged, to every channel announceDestinations
// returns. Per-destination failures are logged and don't stop the rest,
// same policy as jailchannels.go's guild-wide syncs: one channel's problem
// must not silence the announcement everywhere else.
func (p *Plugin) broadcast(guildID string, invokingChannelID, content string) {
	for _, chID := range p.announceDestinations(guildID, invokingChannelID) {
		if _, err := p.ops(guildID).ChannelMessageSendComplex(chID, &discordgo.MessageSend{Content: content}); err != nil {
			p.log.Warn("roles: failed to post channel announcement", "guild", guildID, "channel", chID, "err", err)
		}
	}
}

// announceDestinations is the invoking channel plus the guild's one
// configured jail-announcement channel (/roles configure announce-channel),
// when set and not the invoking channel itself.
//
// It used to fan out to every jail-allowed channel instead. That allowlist
// exists to decide what a jailed member can *see*, which is a different
// question from where a jail is announced: a guild with several visible
// rooms got the same notice repeated in all of them. One configured channel
// (usually the one the jailed member lands in) says it once, where the
// person it happened to will read it.
//
// No channel-type filtering and no GuildChannels call: the option is a
// native Channel picker restricted to text and announcement channels
// (commands.go), so an unpostable destination can't be configured in the
// first place, and a channel deleted afterwards just makes broadcast log a
// failed send.
func (p *Plugin) announceDestinations(guildID, invokingChannelID string) []string {
	out := []string{invokingChannelID}
	if id := p.jailChannelConfig.JailAnnounceChannelID(guildID); id != "" && id != invokingChannelID {
		out = append(out, id)
	}
	return out
}
