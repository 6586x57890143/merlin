package roles

import (
	"context"
	"errors"
	"testing"
	"time"
)

// A rehearsing guild's sweep must leave its pending work exactly where it
// found it. Release and revoke both untrack their row as part of doing the
// work, so a dry-run that let them run would quietly forget the jails the
// real sweep still owes: the member would stay jailed forever with nothing
// tracking them, which is the precise failure the record-before-strip
// ordering in applyJail exists to prevent.
func TestSweepDryRunReleasesNothingAndKeepsTracking(t *testing.T) {
	ops := newFakeOps()
	ops.setMember("7000001", "7000102", []string{"7000301"})

	store := newFakeStore()
	due := fixedNow.Add(-time.Minute)
	if err := store.InsertJail(context.Background(), JailRecord{
		GuildID: "7000001", UserID: "7000102", SnapshotRoleIDs: []string{"7000302"},
		JailRoleID: "7000301", ReleaseAt: &due,
	}); err != nil {
		t.Fatalf("InsertJail: %v", err)
	}

	p := newTestPlugin(ops, store, newFakeSettings(), newFakeAudit(), newFakePerms(), newFakeScheduler())
	p.dryRun = func(string) bool { return true }

	if err := p.sweep(context.Background(), "7000001"); err != nil {
		t.Fatalf("sweep under dry-run should succeed silently: %v", err)
	}

	if _, ok, _ := store.GetJail(context.Background(), "7000001", "7000102"); !ok {
		t.Error("dry-run sweep untracked a due jail; the real sweep would never release that member")
	}
	member, err := ops.GuildMember("7000001", "7000102")
	if err != nil {
		t.Fatalf("GuildMember: %v", err)
	}
	if len(member.Roles) != 1 || member.Roles[0] != "7000301" {
		t.Errorf("dry-run sweep changed the member's roles to %v", member.Roles)
	}
}

// The jail snapshot is the only record of what roles a member held before
// being stripped, and release is the only thing that gives them back. A
// second concurrent jail must therefore never overwrite it: the loser would
// be writing the *stripped* role list (just the marker) over the real one,
// and releasing the member afterwards would restore nothing.
func TestConcurrentJailDoesNotOverwriteRoleSnapshot(t *testing.T) {
	ops := newFakeOps()
	ops.setMember("7000001", "7000102", []string{"7000302", "7000303"})
	store := newFakeStore()
	p := newTestPlugin(ops, store, newFakeSettings(), newFakeAudit(), newFakePerms(), newFakeScheduler())

	// First jail wins and records the member's real roles.
	if _, err := p.applyJail(context.Background(), "7000001", "7000301", jailTarget{userID: "7000102", roles: []string{"7000302", "7000303"}}, time.Hour, "7000112", "first"); err != nil {
		t.Fatalf("first jail: %v", err)
	}

	// Second jail arrives after the first stripped them, so all it can see
	// is the marker role.
	_, err := p.applyJail(context.Background(), "7000001", "7000301", jailTarget{userID: "7000102", roles: []string{"7000301"}}, time.Hour, "7000113", "second")
	if !errors.Is(err, ErrAlreadyJailed) {
		t.Fatalf("second jail returned %v, want ErrAlreadyJailed", err)
	}

	rec, ok, err := store.GetJail(context.Background(), "7000001", "7000102")
	if err != nil || !ok {
		t.Fatalf("GetJail: %v (found=%v)", err, ok)
	}
	if len(rec.SnapshotRoleIDs) != 2 || rec.SnapshotRoleIDs[0] != "7000302" || rec.SnapshotRoleIDs[1] != "7000303" {
		t.Errorf("snapshot is %v, want the pre-jail roles [role-a role-b]; the member's roles are unrecoverable", rec.SnapshotRoleIDs)
	}
	if rec.JailedBy != "7000112" {
		t.Errorf("JailedBy = %q, want the winning call's actor", rec.JailedBy)
	}
}

// Same reasoning as rotation's: a rehearsing guild is a deliberate operator
// state, and reporting it as a job failure would spend the sweep's
// consecutive-failure budget and alert #bird-status about nothing.
func TestSweepJobDryRunReportsSuccess(t *testing.T) {
	p := newTestPlugin(newFakeOps(), newFakeStore(), newFakeSettings(), newFakeAudit(), newFakePerms(), newFakeScheduler())
	p.dryRun = func(string) bool { return true }

	if err := p.makeSweepJob("7000001")(context.Background()); err != nil {
		t.Errorf("sweep job under dry-run returned %v, want nil", err)
	}
}
