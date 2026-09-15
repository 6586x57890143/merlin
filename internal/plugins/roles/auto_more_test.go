package roles

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/6586x57890143/merlin/internal/core"
)

func autoPlugin(ops *fakeOps, store *fakeStore, perms *fakePerms) (*Plugin, *fakeTimers, *time.Time) {
	p := newTestPlugin(ops, store, newFakeSettings(), newFakeAudit(), perms, newFakeScheduler())
	ft := &fakeTimers{}
	p.afterFunc = ft.afterFunc
	now := fixedNow
	p.now = func() time.Time { return now }
	return p, ft, &now
}

// TestJailAutomaticIsAnOrdinaryJail: the automated entry point runs the
// same applyJail as the command, so the record carries ActorSystem, the
// roles are stripped, and a release timer is armed. Nothing shaped like a
// jail but tracked differently.
func TestJailAutomaticIsAnOrdinaryJail(t *testing.T) {
	ops := newFakeOps()
	ops.setMember("g1", "u1", []string{"role-a"})
	ops.roles["g1"] = []*discordgo.Role{{ID: "role-a"}, {ID: "jail-role", Name: jailRoleName}}
	store := newFakeStore()
	p, ft, _ := autoPlugin(ops, store, newFakePerms())

	if err := p.JailAutomatic(context.Background(), "g1", "u1", time.Hour, "spam", false); err != nil {
		t.Fatalf("JailAutomatic: %v", err)
	}
	rec, ok, _ := store.GetJail(context.Background(), "g1", "u1")
	if !ok || rec.JailedBy != core.ActorSystem || rec.Reason != "spam" {
		t.Fatalf("expected a system-authored jail record, got %+v ok=%v", rec, ok)
	}
	if rec.ReleaseAt == nil || !rec.ReleaseAt.Equal(fixedNow.Add(time.Hour)) {
		t.Fatalf("expected release an hour out, got %v", rec.ReleaseAt)
	}
	m, _ := ops.GuildMember("g1", "u1")
	if len(m.Roles) != 1 || m.Roles[0] != "jail-role" {
		t.Fatalf("expected the member stripped to the marker, got %v", m.Roles)
	}
	if len(ft.armed) != 1 {
		t.Fatalf("expected one release timer, got %d", len(ft.armed))
	}
}

// TestJailAutomaticRefusals: the four ways in that create nothing. Each
// leaves no record, no role edit and no timer.
func TestJailAutomaticRefusals(t *testing.T) {
	for _, tc := range []struct {
		name      string
		duration  time.Duration
		consented bool
		setup     func(ops *fakeOps, perms *fakePerms)
	}{
		{"non-positive duration", 0, false, func(*fakeOps, *fakePerms) {}},
		{"bootstrap admin, even consenting", time.Hour, true, func(_ *fakeOps, perms *fakePerms) { perms.bootstrapID = "u1" }},
		{"member cannot be fetched", time.Hour, false, func(ops *fakeOps, _ *fakePerms) { ops.memberFetchErr = transientErr() }},
		{"admin-equivalent target", time.Hour, false, func(_ *fakeOps, perms *fakePerms) { perms.protected["u1"] = true }},
		{"guild state unresolvable", time.Hour, false, func(_ *fakeOps, perms *fakePerms) { perms.moderateErr = errors.New("no guild state") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ops := newFakeOps()
			ops.setMember("g1", "u1", []string{"role-a"})
			perms := newFakePerms()
			tc.setup(ops, perms)
			store := newFakeStore()
			p, ft, _ := autoPlugin(ops, store, perms)

			if err := p.JailAutomatic(context.Background(), "g1", "u1", tc.duration, "", tc.consented); err == nil {
				t.Fatal("expected a refusal")
			}
			if _, ok, _ := store.GetJail(context.Background(), "g1", "u1"); ok {
				t.Fatal("a refusal must record nothing")
			}
			if len(ops.memberEditCalls["u1"]) != 0 || len(ft.armed) != 0 {
				t.Fatal("a refusal must strip nobody and arm nothing")
			}
			if len(ops.roles["g1"]) != 0 {
				t.Fatal("a refusal must not create the jail role")
			}
		})
	}
}

// TestJailAutomaticConsentWaivesOnlyTheRankCheck: a member on the opt-in
// list can be jailed even when CanModerate would refuse them. The flag is
// consent to be moderated, nothing more, which the bootstrap case above
// pins from the other side.
func TestJailAutomaticConsentWaivesOnlyTheRankCheck(t *testing.T) {
	ops := newFakeOps()
	ops.setMember("g1", "u1", []string{"role-a"})
	perms := newFakePerms()
	perms.protected["u1"] = true
	store := newFakeStore()
	p, _, _ := autoPlugin(ops, store, perms)

	if err := p.JailAutomatic(context.Background(), "g1", "u1", time.Hour, "", true); err != nil {
		t.Fatalf("a consenting admin should be jailable: %v", err)
	}
	if _, ok, _ := store.GetJail(context.Background(), "g1", "u1"); !ok {
		t.Fatal("expected the jail recorded")
	}
}

