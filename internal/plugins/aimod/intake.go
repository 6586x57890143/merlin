package aimod

import (
	"github.com/disgoorg/disgo/discord"
)

// message is what aimod reads of a gateway message: its IDs, who wrote it,
// and its text, including the text a forward carries in its snapshots and
// the message it replies to.
type message struct {
	ID, GuildID, ChannelID string
	// AuthorID is empty when Discord did not send the author, which an
	// edit's payload can omit.
	AuthorID string
	Bot      bool
	Webhook  bool
	Edited   bool
	RoleIDs  []string
	Content  string
	// Snapshots is the text of each message a forward carries.
	Snapshots []string
	// ReplyTo is the text of the message this one replies to, if any.
	ReplyTo string
}

// gatewayMessage is m as aimod reads it.
func gatewayMessage(m discord.Message) message {
	out := message{
		ID: m.ID.String(), ChannelID: m.ChannelID.String(),
		Bot: m.Author.Bot, Webhook: m.WebhookID != nil, Edited: m.EditedTimestamp != nil,
		Content: m.Content,
	}
	if m.GuildID != nil {
		out.GuildID = m.GuildID.String()
	}
	if m.Author.ID != 0 {
		out.AuthorID = m.Author.ID.String()
	}
	if m.Member != nil {
		out.RoleIDs = make([]string, len(m.Member.RoleIDs))
		for i, r := range m.Member.RoleIDs {
			out.RoleIDs[i] = r.String()
		}
	}
	for _, s := range m.MessageSnapshots {
		out.Snapshots = append(out.Snapshots, s.Message.Content)
	}
	if m.ReferencedMessage != nil {
		out.ReplyTo = m.ReferencedMessage.Content
	}
	return out
}

// HandleMessage is the gateway entry point, called from the MessageCreate
// listener in cmd/bot/main.go. It returns immediately: everything past the
// free rungs happens on the batch timer, so a busy server's messages never
// pile up goroutines waiting on a model.
func (p *Plugin) HandleMessage(m discord.Message) {
	if !p.scanning || m.GuildID == nil {
		return
	}
	// Most guilds run with aimod off, and every message they send lands
	// here. Answer that from the cached config before building a message
	// view, so it costs a map read. Off skips everything handle does,
	// including its debug line: a guild that scans nothing has no miss to
	// explain.
	if cs, ok := p.store.(*cachingStore); ok {
		if mode, fresh := cs.cachedMode(*m.GuildID); fresh && mode == ModeOff {
			return
		}
	}
	p.handle(gatewayMessage(m))
}

// HandleMessageEdit is the MessageUpdate entry point.
//
// Scanning edits is what closes the plugin's most obvious bypass: post
// something innocuous, let it be cleared, then edit it into a violation. The
// classification path is identical, so this is a guard plus a call.
//
// The guard matters more than it looks. MessageUpdate also fires when Discord
// resolves a link preview or pins a message, with the content untouched, and
// those are common enough that scanning them would raise traffic
// substantially for nothing. EditedTimestamp is nil on exactly those and set
// on a real edit.
//
// The payload is partial in a way MessageCreate's never is: the author can
// be missing. shouldSkip returns skipBot for that, so an edit whose author
// Discord did not send is dropped rather than misattributed, which is the
// right direction and is worth not rediscovering.
//
// Cost is bounded by dedupeCache: an edit that changes nothing this plugin
// cares about (whitespace, a typo fixed) hashes to a key that may already
// carry a verdict, and re-editing the same text repeatedly is free.
func (p *Plugin) HandleMessageEdit(m discord.Message) {
	if m.EditedTimestamp == nil {
		return
	}
	// Before the rescan, so a verdict still in flight on the old text finds
	// it has been superseded. See revision.go.
	p.noteRevision(m.ID.String(), m.Content)
	p.HandleMessage(m)
}
