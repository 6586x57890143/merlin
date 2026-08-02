package core

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// EventType names a class of internal event. Plugins never call each other
// directly (Design Principle 1); they only publish/subscribe here, so e.g.
// a future rotation plugin's channel.rotated event reaches factions without
// either package importing the other.
type EventType string

const (
	EventReady         EventType = "core.ready"
	EventConfigChanged EventType = "config.changed"

	// EventChannelRotated is published after a channel rotation completes
	// successfully. Payload is ChannelRotatedPayload. Anything hardcoded to
	// the old channel ID (external webhooks, other bots' configs) will keep
	// posting into what is now the hidden archive. This event exists so
	// in-process subscribers can react, and to document that this is a
	// known, accepted limitation of rotation (spec.MD §6).
	EventChannelRotated EventType = "channel.rotated"
)

// ChannelRotatedPayload is the Event.Payload for EventChannelRotated.
type ChannelRotatedPayload struct {
	OldChannelID string
	NewChannelID string
}

// Event is the envelope delivered to every subscriber of its Type. Payload's
// concrete type is documented per EventType by whichever plugin defines it.
type Event struct {
	Type    EventType
	GuildID string
	Payload any
	At      time.Time
}

// Handler processes one Event. Handlers run synchronously on the publishing
// goroutine and should not block for long.
type Handler func(ctx context.Context, ev Event)

type subscriber struct {
	plugin string
	fn     Handler
}

// EventBus is the internal pub/sub every plugin is wired to via Deps.
type EventBus struct {
	mu       sync.RWMutex
	handlers map[EventType][]subscriber
	log      *slog.Logger
}

func NewEventBus(log *slog.Logger) *EventBus {
	return &EventBus{handlers: make(map[EventType][]subscriber), log: log}
}

// Subscribe registers fn for every Event of type t. pluginName is used only
// for panic-isolation logging/attribution; it doesn't gate delivery.
func (b *EventBus) Subscribe(t EventType, pluginName string, fn Handler) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.handlers[t] = append(b.handlers[t], subscriber{plugin: pluginName, fn: fn})
}

// Publish dispatches ev to every current subscriber of ev.Type, each under
// its own recover() so one plugin's panic can never reach another plugin's
// handler or the publisher (spec.MD "panic isolation").
func (b *EventBus) Publish(ctx context.Context, ev Event) {
	ev.At = time.Now().UTC()
	b.mu.RLock()
	subs := append([]subscriber(nil), b.handlers[ev.Type]...)
	b.mu.RUnlock()

	for _, s := range subs {
		func(s subscriber) {
			defer func() {
				if r := recover(); r != nil {
					b.log.Error("event handler panicked",
						"plugin", s.plugin, "event", ev.Type, "panic", r)
				}
			}()
			s.fn(ctx, ev)
		}(s)
	}
}
