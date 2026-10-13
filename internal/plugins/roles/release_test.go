package roles

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/6586x57890143/merlin/internal/core"
)

// fakeTimers stands in for time.AfterFunc: it records each armed delay and
// function so a test can fire one by hand, with the plugin's clock moved to
// wherever it likes first.
type fakeTimers struct {
	armed []fakeTimer
}

type fakeTimer struct {
	delay time.Duration
	fire  func()
}

func (f *fakeTimers) afterFunc(d time.Duration, fn func()) *time.Timer {
	f.armed = append(f.armed, fakeTimer{delay: d, fire: fn})
	// A real, stopped timer so arm's Stop and the map hold something valid.
	t := time.AfterFunc(time.Hour, func() {})
	t.Stop()
	return t
}

func newTimedPlugin(ops *fakeOps, store *fakeStore) (*Plugin, *fakeTimers, *time.Time) {
	p := newTestPlugin(ops, store, newFakeSettings(), newFakeAudit(), newFakePerms(), newFakeScheduler())
	ft := &fakeTimers{}
	p.afterFunc = ft.afterFunc
	now := fixedNow
	p.now = func() time.Time { return now }
	return p, ft, &now
}

// TestJailArmsReleaseAtDueInstant is the change itself: a jail's release
// fires when the sentence ends, not when a sweep next happens to look.
func TestJailArmsReleaseAtDueInstant(t *testing.T) {
	ops := newFakeOps()
	ops.setMember("7000001", "7000102", []string{"7000302"})
	ops.roles["7000001"] = []*frole{{ID: "7000302"}, {ID: "7000301"}}
	p, ft, now := newTimedPlugin(ops, newFakeStore())

	if _, err := p.applyJail(context.Background(), "7000001", "7000301", jailTarget{userID: "7000102", roles: []string{"7000302"}}, 5*time.Minute, "7000319", ""); err != nil {
		t.Fatalf("applyJail: %v", err)
	}
	if len(ft.armed) != 1 || ft.armed[0].delay != 5*time.Minute {
		t.Fatalf("expected one timer armed 5m out, got %+v", ft.armed)
	}

	*now = fixedNow.Add(5 * time.Minute)
	ft.armed[0].fire()

	if _, ok, _ := p.store.GetJail(context.Background(), "7000001", "7000102"); ok {
		t.Fatal("expected jail released by its timer")
	}
	m, _ := ops.GuildMember("7000001", "7000102")
	if len(m.Roles) != 1 || m.Roles[0] != "7000302" {
		t.Fatalf("expected roles restored by the timer, got %v", m.Roles)
	}
}

// TestRedateOutrunsOldTimer: a re-jail moves the sentence, and the timer
// armed for the old date must not let them out on it. Stop is not enough on
// its own, since the old callback may already be running; the fire path
// re-reads the row and finds the new date still ahead.
func TestRedateOutrunsOldTimer(t *testing.T) {
	ops := newFakeOps()
	ops.setMember("7000001", "7000102", []string{"7000302"})
	p, ft, now := newTimedPlugin(ops, newFakeStore())

	if _, err := p.applyJail(context.Background(), "7000001", "7000301", jailTarget{userID: "7000102", roles: []string{"7000302"}}, 5*time.Minute, "7000319", ""); err != nil {
		t.Fatalf("applyJail: %v", err)
	}
	// Re-jailed for an hour: the single-member path goes through jailMany.
	res := p.jailMany(context.Background(), "7000001", "7000301", []jailTarget{{userID: "7000102", roles: []string{"7000301"}}}, time.Hour, "7000319", "")
	if len(res.redated) != 1 {
		t.Fatalf("expected a re-date, got %+v", res)
	}
	if len(ft.armed) != 2 || ft.armed[1].delay != time.Hour {
		t.Fatalf("expected a second timer an hour out, got %+v", ft.armed)
	}

	*now = fixedNow.Add(5 * time.Minute)
	ft.armed[0].fire() // the old one
	if _, ok, _ := p.store.GetJail(context.Background(), "7000001", "7000102"); !ok {
		t.Fatal("old timer released a re-dated jail early")
	}

	*now = fixedNow.Add(time.Hour)
	ft.armed[1].fire()
	if _, ok, _ := p.store.GetJail(context.Background(), "7000001", "7000102"); ok {
		t.Fatal("expected the re-dated timer to release")
	}
}

