package main

import (
	"encoding/json"
	"io"
	"log/slog"

	"github.com/bwmarrin/discordgo"
	"github.com/disgoorg/disgo/events"
	"github.com/disgoorg/disgo/gateway"
	"github.com/disgoorg/snowflake/v2"
)

// legacy routes the gateway events whose handlers still take discordgo's
// types. Each is Discord's own JSON decoded into the struct discordgo
// decoded it into, so those handlers see exactly what they saw when
// discordgo held the connection. One raw listener reads each payload once
// and runs its handlers in order.
type legacy struct {
	log      *slog.Logger
	handlers map[gateway.EventType][]func([]byte)
}

func newLegacy(log *slog.Logger) *legacy {
	return &legacy{log: log, handlers: map[gateway.EventType][]func([]byte){}}
}

// on registers f for event t, decoded into T.
func on[T any](l *legacy, t gateway.EventType, f func(*T)) {
	l.handlers[t] = append(l.handlers[t], func(raw []byte) {
		var v T
		if err := json.Unmarshal(raw, &v); err != nil {
			l.log.Error("gateway event not decoded", "event", t, "err", err)
			return
		}
		f(&v)
	})
}

func (l *legacy) OnRaw(e *events.Raw) {
	hs := l.handlers[e.EventType]
	if len(hs) == 0 {
		return
	}
	raw, err := io.ReadAll(e.Payload)
	if err != nil {
		l.log.Error("gateway event not read", "event", e.EventType, "err", err)
		return
	}
	for _, h := range hs {
		h(raw)
	}
}

// voiceUpdate is a voice state change in discordgo's shape, with the state
// before it, which only the gateway cache knows and which tells a move from
// a mute.
func voiceUpdate(e *events.GuildVoiceStateUpdate) *discordgo.VoiceStateUpdate {
	vs := e.VoiceState
	out := &discordgo.VoiceStateUpdate{VoiceState: &discordgo.VoiceState{
		GuildID: vs.GuildID.String(), UserID: vs.UserID.String(), ChannelID: idOf(vs.ChannelID),
	}}
	if e.OldVoiceState.UserID != 0 {
		out.BeforeUpdate = &discordgo.VoiceState{
			GuildID: vs.GuildID.String(), UserID: vs.UserID.String(), ChannelID: idOf(e.OldVoiceState.ChannelID),
		}
	}
	if u := e.Member.User; u.ID != 0 {
		out.Member = &discordgo.Member{
			GuildID: vs.GuildID.String(), Nick: str(e.Member.Nick),
			User: &discordgo.User{ID: u.ID.String(), Username: u.Username, GlobalName: str(u.GlobalName), Avatar: str(u.Avatar), Bot: u.Bot},
		}
	}
	return out
}

func idOf(id *snowflake.ID) string {
	if id == nil {
		return ""
	}
	return id.String()
}

func str(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
