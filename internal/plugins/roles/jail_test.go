package roles

import (
	"context"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
)

// TestReleaseJailRestoresSnapshotFilteredToExistingRoles verifies release
// restores exactly the member's pre-jail roles, minus any role that was
// deleted from the guild in the meantime (release must not hand back a
// dangling role ID Discord would reject).
func TestReleaseJailRestoresSnapshotFilteredToExistingRoles(t *testing.T) {
	ops := newFakeOps()
	ops.setMember("g1", "u1", []string{"jail-role"})
	ops.roles["g1"] = []*discordgo.Role{{ID: "role-a"}, {ID: "jail-role"}} // role-b was deleted since jailing

	p := newTestPlugin(ops, newFakeStore(), newFakeSettings(), newFakeAudit(), newFakePerms(), newFakeScheduler())

	rec := JailRecord{
		GuildID: "g1", UserID: "u1",
		SnapshotRoleIDs: []string{"role-a", "role-b"},
		JailRoleID:      "jail-role",
	}
	if err := p.releaseJail(context.Background(), "g1", "u1", rec); err != nil {
		t.Fatalf("releaseJail: %v", err)
	}

	m, _ := ops.GuildMember("g1", "u1")
	if len(m.Roles) != 1 || m.Roles[0] != "role-a" {
		t.Fatalf("expected only surviving role-a restored, got %v", m.Roles)
	}
	if _, ok, _ := p.store.GetJail(context.Background(), "g1", "u1"); ok {
		t.Fatal("expected jail record removed after release")
	}
}

// TestReleaseJailConfusedDeputyRescue verifies the safeguard central to this
// plugin's design: if a mod already manually removed the jail marker role
// before the scheduled/explicit release ran, releaseJail must not fight
// that override — no restore write, just stop tracking.
func TestReleaseJailConfusedDeputyRescue(t *testing.T) {
	ops := newFakeOps()
	ops.setMember("g1", "u1", []string{"some-other-role"}) // jail-role no longer present

	p := newTestPlugin(ops, newFakeStore(), newFakeSettings(), newFakeAudit(), newFakePerms(), newFakeScheduler())

	rec := JailRecord{GuildID: "g1", UserID: "u1", SnapshotRoleIDs: []string{"role-a"}, JailRoleID: "jail-role"}
	if err := p.releaseJail(context.Background(), "g1", "u1", rec); err != nil {
		t.Fatalf("releaseJail: %v", err)
	}

	if len(ops.memberEditCalls["u1"]) != 0 {
		t.Fatalf("expected no GuildMemberEdit call when jail marker already gone, got %v", ops.memberEditCalls["u1"])
	}
	if _, ok, _ := p.store.GetJail(context.Background(), "g1", "u1"); ok {
		t.Fatal("expected jail record removed even though no restore happened")
	}
}

// TestReleaseJailMemberGoneCleansUpRecord verifies a member who left the
// guild before release doesn't leave a permanently-stuck tracking row.
func TestReleaseJailMemberGoneCleansUpRecord(t *testing.T) {
	ops := newFakeOps() // no member registered at all

	p := newTestPlugin(ops, newFakeStore(), newFakeSettings(), newFakeAudit(), newFakePerms(), newFakeScheduler())
	_ = p.store.InsertJail(context.Background(), JailRecord{GuildID: "g1", UserID: "u1", JailRoleID: "jail-role"})

	rec, _, _ := p.store.GetJail(context.Background(), "g1", "u1")
	if err := p.releaseJail(context.Background(), "g1", "u1", rec); err != nil {
		t.Fatalf("releaseJail: %v", err)
	}
	if _, ok, _ := p.store.GetJail(context.Background(), "g1", "u1"); ok {
		t.Fatal("expected jail record removed for a member who left the guild")
	}
}

// TestHandleJailStripsRolesKeepingUnmanageableOnes verifies the role-
// hierarchy safeguard (CanManageRole, spec.MD §4 item 4): a role the bot
// can't manage must survive jailing rather than being silently dropped or
// causing the whole operation to fail.
func TestJailRolesKeepsUnmanageableRolesInPlace(t *testing.T) {
	perms := newFakePerms()
	perms.unmanageable["unmanageable-role"] = true

	newRoles, unmanageable := jailRoles(perms, "g1", "jail-role", []string{"managed-role", "unmanageable-role"})

	if len(unmanageable) != 1 || unmanageable[0] != "unmanageable-role" {
		t.Fatalf("expected exactly unmanageable-role flagged, got %v", unmanageable)
	}
	if len(newRoles) != 2 || newRoles[0] != "jail-role" || newRoles[1] != "unmanageable-role" {
		t.Fatalf("expected the jail marker plus the untouchable role, got %v", newRoles)
	}
}

// TestJailRolesStripsEverythingItCan is the ordinary case: a member whose
// roles are all below the bot's ends up holding nothing but the marker.
func TestJailRolesStripsEverythingItCan(t *testing.T) {
	newRoles, unmanageable := jailRoles(newFakePerms(), "g1", "jail-role", []string{"role-a", "role-b", "role-c"})

	if len(unmanageable) != 0 {
		t.Fatalf("expected nothing flagged as unmanageable, got %v", unmanageable)
	}
	if len(newRoles) != 1 || newRoles[0] != "jail-role" {
		t.Fatalf("expected only the jail marker to remain, got %v", newRoles)
	}
}

