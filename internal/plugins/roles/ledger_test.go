package roles

import (
	"context"
	"testing"
	"time"

	"github.com/6586x57890143/merlin/internal/core"
)

// The ledger seam: every jail, re-sentence and release goes out on the bus
// as a core.EventModerationAction, and a release names who released.

type busRecorder struct{ events []core.Event }

func (r *busRecorder) record(bus *core.EventBus) {
	bus.Subscribe(core.EventModerationAction, "test", func(_ context.Context, ev core.Event) {
		r.events = append(r.events, ev)
	})
}

func (r *busRecorder) payloads() []core.ModerationActionPayload {
	var out []core.ModerationActionPayload
	for _, ev := range r.events {
		out = append(out, ev.Payload.(core.ModerationActionPayload))
	}
	return out
}

func TestJailAndReleasePublishToTheLedger(t *testing.T) {
	ops := newFakeOps()
	ops.setMember("7000001", "7000102", []string{"7000302"})
	ops.roles["7000001"] = []*frole{{ID: "7000302"}, {ID: "7000301"}}
	audit := newFakeAudit()
	p := newTestPlugin(ops, newFakeStore(), newFakeSettings(), audit, newFakePerms(), newFakeScheduler())
	bus := core.NewEventBus(testLogger())
	rec := &busRecorder{}
	rec.record(bus)
	p.bus = bus

	if _, err := p.applyJail(context.Background(), "7000001", "7000301", jailTarget{userID: "7000102", roles: []string{"7000302"}}, 2*time.Hour, "7000114", "spam"); err != nil {
		t.Fatalf("applyJail: %v", err)
	}
	jr, _, _ := p.store.GetJail(context.Background(), "7000001", "7000102")
	ops.setMember("7000001", "7000102", []string{"7000301"})
	if err := p.releaseJail(context.Background(), "7000001", "7000102", jr, "7000115"); err != nil {
		t.Fatalf("releaseJail: %v", err)
	}

	got := rec.payloads()
	if len(got) != 2 {
		t.Fatalf("published %d events, want jail + release: %+v", len(got), got)
	}
	jail, release := got[0], got[1]
	if jail.Kind != "jail" || jail.UserID != "7000102" || jail.ActorID != "7000114" || jail.Reason != "spam" ||
		jail.Duration != 2*time.Hour || jail.EndsAt == nil || jail.Source != "roles" {
		t.Errorf("jail payload = %+v", jail)
	}
	if release.Kind != "release" || release.ActorID != "7000115" || release.Source != "roles" {
		t.Errorf("release payload = %+v", release)
	}

	// The audit row names the releasing mod too. It used to say "system"
	// whoever ran /roles release.
	found := false
	for _, r := range audit.records {
		if r.action == "roles.release" {
			found = true
			if r.actorID != "7000115" {
				t.Errorf("release audited as %q, want the mod", r.actorID)
			}
		}
	}
	if !found {
		t.Error("release not audited")
	}
}

func TestAResentenceIsPublishedAsANoteNotASecondJail(t *testing.T) {
	ops := newFakeOps()
	ops.setMember("7000001", "7000102", []string{"7000301"})
	p := newTestPlugin(ops, newFakeStore(), newFakeSettings(), newFakeAudit(), newFakePerms(), newFakeScheduler())
	bus := core.NewEventBus(testLogger())
	rec := &busRecorder{}
	rec.record(bus)
	p.bus = bus
	_ = p.store.InsertJail(context.Background(), JailRecord{GuildID: "7000001", UserID: "7000102", JailRoleID: "7000301", JailedAt: p.now()})

	res := p.jailMany(context.Background(), "7000001", "7000301",
		[]jailTarget{{userID: "7000102", roles: []string{"7000301"}}}, 3*time.Hour, "7000114", "again")
	if len(res.redated) != 1 {
		t.Fatalf("res = %+v, want one redated", res)
	}
	got := rec.payloads()
	if len(got) != 1 || got[0].Kind != "note" || got[0].ActorID != "7000114" || got[0].Reason != "sentence moved to 3h: again" {
		t.Errorf("payloads = %+v", got)
	}
}

func TestPublishingWithNoBusIsANoOp(t *testing.T) {
	p := newTestPlugin(newFakeOps(), newFakeStore(), newFakeSettings(), newFakeAudit(), newFakePerms(), newFakeScheduler())
	p.bus = nil
	p.publishJailed(context.Background(), "7000001", "7000102", "m", "r", time.Hour, ptrTime(time.Now()))
	p.publishResentenced(context.Background(), "7000001", "7000102", "m", "", time.Hour)
	p.publishReleased(context.Background(), "7000001", "7000102", core.ActorSystem)
}
