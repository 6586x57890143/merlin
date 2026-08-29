package settings

import (
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/6586x57890143/merlin/internal/core"
	"github.com/6586x57890143/merlin/internal/dbtest"
)

// unreachablePool builds a pool that will never successfully connect,
// without needing TEST_DATABASE_URL or any live database at all.
// pgxpool.New is lazy (it parses config and returns immediately; nothing
// dials until the first query), so construction always succeeds, and the
// first query against it fails fast on connection refused. Used by the
// tests that need to observe what a genuine write/refresh failure does to
// the cache and stale set, which is not something a healthy database can
// ever produce on demand.
func unreachablePool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), "postgres://nobody:nobody@127.0.0.1:1/nowhere?sslmode=disable&connect_timeout=1")
	if err != nil {
		t.Fatalf("unreachablePool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// eventRecorder subscribes to core.EventConfigChanged and records every
// delivery, so a test can assert exactly how many mutations actually
// published rather than trusting that they did. EventBus.Publish runs every
// subscriber synchronously on the caller's goroutine (see
// internal/core/eventbus.go), so by the time a Store method returns, any
// event it published has already been recorded here: no sleeping, no
// polling, and no risk of a race between the mutator returning and the
// assertion running.
type eventRecorder struct {
	guildIDs []string
}

func (r *eventRecorder) record(_ context.Context, ev core.Event) {
	r.guildIDs = append(r.guildIDs, ev.GuildID)
}

func (r *eventRecorder) countFor(guildID string) int {
	n := 0
	for _, g := range r.guildIDs {
		if g == guildID {
			n++
		}
	}
	return n
}

// setupStore wires a Store against a real, migrated Postgres (dbtest.Pool),
// with a recorder attached to its EventBus so mutation tests can assert on
// EventConfigChanged, and a guild ID derived from the test's own name so
// concurrent or sequential tests never share a row on the shared database.
//
// The guild ID is stable across runs (it comes from the test name, not a
// random suffix), which is deliberate for readability while debugging a
// failure against the live database -- but it means a second local run
// against the same, persistent test database would otherwise collide with
// the first run's leftover rows. dbtest.Pool's database is not truncated
// between runs (only between CI jobs, where the service container is
// ephemeral), so cleanup here is what makes repeated local runs idempotent.
func setupStore(t *testing.T) (*Store, *eventRecorder, string) {
	t.Helper()
	pool := dbtest.Pool(t)
	bus := core.NewEventBus(slog.New(slog.DiscardHandler))
	rec := &eventRecorder{}
	bus.Subscribe(core.EventConfigChanged, "test", rec.record)

	guildID := "g-" + strings.NewReplacer("/", "-", " ", "-").Replace(t.Name())
	t.Cleanup(func() {
		ctx := context.Background()
		// settings_rotation_channels' rotation_notices rows cascade on
		// delete (migration 0017); nothing else references these tables.
		for _, table := range []string{"settings_guild", "settings_permission_overrides", "settings_rotation_channels"} {
			if _, err := pool.Exec(ctx, `DELETE FROM `+table+` WHERE guild_id = $1`, guildID); err != nil {
				t.Logf("cleanup: delete from %s for %s: %v", table, guildID, err)
			}
		}
	})
	return New(pool, bus), rec, guildID
}

// --- the fail-closed security boundary ---
//
// core.Permissions.Authorize reads mod roles, admins, and overrides
// exclusively through these accessors. Everything else in this file proves
// individual mutators round-trip correctly; these two prove the property the
// whole authorization model is actually built on.

// A guild that was never Refreshed at all -- never joined, never loaded,
// nothing -- must answer every privileged accessor as though nothing is
// configured, and every operational one as though nothing is wrong. Getting
// either direction backwards is a real incident: failing the privileged
// accessors open would mean a database blip grants moderator powers to
// nobody-in-particular, and failing the operational ones closed would mean
// the same blip pauses every guild's write path with no operator action.
func TestUnrefreshedGuildFailsClosedOnPrivilegedAccessorsAndOpenOnOperational(t *testing.T) {
	store, _, guildID := setupStore(t)

	if got := store.ModRoleIDs(guildID); len(got) != 0 {
		t.Errorf("ModRoleIDs = %v, want empty", got)
	}
	if got := store.AdminUserIDs(guildID); len(got) != 0 {
		t.Errorf("AdminUserIDs = %v, want empty", got)
	}
	if got := store.JailAllowedChannelIDs(guildID); len(got) != 0 {
		t.Errorf("JailAllowedChannelIDs = %v, want empty", got)
	}

	policy := store.ActionPolicy(guildID, "config.mutate")
	if policy.RequiredTier.IsSet() {
		t.Errorf("ActionPolicy.RequiredTier is set for a never-refreshed guild: %v", policy.RequiredTier)
	}
	if len(policy.AllowRoleIDs) != 0 || len(policy.AllowUserIDs) != 0 || len(policy.DenyRoleIDs) != 0 || len(policy.DenyUserIDs) != 0 {
		t.Errorf("ActionPolicy carries data for a never-refreshed guild: %+v", policy)
	}

	if got := store.DisabledPlugins(guildID); len(got) != 0 {
		t.Errorf("DisabledPlugins = %v, want empty", got)
	}
	if got := store.Overrides(guildID); len(got) != 0 {
		t.Errorf("Overrides = %v, want empty", got)
	}
	if got := store.RotationChannels(guildID); len(got) != 0 {
		t.Errorf("RotationChannels = %v, want empty", got)
	}
	if _, ok := store.RotationChannel(guildID, "any-channel"); ok {
		t.Error("RotationChannel found a channel for a never-refreshed guild")
	}

	// The inverted direction: these three read "safe" as permissive, not
	// restrictive, and a future change that flips one of them to fail
	// closed like the accessors above would freeze or disable every guild
	// on a database blip.
	if !store.PluginEnabled(guildID, "rotation") {
		t.Error("PluginEnabled is false for a never-refreshed guild; a DB blip would disable every plugin everywhere")
	}
	if store.WritesPaused(guildID) {
		t.Error("WritesPaused is true for a never-refreshed guild; a DB blip would freeze every guild's write path")
	}
	if store.WritesDryRun(guildID) {
		t.Error("WritesDryRun is true for a never-refreshed guild")
	}
}

// invalidate is a pure in-memory drop, never a Postgres revert. Writing real
// state, dropping the cache, and confirming the row survives in the database
// while the accessor reports fail-closed is the only way to actually prove
// that distinction.
func TestInvalidateDropsTheCacheAndMarksStale(t *testing.T) {
	store, _, guildID := setupStore(t)
	ctx := context.Background()

	if err := store.AddModRole(ctx, guildID, "role-1"); err != nil {
		t.Fatalf("AddModRole: %v", err)
	}
	if got := store.ModRoleIDs(guildID); len(got) != 1 || got[0] != "role-1" {
		t.Fatalf("ModRoleIDs before invalidate = %v, want [role-1]", got)
	}

	store.invalidate(guildID)

	if got := store.ModRoleIDs(guildID); len(got) != 0 {
		t.Errorf("ModRoleIDs after invalidate = %v, want empty (the Postgres row is untouched, only the cache dropped)", got)
	}
	store.mu.RLock()
	stale := store.stale[guildID]
	store.mu.RUnlock()
	if !stale {
		t.Error("invalidate did not mark the guild stale")
	}

	// Confirm the row really did survive: a fresh Refresh brings the same
	// data back, which invalidate reverting the DB write instead of the
	// cache would not.
	if err := store.Refresh(ctx, guildID); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if got := store.ModRoleIDs(guildID); len(got) != 1 || got[0] != "role-1" {
		t.Errorf("ModRoleIDs after re-Refresh = %v, want [role-1]; invalidate must not have touched Postgres", got)
	}
}

// Forget is for a guild the bot has actually left, where MarkStale/invalidate
// are for one whose settings are merely unreadable right now. Confusing the
// two would either retry-forever a guild that's really gone, or silently
// delete a guild's configuration on what might be a temporary kick.
func TestForgetDropsStaleWithoutTouchingPostgres(t *testing.T) {
	store, _, guildID := setupStore(t)
	ctx := context.Background()

	if err := store.AddAdmin(ctx, guildID, "user-1"); err != nil {
		t.Fatalf("AddAdmin: %v", err)
	}
	store.MarkStale(guildID)

	store.mu.RLock()
	_, staleBefore := store.stale[guildID]
	store.mu.RUnlock()
	if !staleBefore {
		t.Fatal("MarkStale did not mark the guild stale")
	}

	store.Forget(guildID)

	store.mu.RLock()
	_, staleAfter := store.stale[guildID]
	store.mu.RUnlock()
	if staleAfter {
		t.Error("Forget left the guild in the stale set")
	}

	if err := store.Refresh(ctx, guildID); err != nil {
		t.Fatalf("Refresh after Forget: %v", err)
	}
	if got := store.AdminUserIDs(guildID); len(got) != 1 || got[0] != "user-1" {
		t.Errorf("AdminUserIDs after Forget+Refresh = %v, want [user-1]; Forget must never delete Postgres rows", got)
	}
}

// The mutator shape documented at the top of the mutations section in
// settings.go has two distinct failure limbs: an Exec failure (the write
// itself never landed) and a post-write Refresh failure (the write landed
// but the cache could not be reloaded to match). Only the second one
// invalidates and marks the guild stale; the first leaves the cache and
// stale set untouched, because there is nothing to recover from and nothing
// to retry. Collapsing the two would either mark a guild stale on every
// ordinary validation-style failure, or leave a guild silently answering
// from a pre-mutation cache after a write that actually landed.
func TestWriteFailureLeavesCacheAndStaleUntouched(t *testing.T) {
	bus := core.NewEventBus(slog.New(slog.DiscardHandler))
	rec := &eventRecorder{}
	bus.Subscribe(core.EventConfigChanged, "test", rec.record)

	// pgxpool.New is lazy: this succeeds even though nothing is listening on
	// the far end, and the first query against it fails fast on connection
	// refused. No TEST_DATABASE_URL dependency for this one.
	brokenPool := unreachablePool(t)
	store := New(brokenPool, bus)
	guildID := "g-broken"

	if err := store.AddModRole(context.Background(), guildID, "role-1"); err == nil {
		t.Fatal("AddModRole against an unreachable database returned no error")
	}

	if len(rec.guildIDs) != 0 {
		t.Errorf("a failed write published %d EventConfigChanged events, want 0", len(rec.guildIDs))
	}
	store.mu.RLock()
	_, stale := store.stale[guildID]
	store.mu.RUnlock()
	if stale {
		t.Error("a pure Exec failure marked the guild stale; only a post-write Refresh failure should")
	}
}

// --- RetryStale ---

func TestRetryStaleRecoversAGuildAndRepublishesConfigChanged(t *testing.T) {
	store, rec, guildID := setupStore(t)
	ctx := context.Background()

	if err := store.AddModRole(ctx, guildID, "role-1"); err != nil {
		t.Fatalf("AddModRole: %v", err)
	}
	eventsBeforeStale := rec.countFor(guildID)

	store.MarkStale(guildID)
	if got := store.ModRoleIDs(guildID); len(got) != 0 {
		t.Fatalf("ModRoleIDs after MarkStale = %v, want empty", got)
	}

	if remaining := store.RetryStale(ctx); remaining != 0 {
		t.Fatalf("RetryStale returned %d still-stale guilds, want 0", remaining)
	}

	if got := store.ModRoleIDs(guildID); len(got) != 1 || got[0] != "role-1" {
		t.Errorf("ModRoleIDs after RetryStale = %v, want [role-1]", got)
	}
	if got := rec.countFor(guildID); got != eventsBeforeStale+1 {
		t.Errorf("RetryStale published %d new events for the recovered guild, want exactly 1 more", got-eventsBeforeStale)
	}
}

// A guild whose settings are unreadable because the database itself is
// unreachable (not just this one guild's row) must come back from RetryStale
// still counted as stale, and must not publish a recovery event it has not
// actually earned.
func TestRetryStaleReportsGuildsStillUnreadable(t *testing.T) {
	bus := core.NewEventBus(slog.New(slog.DiscardHandler))
	rec := &eventRecorder{}
	bus.Subscribe(core.EventConfigChanged, "test", rec.record)

	store := New(unreachablePool(t), bus)
	guildID := "g-unreachable"
	store.MarkStale(guildID)

	if remaining := store.RetryStale(context.Background()); remaining != 1 {
		t.Errorf("RetryStale returned %d, want 1 (the guild should still be stale)", remaining)
	}
	if len(rec.guildIDs) != 0 {
		t.Errorf("RetryStale published %d events for a guild it could not actually recover", len(rec.guildIDs))
	}
}

// --- mutator round trips ---

func TestModRoleRoundTrip(t *testing.T) {
	store, rec, guildID := setupStore(t)
	ctx := context.Background()

	if err := store.AddModRole(ctx, guildID, "role-1"); err != nil {
		t.Fatalf("AddModRole: %v", err)
	}
	if got := store.ModRoleIDs(guildID); len(got) != 1 || got[0] != "role-1" {
		t.Fatalf("ModRoleIDs after add = %v, want [role-1]", got)
	}

	if err := store.RemoveModRole(ctx, guildID, "role-1"); err != nil {
		t.Fatalf("RemoveModRole: %v", err)
	}
	if got := store.ModRoleIDs(guildID); len(got) != 0 {
		t.Errorf("ModRoleIDs after remove = %v, want empty", got)
	}

	if got := rec.countFor(guildID); got != 2 {
		t.Errorf("published %d events for add+remove, want 2", got)
	}
}

func TestAdminRoundTrip(t *testing.T) {
	store, rec, guildID := setupStore(t)
	ctx := context.Background()

	if err := store.AddAdmin(ctx, guildID, "user-1"); err != nil {
		t.Fatalf("AddAdmin: %v", err)
	}
	if got := store.AdminUserIDs(guildID); len(got) != 1 || got[0] != "user-1" {
		t.Fatalf("AdminUserIDs after add = %v, want [user-1]", got)
	}

	if err := store.RemoveAdmin(ctx, guildID, "user-1"); err != nil {
		t.Fatalf("RemoveAdmin: %v", err)
	}
	if got := store.AdminUserIDs(guildID); len(got) != 0 {
		t.Errorf("AdminUserIDs after remove = %v, want empty", got)
	}
	if got := rec.countFor(guildID); got != 2 {
		t.Errorf("published %d events for add+remove, want 2", got)
	}
}

func TestJailAllowedChannelRoundTrip(t *testing.T) {
	store, _, guildID := setupStore(t)
	ctx := context.Background()

	if err := store.AddJailAllowedChannel(ctx, guildID, "chan-1"); err != nil {
		t.Fatalf("AddJailAllowedChannel: %v", err)
	}
	if got := store.JailAllowedChannelIDs(guildID); len(got) != 1 || got[0] != "chan-1" {
		t.Fatalf("JailAllowedChannelIDs after add = %v, want [chan-1]", got)
	}

	if err := store.RemoveJailAllowedChannel(ctx, guildID, "chan-1"); err != nil {
		t.Fatalf("RemoveJailAllowedChannel: %v", err)
	}
	if got := store.JailAllowedChannelIDs(guildID); len(got) != 0 {
		t.Errorf("JailAllowedChannelIDs after remove = %v, want empty", got)
	}
}

// The announce channel is a single value, set and cleared through one
// setter, so the round trip has to cover the empty-string clear reaching
// Postgres as NULL and reading back as "".
func TestJailAnnounceChannelRoundTrip(t *testing.T) {
	store, _, guildID := setupStore(t)
	ctx := context.Background()

	if got := store.JailAnnounceChannelID(guildID); got != "" {
		t.Fatalf("JailAnnounceChannelID before any set = %q, want empty", got)
	}
	if err := store.SetJailAnnounceChannel(ctx, guildID, "chan-1"); err != nil {
		t.Fatalf("SetJailAnnounceChannel: %v", err)
	}
	if got := store.JailAnnounceChannelID(guildID); got != "chan-1" {
		t.Fatalf("JailAnnounceChannelID after set = %q, want chan-1", got)
	}

	if err := store.SetJailAnnounceChannel(ctx, guildID, ""); err != nil {
		t.Fatalf("SetJailAnnounceChannel(clear): %v", err)
	}
	if got := store.JailAnnounceChannelID(guildID); got != "" {
		t.Errorf("JailAnnounceChannelID after clear = %q, want empty", got)
	}
}

// GrantOverride and DenyOverride take roleID and userID together, with an
// empty string meaning "leave this column alone" (the CASE WHEN $3 = ''
// SQL). Granting a role, then separately granting a user for the same
// action, has to land both without either overwriting the other -- the one
// case in that SQL that has never run against real Postgres before.
func TestOverrideGrantDenyRoundTrip(t *testing.T) {
	store, _, guildID := setupStore(t)
	ctx := context.Background()
	const action = "config.mutate"

	if err := store.GrantOverride(ctx, guildID, action, "role-1", ""); err != nil {
		t.Fatalf("GrantOverride role: %v", err)
	}
	if err := store.GrantOverride(ctx, guildID, action, "", "user-1"); err != nil {
		t.Fatalf("GrantOverride user: %v", err)
	}
	policy := store.ActionPolicy(guildID, action)
	if len(policy.AllowRoleIDs) != 1 || policy.AllowRoleIDs[0] != "role-1" {
		t.Errorf("AllowRoleIDs = %v, want [role-1]; the user-only grant clobbered it", policy.AllowRoleIDs)
	}
	if len(policy.AllowUserIDs) != 1 || policy.AllowUserIDs[0] != "user-1" {
		t.Errorf("AllowUserIDs = %v, want [user-1]; the role-only grant clobbered it", policy.AllowUserIDs)
	}

	if err := store.RevokeOverride(ctx, guildID, action, "role-1", ""); err != nil {
		t.Fatalf("RevokeOverride role: %v", err)
	}
	policy = store.ActionPolicy(guildID, action)
	if len(policy.AllowRoleIDs) != 0 {
		t.Errorf("AllowRoleIDs after revoke = %v, want empty", policy.AllowRoleIDs)
	}
	if len(policy.AllowUserIDs) != 1 || policy.AllowUserIDs[0] != "user-1" {
		t.Errorf("AllowUserIDs after revoking only the role = %v, want [user-1] untouched", policy.AllowUserIDs)
	}

	if err := store.DenyOverride(ctx, guildID, action, "role-2", "user-2"); err != nil {
		t.Fatalf("DenyOverride: %v", err)
	}
	policy = store.ActionPolicy(guildID, action)
	if len(policy.DenyRoleIDs) != 1 || policy.DenyRoleIDs[0] != "role-2" {
		t.Errorf("DenyRoleIDs = %v, want [role-2]", policy.DenyRoleIDs)
	}
	if len(policy.DenyUserIDs) != 1 || policy.DenyUserIDs[0] != "user-2" {
		t.Errorf("DenyUserIDs = %v, want [user-2]", policy.DenyUserIDs)
	}

	if err := store.UndenyOverride(ctx, guildID, action, "role-2", "user-2"); err != nil {
		t.Fatalf("UndenyOverride: %v", err)
	}
	policy = store.ActionPolicy(guildID, action)
	if len(policy.DenyRoleIDs) != 0 || len(policy.DenyUserIDs) != 0 {
		t.Errorf("deny lists after undeny = role:%v user:%v, want both empty", policy.DenyRoleIDs, policy.DenyUserIDs)
	}
}

func TestSetActionTierAndClear(t *testing.T) {
	store, _, guildID := setupStore(t)
	ctx := context.Background()
	const action = "config.mutate"

	if err := store.SetActionTier(ctx, guildID, action, core.TierMod); err != nil {
		t.Fatalf("SetActionTier: %v", err)
	}
	if got := store.ActionPolicy(guildID, action).RequiredTier; got != core.TierMod {
		t.Errorf("RequiredTier = %v, want TierMod", got)
	}

	if err := store.ClearActionTier(ctx, guildID, action); err != nil {
		t.Fatalf("ClearActionTier: %v", err)
	}
	if got := store.ActionPolicy(guildID, action).RequiredTier; got.IsSet() {
		t.Errorf("RequiredTier after clear = %v, want unset", got)
	}
}

func TestPluginToggleRoundTrip(t *testing.T) {
	store, _, guildID := setupStore(t)
	ctx := context.Background()
	const plugin = "rotation"

	if !store.PluginEnabled(guildID, plugin) {
		t.Fatal("plugin reported disabled before anything ran")
	}

	if err := store.DisablePlugin(ctx, guildID, plugin); err != nil {
		t.Fatalf("DisablePlugin: %v", err)
	}
	if store.PluginEnabled(guildID, plugin) {
		t.Error("PluginEnabled is true right after DisablePlugin")
	}

	if err := store.EnablePlugin(ctx, guildID, plugin); err != nil {
		t.Fatalf("EnablePlugin: %v", err)
	}
	if !store.PluginEnabled(guildID, plugin) {
		t.Error("PluginEnabled is false after EnablePlugin")
	}
}

// setWriteControl builds its SQL by interpolating a column name via
// fmt.Sprintf. Both of its callers are internal and pass a fixed literal, so
// this is not an injection risk, but nothing had ever proven the two
// literals actually address different columns -- a copy-paste slip in
// either constant would make pause and dry-run permanently aliased.
func TestWriteControlsAddressDifferentColumns(t *testing.T) {
	store, _, guildID := setupStore(t)
	ctx := context.Background()

	if err := store.SetWritesPaused(ctx, guildID, true); err != nil {
		t.Fatalf("SetWritesPaused: %v", err)
	}
	if !store.WritesPaused(guildID) {
		t.Error("WritesPaused is false after SetWritesPaused(true)")
	}
	if store.WritesDryRun(guildID) {
		t.Error("WritesDryRun became true as a side effect of SetWritesPaused")
	}

	if err := store.SetWritesDryRun(ctx, guildID, true); err != nil {
		t.Fatalf("SetWritesDryRun: %v", err)
	}
	if !store.WritesDryRun(guildID) {
		t.Error("WritesDryRun is false after SetWritesDryRun(true)")
	}
	if !store.WritesPaused(guildID) {
		t.Error("WritesPaused was cleared as a side effect of SetWritesDryRun")
	}

	if err := store.SetWritesPaused(ctx, guildID, false); err != nil {
		t.Fatalf("SetWritesPaused(false): %v", err)
	}
	if store.WritesPaused(guildID) {
		t.Error("WritesPaused is true after SetWritesPaused(false)")
	}
	if !store.WritesDryRun(guildID) {
		t.Error("WritesDryRun was cleared as a side effect of SetWritesPaused(false)")
	}
}

func TestChannelSettersRoundTrip(t *testing.T) {
	store, _, guildID := setupStore(t)
	ctx := context.Background()

	if err := store.SetAuditLogChannel(ctx, guildID, "audit-chan"); err != nil {
		t.Fatalf("SetAuditLogChannel: %v", err)
	}
	if err := store.SetStatusChannel(ctx, guildID, "status-chan"); err != nil {
		t.Fatalf("SetStatusChannel: %v", err)
	}

	gs := store.GuildSettings(guildID)
	if gs.AuditLogChannelID != "audit-chan" {
		t.Errorf("AuditLogChannelID = %q, want audit-chan", gs.AuditLogChannelID)
	}
	if gs.StatusChannelID != "status-chan" {
		t.Errorf("StatusChannelID = %q, want status-chan", gs.StatusChannelID)
	}
	if store.AuditLogChannelID(guildID) != "audit-chan" {
		t.Errorf("AuditLogChannelID() = %q, want audit-chan", store.AuditLogChannelID(guildID))
	}
	if store.StatusChannelID(guildID) != "status-chan" {
		t.Errorf("StatusChannelID() = %q, want status-chan", store.StatusChannelID(guildID))
	}
}

func TestMarkOnboardingNudgeSent(t *testing.T) {
	store, _, guildID := setupStore(t)
	ctx := context.Background()

	if got := store.GuildSettings(guildID).OnboardingNudgeSentAt; got != nil {
		t.Fatalf("OnboardingNudgeSentAt = %v before anything ran, want nil", got)
	}

	if err := store.MarkOnboardingNudgeSent(ctx, guildID); err != nil {
		t.Fatalf("MarkOnboardingNudgeSent: %v", err)
	}
	if got := store.GuildSettings(guildID).OnboardingNudgeSentAt; got == nil {
		t.Error("OnboardingNudgeSentAt is still nil after MarkOnboardingNudgeSent")
	}
}

// --- rotation channels ---

func fullyPopulatedRotationChannel(guildID, channelID string) RotationChannel {
	hours := 72
	return RotationChannel{
		GuildID:                 guildID,
		ChannelID:               channelID,
		IntervalMinutes:         90,
		ArchiveCategoryID:       "cat-1",
		ArchiveVisibility:       "whitelist",
		ArchiveWhitelistRoleIDs: []string{"role-a", "role-b"},
		ArchiveWhitelistUserIDs: []string{"user-a"},
		RetentionHours:          &hours,
		StickyEnabled:           true,
		StickyMessages:          []string{"first sticky", "second sticky"},
		NoticeLeadMinutes:       10,
		Disclosure:              DisclosureCadence,
	}
}

// The Upsert path is the one this whole test file exists partly because of:
// a 12-column INSERT/UPDATE, the Refresh SELECT+Scan positional pairing
// that has to stay in sync with it, and the string<->Disclosure named-type
// conversion settings.go's own comment flagged as never having run against
// real Postgres. This proves every field survives the round trip, not just
// the ones a hand-check would think to look at.
func TestUpsertRotationChannelRoundTripsEveryField(t *testing.T) {
	store, _, guildID := setupStore(t)
	ctx := context.Background()

	want := fullyPopulatedRotationChannel(guildID, "chan-1")
	if err := store.UpsertRotationChannel(ctx, want); err != nil {
		t.Fatalf("UpsertRotationChannel: %v", err)
	}

	got, ok := store.RotationChannel(guildID, "chan-1")
	if !ok {
		t.Fatal("RotationChannel did not find the channel just upserted")
	}

	if got.IntervalMinutes != want.IntervalMinutes {
		t.Errorf("IntervalMinutes = %d, want %d", got.IntervalMinutes, want.IntervalMinutes)
	}
	if got.ArchiveCategoryID != want.ArchiveCategoryID {
		t.Errorf("ArchiveCategoryID = %q, want %q", got.ArchiveCategoryID, want.ArchiveCategoryID)
	}
	if got.ArchiveVisibility != want.ArchiveVisibility {
		t.Errorf("ArchiveVisibility = %q, want %q", got.ArchiveVisibility, want.ArchiveVisibility)
	}
	if !stringSlicesEqual(got.ArchiveWhitelistRoleIDs, want.ArchiveWhitelistRoleIDs) {
		t.Errorf("ArchiveWhitelistRoleIDs = %v, want %v", got.ArchiveWhitelistRoleIDs, want.ArchiveWhitelistRoleIDs)
	}
	if !stringSlicesEqual(got.ArchiveWhitelistUserIDs, want.ArchiveWhitelistUserIDs) {
		t.Errorf("ArchiveWhitelistUserIDs = %v, want %v", got.ArchiveWhitelistUserIDs, want.ArchiveWhitelistUserIDs)
	}
	if got.RetentionHours == nil || *got.RetentionHours != *want.RetentionHours {
		t.Errorf("RetentionHours = %v, want %d", got.RetentionHours, *want.RetentionHours)
	}
	if got.StickyEnabled != want.StickyEnabled {
		t.Errorf("StickyEnabled = %v, want %v", got.StickyEnabled, want.StickyEnabled)
	}
	if !stringSlicesEqual(got.StickyMessages, want.StickyMessages) {
		t.Errorf("StickyMessages = %v, want %v", got.StickyMessages, want.StickyMessages)
	}
	if got.NoticeLeadMinutes != want.NoticeLeadMinutes {
		t.Errorf("NoticeLeadMinutes = %d, want %d", got.NoticeLeadMinutes, want.NoticeLeadMinutes)
	}
	// The one field settings.go's own comment worried about: it is scanned
	// through a plain string and converted, specifically because a named
	// string type's pgx codec path had never been exercised for real.
	if got.Disclosure != want.Disclosure {
		t.Errorf("Disclosure = %q, want %q", got.Disclosure, want.Disclosure)
	}
	if got.ID == 0 {
		t.Error("ID was not assigned a stable identity by the database")
	}
}

func TestUpsertRotationChannelReplacesRatherThanDuplicates(t *testing.T) {
	store, _, guildID := setupStore(t)
	ctx := context.Background()

	first := fullyPopulatedRotationChannel(guildID, "chan-1")
	if err := store.UpsertRotationChannel(ctx, first); err != nil {
		t.Fatalf("first Upsert: %v", err)
	}
	firstID, _ := store.RotationChannel(guildID, "chan-1")

	second := first
	second.Disclosure = DisclosureGeneric
	second.RetentionHours = nil // forever
	if err := store.UpsertRotationChannel(ctx, second); err != nil {
		t.Fatalf("second Upsert: %v", err)
	}

	all := store.RotationChannels(guildID)
	if len(all) != 1 {
		t.Fatalf("RotationChannels = %d rows, want exactly 1 (Upsert should replace, not duplicate)", len(all))
	}
	if all[0].ID != firstID.ID {
		t.Errorf("the stable ID changed across an Upsert of the same (guild, channel): %d -> %d", firstID.ID, all[0].ID)
	}
	if all[0].Disclosure != DisclosureGeneric {
		t.Errorf("Disclosure = %q after the second Upsert, want generic", all[0].Disclosure)
	}
	if all[0].RetentionHours != nil {
		t.Errorf("RetentionHours = %v after clearing to forever, want nil", all[0].RetentionHours)
	}
}

// Every array column here is NOT NULL, and a nil Go slice binds as SQL NULL
// via pgx. A caller building a RotationChannel from scratch (the common case:
// /rotation configure add only sets the fields its own options cover)
// leaves these nil, so Upsert normalizes them. Likewise Disclosure: the zero
// value must resolve to full, not reach Postgres as an empty string the
// CHECK constraint would reject outright.
func TestUpsertRotationChannelNormalizesNilSlicesAndEmptyDisclosure(t *testing.T) {
	store, _, guildID := setupStore(t)
	ctx := context.Background()

	minimal := RotationChannel{
		GuildID:           guildID,
		ChannelID:         "chan-1",
		IntervalMinutes:   60,
		ArchiveCategoryID: "cat-1",
		ArchiveVisibility: "mod_only",
		// ArchiveWhitelistRoleIDs, ArchiveWhitelistUserIDs, StickyMessages,
		// and Disclosure are all left at their Go zero values on purpose.
	}
	if err := store.UpsertRotationChannel(ctx, minimal); err != nil {
		t.Fatalf("Upsert with nil slices and empty Disclosure: %v", err)
	}

	got, ok := store.RotationChannel(guildID, "chan-1")
	if !ok {
		t.Fatal("RotationChannel did not find the channel just upserted")
	}
	if got.ArchiveWhitelistRoleIDs == nil {
		t.Error("ArchiveWhitelistRoleIDs came back nil, want empty-non-nil")
	}
	if got.ArchiveWhitelistUserIDs == nil {
		t.Error("ArchiveWhitelistUserIDs came back nil, want empty-non-nil")
	}
	if got.StickyMessages == nil {
		t.Error("StickyMessages came back nil, want empty-non-nil")
	}
	if got.Disclosure != DisclosureFull {
		t.Errorf("Disclosure = %q, want full (the normalized default)", got.Disclosure)
	}
}

func TestRemoveRotationChannel(t *testing.T) {
	store, _, guildID := setupStore(t)
	ctx := context.Background()

	rc := fullyPopulatedRotationChannel(guildID, "chan-1")
	if err := store.UpsertRotationChannel(ctx, rc); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if err := store.RemoveRotationChannel(ctx, guildID, "chan-1"); err != nil {
		t.Fatalf("RemoveRotationChannel: %v", err)
	}
	if _, ok := store.RotationChannel(guildID, "chan-1"); ok {
		t.Error("the channel is still found after RemoveRotationChannel")
	}
}

// RetargetRotationChannel exists so a slot's stable ID survives a rotation
// swap; migration 0009 was written specifically so the Scheduler's per-job
// state and rotation_archives' retention re-derivation (migration 0013)
// keep working across a retarget. This is the one property that matters
// about it.
func TestRetargetRotationChannel(t *testing.T) {
	store, _, guildID := setupStore(t)
	ctx := context.Background()

	rc := fullyPopulatedRotationChannel(guildID, "old-chan")
	if err := store.UpsertRotationChannel(ctx, rc); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	before, _ := store.RotationChannel(guildID, "old-chan")

	if err := store.RetargetRotationChannel(ctx, guildID, "old-chan", "new-chan"); err != nil {
		t.Fatalf("RetargetRotationChannel: %v", err)
	}

	if _, ok := store.RotationChannel(guildID, "old-chan"); ok {
		t.Error("the old channel ID is still resolvable after retargeting")
	}
	after, ok := store.RotationChannel(guildID, "new-chan")
	if !ok {
		t.Fatal("the new channel ID does not resolve after retargeting")
	}
	if after.ID != before.ID {
		t.Errorf("stable ID changed across a retarget: %d -> %d", before.ID, after.ID)
	}

	byID, ok := store.RotationChannelByID(guildID, before.ID)
	if !ok || byID.ChannelID != "new-chan" {
		t.Errorf("RotationChannelByID(%d) = %+v, ok=%v; want the retargeted channel", before.ID, byID, ok)
	}
}

// --- PruneDeletedRole ---

func TestPruneDeletedRoleRemovesFromModRolesAllowAndDenyLists(t *testing.T) {
	store, rec, guildID := setupStore(t)
	ctx := context.Background()
	const roleID = "role-1"

	if err := store.AddModRole(ctx, guildID, roleID); err != nil {
		t.Fatalf("AddModRole: %v", err)
	}
	if err := store.GrantOverride(ctx, guildID, "action.a", roleID, ""); err != nil {
		t.Fatalf("GrantOverride: %v", err)
	}
	if err := store.DenyOverride(ctx, guildID, "action.b", roleID, ""); err != nil {
		t.Fatalf("DenyOverride: %v", err)
	}
	eventsBeforePrune := rec.countFor(guildID)

	removed, err := store.PruneDeletedRole(ctx, guildID, roleID)
	if err != nil {
		t.Fatalf("PruneDeletedRole: %v", err)
	}
	if len(removed) != 3 {
		t.Errorf("PruneDeletedRole removed %d entries, want 3 (mod role, allow, deny): %v", len(removed), removed)
	}

	if got := store.ModRoleIDs(guildID); len(got) != 0 {
		t.Errorf("mod roles after prune = %v, want empty", got)
	}
	if got := store.ActionPolicy(guildID, "action.a").AllowRoleIDs; len(got) != 0 {
		t.Errorf("allow roles after prune = %v, want empty", got)
	}
	if got := store.ActionPolicy(guildID, "action.b").DenyRoleIDs; len(got) != 0 {
		t.Errorf("deny roles after prune = %v, want empty", got)
	}

	// One mutator call per list the role was actually removed from.
	if got := rec.countFor(guildID) - eventsBeforePrune; got != len(removed) {
		t.Errorf("PruneDeletedRole published %d events, want %d (one per removal)", got, len(removed))
	}
}

func TestPruneDeletedRoleIsANoOpForAnUnreferencedRole(t *testing.T) {
	store, rec, guildID := setupStore(t)
	ctx := context.Background()

	// Give the guild some real state that must survive untouched.
	if err := store.AddModRole(ctx, guildID, "unrelated-role"); err != nil {
		t.Fatalf("AddModRole: %v", err)
	}
	eventsBefore := rec.countFor(guildID)

	removed, err := store.PruneDeletedRole(ctx, guildID, "never-referenced-role")
	if err != nil {
		t.Fatalf("PruneDeletedRole: %v", err)
	}
	if len(removed) != 0 {
		t.Errorf("PruneDeletedRole removed %v for a role that was never referenced", removed)
	}
	if got := rec.countFor(guildID); got != eventsBefore {
		t.Errorf("PruneDeletedRole published an event for a no-op prune")
	}
	if got := store.ModRoleIDs(guildID); len(got) != 1 || got[0] != "unrelated-role" {
		t.Errorf("unrelated mod role was disturbed by an unrelated prune: %v", got)
	}
}

func stringSlicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	seen := map[string]int{}
	for _, v := range a {
		seen[v]++
	}
	for _, v := range b {
		seen[v]--
	}
	for _, n := range seen {
		if n != 0 {
			return false
		}
	}
	return true
}

func TestArchiveViewerRoleRoundTrip(t *testing.T) {
	store, _, guildID := setupStore(t)
	ctx := context.Background()

	if err := store.AddArchiveViewerRole(ctx, guildID, "role-1"); err != nil {
		t.Fatalf("AddArchiveViewerRole: %v", err)
	}
	// Adding twice must not duplicate: the overwrite set built from this list
	// is compared for equality against Discord's live one to decide whether to
	// write at all, and a duplicated role would make that comparison differ
	// forever.
	if err := store.AddArchiveViewerRole(ctx, guildID, "role-1"); err != nil {
		t.Fatalf("AddArchiveViewerRole (again): %v", err)
	}
	if got := store.ArchiveViewerRoleIDs(guildID); len(got) != 1 || got[0] != "role-1" {
		t.Fatalf("ArchiveViewerRoleIDs after add = %v, want [role-1]", got)
	}

	if err := store.RemoveArchiveViewerRole(ctx, guildID, "role-1"); err != nil {
		t.Fatalf("RemoveArchiveViewerRole: %v", err)
	}
	if got := store.ArchiveViewerRoleIDs(guildID); len(got) != 0 {
		t.Errorf("ArchiveViewerRoleIDs after remove = %v, want empty", got)
	}
}
