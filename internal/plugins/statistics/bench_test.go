package statistics

import (
	"testing"
	"time"

	"github.com/disgoorg/disgo/discord"
	snow "github.com/disgoorg/snowflake/v2"

	"github.com/6586x57890143/merlin/internal/settings"
)

// HandleMessage runs for every member message on every server. The gate is
// the real settings store, and the channel name a lookup, as cmd/bot wires
// them, so what this measures is what production pays.
func BenchmarkHandleMessage(b *testing.B) {
	p := &Plugin{count: newCounter(), now: time.Now, gate: settings.New(nil, nil),
		channelName: func(_, _ snow.ID) string { return "general" }}
	g := snow.ID(1459404984943644747)
	m := discord.Message{GuildID: &g, ChannelID: 1549707140753793054, CreatedAt: time.Now(),
		Author: discord.User{ID: 175928847299117063, Username: "steve"}}
	b.ReportAllocs()
	for b.Loop() {
		p.HandleMessage(m)
	}
}

func BenchmarkHandleVoiceState(b *testing.B) {
	p := &Plugin{count: newCounter(), now: time.Now}
	ch := snow.ID(1)
	member := discord.Member{User: discord.User{ID: 175928847299117063, Username: "steve"}}
	join := discord.VoiceState{GuildID: 1459404984943644747, UserID: 175928847299117063, ChannelID: &ch}
	leave := discord.VoiceState{GuildID: 1459404984943644747, UserID: 175928847299117063}
	b.ReportAllocs()
	for b.Loop() {
		p.HandleVoiceState(join, leave, member)
		p.HandleVoiceState(leave, join, member)
	}
}
