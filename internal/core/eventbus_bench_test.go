package core

import (
	"context"
	"io"
	"log/slog"
	"testing"
)

// Publish runs every subscriber on the publisher's goroutine, so its own
// cost is paid once per event by whatever published it.
func BenchmarkPublish(b *testing.B) {
	bus := NewEventBus(slog.New(slog.NewTextHandler(io.Discard, nil)))
	for _, name := range []string{"a", "b", "c"} {
		bus.Subscribe(EventConfigChanged, name, func(context.Context, Event) {})
	}
	ev := Event{Type: EventConfigChanged, GuildID: "1459404984943644747"}
	ctx := context.Background()
	b.ReportAllocs()
	for b.Loop() {
		bus.Publish(ctx, ev)
	}
}
