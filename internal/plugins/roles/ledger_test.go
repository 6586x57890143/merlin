package roles

import (
	"context"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"

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
	ops.setMember("g1", "u1", []string{"role-a"})
	ops.roles["g1"] = []*discordgo.Role{{ID: "role-a"}, {ID: "jail-role"}}
	audit := newFakeAudit()
	p := newTestPlugin(ops, newFakeStore(), newFakeSettings(), audit, newFakePerms(), newFakeScheduler())
	bus := core.NewEventBus(testLogger())
	rec := &busRecorder{}
	rec.record(bus)
	p.bus = bus

	if _, err := p.applyJail(context.Background(), "g1", "jail-role", jailTarget{userID: "u1", roles: []string{"role-a"}}, 2*time.Hour, "mod-1", "spam"); err != nil {
		t.Fatalf("applyJail: %v", err)
	}
	jr, _, _ := p.store.GetJail(context.Background(), "g1", "u1")
	ops.setMember("g1", "u1", []string{"jail-role"})
	if err := p.releaseJail(context.Background(), "g1", "u1", jr, "mod-2"); err != nil {
		t.Fatalf("releaseJail: %v", err)
	}

	got := rec.payloads()
	if len(got) != 2 {
		t.Fatalf("published %d events, want jail + release: %+v", len(got), got)
	}
	jail, release := got[0], got[1]
	if jail.Kind != "jail" || jail.UserID != "u1" || jail.ActorID != "mod-1" || jail.Reason != "spam" ||
		jail.Duration != 2*time.Hour || jail.EndsAt == nil || jail.Source != "roles" {
		t.Errorf("jail payload = %+v", jail)
	}
	if release.Kind != "release" || release.ActorID != "mod-2" || release.Source != "roles" {
		t.Errorf("release payload = %+v", release)
	}

	// The audit row names the releasing mod too. It used to say "system"
	// whoever ran /roles release.
	found := false
	for _, r := range audit.records {
		if r.action == "roles.release" {
			found = true
			if r.actorID != "mod-2" {
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
	ops.setMember("g1", "u1", []string{"jail-role"})
	p := newTestPlugin(ops, newFakeStore(), newFakeSettings(), newFakeAudit(), newFakePerms(), newFakeScheduler())
	bus := core.NewEventBus(testLogger())
	rec := &busRecorder{}
	rec.record(bus)
	p.bus = bus
	_ = p.store.InsertJail(context.Background(), JailRecord{GuildID: "g1", UserID: "u1", JailRoleID: "jail-role", JailedAt: p.now()})

	res := p.jailMany(context.Background(), "g1", "jail-role",
		[]jailTarget{{userID: "u1", roles: []string{"jail-role"}}}, 3*time.Hour, "mod-1", "again")
	if len(res.redated) != 1 {
		t.Fatalf("res = %+v, want one redated", res)
	}
	got := rec.payloads()
	if len(got) != 1 || got[0].Kind != "note" || got[0].ActorID != "mod-1" || got[0].Reason != "sentence moved to 3h: again" {
		t.Errorf("payloads = %+v", got)
	}
}

func TestPublishingWithNoBusIsANoOp(t *testing.T) {
	p := newTestPlugin(newFakeOps(), newFakeStore(), newFakeSettings(), newFakeAudit(), newFakePerms(), newFakeScheduler())
	p.bus = nil
	p.publishJailed(context.Background(), "g1", "u1", "m", "r", time.Hour, time.Now())
	p.publishResentenced(context.Background(), "g1", "u1", "m", "", time.Hour)
	p.publishReleased(context.Background(), "g1", "u1", core.ActorSystem)
}
