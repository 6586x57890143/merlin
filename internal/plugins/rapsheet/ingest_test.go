package rapsheet

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/6586x57890143/merlin/internal/core"
)

func publishAction(h *harness, guildID string, payload core.ModerationActionPayload) {
	h.bus.Publish(context.Background(), core.Event{Type: core.EventModerationAction, GuildID: guildID, Payload: payload})
}

func TestARolesJailArrivesAsAnEntry(t *testing.T) {
	h := newHarness()
	h.ops.addMember("u1")
	ends := testNow.Add(2 * time.Hour)
	publishAction(h, testGuild, core.ModerationActionPayload{
		UserID: "u1", Kind: "jail", ActorID: modID, Reason: "spamming", Duration: 2 * time.Hour, EndsAt: &ends, Source: "roles",
	})
	entries := h.store.all()
	if len(entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(entries))
	}
	e := entries[0]
	if e.Kind != KindJail || e.Source != SourceRoles || e.Category != CategoryServerRule || e.ActorID != modID ||
		e.Duration != 2*time.Hour || e.EndsAt == nil || e.Points != 25 {
		t.Errorf("entry = %+v", e)
	}
	// A mod's jail carries points; the same jail applied automatically
	// (aimod's sanction, the ladder) does not, because its offence already
	// did.
	publishAction(h, testGuild, core.ModerationActionPayload{
		UserID: "u2", Kind: "jail", ActorID: core.ActorSystem, Reason: "automatic", Duration: time.Hour, EndsAt: &ends, Source: "roles",
	})
	if e := h.store.all()[1]; e.Points != 0 {
		t.Errorf("an automatic jail scored %d points", e.Points)
	}
	if cf, ok, _ := h.store.CaseFile(context.Background(), testGuild, "u1"); !ok || cf.Username != "user-u1" {
		t.Errorf("case file should be opened from the fetched user, got %+v %v", cf, ok)
	}
}

func TestAnAIModRemovalArrivesOnceHoweverOftenItIsDelivered(t *testing.T) {
	h := newHarness()
	payload := core.ModerationActionPayload{
		UserID: "u1", Kind: "removal", Category: "hate_speech", ActorID: core.ActorSystem,
		Reason: "message removed automatically: slur", Source: "aimod", Ref: "77",
	}
	publishAction(h, testGuild, payload)
	publishAction(h, testGuild, payload)
	entries := h.store.all()
	if len(entries) != 1 {
		t.Fatalf("entries = %d, want 1 (same ref twice)", len(entries))
	}
	if entries[0].Kind != KindRemoval || entries[0].Category != CategoryHateSpeech || entries[0].Points != 50 || entries[0].Ref != "77" {
		t.Errorf("entry = %+v", entries[0])
	}
}

func TestAReversalVoidsTheEntryItNames(t *testing.T) {
	h := newHarness()
	publishAction(h, testGuild, core.ModerationActionPayload{
		UserID: "u1", Kind: "removal", Category: "spam", ActorID: core.ActorSystem, Source: "aimod", Ref: "5",
	})
	h.bus.Publish(context.Background(), core.Event{Type: core.EventModerationReversed, GuildID: testGuild,
		Payload: core.ModerationReversedPayload{Source: "aimod", Ref: "5", ActorID: modID, Reason: "undone by a moderator"}})
	e := h.store.all()[0]
	if !e.Voided() || e.VoidedBy != modID || e.VoidReason != "undone by a moderator" {
		t.Errorf("entry = %+v", e)
	}
	// A reversal for something never recorded, or already voided, is quiet.
	h.bus.Publish(context.Background(), core.Event{Type: core.EventModerationReversed, GuildID: testGuild,
		Payload: core.ModerationReversedPayload{Source: "aimod", Ref: "5", ActorID: modID}})
	h.bus.Publish(context.Background(), core.Event{Type: core.EventModerationReversed, GuildID: testGuild,
		Payload: core.ModerationReversedPayload{Source: "aimod", Ref: "nope", ActorID: modID}})
	if got := h.store.all()[0]; got.VoidedBy != modID {
		t.Errorf("a second reversal changed the void: %+v", got)
	}
}

func TestIngestionRespectsTheGuildToggle(t *testing.T) {
	h := newHarness()
	h.p.gate = fakeGate{disabled: map[string]bool{testGuild: true}}
	publishAction(h, testGuild, core.ModerationActionPayload{UserID: "u1", Kind: "jail", ActorID: modID, Source: "roles"})
	publishAction(h, "g2", core.ModerationActionPayload{UserID: "u1", Kind: "jail", ActorID: modID, Source: "roles"})
	entries := h.store.all()
	if len(entries) != 1 || entries[0].GuildID != "g2" {
		t.Errorf("a disabled guild's events were recorded: %+v", entries)
	}
}

func TestTheLaddersOwnJailIsNotRecordedTwice(t *testing.T) {
	h := newHarness()
	publishAction(h, testGuild, core.ModerationActionPayload{
		UserID: "u1", Kind: "jail", ActorID: core.ActorSystem, Reason: ladderReasonPrefix + "12: score 63", Source: "roles",
	})
	if n := len(h.store.all()); n != 0 {
		t.Errorf("the ladder's own jail came back as %d entries", n)
	}
}

func TestUnknownKindsAndSourcesAreDropped(t *testing.T) {
	h := newHarness()
	publishAction(h, testGuild, core.ModerationActionPayload{UserID: "u1", Kind: "smite", ActorID: modID, Source: "roles"})
	publishAction(h, testGuild, core.ModerationActionPayload{UserID: "u1", Kind: "jail", ActorID: modID, Source: "carrier pigeon"})
	publishAction(h, testGuild, core.ModerationActionPayload{UserID: "u1", Kind: "jail", ActorID: modID, Source: "command"})
	if n := len(h.store.all()); n != 0 {
		t.Errorf("%d entries from unknown publishers", n)
	}
	// And a payload of the wrong type, or a panic in the write, reaches the
	// publisher as nothing at all.
	h.bus.Publish(context.Background(), core.Event{Type: core.EventModerationAction, GuildID: testGuild, Payload: "garbage"})
	h.store.insertErr = errors.New("db down")
	publishAction(h, testGuild, core.ModerationActionPayload{UserID: "u1", Kind: "jail", ActorID: modID, Source: "roles"})
}

func TestTheSubscriptionIsMadeAtInit(t *testing.T) {
	h := newHarness()
	fresh := New(h.store, func(string) DiscordOps { return h.ops }, fakeModRoles{}, h.voice)
	router := core.NewCommandRouter(nil, nil, quietLog())
	bus := core.NewEventBus(quietLog())
	if err := fresh.Init(core.Deps{Bus: bus, Commands: router, Logger: quietLog(), Audit: h.audit}); err != nil {
		t.Fatal(err)
	}
	fresh.synchronous = true
	bus.Publish(context.Background(), core.Event{Type: core.EventModerationAction, GuildID: testGuild,
		Payload: core.ModerationActionPayload{UserID: "u1", Kind: "jail", ActorID: modID, Source: "roles"}})
	if n := len(h.store.all()); n != 1 {
		t.Errorf("Init did not subscribe: %d entries", n)
	}
}
