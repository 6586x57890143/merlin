package aimod

import (
	"testing"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/snowflake/v2"
)

// HandleMessage is the gateway's way in: the config read, rung 1 and the
// skips, for a guild with aimod off, which is everything merlin pays per
// message before a model is involved.
func BenchmarkHandleMessageModeOff(b *testing.B) {
	store := newFakeStore()
	store.setConfig(Config{GuildID: "1", Mode: ModeOff})
	p := testPlugin(b, store, nil, &fakeOps{}, &fakeAudit{})
	gid := snowflake.ID(1)
	m := discord.Message{ID: 1, GuildID: &gid, ChannelID: 2, Content: cleanMessages[1],
		Author: discord.User{ID: 175928847299117063}}
	b.ReportAllocs()
	for b.Loop() {
		p.HandleMessage(m)
	}
}
