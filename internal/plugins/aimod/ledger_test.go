package aimod

import (
	"context"
	"strconv"
	"testing"

	"github.com/bwmarrin/discordgo"

	"github.com/6586x57890143/merlin/internal/core"
)

// The ledger seam: a removal goes out on the bus with the incident id as
// its ref, and an undo takes it back by the same ref, along with the
// sanction row that followed it.

type busRecorder struct{ events []core.Event }

func (r *busRecorder) record(bus *core.EventBus) {
	for _, t := range []core.EventType{core.EventModerationAction, core.EventModerationReversed} {
		bus.Subscribe(t, "test", func(_ context.Context, ev core.Event) { r.events = append(r.events, ev) })
	}
}

func TestARemovalIsPublishedWithItsIncidentAsTheRef(t *testing.T) {
	store := newFakeStore()
	ops := newFakeOps()
	p := testPlugin(t, store, &fakeClassifier{}, ops, &fakeAudit{})
	bus := core.NewEventBus(testLogger())
	rec := &busRecorder{}
	rec.record(bus)
	p.bus = bus

	p.enforce(context.Background(), enforcingConfig(),
		candidate{MessageID: "m1", ChannelID: "c1", AuthorID: "u1", Content: "text"},
		BucketThreats, ActionRemove, confirmed("a threat"))

	inc, err := store.IncidentByMessage(context.Background(), "g1", "m1")
	if err != nil {
		t.Fatalf("IncidentByMessage: %v", err)
	}
	var actions []core.ModerationActionPayload
	for _, ev := range rec.events {
		if ev.Type == core.EventModerationAction {
			actions = append(actions, ev.Payload.(core.ModerationActionPayload))
		}
	}
	if len(actions) != 1 {
		t.Fatalf("published %d actions, want 1: %+v", len(actions), actions)
	}
	got := actions[0]
	if got.Kind != "removal" || got.UserID != "u1" || got.Category != "threats" || got.ActorID != core.ActorSystem ||
		got.Source != "aimod" || got.Ref != strconv.FormatInt(inc.ID, 10) || got.Reason != "message removed automatically: a threat" {
		t.Errorf("payload = %+v", got)
	}
}

func TestAFlagPublishesNothing(t *testing.T) {
	store := newFakeStore()
	p := testPlugin(t, store, &fakeClassifier{}, newFakeOps(), &fakeAudit{})
	bus := core.NewEventBus(testLogger())
	rec := &busRecorder{}
	rec.record(bus)
	p.bus = bus
	cfg := enforcingConfig()
	cfg.Mode = ModeFlag
	p.enforce(context.Background(), cfg,
		candidate{MessageID: "m1", ChannelID: "c1", AuthorID: "u1", Content: "text"},
		BucketThreats, ActionRemove, confirmed("r"))
	if len(rec.events) != 0 {
		t.Errorf("a flag reached the ledger: %+v", rec.events)
	}
}

func TestUndoReversesTheSanctionRowTooAndPublishes(t *testing.T) {
	ctx := context.Background()
	store := newFakeStore()
	store.setConfig(enforcingConfig())
	ops := newFakeOps()
	ops.members["u1"] = &discordgo.Member{User: &discordgo.User{ID: "u1", Username: "someguy"}}
	p := testPlugin(t, store, &fakeClassifier{}, ops, &fakeAudit{})
	bus := core.NewEventBus(testLogger())
	rec := &busRecorder{}
	rec.record(bus)
	p.bus = bus

	id, err := store.RecordIncident(ctx, Incident{
		GuildID: "g1", ChannelID: "c1", MessageID: "m1", AuthorID: "u1",
		Bucket: BucketThreats, Action: ActionRemove, Content: "the original words", CreatedAt: testNow,
	})
	if err != nil {
		t.Fatalf("RecordIncident: %v", err)
	}
	if _, err := store.RecordIncident(ctx, Incident{
		GuildID: "g1", ChannelID: "c1", MessageID: "m1:sanction", AuthorID: "u1",
		Bucket: BucketThreats, Action: ActionSanction, Confidence: 1, CreatedAt: testNow,
	}); err != nil {
		t.Fatalf("RecordIncident: %v", err)
	}

	p.handleUndo(ctx, testSession(t), interaction("g1", "", "undo", strOpt("message_id", "m1")))

	sanc, err := store.IncidentByMessage(ctx, "g1", "m1:sanction")
	if err != nil {
		t.Fatalf("IncidentByMessage: %v", err)
	}
	if !sanc.Undone {
		t.Error("the sanction row was left counting as a prior after the offence was undone")
	}
	n, _ := store.CountSanctions(ctx, "g1", "u1", testNow.Add(-repeatWindow))
	if n != 0 {
		t.Errorf("CountSanctions = %d after undo, want 0", n)
	}

	var reversed []core.ModerationReversedPayload
	for _, ev := range rec.events {
		if ev.Type == core.EventModerationReversed {
			reversed = append(reversed, ev.Payload.(core.ModerationReversedPayload))
		}
	}
	if len(reversed) != 1 || reversed[0].Ref != strconv.FormatInt(id, 10) || reversed[0].Source != "aimod" || reversed[0].ActorID != "actor" {
		t.Errorf("reversed = %+v", reversed)
	}
}
