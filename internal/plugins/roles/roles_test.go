package roles

import (
	"context"
	"testing"
	"time"

	"github.com/disgoorg/disgo/discord"

	"github.com/6586x57890143/merlin/internal/core"
	"github.com/6586x57890143/merlin/internal/voice"
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

// NextDue is unused by this plugin: roles has one fixed sweep and nothing
// that counts down to it.
func (f *fakeScheduler) NextDue(ctx context.Context, jobKey string) (time.Time, bool, error) {
	return time.Time{}, false, nil
}

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
		vacationRoleID:    make(map[string]string),
		joinRoleSeen:      make(map[string]joinRoleShape),
		timers:            make(map[string]*time.Timer),
		inFlight:          make(map[string]bool),
		afterFunc:         time.AfterFunc,
		voice:             testVoice(),
		voiceChannelOf:    func(string, string) (string, bool) { return "", false },
	}
}

// testVoice is the real catalog, not a stub. The DM a jailed member gets is
// text that reaches an actual person, so substituting fixed strings here
// would exercise the plumbing while leaving the message unchecked.
func testVoice() voice.Source {
	sp, err := voice.New(testLogger())
	if err != nil {
		panic("voice catalog does not load: " + err.Error())
	}
	return sp
}

func TestResolveJailRoleCreatesWhenMissing(t *testing.T) {
	ops := newFakeOps()
	p := newTestPlugin(ops, newFakeStore(), newFakeSettings(), newFakeAudit(), newFakePerms(), newFakeScheduler())

	id, err := p.resolveJailRole("7000001")
	if err != nil {
		t.Fatalf("resolveJailRole: %v", err)
	}
	if id == "" {
		t.Fatal("expected a non-empty role ID")
	}
	roles := ops.roles["7000001"]
	if len(roles) != 1 || roles[0].Name != jailRoleName {
		t.Fatalf("expected exactly one %q role created, got %+v", jailRoleName, roles)
	}
}

func TestResolveJailRoleReusesExisting(t *testing.T) {
	ops := newFakeOps()
	ops.roles["7000001"] = []*frole{{ID: "7000333", Name: jailRoleName}}
	p := newTestPlugin(ops, newFakeStore(), newFakeSettings(), newFakeAudit(), newFakePerms(), newFakeScheduler())

	id, err := p.resolveJailRole("7000001")
	if err != nil {
		t.Fatalf("resolveJailRole: %v", err)
	}
	if id != "7000333" {
		t.Fatalf("expected to reuse existing role, got %q", id)
	}
	// Must not have created a second one.
	if len(ops.roles["7000001"]) != 1 {
		t.Fatalf("expected no new role created, got %d roles", len(ops.roles["7000001"]))
	}
}

func TestResolveJailRoleUsesConfiguredMarkerRoleDirectly(t *testing.T) {
	ops := newFakeOps()
	ops.roles["7000001"] = []*frole{{ID: "7000324", Name: "Marker"}}
	settings := newFakeSettings()
	settings.markerRole["7000001"] = "7000324"
	p := newTestPlugin(ops, newFakeStore(), settings, newFakeAudit(), newFakePerms(), newFakeScheduler())

	id, err := p.resolveJailRole("7000001")
	if err != nil {
		t.Fatalf("resolveJailRole: %v", err)
	}
	if id != "7000324" {
		t.Fatalf("expected configured marker role to be used directly, got %q", id)
	}
	if len(ops.roles["7000001"]) != 1 {
		t.Fatalf("expected no new role created, got %d roles", len(ops.roles["7000001"]))
	}
}

// Cold resolves establish restrictions; the overwrite diff keeps subsequent
// reconciliations quiet when permissions already match.
func TestResolveJailRoleColdCacheReconcilesConfiguredMarkerRole(t *testing.T) {
	ops := newFakeOps()
	ops.roles["7000001"] = []*frole{{ID: "7000324", Name: "Marker"}}
	ops.channel["7000602"] = &fch{ID: "7000602", GuildID: "7000001", Type: discord.ChannelTypeGuildText}
	settings := newFakeSettings()
	settings.markerRole["7000001"] = "7000324"
	p := newTestPlugin(ops, newFakeStore(), settings, newFakeAudit(), newFakePerms(), newFakeScheduler())

	// No prior sync has happened; this simulates the first resolve after a
	// fresh process start finds an already-configured marker role.
	if _, err := p.resolveJailRole("7000001"); err != nil {
		t.Fatalf("resolveJailRole: %v", err)
	}
	if ow := ops.overwrites[overwriteKey{"7000602", "7000324"}]; ow.deny&discord.PermissionViewChannel == 0 {
		t.Fatal("cold resolve must establish restrictions before assigning a marker")
	}
}

func TestResolveJailRoleUsesMeltingPotDefaultWhenPresent(t *testing.T) {
	ops := newFakeOps()
	ops.roles[meltingPotGuildID] = []*frole{{ID: meltingPotDefaultJailRoleID, Name: "Melting Pot Jail Marker"}}
	p := newTestPlugin(ops, newFakeStore(), newFakeSettings(), newFakeAudit(), newFakePerms(), newFakeScheduler())

	id, err := p.resolveJailRole(meltingPotGuildID)
	if err != nil {
		t.Fatalf("resolveJailRole: %v", err)
	}
	if id != meltingPotDefaultJailRoleID {
		t.Fatalf("expected melting pot default jail role to be used, got %q", id)
	}
	if len(ops.roles[meltingPotGuildID]) != 1 {
		t.Fatalf("expected no new role created, got %d roles", len(ops.roles[meltingPotGuildID]))
	}
}

