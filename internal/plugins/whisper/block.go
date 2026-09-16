package whisper

import (
	"time"
)

// A run of whispers from one member with nothing between them is one
// message, not a stack of them: the next whisper is posted as the whole run
// so far plus the new line, with the marker under it, and the message it
// grew out of is then deleted. Editing the previous one to drop its marker
// was the first version of this, and it stamped "(edited)" on every line
// but the last, which read worse than the stutter it was fixing. A fresh
// post carries no such stamp.
//
// Post first, delete second. A failed delete leaves the old copy standing
// under the new one, which is a duplicate line; the other order could
// leave a run with no message at all.
//
// "Directly under it" is read off the channel, never assumed: the newest
// message there has to be the run this plugin last posted. Somebody talking
// in between, a moderator having deleted the run, or more than blockGap
// having passed (Discord draws a new header then anyway), and the new
// whisper stands alone. That read is also why a deletion needs no handler:
// a run that is gone is simply not the newest message any more.

// blockGap is Discord's own grouping window for consecutive messages.
const blockGap = 7 * time.Minute

// maxRunLen is Discord's content limit. A run that would grow past it with
// the marker on starts over as a new message instead.
const maxRunLen = 2000

type whisperMsg struct {
	id, hookID, token, text string // text is the run so far, marker excluded
	at                      time.Time
}

type block struct {
	userID string
	msg    whisperMsg
}

// continues reports the run the next whisper from userID would grow, or
// nil if it stands alone.
func (p *Plugin) continues(ops DiscordOps, channelID, userID, text string, now time.Time) *whisperMsg {
	p.blockMu.Lock()
	b := p.blocks[channelID]
	p.blockMu.Unlock()
	if b == nil || b.userID != userID || now.Sub(b.msg.at) > blockGap {
		return nil
	}
	if len(b.msg.text)+1+len(text)+len(marker(userID)) > maxRunLen {
		return nil
	}
	latest, err := ops.ChannelMessages(channelID, 1, "", "", "")
	if err != nil || len(latest) == 0 || latest[0].ID != b.msg.id {
		return nil
	}
	return &b.msg
}

// remember records the message now at the end of the channel's run.
func (p *Plugin) remember(channelID, userID string, msg whisperMsg) {
	p.blockMu.Lock()
	defer p.blockMu.Unlock()
	p.blocks[channelID] = &block{userID: userID, msg: msg}
}
