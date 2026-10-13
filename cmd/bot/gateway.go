package main

import (
	"encoding/json"
	"io"
	"log/slog"

	"github.com/disgoorg/disgo/events"
	"github.com/disgoorg/disgo/gateway"
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