func TestResolveJailRoleIsCachedPerGuild(t *testing.T) {
	ops := newFakeOps()
	p := newTestPlugin(ops, newFakeStore(), newFakeSettings(), newFakeAudit(), newFakePerms(), newFakeScheduler())

	first, err := p.resolveJailRole("7000001")
	if err != nil {
		t.Fatalf("resolveJailRole: %v", err)
	}
	second, err := p.resolveJailRole("7000001")
	if err != nil {
		t.Fatalf("resolveJailRole: %v", err)
	}
	if first != second {
		t.Fatalf("expected cached role ID, got %q then %q", first, second)
	}
	if len(ops.roles["7000001"]) != 1 {
		t.Fatalf("expected exactly one role ever created, got %d", len(ops.roles["7000001"]))
	}
}

func TestSyncGuildRegistersSweepJobOnce(t *testing.T) {
	sched := newFakeScheduler()
	p := newTestPlugin(newFakeOps(), newFakeStore(), newFakeSettings(), newFakeAudit(), newFakePerms(), sched)

	p.SyncGuild("7000001")
	p.SyncGuild("7000001") // idempotent: must not attempt a second Register

	if !sched.registered["7000001:roles-sweep"] {
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
	ops.roles["7000001"] = []*frole{{ID: "7000397", Name: jailRoleName}}
	p := newTestPlugin(ops, newFakeStore(), newFakeSettings(), newFakeAudit(), newFakePerms(), newFakeScheduler())

	first, err := p.resolveJailRole("7000001")
	if err != nil {
		t.Fatalf("resolveJailRole: %v", err)
	}
	if first != "7000397" {
		t.Fatalf("expected the existing role to be reused, got %q", first)
	}

	// The mod deletes it in Discord, and the bot is told so.
	ops.roles["7000001"] = nil
	p.forgetJailRole("7000001")

	second, err := p.resolveJailRole("7000001")
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
// cmd/bot/main.go drives on every GuildCreate; Discord re-sends those on
// every reconnect, and the Scheduler rejects a duplicate job key, so a
// second call must be a no-op rather than an error storm.
func TestSyncGuildIsIdempotentAcrossRepeatedCalls(t *testing.T) {
	sched := newFakeScheduler()
	p := newTestPlugin(newFakeOps(), newFakeStore(), newFakeSettings(), newFakeAudit(), newFakePerms(), sched)

	for range 3 {
		p.SyncGuild("7000001")
	}
	if len(sched.registered) != 1 {
		t.Fatalf("expected exactly 1 registered job across repeated SyncGuild calls, got %v", sched.registered)
	}
}

// The jail role ID is cached for the process lifetime once resolved, which
// is deliberate: renaming the role must not spawn a duplicate. The cost is
// that deleting it leaves the cache pointing at an ID Discord no longer
// knows, and without this handler every jail in that guild keeps failing
// against the dead ID until somebody restarts the process.
func TestDeletingTheJailRoleDropsTheCachedID(t *testing.T) {
	ops := newFakeOps()
	p := newTestPlugin(ops, newFakeStore(), newFakeSettings(), newFakeAudit(), newFakePerms(), newFakeScheduler())

	first, err := p.resolveJailRole("7000001")
	if err != nil {
		t.Fatalf("resolveJailRole: %v", err)
	}

	// The role really is gone from Discord's side, not just from the cache.
	ops.deleteRole("7000001", first)
	p.HandleRoleDeleted(context.Background(), "7000001", first)

	second, err := p.resolveJailRole("7000001")
	if err != nil {
		t.Fatalf("resolveJailRole after deletion: %v", err)
	}
	if second == first {
		t.Errorf("still resolving to the deleted role %s; the next jail would fail against an ID Discord does not know", first)
	}
}

// Guilds delete roles all the time, and all but one of them are none of
// this plugin's business. Dropping the cache on an unrelated deletion would
// mean re-listing the guild's roles on the next jail for no reason, and in
// the worst case creating a second marker role.
func TestDeletingAnUnrelatedRoleLeavesTheCacheAlone(t *testing.T) {
	ops := newFakeOps()
	p := newTestPlugin(ops, newFakeStore(), newFakeSettings(), newFakeAudit(), newFakePerms(), newFakeScheduler())

	first, err := p.resolveJailRole("7000001")
	if err != nil {
		t.Fatalf("resolveJailRole: %v", err)
	}

	p.HandleRoleDeleted(context.Background(), "7000001", "7000327")
	p.HandleRoleDeleted(context.Background(), "7000003", first)

	second, err := p.resolveJailRole("7000001")
	if err != nil {
		t.Fatalf("resolveJailRole: %v", err)
	}
	if second != first {
		t.Errorf("cache was dropped for an unrelated deletion: %s became %s", first, second)
	}
}

// A deletion arriving for a guild this process has never jailed in must be
// a no-op rather than a panic on a nil map read or a spurious warning.
func TestRoleDeletedBeforeAnyJailIsHarmless(t *testing.T) {
	p := newTestPlugin(newFakeOps(), newFakeStore(), newFakeSettings(), newFakeAudit(), newFakePerms(), newFakeScheduler())
	p.HandleRoleDeleted(context.Background(), "7000124", "7000328")
}
