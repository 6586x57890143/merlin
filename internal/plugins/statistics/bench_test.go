package statistics

import (
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
)

// HandleMessage runs for every member message on every server.
func BenchmarkHandleMessage(b *testing.B) {
	p := &Plugin{count: newCounter(), now: time.Now}
	m := &discordgo.Message{GuildID: "1459404984943644747", ChannelID: "1549707140753793054", Timestamp: time.Now(),
		Author: &discordgo.User{ID: "175928847299117063", Username: "steve"}}
	b.ReportAllocs()
	for b.Loop() {
		p.HandleMessage(m)
	}
}

func BenchmarkHandleVoiceState(b *testing.B) {
	p := &Plugin{count: newCounter(), now: time.Now}
	join := &discordgo.VoiceStateUpdate{VoiceState: &discordgo.VoiceState{GuildID: "1459404984943644747", UserID: "175928847299117063", ChannelID: "1"}}
	leave := &discordgo.VoiceStateUpdate{VoiceState: &discordgo.VoiceState{GuildID: "1459404984943644747", UserID: "175928847299117063"}}
	b.ReportAllocs()
	for b.Loop() {
		p.HandleVoiceState(join)
		p.HandleVoiceState(leave)
	}
}
