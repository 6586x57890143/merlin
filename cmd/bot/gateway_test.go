package main

import (
	"bytes"
	"io"
	"log/slog"
	"testing"

	"github.com/bwmarrin/discordgo"
	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/events"
	"github.com/disgoorg/disgo/gateway"
	"github.com/disgoorg/snowflake/v2"
)

// Every handler of an event sees the same payload, decoded as discordgo
// decoded it; other events reach none of them.
func TestLegacyDecodesEachPayloadForEveryHandler(t *testing.T) {
	l := newLegacy(slog.New(slog.NewTextHandler(io.Discard, nil)))
	var got []string
	for range 2 {
		on(l, gateway.EventTypeMessageCreate, func(mc *discordgo.MessageCreate) {
			got = append(got, mc.GuildID+"/"+mc.Author.ID+"/"+mc.Content)
		})
	}
	on(l, gateway.EventTypeGuildDelete, func(*discordgo.GuildDelete) { t.Error("a guild delete handler ran for a message") })
	l.OnRaw(&events.Raw{EventRaw: gateway.EventRaw{EventType: gateway.EventTypeMessageCreate, Payload: bytes.NewReader(
		[]byte(`{"id":"1","channel_id":"2","guild_id":"3","author":{"id":"4","username":"u"},"content":"hi"}`))}})
	l.OnRaw(&events.Raw{EventRaw: gateway.EventRaw{EventType: gateway.EventTypeMessageCreate, Payload: bytes.NewReader([]byte(`not json`))}})
	if len(got) != 2 || got[0] != "3/4/hi" || got[1] != got[0] {
		t.Fatalf("handlers saw %v", got)
	}
}

// A mute in the same channel carries the channel it was in before, which is
// how statistics tells it from a move; a join carries none.
func TestVoiceUpdateCarriesTheStateBefore(t *testing.T) {
	ch, nick := snowflake.ID(7), "nick"
	e := &events.GuildVoiceStateUpdate{
		GenericGuildVoiceState: &events.GenericGuildVoiceState{
			VoiceState: discord.VoiceState{GuildID: 3, UserID: 4, ChannelID: &ch, SelfMute: true},
			Member:     discord.Member{Nick: &nick, User: discord.User{ID: 4, Username: "u", Bot: true}},
		},
		OldVoiceState: discord.VoiceState{GuildID: 3, UserID: 4, ChannelID: &ch},
	}
	vs := voiceUpdate(e)
	if vs.GuildID != "3" || vs.UserID != "4" || vs.ChannelID != "7" || vs.BeforeUpdate == nil || vs.BeforeUpdate.ChannelID != "7" {
		t.Fatalf("update %+v before %+v", vs.VoiceState, vs.BeforeUpdate)
	}
	if vs.Member == nil || vs.Member.Nick != "nick" || !vs.Member.User.Bot {
		t.Fatalf("member %+v", vs.Member)
	}
	e.OldVoiceState = discord.VoiceState{}
	if voiceUpdate(e).BeforeUpdate != nil {
		t.Fatal("a join carried a state before it")
	}
}
