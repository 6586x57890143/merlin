package main

import (
	"bytes"
	"io"
	"log/slog"
	"testing"

	"github.com/bwmarrin/discordgo"
	"github.com/disgoorg/disgo/events"
	"github.com/disgoorg/disgo/gateway"
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
