package roles

import (
	"context"
	"slices"
	"testing"
	"time"
)

// jailedAt is far enough in the past that a "rejoined" member's JoinedAt is
// unambiguously later, and a "never left" member's is unambiguously earlier.
var jailedAt = fixedNow.Add(-2 * time.Hour)

// activeJail returns a jail still in force at fixedNow.
func activeJail(userID string, snapshot []string) JailRecord {
	releaseAt := fixedNow.Add(time.Hour)
	return JailRecord{
		GuildID: "g1", UserID: userID, SnapshotRoleIDs: snapshot,
		JailRoleID: "jail-role", JailedAt: jailedAt, ReleaseAt: &releaseAt,
		JailedBy: "mod1", Reason: "test",
	}
}

func newEvasionPlugin(t *testing.T, ops *fakeOps, store *fakeStore) *Plugin {
	t.Helper()
	return newTestPlugin(ops, store, newFakeSettings(), newFakeAudit(), newFakePerms(), newFakeScheduler())
}

// The evasion itself: a jailed member leaves, and Discord drops every role
// they held, including the Jailed marker whose overwrites were the only
// thing restricting them. They rejoin with full access while the bot's own
// record still says "jailed". The sweep has to put the marker back.
func TestSweepReJailsMemberWhoLeftAndRejoined(t *testing.T) {
	ops := newFakeOps()
	// Back in the guild, no roles at all, joined *after* being jailed.
	ops.setMemberJoined("g1", "u1", nil, fixedNow.Add(-time.Minute))

	store := newFakeStore()
	if err := store.InsertJail(context.Background(), activeJail("u1", []string{"role-a", "role-b"})); err != nil {
		t.Fatalf("InsertJail: %v", err)
	}

	p := newEvasionPlugin(t, ops, store)
	if err := p.sweep(context.Background(), "g1"); err != nil {
		t.Fatalf("sweep: %v", err)
	}

	member, err := ops.GuildMember("g1", "u1")
	if err != nil {
		t.Fatalf("GuildMember: %v", err)
	}
	if !slices.Contains(member.Roles, "jail-role") {
		t.Errorf("member escaped their jail by rejoining; roles = %v, want the jail marker back", member.Roles)
	}
}

// The snapshot is the only record of what the member held before being
// jailed. Re-applying must not overwrite it with what they hold now
// (nothing, having just rejoined), or their eventual release restores nothing,
// exactly the way the concurrent-jail race used to destroy it.
func TestReJailPreservesSnapshotAndReleaseTime(t *testing.T) {
	ops := newFakeOps()
	ops.setMemberJoined("g1", "u1", nil, fixedNow.Add(-time.Minute))

	store := newFakeStore()
	original := activeJail("u1", []string{"role-a", "role-b"})
	if err := store.InsertJail(context.Background(), original); err != nil {
		t.Fatalf("InsertJail: %v", err)
	}

	p := newEvasionPlugin(t, ops, store)
	if err := p.reapplyEvadedJails(context.Background(), "g1"); err != nil {
		t.Fatalf("reapplyEvadedJails: %v", err)
	}

	rec, ok, err := store.GetJail(context.Background(), "g1", "u1")
	if err != nil || !ok {
		t.Fatalf("GetJail: %v (found=%v)", err, ok)
	}
	if !slices.Equal(rec.SnapshotRoleIDs, []string{"role-a", "role-b"}) {
		t.Errorf("snapshot is %v, want [role-a role-b]; the member's real roles are now unrecoverable", rec.SnapshotRoleIDs)
	}
	// Leaving the server neither serves the sentence nor extends it.
	if !rec.ReleaseAt.Equal(*original.ReleaseAt) {
		t.Errorf("ReleaseAt moved from %v to %v", *original.ReleaseAt, *rec.ReleaseAt)
	}
	if rec.JailedBy != "mod1" || rec.Reason != "test" {
		t.Errorf("re-apply rewrote the record's provenance: %+v", rec)
	}
}

// The counterweight, and the reason JoinedAt is the discriminator rather than
// "the marker is missing": a mod who strips the marker by hand is deliberately
// letting someone out early, and the confused-deputy rule says don't fight it.
// That member never left, so their JoinedAt predates the jail.
func TestSweepDoesNotFightAManualRelease(t *testing.T) {
	ops := newFakeOps()
	// Marker gone, but they have been in the guild since long before the jail.
	ops.setMemberJoined("g1", "u1", []string{"role-a"}, jailedAt.Add(-24*time.Hour))

	store := newFakeStore()
	if err := store.InsertJail(context.Background(), activeJail("u1", []string{"role-a"})); err != nil {
		t.Fatalf("InsertJail: %v", err)
	}

	p := newEvasionPlugin(t, ops, store)
	if err := p.reapplyEvadedJails(context.Background(), "g1"); err != nil {
		t.Fatalf("reapplyEvadedJails: %v", err)
	}

	member, err := ops.GuildMember("g1", "u1")
	if err != nil {
		t.Fatalf("GuildMember: %v", err)
	}
	if slices.Contains(member.Roles, "jail-role") {
		t.Error("re-jailed a member a mod had deliberately released by hand")
	}
}

