package core

import (
	"context"
	"io"
	"log/slog"
	"testing"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestEventBusPublishSubscribe(t *testing.T) {
	bus := NewEventBus(testLogger())
	received := make(chan Event, 1)
	bus.Subscribe(EventReady, "test", func(ctx context.Context, ev Event) {
		received <- ev
	})

	bus.Publish(context.Background(), Event{Type: EventReady, GuildID: "g1"})

	select {
	case ev := <-received:
		if ev.GuildID != "g1" {
			t.Fatalf("expected GuildID g1, got %q", ev.GuildID)
		}
		if ev.At.IsZero() {
			t.Fatal("expected At to be set by Publish")
		}
	default:
		t.Fatal("handler was not called")
	}
}

func TestEventBusPanicIsolation(t *testing.T) {
	bus := NewEventBus(testLogger())
	called := false

	bus.Subscribe(EventReady, "panicky", func(ctx context.Context, ev Event) {
		panic("boom")
	})
	bus.Subscribe(EventReady, "well-behaved", func(ctx context.Context, ev Event) {
		called = true
	})

	// Must not panic, and the second subscriber must still run.
	bus.Publish(context.Background(), Event{Type: EventReady})

	if !called {
		t.Fatal("second subscriber should still be called after first panics")
	}
}

func TestEventBusNoSubscribers(t *testing.T) {
	bus := NewEventBus(testLogger())
	// Publishing with no subscribers must not panic or error.
	bus.Publish(context.Background(), Event{Type: EventConfigChanged})
}