// TestJailRolesOnRolelessMember covers a member holding no roles at all —
// the marker still has to be applied, and an empty roles list must not read
// as "strip nothing."
func TestJailRolesOnRolelessMember(t *testing.T) {
	newRoles, unmanageable := jailRoles(newFakePerms(), "g1", "jail-role", nil)
	if len(unmanageable) != 0 || len(newRoles) != 1 || newRoles[0] != "jail-role" {
		t.Fatalf("jailRoles(no roles) = (%v, %v), want ([jail-role], [])", newRoles, unmanageable)
	}
}

func TestSweepReleasesDueJailsAndSkipsNotYetDue(t *testing.T) {
	ops := newFakeOps()
	ops.setMember("g1", "u1", []string{"jail-role"})
	ops.setMember("g1", "u2", []string{"jail-role"})

	store := newFakeStore()
	due := fixedNow.Add(-time.Minute)
	notDue := fixedNow.Add(time.Hour)
	_ = store.InsertJail(context.Background(), JailRecord{GuildID: "g1", UserID: "u1", JailRoleID: "jail-role", ReleaseAt: &due})
	_ = store.InsertJail(context.Background(), JailRecord{GuildID: "g1", UserID: "u2", JailRoleID: "jail-role", ReleaseAt: &notDue})

	p := newTestPlugin(ops, store, newFakeSettings(), newFakeAudit(), newFakePerms(), newFakeScheduler())

	if err := p.sweep(context.Background(), "g1"); err != nil {
		t.Fatalf("sweep: %v", err)
	}

	if _, ok, _ := store.GetJail(context.Background(), "g1", "u1"); ok {
		t.Fatal("expected due jail released and untracked")
	}
	if _, ok, _ := store.GetJail(context.Background(), "g1", "u2"); !ok {
		t.Fatal("expected not-yet-due jail left untouched")
	}
}

// TestReleaseJailKeepsTrackingOnTransientFetchFailure is the counterpart to
// TestReleaseJailMemberGoneCleansUpRecord. releaseJail used to read *any*
// member-fetch error as "they left the guild" and drop the tracking row —
// so a rate limit or a 5xx during the sweep left the member jailed forever,
// with nothing left to ever release them and no error surfaced afterwards.
// Only Discord's own "unknown member" may untrack.
func TestReleaseJailKeepsTrackingOnTransientFetchFailure(t *testing.T) {
	ops := newFakeOps()
	ops.setMember("g1", "u1", []string{"jail-role"})
	ops.roles["g1"] = []*discordgo.Role{{ID: "role-a"}, {ID: "jail-role"}}
	ops.memberFetchErr = transientErr()

	p := newTestPlugin(ops, newFakeStore(), newFakeSettings(), newFakeAudit(), newFakePerms(), newFakeScheduler())
	rec := JailRecord{GuildID: "g1", UserID: "u1", SnapshotRoleIDs: []string{"role-a"}, JailRoleID: "jail-role"}
	if err := p.store.InsertJail(context.Background(), rec); err != nil {
		t.Fatalf("InsertJail: %v", err)
	}

	if err := p.releaseJail(context.Background(), "g1", "u1", rec); err == nil {
		t.Fatal("expected releaseJail to report the transient fetch failure")
	}
	if _, ok, _ := p.store.GetJail(context.Background(), "g1", "u1"); !ok {
		t.Fatal("jail record was dropped on a transient error — the member would stay jailed forever")
	}

	// Discord recovers: the next sweep releases them properly.
	ops.memberFetchErr = nil
	if err := p.releaseJail(context.Background(), "g1", "u1", rec); err != nil {
		t.Fatalf("retry releaseJail: %v", err)
	}
	m, _ := ops.GuildMember("g1", "u1")
	if len(m.Roles) != 1 || m.Roles[0] != "role-a" {
		t.Fatalf("expected prior roles restored on retry, got %v", m.Roles)
	}
	if _, ok, _ := p.store.GetJail(context.Background(), "g1", "u1"); ok {
		t.Fatal("expected the jail record to be cleared once the release succeeded")
	}
}

// TestSweepRetriesJailReleaseUntilItSucceeds exercises the same failure
// through the sweep job, which is where it actually bites: a due jail that
// couldn't be released must still be due on the next minute's sweep.
func TestSweepRetriesJailReleaseUntilItSucceeds(t *testing.T) {
	ops := newFakeOps()
	ops.setMember("g1", "u1", []string{"jail-role"})
	ops.roles["g1"] = []*discordgo.Role{{ID: "role-a"}, {ID: "jail-role"}}
	ops.memberFetchErr = transientErr()

	p := newTestPlugin(ops, newFakeStore(), newFakeSettings(), newFakeAudit(), newFakePerms(), newFakeScheduler())
	due := fixedNow.Add(-time.Minute)
	if err := p.store.InsertJail(context.Background(), JailRecord{
		GuildID: "g1", UserID: "u1", SnapshotRoleIDs: []string{"role-a"},
		JailRoleID: "jail-role", ReleaseAt: &due,
	}); err != nil {
		t.Fatalf("InsertJail: %v", err)
	}

	if err := p.sweep(context.Background(), "g1"); err == nil {
		t.Fatal("expected the sweep to report the failed release")
	}
	if _, ok, _ := p.store.GetJail(context.Background(), "g1", "u1"); !ok {
		t.Fatal("expected the due jail to still be tracked after a failed sweep")
	}

	ops.memberFetchErr = nil
	if err := p.sweep(context.Background(), "g1"); err != nil {
		t.Fatalf("second sweep: %v", err)
	}
	if _, ok, _ := p.store.GetJail(context.Background(), "g1", "u1"); ok {
		t.Fatal("expected the jail to be released on the retry sweep")
	}
}