// A member still serving their jail normally must not be touched: no
// pointless role edit, no audit noise, once a minute, forever.
func TestSweepLeavesAnOrdinaryJailAlone(t *testing.T) {
	ops := newFakeOps()
	ops.setMemberJoined("g1", "u1", []string{"jail-role"}, jailedAt.Add(-24*time.Hour))

	store := newFakeStore()
	if err := store.InsertJail(context.Background(), activeJail("u1", []string{"role-a"})); err != nil {
		t.Fatalf("InsertJail: %v", err)
	}

	p := newEvasionPlugin(t, ops, store)
	if err := p.reapplyEvadedJails(context.Background(), "g1"); err != nil {
		t.Fatalf("reapplyEvadedJails: %v", err)
	}

	if calls := ops.memberEditCalls["u1"]; len(calls) != 0 {
		t.Errorf("edited a correctly-jailed member's roles: %v", calls)
	}
}

// Clock skew guard: this bot stamps JailedAt from its own clock while Discord
// stamps JoinedAt, so a member jailed seconds after joining can have the two
// nearly equal. Treating that as a rejoin would re-jail someone a mod had just
// released.
func TestRejoinDetectionToleratesClockSkew(t *testing.T) {
	rec := JailRecord{JailedAt: fixedNow}
	cases := []struct {
		name     string
		joinedAt time.Time
		want     bool
	}{
		{"joined long before the jail", fixedNow.Add(-24 * time.Hour), false},
		{"joined moments before the jail", fixedNow.Add(-2 * time.Second), false},
		{"skew: stamped just after the jail", fixedNow.Add(2 * time.Second), false},
		{"skew: still inside the grace window", fixedNow.Add(rejoinGrace - time.Second), false},
		{"genuinely rejoined later", fixedNow.Add(rejoinGrace + time.Minute), true},
		{"no timestamp at all", time.Time{}, false},
	}
	for _, tc := range cases {
		got := rejoinedSinceJail(memberWithJoinedAt(tc.joinedAt), rec)
		if got != tc.want {
			t.Errorf("%s: rejoinedSinceJail = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// A member who left and has not come back must keep their record, so the jail
// is still waiting for them if they return before it expires.
func TestEvasionCheckKeepsRecordForAbsentMember(t *testing.T) {
	ops := newFakeOps() // no member registered: Discord reports Unknown Member
	store := newFakeStore()
	if err := store.InsertJail(context.Background(), activeJail("u1", []string{"role-a"})); err != nil {
		t.Fatalf("InsertJail: %v", err)
	}

	p := newEvasionPlugin(t, ops, store)
	if err := p.reapplyEvadedJails(context.Background(), "g1"); err != nil {
		t.Fatalf("an absent member is not an error: %v", err)
	}
	if _, ok, _ := store.GetJail(context.Background(), "g1", "u1"); !ok {
		t.Error("dropped the jail record for a member who merely left; they would return unjailed")
	}
}

// A transient failure must not be mistaken for "they're fine": the check has
// to report it so the next sweep retries, rather than silently leaving an
// evader loose.
func TestEvasionCheckRetriesOnTransientFailure(t *testing.T) {
	ops := newFakeOps()
	ops.memberFetchErr = transientErr()

	store := newFakeStore()
	if err := store.InsertJail(context.Background(), activeJail("u1", []string{"role-a"})); err != nil {
		t.Fatalf("InsertJail: %v", err)
	}

	p := newEvasionPlugin(t, ops, store)
	if err := p.reapplyEvadedJails(context.Background(), "g1"); err == nil {
		t.Error("a failed member fetch must be reported, not swallowed")
	}
	if _, ok, _ := store.GetJail(context.Background(), "g1", "u1"); !ok {
		t.Error("a transient failure dropped the jail record")
	}
}

// The gateway path (opt-in GUILD_MEMBERS intent) closes the same gap
// immediately instead of on the next sweep, and must reach the same outcome.
func TestHandleMemberJoinReJailsImmediately(t *testing.T) {
	ops := newFakeOps()
	ops.setMemberJoined("g1", "u1", nil, fixedNow.Add(-time.Minute))

	store := newFakeStore()
	if err := store.InsertJail(context.Background(), activeJail("u1", []string{"role-a"})); err != nil {
		t.Fatalf("InsertJail: %v", err)
	}

	p := newEvasionPlugin(t, ops, store)
	p.HandleMemberJoin(context.Background(), "g1", "u1")

	member, err := ops.GuildMember("g1", "u1")
	if err != nil {
		t.Fatalf("GuildMember: %v", err)
	}
	if !slices.Contains(member.Roles, "jail-role") {
		t.Errorf("rejoin handler did not re-apply the jail; roles = %v", member.Roles)
	}
}

// Someone whose jail expired while they were away has served it. Rejoining
// must not re-jail them: the ordinary sweep closes the record out instead.
func TestHandleMemberJoinIgnoresAnExpiredJail(t *testing.T) {
	ops := newFakeOps()
	ops.setMemberJoined("g1", "u1", nil, fixedNow.Add(-time.Minute))

	store := newFakeStore()
	expired := fixedNow.Add(-time.Minute)
	if err := store.InsertJail(context.Background(), JailRecord{
		GuildID: "g1", UserID: "u1", SnapshotRoleIDs: []string{"role-a"},
		JailRoleID: "jail-role", JailedAt: jailedAt, ReleaseAt: &expired,
	}); err != nil {
		t.Fatalf("InsertJail: %v", err)
	}

	p := newEvasionPlugin(t, ops, store)
	p.HandleMemberJoin(context.Background(), "g1", "u1")

	member, err := ops.GuildMember("g1", "u1")
	if err != nil {
		t.Fatalf("GuildMember: %v", err)
	}
	if slices.Contains(member.Roles, "jail-role") {
		t.Error("re-jailed a member whose sentence had already expired")
	}
}
