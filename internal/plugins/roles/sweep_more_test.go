package roles

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/6586x57890143/merlin/internal/discordguard"
)

// TestSweepCarriesOnPastOneFailingRowAndReportsIt: one member whose fetch
// fails must not block the release of everyone else due in the same sweep,
// and the sweep still reports the failure so the Scheduler backs off and
// retries rather than reading a half-done pass as success.
func TestSweepCarriesOnPastOneFailingRowAndReportsIt(t *testing.T) {
	ops := newFakeOps()
	ops.setMember("g1", "bad", []string{"jail-role"})
	ops.setMember("g1", "good", []string{"jail-role"})
	ops.memberFetchErrFor = map[string]error{"bad": transientErr()}
	store := newFakeStore()
	due := fixedNow.Add(-time.Minute)
	for _, u := range []string{"bad", "good"} {
		_ = store.InsertJail(context.Background(), JailRecord{GuildID: "g1", UserID: u, JailRoleID: "jail-role", ReleaseAt: &due})
	}
	p := newTestPlugin(ops, store, newFakeSettings(), newFakeAudit(), newFakePerms(), newFakeScheduler())

	err := p.sweep(context.Background(), "g1")
	if err == nil {
		t.Fatal("expected the failing row's error to be returned")
	}
	if _, ok, _ := store.GetJail(context.Background(), "g1", "good"); ok {
		t.Fatal("the healthy row must still be released")
	}
	if _, ok, _ := store.GetJail(context.Background(), "g1", "bad"); !ok {
		t.Fatal("the failing row must stay for the next sweep")
	}
}

// TestSweepJobTreatsPausedAsSuccess: the Scheduler job wrapper turns a
// paused guild into a nil return, so an operator's pause never burns the
// job's failure budget or alerts #bird-status. Anything else is passed up.
func TestSweepJobTreatsPausedAsSuccess(t *testing.T) {
	ops := newFakeOps()
	ops.setMember("g1", "u1", []string{"jail-role"})
	store := newFakeStore()
	due := fixedNow.Add(-time.Minute)
	_ = store.InsertJail(context.Background(), JailRecord{GuildID: "g1", UserID: "u1", JailRoleID: "jail-role", ReleaseAt: &due})
	p := newTestPlugin(ops, store, newFakeSettings(), newFakeAudit(), newFakePerms(), newFakeScheduler())
	job := p.makeSweepJob("g1")

	ops.memberEditErr = discordguard.ErrPaused
	if err := job(context.Background()); err != nil {
		t.Fatalf("a paused guild must read as success, got %v", err)
	}
	if _, ok, _ := store.GetJail(context.Background(), "g1", "u1"); !ok {
		t.Fatal("a paused sweep must leave the row")
	}

	ops.memberEditErr = transientErr()
	if err := job(context.Background()); err == nil {
		t.Fatal("a genuine failure must be reported")
	}
}

// TestSweepFailsWhenTheDueQueriesFail: an unreadable due list is not an
// empty one. The sweep returns the error rather than silently doing nothing
// and looking healthy.
func TestSweepFailsWhenTheDueQueriesFail(t *testing.T) {
	for name, set := range map[string]func(*fakeStore){
		"jails":  func(s *fakeStore) { s.dueJailsErr = errors.New("db down") },
		"grants": func(s *fakeStore) { s.dueGrantsErr = errors.New("db down") },
	} {
		t.Run(name, func(t *testing.T) {
			store := newFakeStore()
			set(store)
			p := newTestPlugin(newFakeOps(), store, newFakeSettings(), newFakeAudit(), newFakePerms(), newFakeScheduler())
			if err := p.sweep(context.Background(), "g1"); err == nil {
				t.Fatal("expected the query failure to be returned")
			}
		})
	}
}

// TestSweepStillReleasesWhenTheEvasionCheckFails: the evasion check runs
// first, and its failure is recorded as the sweep's error, but the releases
// that are due still happen. Someone whose sentence ended must not stay in
// because an unrelated query broke.
func TestSweepStillReleasesWhenTheEvasionCheckFails(t *testing.T) {
	ops := newFakeOps()
	ops.setMember("g1", "u1", []string{"jail-role"})
	store := newFakeStore()
	store.activeJailsErr = errors.New("db down")
	due := fixedNow.Add(-time.Minute)
	_ = store.InsertJail(context.Background(), JailRecord{GuildID: "g1", UserID: "u1", JailRoleID: "jail-role", ReleaseAt: &due})
	p := newTestPlugin(ops, store, newFakeSettings(), newFakeAudit(), newFakePerms(), newFakeScheduler())

	if err := p.sweep(context.Background(), "g1"); err == nil {
		t.Fatal("expected the evasion check's failure to be reported")
	}
	if _, ok, _ := store.GetJail(context.Background(), "g1", "u1"); ok {
		t.Fatal("the due jail must still be released")
	}
}