// TestJailAutomaticExtendsOnlyLater: a second offence moves the release
// later and re-arms the timer, never earlier. Shortening would let a member
// cut their own sentence by offending again. Only release_at moves; the
// snapshot stays the pre-jail one.
func TestJailAutomaticExtendsOnlyLater(t *testing.T) {
	ops := newFakeOps()
	ops.setMember("g1", "u1", []string{"jail-role"})
	store := newFakeStore()
	end := fixedNow.Add(2 * time.Hour)
	_ = store.InsertJail(context.Background(), JailRecord{
		GuildID: "g1", UserID: "u1", JailRoleID: "jail-role", SnapshotRoleIDs: []string{"role-a"}, ReleaseAt: &end,
	})
	p, ft, _ := autoPlugin(ops, store, newFakePerms())

	// Shorter than what is left: untouched.
	if err := p.JailAutomatic(context.Background(), "g1", "u1", time.Hour, "", false); err != nil {
		t.Fatalf("JailAutomatic: %v", err)
	}
	rec, _, _ := store.GetJail(context.Background(), "g1", "u1")
	if !rec.ReleaseAt.Equal(end) {
		t.Fatalf("a shorter sentence must not shorten the jail, got %v", rec.ReleaseAt)
	}
	if len(ft.armed) != 0 {
		t.Fatal("nothing changed, so nothing should be re-armed")
	}

	// Longer: extended and re-armed, snapshot intact.
	if err := p.JailAutomatic(context.Background(), "g1", "u1", 3*time.Hour, "", false); err != nil {
		t.Fatalf("JailAutomatic: %v", err)
	}
	rec, _, _ = store.GetJail(context.Background(), "g1", "u1")
	if !rec.ReleaseAt.Equal(fixedNow.Add(3 * time.Hour)) {
		t.Fatalf("expected the jail extended to 3h, got %v", rec.ReleaseAt)
	}
	if len(rec.SnapshotRoleIDs) != 1 || rec.SnapshotRoleIDs[0] != "role-a" {
		t.Fatalf("extension must not touch the snapshot, got %v", rec.SnapshotRoleIDs)
	}
	if len(ft.armed) != 1 || ft.armed[0].delay != 3*time.Hour {
		t.Fatalf("expected a timer re-armed 3h out, got %+v", ft.armed)
	}
	if len(ops.memberEditCalls["u1"]) != 0 {
		t.Fatal("an extension re-strips nobody")
	}
}

// TestJailAutomaticLeavesAnIndefiniteJailAlone: there is nothing an
// automatic escalation can add to a sentence with no end.
func TestJailAutomaticLeavesAnIndefiniteJailAlone(t *testing.T) {
	ops := newFakeOps()
	ops.setMember("g1", "u1", []string{"jail-role"})
	store := newFakeStore()
	_ = store.InsertJail(context.Background(), JailRecord{GuildID: "g1", UserID: "u1", JailRoleID: "jail-role"})
	p, ft, _ := autoPlugin(ops, store, newFakePerms())

	if err := p.JailAutomatic(context.Background(), "g1", "u1", time.Hour, "", false); err != nil {
		t.Fatalf("JailAutomatic: %v", err)
	}
	rec, _, _ := store.GetJail(context.Background(), "g1", "u1")
	if rec.ReleaseAt != nil || len(ft.armed) != 0 {
		t.Fatalf("an indefinite jail must stay indefinite and unarmed, got %v / %d timers", rec.ReleaseAt, len(ft.armed))
	}
}

// TestJailAutomaticReportsAFailedExtension: when the sentence cannot be
// moved, the caller hears about it rather than believing the escalation
// landed.
func TestJailAutomaticReportsAFailedExtension(t *testing.T) {
	ops := newFakeOps()
	ops.setMember("g1", "u1", []string{"jail-role"})
	store := newFakeStore()
	end := fixedNow.Add(time.Minute)
	_ = store.InsertJail(context.Background(), JailRecord{GuildID: "g1", UserID: "u1", JailRoleID: "jail-role", ReleaseAt: &end})
	store.setJailReleaseErr = errors.New("db down")
	p, ft, _ := autoPlugin(ops, store, newFakePerms())

	if err := p.JailAutomatic(context.Background(), "g1", "u1", time.Hour, "", false); err == nil {
		t.Fatal("expected the failed extension reported")
	}
	if len(ft.armed) != 0 {
		t.Fatal("a timer must not be armed for a date that was never written")
	}
}
