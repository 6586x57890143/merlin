package roles

import (
	"context"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/6586x57890143/merlin/internal/core"
)

var fixedNow = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// fakeScheduler is an in-memory core.Scheduler for testing SyncGuild without
// a live Postgres-backed Scheduler.
type fakeScheduler struct {
	registered map[string]bool
	seeded     map[string]time.Time
}

func newFakeScheduler() *fakeScheduler {
	return &fakeScheduler{registered: make(map[string]bool), seeded: make(map[string]time.Time)}
}

func (f *fakeScheduler) Register(jobKey string, spec core.CronSpec, fn func(ctx context.Context) error) error {
	if f.registered[jobKey] {
		return context.DeadlineExceeded
	}
	f.registered[jobKey] = true
	return nil
}

func (f *fakeScheduler) Unregister(jobKey string) error {
	delete(f.registered, jobKey)
	return nil
}

func (f *fakeScheduler) RunNow(ctx context.Context, jobKey string) error { return nil }

func (f *fakeScheduler) Seed(ctx context.Context, jobKey string, at time.Time) error {
	f.seeded[jobKey] = at
	return nil
}

func newTestPlugin(ops *fakeOps, store *fakeStore, settings *fakeSettings, audit *fakeAudit, perms *fakePerms, sched *fakeScheduler) *Plugin {
	return &Plugin{
		ops:               func(string) DiscordMemberOps { return ops },
		dryRun:            func(string) bool { return false },
		store:             store,
		jailChannelConfig: settings,
		perms:             perms,
		audit:             audit,
		log:               testLogger(),
		sched:             sched,
		now:               func() time.Time { return fixedNow },
		sweepRegistered:   make(map[string]bool),
		jailRoleID:        make(map[string]string),
	}
}

func TestResolveJailRoleCreatesWhenMissing(t *testing.T) {
	ops := newFakeOps()
	p := newTestPlugin(ops, newFakeStore(), newFakeSettings(), newFakeAudit(), newFakePerms(), newFakeScheduler())

	id, err := p.resolveJailRole("g1")
	if err != nil {
		t.Fatalf("resolveJailRole: %v", err)
	}
	if id == "" {
		t.Fatal("expected a non-empty role ID")
	}
	roles, _ := ops.GuildRoles("g1")
	if len(roles) != 1 || roles[0].Name != jailRoleName {
		t.Fatalf("expected exactly one %q role created, got %+v", jailRoleName, roles)
	}
}

func TestResolveJailRoleReusesExisting(t *testing.T) {
	ops := newFakeOps()
	ops.roles["g1"] = []*discordgo.Role{{ID: "existing-jail-role", Name: jailRoleName}}
	p := newTestPlugin(ops, newFakeStore(), newFakeSettings(), newFakeAudit(), newFakePerms(), newFakeScheduler())

	id, err := p.resolveJailRole("g1")
	if err != nil {
		t.Fatalf("resolveJailRole: %v", err)
	}
	if id != "existing-jail-role" {
		t.Fatalf("expected to reuse existing role, got %q", id)
	}
	// Must not have created a second one.
	if len(ops.roles["g1"]) != 1 {
		t.Fatalf("expected no new role created, got %d roles", len(ops.roles["g1"]))
	}
}

func TestResolveJailRoleIsCachedPerGuild(t *testing.T) {
	ops := newFakeOps()
	p := newTestPlugin(ops, newFakeStore(), newFakeSettings(), newFakeAudit(), newFakePerms(), newFakeScheduler())

	first, err := p.resolveJailRole("g1")
	if err != nil {
		t.Fatalf("resolveJailRole: %v", err)
	}
	second, err := p.resolveJailRole("g1")
	if err != nil {
		t.Fatalf("resolveJailRole: %v", err)
	}
	if first != second {
		t.Fatalf("expected cached role ID, got %q then %q", first, second)
	}
	if len(ops.roles["g1"]) != 1 {
		t.Fatalf("expected exactly one role ever created, got %d", len(ops.roles["g1"]))
	}
}

func TestSyncGuildRegistersSweepJobOnce(t *testing.T) {
	sched := newFakeScheduler()
	p := newTestPlugin(newFakeOps(), newFakeStore(), newFakeSettings(), newFakeAudit(), newFakePerms(), sched)

	p.SyncGuild("g1")
	p.SyncGuild("g1") // idempotent: must not attempt a second Register

	if !sched.registered["g1:roles-sweep"] {
		t.Fatal("expected roles-sweep job registered for g1")
	}
	if len(sched.registered) != 1 {
		t.Fatalf("expected exactly one registered job, got %d", len(sched.registered))
	}
}

// TestForgetJailRoleAllowsRecreation covers recovery from a mod deleting the
// Jailed role in Discord. The resolved ID is cached for the process
// lifetime, so without invalidation every subsequent jail in that guild
// would keep failing against a dead role ID until the bot restarted.
func TestForgetJailRoleAllowsRecreation(t *testing.T) {
	ops := newFakeOps()
	ops.roles["g1"] = []*discordgo.Role{{ID: "existing-jail", Name: jailRoleName}}
	p := newTestPlugin(ops, newFakeStore(), newFakeSettings(), newFakeAudit(), newFakePerms(), newFakeScheduler())

	first, err := p.resolveJailRole("g1")
	if err != nil {
		t.Fatalf("resolveJailRole: %v", err)
	}
	if first != "existing-jail" {
		t.Fatalf("expected the existing role to be reused, got %q", first)
	}

	// The mod deletes it in Discord, and the bot is told so.
	ops.roles["g1"] = nil
	p.forgetJailRole("g1")

	second, err := p.resolveJailRole("g1")
	if err != nil {
		t.Fatalf("resolveJailRole after invalidation: %v", err)
	}
	if second == first {
		t.Fatal("expected a fresh jail role to be created after the cache was invalidated")
	}
	if second == "" {
		t.Fatal("expected a non-empty recreated jail role ID")
	}
}

// TestSyncGuildIsIdempotentAcrossRepeatedCalls guards the registration path
// cmd/bot/main.go drives on every GuildCreate — Discord re-sends those on
// every reconnect, and the Scheduler rejects a duplicate job key, so a
// second call must be a no-op rather than an error storm.
func TestSyncGuildIsIdempotentAcrossRepeatedCalls(t *testing.T) {
	sched := newFakeScheduler()
	p := newTestPlugin(newFakeOps(), newFakeStore(), newFakeSettings(), newFakeAudit(), newFakePerms(), sched)

	for range 3 {
		p.SyncGuild("g1")
	}
	if len(sched.registered) != 1 {
		t.Fatalf("expected exactly 1 registered job across repeated SyncGuild calls, got %v", sched.registered)
	}
}
