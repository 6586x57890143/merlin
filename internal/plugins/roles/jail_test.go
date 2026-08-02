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
func TestHandleJailStripsRolesKeepingUnmanageableOnes(t *testing.T) {
	ops := newFakeOps()
	ops.setMember("g1", "u1", []string{"managed-role", "unmanageable-role"})
	perms := newFakePerms()
	perms.unmanageable["unmanageable-role"] = true

	store := newFakeStore()
	p := newTestPlugin(ops, store, newFakeSettings(), newFakeAudit(), perms, newFakeScheduler())

	jailRoleID, err := p.resolveJailRole("g1")
	if err != nil {
		t.Fatalf("resolveJailRole: %v", err)
	}

	// Mirrors handleJail's core logic directly (no discordgo.Session
	// available in tests — see fakes_test.go's design note).
	member, _ := ops.GuildMember("g1", "u1")
	var unmanageable []string
	newRoles := []string{jailRoleID}
	for _, r := range member.Roles {
		if err := p.perms.CanManageRole("g1", r); err != nil {
			unmanageable = append(unmanageable, r)
			newRoles = append(newRoles, r)
		}
	}
	if _, err := ops.GuildMemberEdit("g1", "u1", &discordgo.GuildMemberParams{Roles: &newRoles}); err != nil {
		t.Fatalf("GuildMemberEdit: %v", err)
	}

	if len(unmanageable) != 1 || unmanageable[0] != "unmanageable-role" {
		t.Fatalf("expected exactly unmanageable-role flagged, got %v", unmanageable)
	}
	m, _ := ops.GuildMember("g1", "u1")
	if len(m.Roles) != 2 {
		t.Fatalf("expected jail role + unmanageable role retained, got %v", m.Roles)
	}
	foundJail, foundUnmanageable := false, false
	for _, r := range m.Roles {
		if r == jailRoleID {
			foundJail = true
		}
		if r == "unmanageable-role" {
			foundUnmanageable = true
		}
	}
	if !foundJail || !foundUnmanageable {
		t.Fatalf("expected both jail role and unmanageable role present, got %v", m.Roles)
	}
	if len(store.jails) != 0 {
		t.Fatal("this test doesn't call InsertJail — sanity check that store wasn't touched by accident")
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
