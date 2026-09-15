package roles

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
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
	ops.setMember("g1", "u1", []string{"role-a"})
	ops.roles["g1"] = []*discordgo.Role{{ID: "role-a"}, {ID: "jail-role"}}
	p, ft, now := newTimedPlugin(ops, newFakeStore())

	if _, err := p.applyJail(context.Background(), "g1", "u1", "jail-role", []string{"role-a"}, 5*time.Minute, "mod", ""); err != nil {
		t.Fatalf("applyJail: %v", err)
	}
	if len(ft.armed) != 1 || ft.armed[0].delay != 5*time.Minute {
		t.Fatalf("expected one timer armed 5m out, got %+v", ft.armed)
	}

	*now = fixedNow.Add(5 * time.Minute)
	ft.armed[0].fire()

	if _, ok, _ := p.store.GetJail(context.Background(), "g1", "u1"); ok {
		t.Fatal("expected jail released by its timer")
	}
	m, _ := ops.GuildMember("g1", "u1")
	if len(m.Roles) != 1 || m.Roles[0] != "role-a" {
		t.Fatalf("expected roles restored by the timer, got %v", m.Roles)
	}
}

// TestRedateOutrunsOldTimer: a re-jail moves the sentence, and the timer
// armed for the old date must not let them out on it. Stop is not enough on
// its own, since the old callback may already be running; the fire path
// re-reads the row and finds the new date still ahead.
func TestRedateOutrunsOldTimer(t *testing.T) {
	ops := newFakeOps()
	ops.setMember("g1", "u1", []string{"role-a"})
	p, ft, now := newTimedPlugin(ops, newFakeStore())

	if _, err := p.applyJail(context.Background(), "g1", "u1", "jail-role", []string{"role-a"}, 5*time.Minute, "mod", ""); err != nil {
		t.Fatalf("applyJail: %v", err)
	}
	// Re-jailed for an hour: the single-member path goes through jailMany.
	res := p.jailMany(context.Background(), "g1", "jail-role", []jailTarget{{userID: "u1", roles: []string{"jail-role"}}}, time.Hour, "mod", "")
	if len(res.redated) != 1 {
		t.Fatalf("expected a re-date, got %+v", res)
	}
	if len(ft.armed) != 2 || ft.armed[1].delay != time.Hour {
		t.Fatalf("expected a second timer an hour out, got %+v", ft.armed)
	}

	*now = fixedNow.Add(5 * time.Minute)
	ft.armed[0].fire() // the old one
	if _, ok, _ := p.store.GetJail(context.Background(), "g1", "u1"); !ok {
		t.Fatal("old timer released a re-dated jail early")
	}

	*now = fixedNow.Add(time.Hour)
	ft.armed[1].fire()
	if _, ok, _ := p.store.GetJail(context.Background(), "g1", "u1"); ok {
		t.Fatal("expected the re-dated timer to release")
	}
}

// TestSweepArmsUpcomingAndReleasesOverdue covers the restart case: rows
// this process never wrote get a timer once they are within the lookahead,
// and anything already overdue is released right there.
func TestSweepArmsUpcomingAndReleasesOverdue(t *testing.T) {
	ops := newFakeOps()
	ops.setMember("g1", "overdue", []string{"jail-role"})
	ops.setMember("g1", "soon", []string{"jail-role"})
	ops.setMember("g1", "later", []string{"jail-role"})
	ops.setMember("g1", "grantee", []string{"role-x"})
	store := newFakeStore()
	overdue := fixedNow.Add(-time.Minute)
	soon := fixedNow.Add(armLookahead / 2)
	later := fixedNow.Add(armLookahead * 2)
	for user, at := range map[string]time.Time{"overdue": overdue, "soon": soon, "later": later} {
		at := at
		_ = store.InsertJail(context.Background(), JailRecord{GuildID: "g1", UserID: user, JailRoleID: "jail-role", ReleaseAt: &at})
	}
	_ = store.InsertGrant(context.Background(), GrantRecord{GuildID: "g1", UserID: "grantee", RoleID: "role-x", ExpiresAt: &soon})

	p, ft, now := newTimedPlugin(ops, store)
	if err := p.sweep(context.Background(), "g1"); err != nil {
		t.Fatalf("sweep: %v", err)
	}

	if _, ok, _ := store.GetJail(context.Background(), "g1", "overdue"); ok {
		t.Fatal("expected the overdue jail released by the sweep itself")
	}
	if len(ft.armed) != 2 {
		t.Fatalf("expected timers for exactly the soon jail and the soon grant, got %d", len(ft.armed))
	}
	if _, ok := p.timers[jailKey("g1", "later")]; ok {
		t.Fatal("a jail beyond the lookahead should not be armed yet")
	}

	*now = soon
	for _, ft := range ft.armed {
		ft.fire()
	}
	if _, ok, _ := store.GetJail(context.Background(), "g1", "soon"); ok {
		t.Fatal("expected the soon jail released by its timer")
	}
	if _, ok, _ := store.GetGrant(context.Background(), "g1", "grantee", "role-x"); ok {
		t.Fatal("expected the soon grant revoked by its timer")
	}
	if _, ok, _ := store.GetJail(context.Background(), "g1", "later"); !ok {
		t.Fatal("the later jail must still be tracked")
	}
}

