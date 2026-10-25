package roles

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/6586x57890143/merlin/internal/scheduler"
)

func TestSyncSupporters(t *testing.T) {
	const g, role = meltingPotGuildID, testSupporterRole
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

// testSupporterRole is the role a test guild's supporters are given.
const testSupporterRole = "7000450"

func TestSupporterSyncRunsOnlyWhereSupportersAreOn(t *testing.T) {
	sched := newFakeScheduler()
	settings := newFakeSettings()
	p := newTestPlugin(newFakeOps(), newFakeStore(), settings, newFakeAudit(), newFakePerms(), sched)
	p.registerSupporterSync(meltingPotGuildID)
	if len(sched.registered) != 0 {
		t.Fatal("registered without the fleet link")
	}
	asked := 0
	p.WithSupporters(SupportersFunc(func(context.Context) ([]Backer, error) { asked++; return nil, nil }))
	p.registerSupporterSync(meltingPotGuildID)
	if len(sched.registered) != 1 {
		t.Fatalf("registered %d jobs", len(sched.registered))
	}
	// Off: the job asks cygnet nothing.
	if err := sched.run(t, scheduler.JobKey(meltingPotGuildID, "roles-supporters")); err != nil || asked != 0 {
		t.Fatalf("off: %v, asked %d", err, asked)
	}
	_ = settings.SetSupporterRole(context.Background(), meltingPotGuildID, testSupporterRole)
	if err := sched.run(t, scheduler.JobKey(meltingPotGuildID, "roles-supporters")); err != nil || asked != 1 {
		t.Fatalf("on: %v, asked %d", err, asked)
	}
}

func TestTurningSupportersOnMakesOneRole(t *testing.T) {
	ops := newFakeOps()
	settings := newFakeSettings()
	p := newTestPlugin(ops, newFakeStore(), settings, newFakeAudit(), newFakePerms(), newFakeScheduler())
	id, err := p.createSupporterRole(context.Background(), meltingPotGuildID)
	if err != nil || id == "" || settings.SupporterRoleID(meltingPotGuildID) != id {
		t.Fatalf("created %q, %v; settings say %q", id, err, settings.SupporterRoleID(meltingPotGuildID))
	}
	again, err := p.createSupporterRole(context.Background(), meltingPotGuildID)
	if err != nil || again != id {
		t.Fatalf("a second turn on made %q, %v", again, err)
	}
}

func TestASupporterIsRemindedOnceAWeekOut(t *testing.T) {
	const g, role, u = meltingPotGuildID, testSupporterRole, "7000201"
	ctx := context.Background()
	ops := newFakeOps()
	ops.setMember(g, u, nil)
	p := newTestPlugin(ops, newFakeStore(), newFakeSettings(), newFakeAudit(), newFakePerms(), newFakeScheduler())
	until := p.now().Add(30 * 24 * time.Hour)
	p.WithSupporters(SupportersFunc(func(context.Context) ([]Backer, error) { return []Backer{{UserID: u, Until: until}}, nil }))
	if err := p.syncSupporters(ctx, g, role); err != nil || len(ops.dmSends) != 0 {
		t.Fatalf("a month out: %v, %d DMs", err, len(ops.dmSends))
	}
	until = p.now().Add(5 * 24 * time.Hour)
	for range 2 {
		if err := p.syncSupporters(ctx, g, role); err != nil {
			t.Fatal(err)
		}
	}
	if len(ops.dmSends) != 1 {
		t.Fatalf("%d reminders, want one", len(ops.dmSends))
	}
	// A renewal ends somewhere else, and is reminded again when it nears.
	until = until.Add(24 * time.Hour)
	if err := p.syncSupporters(ctx, g, role); err != nil || len(ops.dmSends) != 2 {
		t.Fatalf("after a renewal: %v, %d reminders", err, len(ops.dmSends))
	}
}

func TestNewSupportersGetOneThankYouCard(t *testing.T) {
	const g, role = meltingPotGuildID, testSupporterRole
	const a, b, renewing = "7000401", "7000402", "7000403"
	ctx := context.Background()
	ops := newFakeOps()
	for _, u := range []string{a, b, renewing} {
		ops.setMember(g, u, nil)
	}
	store := newFakeStore()
	settings := newFakeSettings()
	_ = settings.SetSupporterChannel(ctx, g, "7000499")
	p := newTestPlugin(ops, store, settings, newFakeAudit(), newFakePerms(), newFakeScheduler())
	soon, later := p.now().Add(20*24*time.Hour), p.now().Add(60*24*time.Hour)
	_ = store.InsertGrant(ctx, GrantRecord{GuildID: g, UserID: renewing, RoleID: role, ExpiresAt: &soon})
	backers := []Backer{{UserID: a, Until: later}, {UserID: b, Until: later}, {UserID: renewing, Until: later}}
	p.WithSupporters(SupportersFunc(func(context.Context) ([]Backer, error) { return backers, nil }))
	for range 2 {
		if err := p.syncSupporters(ctx, g, role); err != nil {
			t.Fatal(err)
		}
	}
	if len(ops.dmSends) != 1 {
		t.Fatalf("%d cards, want one", len(ops.dmSends))
	}
	card := ops.dmSends[0]
	desc := card.data.Embeds[0].Description
	if card.channelID != "7000499" || !strings.Contains(desc, "<@"+a+">") || !strings.Contains(desc, "<@"+b+">") || strings.Contains(desc, renewing) {
		t.Fatalf("card in %s: %q", card.channelID, desc)
	}
	if f := card.data.Embeds[0].Fields; len(f) != 1 || f[0].Value != supportURL {
		t.Fatalf("fields %+v", f)
	}
}
