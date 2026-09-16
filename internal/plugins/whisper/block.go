package whisper

import (
	"slices"
	"time"
)

// A run of whispers from one member with nothing between them renders as
// one message in Discord, and a marker under every line of it reads as a
// bot with a stutter. So only the newest whisper in a run wears the marker:
// when the next one from the same member lands directly under it, the one
// before is edited to drop its line.
//
// Post first, strip second. A failed strip leaves two markers, which is the
// old behaviour; the other order could leave a whisper wearing none, and
// the marker is the disclosure that somebody is talking through the bot.
//
// "Directly under it" is read off the channel, never assumed: the newest
// message there has to be the whisper this plugin last posted. Somebody
// talking in between, the whisper having been deleted, or more than
// blockGap having passed (Discord draws a new header then anyway), and the
// new one stands alone with its own marker. A deleted tail hands the marker
// back to the whisper before it (HandleMessageDelete), or a run whose last
// line was removed would be left with no marker at all.

// blockGap is Discord's own grouping window for consecutive messages.
const blockGap = 7 * time.Minute

type whisperMsg struct {
	id, hookID, token, text string
	at                      time.Time
}

type block struct {
	guildID, userID, username string
	msgs                      []whisperMsg // oldest first; only the last wears the marker
}

// continues reports the whisper the next one from userID would sit
// directly under, or nil if it stands alone.
func (p *Plugin) continues(ops DiscordOps, channelID, userID string, now time.Time) *whisperMsg {
	p.blockMu.Lock()
	b := p.blocks[channelID]
	p.blockMu.Unlock()
	if b == nil || b.userID != userID || len(b.msgs) == 0 {
		return nil
	}
	tail := b.msgs[len(b.msgs)-1]
	if now.Sub(tail.at) > blockGap {
		return nil
	}
	latest, err := ops.ChannelMessages(channelID, 1, "", "", "")
	if err != nil || len(latest) == 0 || latest[0].ID != tail.id {
		return nil
	}
	return &tail
}

// extend records a posted whisper, joining the channel's run when it
// continued one and starting a new run otherwise.
func (p *Plugin) extend(channelID, guildID, userID, username string, msg whisperMsg, joined bool) {
	p.blockMu.Lock()
	defer p.blockMu.Unlock()
	b := p.blocks[channelID]
	if !joined || b == nil || b.userID != userID {
		b = &block{guildID: guildID, userID: userID}
		p.blocks[channelID] = b
	}
	b.username = username
	b.msgs = append(b.msgs, msg)
}

// HandleMessageDelete forgets a deleted whisper and, if it was the one
// wearing the marker, puts the marker back on the whisper now at the end
// of the run.
func (p *Plugin) HandleMessageDelete(channelID, messageID string) {
	p.blockMu.Lock()
	b := p.blocks[channelID]
	if b == nil {
		p.blockMu.Unlock()
		return
	}
	i := slices.IndexFunc(b.msgs, func(m whisperMsg) bool { return m.id == messageID })
	if i < 0 {
		p.blockMu.Unlock()
		return
	}
	wasTail := i == len(b.msgs)-1
	b.msgs = slices.Delete(b.msgs, i, i+1)
	if len(b.msgs) == 0 {
		delete(p.blocks, channelID)
		p.blockMu.Unlock()
		return
	}
	tail := b.msgs[len(b.msgs)-1]
	guildID, username := b.guildID, b.username
	p.blockMu.Unlock()
	if !wasTail {
		return
	}
	if err := p.ops(guildID).WhisperEdit(tail.hookID, tail.token, tail.id, tail.text+marker(username)); err != nil {
		p.log.Warn("whisper: could not put the marker back after a deletion", "guild", guildID, "channel", channelID, "message", tail.id, "err", err)
	}
}
