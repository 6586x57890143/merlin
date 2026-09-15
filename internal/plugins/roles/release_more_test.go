package roles

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/6586x57890143/merlin/internal/discordguard"
)

// TestTimerForHandDeletedRowIsANoOp: a timer outlives the row it was armed
// for whenever a mod releases by hand first. Firing must not restore
// anything, since the fire path re-reads the row rather than trusting what
// it was armed with.
func TestTimerForHandDeletedRowIsANoOp(t *testing.T) {
	ops := newFakeOps()
	ops.setMember("g1", "u1", []string{"jail-role"})
	p, ft, now := newTimedPlugin(ops, newFakeStore())
	p.armJailRelease("g1", "u1", fixedNow.Add(time.Minute))
	p.armGrantRevoke("g1", "u1", "role-x", fixedNow.Add(time.Minute))

	*now = fixedNow.Add(time.Minute)
	for _, a := range ft.armed {
		a.fire()
	}
	if len(ops.memberEditCalls["u1"]) != 0 || len(ops.roleRemoveCalls) != 0 {
		t.Fatal("a timer with no row behind it must touch nobody")
	}
}

// TestTimerLeavesRowThatIsNotYetDue: the row's own date wins over the
// timer's. A timer that fires early (a clock moved, a re-date) must leave
// the member in.
func TestTimerLeavesRowThatIsNotYetDue(t *testing.T) {
	ops := newFakeOps()
	ops.setMember("g1", "u1", []string{"jail-role"})
	store := newFakeStore()
	p, _, _ := newTimedPlugin(ops, store)
	at := fixedNow.Add(time.Hour)
	_ = store.InsertJail(context.Background(), JailRecord{GuildID: "g1", UserID: "u1", JailRoleID: "jail-role", ReleaseAt: &at})
	_ = store.InsertGrant(context.Background(), GrantRecord{GuildID: "g1", UserID: "u1", RoleID: "role-x", ExpiresAt: &at})

	p.fireJailRelease("g1", "u1")
	p.fireGrantRevoke("g1", "u1", "role-x")
	if _, ok, _ := store.GetJail(context.Background(), "g1", "u1"); !ok {
		t.Fatal("jail released before its date")
	}
	if _, ok, _ := store.GetGrant(context.Background(), "g1", "u1", "role-x"); !ok {
		t.Fatal("grant revoked before its date")
	}
}

// TestTimerLeavesRowForSweepWhenPaused: a paused guild is an operator's
// choice, not a failure. The timer steps aside and the row stays for the
// first sweep after the pause lifts, exactly as dry-run does.
func TestTimerLeavesRowForSweepWhenPaused(t *testing.T) {
	ops := newFakeOps()
	ops.setMember("g1", "u1", []string{"jail-role"})
	ops.memberEditErr = discordguard.ErrPaused
	store := newFakeStore()
	p, _, now := newTimedPlugin(ops, store)
	at := fixedNow.Add(time.Minute)
	_ = store.InsertJail(context.Background(), JailRecord{GuildID: "g1", UserID: "u1", JailRoleID: "jail-role", ReleaseAt: &at})

	*now = at
	p.fireJailRelease("g1", "u1")
	if _, ok, _ := store.GetJail(context.Background(), "g1", "u1"); !ok {
		t.Fatal("a paused release must leave the row for the sweep")
	}
}

// TestTimerDoesNothingWhenTheRowCannotBeRead: an unreadable row is not a
// missing one. The timer logs and leaves everything alone; the sweep is the
// retry path.
func TestTimerDoesNothingWhenTheRowCannotBeRead(t *testing.T) {
	ops := newFakeOps()
	ops.setMember("g1", "u1", []string{"jail-role"})
	store := newFakeStore()
	store.getJailErr = errors.New("db down")
	store.getGrantErr = errors.New("db down")
	p, _, now := newTimedPlugin(ops, store)
	*now = fixedNow.Add(time.Hour)

	p.fireJailRelease("g1", "u1")
	p.fireGrantRevoke("g1", "u1", "role-x")
	if len(ops.memberEditCalls["u1"]) != 0 || len(ops.roleRemoveCalls) != 0 {
		t.Fatal("a timer that cannot read its row must not act")
	}
}

// TestGrantTimerHonoursDryRun mirrors TestTimerHonoursDryRun for grants.
func TestGrantTimerHonoursDryRun(t *testing.T) {
	ops := newFakeOps()
	ops.setMember("g1", "u1", []string{"role-x"})
	store := newFakeStore()
	p, _, now := newTimedPlugin(ops, store)
	p.dryRun = func(string) bool { return true }
	at := fixedNow.Add(time.Minute)
	_ = store.InsertGrant(context.Background(), GrantRecord{GuildID: "g1", UserID: "u1", RoleID: "role-x", ExpiresAt: &at})

	*now = at
	p.fireGrantRevoke("g1", "u1", "role-x")
	if _, ok, _ := store.GetGrant(context.Background(), "g1", "u1", "role-x"); !ok {
		t.Fatal("dry-run timer must leave the grant tracked")
	}
	if len(ops.roleRemoveCalls) != 0 {
		t.Fatal("dry-run timer must not touch the member")
	}
}

// TestGrantTimerLeavesRowForSweepOnFailure: a failed revoke logs and leaves
// the row, so the next sweep retries rather than the grant going permanent.
func TestGrantTimerLeavesRowForSweepOnFailure(t *testing.T) {
	ops := newFakeOps()
	ops.memberFetchErr = transientErr()
	store := newFakeStore()
	p, _, now := newTimedPlugin(ops, store)
	at := fixedNow.Add(time.Minute)
	_ = store.InsertGrant(context.Background(), GrantRecord{GuildID: "g1", UserID: "u1", RoleID: "role-x", ExpiresAt: &at})

	*now = at
	p.fireGrantRevoke("g1", "u1", "role-x")
	if _, ok, _ := store.GetGrant(context.Background(), "g1", "u1", "role-x"); !ok {
		t.Fatal("a failed revoke must keep the row for the sweep")
	}
}

// TestArmReplacesAndStopsTheOldTimer: re-arming a key stops the timer
// already under it, so a re-dated jail never has two timers waiting, and
// the map holds only the newest.
func TestArmReplacesAndStopsTheOldTimer(t *testing.T) {
	p := newTestPlugin(newFakeOps(), newFakeStore(), newFakeSettings(), newFakeAudit(), newFakePerms(), newFakeScheduler())
	p.arm("k", fixedNow.Add(time.Hour), func() { t.Error("old timer must never fire") })
	old := p.timers["k"]
	p.arm("k", fixedNow.Add(2*time.Hour), func() {})
	defer p.disarm("")

	if p.timers["k"] == old {
		t.Fatal("expected the map to hold the new timer")
	}
	// Stop reports false when the timer was already stopped.
	if old.Stop() {
		t.Fatal("arm must stop the timer it replaces")
	}
	if len(p.timers) != 1 {
		t.Fatalf("expected exactly one timer, got %d", len(p.timers))
	}
}

// TestTimerPanicIsRecovered: a timer goroutine has nobody above it, so a
// panic there would take the whole process down. The callback recovers and
// logs; the test only survives if it does.
func TestTimerPanicIsRecovered(t *testing.T) {
	p := newTestPlugin(newFakeOps(), newFakeStore(), newFakeSettings(), newFakeAudit(), newFakePerms(), newFakeScheduler())
	done := make(chan struct{})
	p.arm("k", fixedNow.Add(-time.Hour), func() {
		defer close(done)
		panic("boom")
	})
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("timer never fired")
	}
}