// TestSweepArmsUpcomingAndReleasesOverdue covers the restart case: rows
// this process never wrote get a timer once they are within the lookahead,
// and anything already overdue is released right there.
func TestSweepArmsUpcomingAndReleasesOverdue(t *testing.T) {
	ops := newFakeOps()
	ops.setMember("7000001", "overdue", []string{"7000301"})
	ops.setMember("7000001", "soon", []string{"7000301"})
	ops.setMember("7000001", "later", []string{"7000301"})
	ops.setMember("7000001", "7000122", []string{"7000306"})
	store := newFakeStore()
	overdue := fixedNow.Add(-time.Minute)
	soon := fixedNow.Add(armLookahead / 2)
	later := fixedNow.Add(armLookahead * 2)
	for user, at := range map[string]time.Time{"overdue": overdue, "soon": soon, "later": later} {
		at := at
		_ = store.InsertJail(context.Background(), JailRecord{GuildID: "7000001", UserID: user, JailRoleID: "7000301", ReleaseAt: &at})
	}
	_ = store.InsertGrant(context.Background(), GrantRecord{GuildID: "7000001", UserID: "7000122", RoleID: "7000306", ExpiresAt: &soon})

	p, ft, now := newTimedPlugin(ops, store)
	if err := p.sweep(context.Background(), "7000001"); err != nil {
		t.Fatalf("sweep: %v", err)
	}

	if _, ok, _ := store.GetJail(context.Background(), "7000001", "overdue"); ok {
		t.Fatal("expected the overdue jail released by the sweep itself")
	}
	if len(ft.armed) != 2 {
		t.Fatalf("expected timers for exactly the soon jail and the soon grant, got %d", len(ft.armed))
	}
	if _, ok := p.timers[jailKey("7000001", "later")]; ok {
		t.Fatal("a jail beyond the lookahead should not be armed yet")
	}

	*now = soon
	for _, ft := range ft.armed {
		ft.fire()
	}
	if _, ok, _ := store.GetJail(context.Background(), "7000001", "soon"); ok {
		t.Fatal("expected the soon jail released by its timer")
	}
	if _, ok, _ := store.GetGrant(context.Background(), "7000001", "7000122", "7000306"); ok {
		t.Fatal("expected the soon grant revoked by its timer")
	}
	if _, ok, _ := store.GetJail(context.Background(), "7000001", "later"); !ok {
		t.Fatal("the later jail must still be tracked")
	}
}

// TestGrantArmsExpiry mirrors the jail case for timed grants.
func TestGrantArmsExpiry(t *testing.T) {
	ops := newFakeOps()
	ops.setMember("7000001", "7000102", []string{"7000302"})
	p, ft, now := newTimedPlugin(ops, newFakeStore())
	at := fixedNow.Add(time.Hour)
	_ = p.store.InsertGrant(context.Background(), GrantRecord{GuildID: "7000001", UserID: "7000102", RoleID: "7000302", ExpiresAt: &at})
	p.armGrantRevoke("7000001", "7000102", "7000302", at)

	*now = at
	ft.armed[0].fire()
	if _, ok, _ := p.store.GetGrant(context.Background(), "7000001", "7000102", "7000302"); ok {
		t.Fatal("expected grant revoked by its timer")
	}
	m, _ := ops.GuildMember("7000001", "7000102")
	if len(m.Roles) != 0 {
		t.Fatalf("expected the granted role removed, got %v", m.Roles)
	}
}