// TestGrantArmsExpiry mirrors the jail case for timed grants.
func TestGrantArmsExpiry(t *testing.T) {
	ops := newFakeOps()
	ops.setMember("g1", "u1", []string{"role-a"})
	p, ft, now := newTimedPlugin(ops, newFakeStore())
	at := fixedNow.Add(time.Hour)
	_ = p.store.InsertGrant(context.Background(), GrantRecord{GuildID: "g1", UserID: "u1", RoleID: "role-a", ExpiresAt: &at})
	p.armGrantRevoke("g1", "u1", "role-a", at)

	*now = at
	ft.armed[0].fire()
	if _, ok, _ := p.store.GetGrant(context.Background(), "g1", "u1", "role-a"); ok {
		t.Fatal("expected grant revoked by its timer")
	}
	m, _ := ops.GuildMember("g1", "u1")
	if len(m.Roles) != 0 {
		t.Fatalf("expected the granted role removed, got %v", m.Roles)
	}
}

// TestTimerHonoursDryRun: the timer defers to the same operator state the
// sweep does, and leaves the row for the sweep afterwards.
func TestTimerHonoursDryRun(t *testing.T) {
	ops := newFakeOps()
	ops.setMember("g1", "u1", []string{"jail-role"})
	p, _, now := newTimedPlugin(ops, newFakeStore())
	p.dryRun = func(string) bool { return true }
	at := fixedNow.Add(time.Minute)
	_ = p.store.InsertJail(context.Background(), JailRecord{GuildID: "g1", UserID: "u1", JailRoleID: "jail-role", ReleaseAt: &at})

	*now = at
	p.fireJailRelease("g1", "u1")
	if _, ok, _ := p.store.GetJail(context.Background(), "g1", "u1"); !ok {
		t.Fatal("dry-run timer must leave the jail tracked")
	}
	if len(ops.memberEditCalls["u1"]) != 0 {
		t.Fatal("dry-run timer must not touch the member")
	}
}

// TestClaimedRowIsLeftToTheFirstCaller: a timer and a sweep landing on the
// same row at once must not both restore, audit and DM.
func TestClaimedRowIsLeftToTheFirstCaller(t *testing.T) {
	ops := newFakeOps()
	ops.setMember("g1", "u1", []string{"jail-role"})
	p, _, _ := newTimedPlugin(ops, newFakeStore())
	rec := JailRecord{GuildID: "g1", UserID: "u1", JailRoleID: "jail-role", SnapshotRoleIDs: []string{"role-a"}}
	_ = p.store.InsertJail(context.Background(), rec)

	if !p.claim(jailKey("g1", "u1")) {
		t.Fatal("first claim should succeed")
	}
	if err := p.releaseJail(context.Background(), "g1", "u1", rec); !errors.Is(err, errReleaseInProgress) {
		t.Fatalf("second caller should report the row as in progress, got %v", err)
	}
	if len(ops.memberEditCalls["u1"]) != 0 {
		t.Fatal("second caller must not restore roles")
	}
	if _, ok, _ := p.store.GetJail(context.Background(), "g1", "u1"); !ok {
		t.Fatal("second caller must not untrack the row the first is working on")
	}
	p.unclaim(jailKey("g1", "u1"))
	if err := p.releaseJail(context.Background(), "g1", "u1", rec); err != nil {
		t.Fatalf("releaseJail after unclaim: %v", err)
	}
	if _, ok, _ := p.store.GetJail(context.Background(), "g1", "u1"); ok {
		t.Fatal("expected release once the claim is gone")
	}
}

// TestForgetGuildDisarmsOnlyThatGuild: a timer left running after the bot
// is removed would read Discord's "unknown" answer as the member having
// left and drop the row ForgetGuild promises to keep.
func TestForgetGuildDisarmsOnlyThatGuild(t *testing.T) {
	p, _, _ := newTimedPlugin(newFakeOps(), newFakeStore())
	at := fixedNow.Add(time.Hour)
	p.armJailRelease("g1", "u1", at)
	p.armGrantRevoke("g1", "u1", "r", at)
	p.armJailRelease("g2", "u1", at)

	p.ForgetGuild("g1")
	if len(p.timers) != 1 {
		t.Fatalf("expected only g2's timer to survive, got %v", p.timers)
	}
	if _, ok := p.timers[jailKey("g2", "u1")]; !ok {
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
