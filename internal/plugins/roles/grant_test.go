package roles

import (
	"context"
	"testing"
	"time"
)

func TestRevokeGrantRemovesRoleAndUntracks(t *testing.T) {
	ops := newFakeOps()
	ops.setMember("g1", "u1", []string{"granted-role"})
	store := newFakeStore()
	_ = store.InsertGrant(context.Background(), GrantRecord{GuildID: "g1", UserID: "u1", RoleID: "granted-role"})

	p := newTestPlugin(ops, store, newFakeSettings(), newFakeAudit(), newFakePerms(), newFakeScheduler())

	if err := p.revokeGrant(context.Background(), "g1", "u1", "granted-role", "mod1"); err != nil {
		t.Fatalf("revokeGrant: %v", err)
	}

	m, _ := ops.GuildMember("g1", "u1")
	if len(m.Roles) != 0 {
		t.Fatalf("expected granted role removed, got %v", m.Roles)
	}
	if _, ok, _ := store.GetGrant(context.Background(), "g1", "u1", "granted-role"); ok {
		t.Fatal("expected grant record removed")
	}
	if len(ops.roleRemoveCalls) != 1 {
		t.Fatalf("expected exactly one GuildMemberRoleRemove call, got %d", len(ops.roleRemoveCalls))
	}
}

// TestRevokeGrantConfusedDeputyRescue mirrors releaseJail's safeguard: if a
// mod already manually removed the granted role, revokeGrant must not call
// GuildMemberRoleRemove again — just stop tracking it.
func TestRevokeGrantConfusedDeputyRescue(t *testing.T) {
	ops := newFakeOps()
	ops.setMember("g1", "u1", []string{}) // role already gone
	store := newFakeStore()
	_ = store.InsertGrant(context.Background(), GrantRecord{GuildID: "g1", UserID: "u1", RoleID: "granted-role"})

	p := newTestPlugin(ops, store, newFakeSettings(), newFakeAudit(), newFakePerms(), newFakeScheduler())

	if err := p.revokeGrant(context.Background(), "g1", "u1", "granted-role", "mod1"); err != nil {
		t.Fatalf("revokeGrant: %v", err)
	}

	if len(ops.roleRemoveCalls) != 0 {
		t.Fatalf("expected no GuildMemberRoleRemove call when role already gone, got %v", ops.roleRemoveCalls)
	}
	if _, ok, _ := store.GetGrant(context.Background(), "g1", "u1", "granted-role"); ok {
		t.Fatal("expected grant record removed even though no Discord call was needed")
	}
}

func TestRevokeGrantMemberGoneCleansUpRecord(t *testing.T) {
	ops := newFakeOps() // no member registered
	store := newFakeStore()
	_ = store.InsertGrant(context.Background(), GrantRecord{GuildID: "g1", UserID: "u1", RoleID: "granted-role"})

	p := newTestPlugin(ops, store, newFakeSettings(), newFakeAudit(), newFakePerms(), newFakeScheduler())

	if err := p.revokeGrant(context.Background(), "g1", "u1", "granted-role", "mod1"); err != nil {
		t.Fatalf("revokeGrant: %v", err)
	}
	if _, ok, _ := store.GetGrant(context.Background(), "g1", "u1", "granted-role"); ok {
		t.Fatal("expected grant record removed for a member who left the guild")
	}
}

func TestSweepRevokesDueGrantsAndSkipsNotYetDue(t *testing.T) {
	ops := newFakeOps()
	ops.setMember("g1", "u1", []string{"role-a"})
	ops.setMember("g1", "u2", []string{"role-b"})

	store := newFakeStore()
	due := fixedNow.Add(-time.Minute)
	notDue := fixedNow.Add(time.Hour)
	_ = store.InsertGrant(context.Background(), GrantRecord{GuildID: "g1", UserID: "u1", RoleID: "role-a", ExpiresAt: &due})
	_ = store.InsertGrant(context.Background(), GrantRecord{GuildID: "g1", UserID: "u2", RoleID: "role-b", ExpiresAt: &notDue})

	p := newTestPlugin(ops, store, newFakeSettings(), newFakeAudit(), newFakePerms(), newFakeScheduler())

	if err := p.sweep(context.Background(), "g1"); err != nil {
		t.Fatalf("sweep: %v", err)
	}

	if _, ok, _ := store.GetGrant(context.Background(), "g1", "u1", "role-a"); ok {
		t.Fatal("expected due grant revoked and untracked")
	}
	if _, ok, _ := store.GetGrant(context.Background(), "g1", "u2", "role-b"); !ok {
		t.Fatal("expected not-yet-due grant left untouched")
	}
}

// TestRevokeGrantKeepsTrackingOnTransientFetchFailure mirrors the jail case:
// untracking a timed grant because Discord hiccuped would silently turn "for
// 24 hours" into permanent, with no record left that it was ever temporary.
func TestRevokeGrantKeepsTrackingOnTransientFetchFailure(t *testing.T) {
	ops := newFakeOps()
	ops.setMember("g1", "u1", []string{"role-a"})
	ops.memberFetchErr = transientErr()

	p := newTestPlugin(ops, newFakeStore(), newFakeSettings(), newFakeAudit(), newFakePerms(), newFakeScheduler())
	if err := p.store.InsertGrant(context.Background(), GrantRecord{GuildID: "g1", UserID: "u1", RoleID: "role-a"}); err != nil {
		t.Fatalf("InsertGrant: %v", err)
	}

	if err := p.revokeGrant(context.Background(), "g1", "u1", "role-a", "system"); err == nil {
		t.Fatal("expected revokeGrant to report the transient fetch failure")
	}
	if _, ok, _ := p.store.GetGrant(context.Background(), "g1", "u1", "role-a"); !ok {
		t.Fatal("grant record was dropped on a transient error — the role would never expire")
	}

	ops.memberFetchErr = nil
	if err := p.revokeGrant(context.Background(), "g1", "u1", "role-a", "system"); err != nil {
		t.Fatalf("retry revokeGrant: %v", err)
	}
	m, _ := ops.GuildMember("g1", "u1")
	if len(m.Roles) != 0 {
		t.Fatalf("expected the granted role removed on retry, got %v", m.Roles)
	}
}