// TestTimerHonoursDryRun: the timer defers to the same operator state the
// sweep does, and leaves the row for the sweep afterwards.
func TestTimerHonoursDryRun(t *testing.T) {
	ops := newFakeOps()
	ops.setMember("7000001", "7000102", []string{"7000301"})
	p, _, now := newTimedPlugin(ops, newFakeStore())
	p.dryRun = func(string) bool { return true }
	at := fixedNow.Add(time.Minute)
	_ = p.store.InsertJail(context.Background(), JailRecord{GuildID: "7000001", UserID: "7000102", JailRoleID: "7000301", ReleaseAt: &at})

	*now = at
	p.fireJailRelease("7000001", "7000102")
	if _, ok, _ := p.store.GetJail(context.Background(), "7000001", "7000102"); !ok {
		t.Fatal("dry-run timer must leave the jail tracked")
	}
	if len(ops.memberEditCalls["7000102"]) != 0 {
		t.Fatal("dry-run timer must not touch the member")
	}
}

// TestClaimedRowIsLeftToTheFirstCaller: a timer and a sweep landing on the
// same row at once must not both restore, audit and DM.
func TestClaimedRowIsLeftToTheFirstCaller(t *testing.T) {
	ops := newFakeOps()
	ops.setMember("7000001", "7000102", []string{"7000301"})
	p, _, _ := newTimedPlugin(ops, newFakeStore())
	rec := JailRecord{GuildID: "7000001", UserID: "7000102", JailRoleID: "7000301", SnapshotRoleIDs: []string{"7000302"}}
	_ = p.store.InsertJail(context.Background(), rec)

	if !p.claim(jailKey("7000001", "7000102")) {
		t.Fatal("first claim should succeed")
	}
	if err := p.releaseJail(context.Background(), "7000001", "7000102", rec, core.ActorSystem); !errors.Is(err, errReleaseInProgress) {
		t.Fatalf("second caller should report the row as in progress, got %v", err)
	}
	if len(ops.memberEditCalls["7000102"]) != 0 {
		t.Fatal("second caller must not restore roles")
	}
	if _, ok, _ := p.store.GetJail(context.Background(), "7000001", "7000102"); !ok {
		t.Fatal("second caller must not untrack the row the first is working on")
	}
	p.unclaim(jailKey("7000001", "7000102"))
	if err := p.releaseJail(context.Background(), "7000001", "7000102", rec, core.ActorSystem); err != nil {
		t.Fatalf("releaseJail after unclaim: %v", err)
	}
	if _, ok, _ := p.store.GetJail(context.Background(), "7000001", "7000102"); ok {
		t.Fatal("expected release once the claim is gone")
	}
}

// TestForgetGuildDisarmsOnlyThatGuild: a timer left running after the bot
// is removed would read Discord's "unknown" answer as the member having
// left and drop the row ForgetGuild promises to keep.
func TestForgetGuildDisarmsOnlyThatGuild(t *testing.T) {
	p, _, _ := newTimedPlugin(newFakeOps(), newFakeStore())
	at := fixedNow.Add(time.Hour)
	p.armJailRelease("7000001", "7000102", at)
	p.armGrantRevoke("7000001", "7000102", "r", at)
	p.armJailRelease("7000002", "7000102", at)

	p.ForgetGuild("7000001")
	if len(p.timers) != 1 {
		t.Fatalf("expected only g2's timer to survive, got %v", p.timers)
	}
	if _, ok := p.timers[jailKey("7000002", "7000102")]; !ok {
		t.Fatal("g2's timer should be untouched")
	}
	if err := p.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(p.timers) != 0 {
		t.Fatal("Shutdown should stop every timer")
	}
}

// TestArmFiresImmediatelyWhenAlreadyDue uses the real time.AfterFunc: an
// instant in the past is handed a non-positive delay and fires at once,
// which is what the sweep-side arm relies on never needing a special case.
func TestArmFiresImmediatelyWhenAlreadyDue(t *testing.T) {
	p := newTestPlugin(newFakeOps(), newFakeStore(), newFakeSettings(), newFakeAudit(), newFakePerms(), newFakeScheduler())
	fired := make(chan struct{})
	p.arm("k", fixedNow.Add(-time.Hour), func() { close(fired) })
	select {
	case <-fired:
	case <-time.After(5 * time.Second):
		t.Fatal("a past instant should fire immediately")
	}
	p.timerMu.Lock()
	defer p.timerMu.Unlock()
	if _, ok := p.timers["k"]; ok {
		t.Fatal("a fired timer should remove its own entry")
	}
}
