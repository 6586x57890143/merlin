package aimod

import (
	"testing"

	"github.com/bwmarrin/discordgo"
)

// HandleMessage is the gateway's way in: the config read, rung 1 and the
// skips, for a guild with aimod off, which is everything merlin pays per
// message before a model is involved.
func BenchmarkHandleMessageModeOff(b *testing.B) {
	store := newFakeStore()
	store.setConfig(Config{GuildID: "g", Mode: ModeOff})
	p := testPlugin(b, store, nil, &fakeOps{}, &fakeAudit{})
	m := &discordgo.Message{ID: "1", GuildID: "g", ChannelID: "c", Content: cleanMessages[1],
		Author: &discordgo.User{ID: "175928847299117063"}}
	b.ReportAllocs()
	for b.Loop() {
		p.HandleMessage(m)
	}
}
