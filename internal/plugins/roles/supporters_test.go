package roles

import (
	"context"
	"testing"
	"time"
)

func TestSyncSupporters(t *testing.T) {
	const g, role = meltingPotGuildID, meltingPotSupporterRoleID
	const fresh, byHand, renewing, absent, lapsed, jailed = "7000101", "7000102", "7000103", "7000104", "7000105", "7000106"
	ctx := context.Background()
	ops := newFakeOps()
	for _, u := range []string{fresh, renewing, lapsed, jailed} {
		ops.setMember(g, u, nil)
	}
	ops.setMember(g, byHand, []string{role})
	store := newFakeStore()
	audit := newFakeAudit()
	p := newTestPlugin(ops, store, newFakeSettings(), audit, newFakePerms(), newFakeScheduler())
	now := p.now()
	soon, later := now.Add(24*time.Hour), now.Add(30*24*time.Hour)
	_ = store.InsertGrant(ctx, GrantRecord{GuildID: g, UserID: renewing, RoleID: role, ExpiresAt: &soon})
	_ = store.InsertJail(ctx, JailRecord{GuildID: g, UserID: jailed})
	backers := []Backer{
		{UserID: fresh, Until: later},
		{UserID: byHand, Until: later},
		{UserID: renewing, Until: later},
		{UserID: absent, Until: later},
		{UserID: lapsed, Until: now.Add(-time.Hour)},
		{UserID: jailed, Until: later},
	}
	p.WithSupporters(SupportersFunc(func(context.Context) ([]Backer, error) { return backers, nil }))

	if err := p.syncSupporters(ctx, g, role); err != nil {
		t.Fatalf("sync: %v", err)
	}
	for u, want := range map[string]bool{fresh: true, renewing: true, lapsed: false, jailed: false} {
		m, _ := ops.GuildMember(g, u)
		if got := len(m.Roles) == 1; got != want {
			t.Errorf("%s holds the role: %v, want %v", u, got, want)
		}
	}
	for _, u := range []string{fresh, renewing} {
		rec, ok, _ := store.GetGrant(ctx, g, u, role)
		if !ok || rec.ExpiresAt == nil || !rec.ExpiresAt.Equal(later) {
			t.Errorf("%s: grant %+v, %v; want ending %v", u, rec, ok, later)
		}
	}
	if _, ok, _ := store.GetGrant(ctx, g, byHand, role); ok {
		t.Error("a role given by hand became a grant that would expire")
	}
	if len(audit.records) != 2 {
		t.Errorf("audited %d, want a grant and a renewal", len(audit.records))
	}

	// A second read with nothing new writes nothing.
	calls := len(ops.roleAddCalls)
	if err := p.syncSupporters(ctx, g, role); err != nil || len(ops.roleAddCalls) != calls || len(audit.records) != 2 {
		t.Errorf("a repeat read changed something: %v, %d adds, %d audits", err, len(ops.roleAddCalls)-calls, len(audit.records))
	}

	// An earlier end never shortens a grant.
	backers = []Backer{{UserID: fresh, Until: soon}}
	if err := p.syncSupporters(ctx, g, role); err != nil {
		t.Fatal(err)
	}
	if rec, _, _ := store.GetGrant(ctx, g, fresh, role); !rec.ExpiresAt.Equal(later) {
		t.Errorf("grant moved to %v", rec.ExpiresAt)
	}
}

func TestSupporterSyncOnlyWhereThereIsARole(t *testing.T) {
	sched := newFakeScheduler()
	p := newTestPlugin(newFakeOps(), newFakeStore(), newFakeSettings(), newFakeAudit(), newFakePerms(), sched)
	p.registerSupporterSync(meltingPotGuildID)
	p.WithSupporters(SupportersFunc(func(context.Context) ([]Backer, error) { return nil, nil }))
	p.registerSupporterSync("7000001")
	if len(sched.registered) != 0 {
		t.Fatalf("registered %d jobs without a link or a role", len(sched.registered))
	}
	p.registerSupporterSync(meltingPotGuildID)
	if len(sched.registered) != 1 {
		t.Fatalf("registered %d jobs for the Melting Pot", len(sched.registered))
	}
}
