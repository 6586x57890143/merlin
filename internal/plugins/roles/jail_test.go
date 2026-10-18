package roles

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/6586x57890143/merlin/internal/core"
	"github.com/disgoorg/disgo/rest"
)

// TestReleaseJailRestoresSnapshotFilteredToExistingRoles verifies release
// restores exactly the member's pre-jail roles, minus any role that was
// deleted from the guild in the meantime (release must not hand back a
// dangling role ID Discord would reject).
func TestReleaseJailRestoresSnapshotFilteredToExistingRoles(t *testing.T) {
	ops := newFakeOps()
	ops.setMember("7000001", "7000102", []string{"7000301"})
	ops.roles["7000001"] = []*frole{{ID: "7000302"}, {ID: "7000301"}} // role-b was deleted since jailing

	p := newTestPlugin(ops, newFakeStore(), newFakeSettings(), newFakeAudit(), newFakePerms(), newFakeScheduler())

	rec := JailRecord{
		GuildID: "7000001", UserID: "7000102",
		SnapshotRoleIDs: []string{"7000302", "7000303"},
		JailRoleID:      "7000301",
	}
	if err := p.releaseJail(context.Background(), "7000001", "7000102", rec, core.ActorSystem); err != nil {
		t.Fatalf("releaseJail: %v", err)
	}

	m, _ := ops.GuildMember("7000001", "7000102")
	if len(m.Roles) != 1 || m.Roles[0] != "7000302" {
		t.Fatalf("expected only surviving role-a restored, got %v", m.Roles)
	}
	if _, ok, _ := p.store.GetJail(context.Background(), "7000001", "7000102"); ok {
		t.Fatal("expected jail record removed after release")
	}
}

// TestReleaseJailConfusedDeputyRescue verifies the safeguard central to this
// plugin's design: if a mod already manually removed the jail marker role
// before the scheduled/explicit release ran, releaseJail must not fight
// that override: no restore write, just stop tracking.
func TestReleaseJailConfusedDeputyRescue(t *testing.T) {
	ops := newFakeOps()
	ops.setMember("7000001", "7000102", []string{"7000327"}) // jail-role no longer present

	p := newTestPlugin(ops, newFakeStore(), newFakeSettings(), newFakeAudit(), newFakePerms(), newFakeScheduler())

	rec := JailRecord{GuildID: "7000001", UserID: "7000102", SnapshotRoleIDs: []string{"7000302"}, JailRoleID: "7000301"}
	if err := p.releaseJail(context.Background(), "7000001", "7000102", rec, core.ActorSystem); err != nil {
		t.Fatalf("releaseJail: %v", err)
	}

	if len(ops.memberEditCalls["7000102"]) != 0 {
		t.Fatalf("expected no GuildMemberEdit call when jail marker already gone, got %v", ops.memberEditCalls["7000102"])
	}
	if _, ok, _ := p.store.GetJail(context.Background(), "7000001", "7000102"); ok {
		t.Fatal("expected jail record removed even though no restore happened")
	}
}

// TestReleaseJailMemberGoneCleansUpRecord verifies a member who left the
// guild before release doesn't leave a permanently-stuck tracking row.
func TestReleaseJailMemberGoneCleansUpRecord(t *testing.T) {
	ops := newFakeOps() // no member registered at all

	p := newTestPlugin(ops, newFakeStore(), newFakeSettings(), newFakeAudit(), newFakePerms(), newFakeScheduler())
	_ = p.store.InsertJail(context.Background(), JailRecord{GuildID: "7000001", UserID: "7000102", JailRoleID: "7000301"})

	rec, _, _ := p.store.GetJail(context.Background(), "7000001", "7000102")
	if err := p.releaseJail(context.Background(), "7000001", "7000102", rec, core.ActorSystem); err != nil {
		t.Fatalf("releaseJail: %v", err)
	}
	if _, ok, _ := p.store.GetJail(context.Background(), "7000001", "7000102"); ok {
		t.Fatal("expected jail record removed for a member who left the guild")
	}
}

// TestHandleJailStripsRolesKeepingUnmanageableOnes verifies the role-
// hierarchy safeguard (CanManageRole, spec.MD §4 item 4): a role the bot
// can't manage must survive jailing rather than being silently dropped or
// causing the whole operation to fail.
func TestJailRolesKeepsUnmanageableRolesInPlace(t *testing.T) {
	perms := newFakePerms()
	perms.unmanageable["7000326"] = true

	newRoles, unmanageable := jailRoles(perms, "7000001", "7000301", []string{"7000337", "7000326"})

	if len(unmanageable) != 1 || unmanageable[0] != "7000326" {
		t.Fatalf("expected exactly unmanageable-role flagged, got %v", unmanageable)
	}
	if len(newRoles) != 2 || newRoles[0] != "7000301" || newRoles[1] != "7000326" {
		t.Fatalf("expected the jail marker plus the untouchable role, got %v", newRoles)
	}
}

// TestJailRolesStripsEverythingItCan is the ordinary case: a member whose
// roles are all below the bot's ends up holding nothing but the marker.
func TestJailRolesStripsEverythingItCan(t *testing.T) {
	newRoles, unmanageable := jailRoles(newFakePerms(), "7000001", "7000301", []string{"7000302", "7000303", "7000304"})

	if len(unmanageable) != 0 {
		t.Fatalf("expected nothing flagged as unmanageable, got %v", unmanageable)
	}
	if len(newRoles) != 1 || newRoles[0] != "7000301" {
		t.Fatalf("expected only the jail marker to remain, got %v", newRoles)
	}
}

// TestJailRolesOnRolelessMember covers a member holding no roles at all,
// the marker still has to be applied, and an empty roles list must not read
// as "strip nothing."
func TestJailRolesOnRolelessMember(t *testing.T) {
	newRoles, unmanageable := jailRoles(newFakePerms(), "7000001", "7000301", nil)
	if len(unmanageable) != 0 || len(newRoles) != 1 || newRoles[0] != "7000301" {
		t.Fatalf("jailRoles(no roles) = (%v, %v), want ([jail-role], [])", newRoles, unmanageable)
	}
}

func TestSweepReleasesDueJailsAndSkipsNotYetDue(t *testing.T) {
	ops := newFakeOps()
	ops.setMember("7000001", "7000102", []string{"7000301"})
	ops.setMember("7000001", "7000103", []string{"7000301"})

	store := newFakeStore()
	due := fixedNow.Add(-time.Minute)
	notDue := fixedNow.Add(time.Hour)
	_ = store.InsertJail(context.Background(), JailRecord{GuildID: "7000001", UserID: "7000102", JailRoleID: "7000301", ReleaseAt: &due})
	_ = store.InsertJail(context.Background(), JailRecord{GuildID: "7000001", UserID: "7000103", JailRoleID: "7000301", ReleaseAt: &notDue})

	p := newTestPlugin(ops, store, newFakeSettings(), newFakeAudit(), newFakePerms(), newFakeScheduler())

	if err := p.sweep(context.Background(), "7000001"); err != nil {
		t.Fatalf("sweep: %v", err)
	}

	if _, ok, _ := store.GetJail(context.Background(), "7000001", "7000102"); ok {
		t.Fatal("expected due jail released and untracked")
	}
	if _, ok, _ := store.GetJail(context.Background(), "7000001", "7000103"); !ok {
		t.Fatal("expected not-yet-due jail left untouched")
	}
}

// TestReleaseJailKeepsTrackingOnTransientFetchFailure is the counterpart to
// TestReleaseJailMemberGoneCleansUpRecord. releaseJail used to read *any*
// member-fetch error as "they left the guild" and drop the tracking row,
// so a rate limit or a 5xx during the sweep left the member jailed forever,
// with nothing left to ever release them and no error surfaced afterwards.
// Only Discord's own "unknown member" may untrack.
func TestReleaseJailKeepsTrackingOnTransientFetchFailure(t *testing.T) {
	ops := newFakeOps()
	ops.setMember("7000001", "7000102", []string{"7000301"})
	ops.roles["7000001"] = []*frole{{ID: "7000302"}, {ID: "7000301"}}
	ops.memberFetchErr = transientErr()

	p := newTestPlugin(ops, newFakeStore(), newFakeSettings(), newFakeAudit(), newFakePerms(), newFakeScheduler())
	rec := JailRecord{GuildID: "7000001", UserID: "7000102", SnapshotRoleIDs: []string{"7000302"}, JailRoleID: "7000301"}
	if err := p.store.InsertJail(context.Background(), rec); err != nil {
		t.Fatalf("InsertJail: %v", err)
	}

	if err := p.releaseJail(context.Background(), "7000001", "7000102", rec, core.ActorSystem); err == nil {
		t.Fatal("expected releaseJail to report the transient fetch failure")
	}
	if _, ok, _ := p.store.GetJail(context.Background(), "7000001", "7000102"); !ok {
		t.Fatal("jail record was dropped on a transient error; the member would stay jailed forever")
	}

	// Discord recovers: the next sweep releases them properly.
	ops.memberFetchErr = nil
	if err := p.releaseJail(context.Background(), "7000001", "7000102", rec, core.ActorSystem); err != nil {
		t.Fatalf("retry releaseJail: %v", err)
	}
	m, _ := ops.GuildMember("7000001", "7000102")
	if len(m.Roles) != 1 || m.Roles[0] != "7000302" {
		t.Fatalf("expected prior roles restored on retry, got %v", m.Roles)
	}
	if _, ok, _ := p.store.GetJail(context.Background(), "7000001", "7000102"); ok {
		t.Fatal("expected the jail record to be cleared once the release succeeded")
	}
}

// TestSweepRetriesJailReleaseUntilItSucceeds exercises the same failure
// through the sweep job, which is where it actually bites: a due jail that
// couldn't be released must still be due on the next minute's sweep.
func TestSweepRetriesJailReleaseUntilItSucceeds(t *testing.T) {
	ops := newFakeOps()
	ops.setMember("7000001", "7000102", []string{"7000301"})
	ops.roles["7000001"] = []*frole{{ID: "7000302"}, {ID: "7000301"}}
	ops.memberFetchErr = transientErr()

	p := newTestPlugin(ops, newFakeStore(), newFakeSettings(), newFakeAudit(), newFakePerms(), newFakeScheduler())
	due := fixedNow.Add(-time.Minute)
	if err := p.store.InsertJail(context.Background(), JailRecord{
		GuildID: "7000001", UserID: "7000102", SnapshotRoleIDs: []string{"7000302"},
		JailRoleID: "7000301", ReleaseAt: &due,
	}); err != nil {
		t.Fatalf("InsertJail: %v", err)
	}

	if err := p.sweep(context.Background(), "7000001"); err == nil {
		t.Fatal("expected the sweep to report the failed release")
	}
	if _, ok, _ := p.store.GetJail(context.Background(), "7000001", "7000102"); !ok {
		t.Fatal("expected the due jail to still be tracked after a failed sweep")
	}

	ops.memberFetchErr = nil
	if err := p.sweep(context.Background(), "7000001"); err != nil {
		t.Fatalf("second sweep: %v", err)
	}
	if _, ok, _ := p.store.GetJail(context.Background(), "7000001", "7000102"); ok {
		t.Fatal("expected the jail to be released on the retry sweep")
	}
}

// TestJailRefusesProtectedTargetBeforeMutatingAnything is the ordering half
// of the rank check (core.Permissions.CanModerate is where the rule itself
// is tested). A refused jail must leave no trace: no roles edited, no jail
// record, no Jailed role created for the guild. Checking after any of those
// would still "work" from the user's point of view while leaving the target
// stripped or the guild carrying a role it never needed.
func TestJailRefusesProtectedTargetBeforeMutatingAnything(t *testing.T) {
	ops := newFakeOps()
	ops.setMember("7000001", "7000111", []string{"7000336"})

	perms := newFakePerms()
	perms.protected["7000111"] = true

	p := newTestPlugin(ops, newFakeStore(), newFakeSettings(), newFakeAudit(), perms, newFakeScheduler())

	member, err := ops.GuildMember("7000001", "7000111")
	if err != nil {
		t.Fatalf("GuildMember: %v", err)
	}
	if err := p.perms.CanModerate("7000001", actorMember("7000116"), "7000111", member.Roles); err == nil {
		t.Fatal("expected the rank check to refuse a mod jailing an admin")
	}

	// Nothing downstream of the check may have run.
	if len(ops.memberEditCalls["7000111"]) != 0 {
		t.Fatalf("a refused jail edited the member's roles: %v", ops.memberEditCalls["7000111"])
	}
	if _, ok, _ := p.store.GetJail(context.Background(), "7000001", "7000111"); ok {
		t.Fatal("a refused jail left a jail record behind")
	}
	if roles := ops.roles["7000001"]; len(roles) != 0 {
		t.Fatalf("a refused jail created guild roles: %v", roles)
	}
}

func actorMember(userID string) *core.Actor {
	return &core.Actor{ID: userID}
}

// --- applyJail ordering ---
//
// The whole correctness argument for the jail mutation is the order of its
// two writes. These pin it down in both failure directions.

// TestApplyJailNeverStripsRolesWithoutTrackingThem is the one that matters.
// The mutation used to strip roles first and record the jail second, so a
// database blip between the two left a member holding nothing but the marker
// role with nothing anywhere tracking them: no sweep would release them, no
// /roles release would find a record, and it looks from the outside like an
// ordinary jail that simply never ends.
func TestApplyJailNeverStripsRolesWithoutTrackingThem(t *testing.T) {
	ops := newFakeOps()
	ops.setMember("7000001", "7000102", []string{"7000302", "7000303"})

	store := newFakeStore()
	store.insertJailErr = errors.New("connection reset by peer")

	p := newTestPlugin(ops, store, newFakeSettings(), newFakeAudit(), newFakePerms(), newFakeScheduler())

	if _, err := p.applyJail(context.Background(), "7000001", "7000301", jailTarget{userID: "7000102", roles: []string{"7000302", "7000303"}}, time.Hour, "7000319", ""); err == nil {
		t.Fatal("expected applyJail to report the failed record write")
	}

	if len(ops.memberEditCalls["7000102"]) != 0 {
		t.Fatalf("roles were stripped even though the jail could not be recorded; the member would stay jailed forever, got %v", ops.memberEditCalls["7000102"])
	}
	m, _ := ops.GuildMember("7000001", "7000102")
	if len(m.Roles) != 2 {
		t.Fatalf("expected the member's roles left untouched, got %v", m.Roles)
	}
}

// TestApplyJailRollsBackRecordWhenRoleUpdateFails is the other direction: the
// record lands, the role edit fails, and the guild must not be left tracking
// a jail nobody is actually in.
func TestApplyJailRollsBackRecordWhenRoleUpdateFails(t *testing.T) {
	ops := newFakeOps()
	ops.setMember("7000001", "7000102", []string{"7000302"})
	ops.memberEditErr = transientErr()

	p := newTestPlugin(ops, newFakeStore(), newFakeSettings(), newFakeAudit(), newFakePerms(), newFakeScheduler())

	if _, err := p.applyJail(context.Background(), "7000001", "7000301", jailTarget{userID: "7000102", roles: []string{"7000302"}}, time.Hour, "7000319", ""); err == nil {
		t.Fatal("expected applyJail to report the failed role update")
	}
	if _, ok, _ := p.store.GetJail(context.Background(), "7000001", "7000102"); ok {
		t.Fatal("expected the jail record rolled back after the role update failed")
	}
}

// TestApplyJailSucceedsAndTracks is the happy path, asserting both writes
// landed and the unmanageable-role report survives the extraction.
func TestApplyJailSucceedsAndTracks(t *testing.T) {
	ops := newFakeOps()
	ops.setMember("7000001", "7000102", []string{"7000302", "7000325"})

	perms := newFakePerms()
	perms.unmanageable["7000325"] = true

	p := newTestPlugin(ops, newFakeStore(), newFakeSettings(), newFakeAudit(), perms, newFakeScheduler())

	unmanageable, err := p.applyJail(context.Background(), "7000001", "7000301", jailTarget{userID: "7000102", roles: []string{"7000302", "7000325"}}, 2*time.Hour, "7000319", "spam")
	if err != nil {
		t.Fatalf("applyJail: %v", err)
	}
	if len(unmanageable) != 1 || unmanageable[0] != "7000325" {
		t.Fatalf("expected the untouchable role reported back, got %v", unmanageable)
	}

	m, _ := ops.GuildMember("7000001", "7000102")
	if len(m.Roles) != 2 || m.Roles[0] != "7000301" || m.Roles[1] != "7000325" {
		t.Fatalf("expected the marker plus the untouchable role, got %v", m.Roles)
	}
	rec, ok, _ := p.store.GetJail(context.Background(), "7000001", "7000102")
	if !ok {
		t.Fatal("expected the jail to be tracked")
	}
	if len(rec.SnapshotRoleIDs) != 2 || rec.JailedBy != "7000319" || rec.Reason != "spam" {
		t.Fatalf("jail record did not capture the pre-jail state: %+v", rec)
	}
	if rec.ReleaseAt == nil || !rec.ReleaseAt.Equal(fixedNow.Add(2*time.Hour)) {
		t.Fatalf("expected release scheduled 2h out, got %v", rec.ReleaseAt)
	}
}

// TestApplyJailForgetsCachedRoleOnlyWhenTheRoleIsGone: GuildMemberEdit
// reports Unknown Member for a target who left and Unknown Role for a deleted
// marker role. Treating them alike threw away a good cache entry on the wrong
// signal.
func TestApplyJailForgetsCachedRoleOnlyWhenTheRoleIsGone(t *testing.T) {
	roleGone := &rest.Error{
		Response: &http.Response{StatusCode: http.StatusNotFound},
		Code:     rest.JSONErrorCodeUnknownRole, Message: "Unknown Role",
	}

	for _, tc := range []struct {
		name        string
		editErr     error
		wantForgets bool
	}{
		{"marker role deleted", roleGone, true},
		{"member left the guild", unknownMemberErr("7000001", "7000102"), false},
		{"transient failure", transientErr(), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ops := newFakeOps()
			ops.setMember("7000001", "7000102", []string{"7000302"})
			ops.memberEditErr = tc.editErr

			p := newTestPlugin(ops, newFakeStore(), newFakeSettings(), newFakeAudit(), newFakePerms(), newFakeScheduler())
			p.jailRoleID["7000001"] = "7000334"

			if _, err := p.applyJail(context.Background(), "7000001", "7000334", jailTarget{userID: "7000102", roles: []string{"7000302"}}, time.Hour, "7000319", ""); err == nil {
				t.Fatal("expected applyJail to fail")
			}

			_, stillCached := p.jailRoleID["7000001"]
			if tc.wantForgets && stillCached {
				t.Fatal("expected the cached jail role to be forgotten when Discord says the role is gone")
			}
			if !tc.wantForgets && !stillCached {
				t.Fatal("cached jail role was discarded on an error that says nothing about the role")
			}
		})
	}
}

// TestReleaseJailKeepsRolesGainedWhileJailed pins the live failure: a member
// who picked up a role the bot can't manage (booster, or one above the bot)
// while jailed. Writing the snapshot alone would remove it, Discord 403s
// the whole edit, and the sweep retries the same 403 until someone notices.
// Release only ever removes the marker.
func TestReleaseJailKeepsRolesGainedWhileJailed(t *testing.T) {
	ops := newFakeOps()
	ops.setMember("7000001", "7000102", []string{"7000301", "7000329"})
	ops.roles["7000001"] = []*frole{{ID: "7000302"}, {ID: "moved-up"}, {ID: "7000301"}, {ID: "7000329"}}
	perms := newFakePerms()
	perms.unmanageable["7000329"] = true
	perms.unmanageable["moved-up"] = true // manageable when jailed, not any more

	p := newTestPlugin(ops, newFakeStore(), newFakeSettings(), newFakeAudit(), perms, newFakeScheduler())

	rec := JailRecord{GuildID: "7000001", UserID: "7000102", SnapshotRoleIDs: []string{"7000302", "moved-up"}, JailRoleID: "7000301"}
	if err := p.releaseJail(context.Background(), "7000001", "7000102", rec, core.ActorSystem); err != nil {
		t.Fatalf("releaseJail: %v", err)
	}
	m, _ := ops.GuildMember("7000001", "7000102")
	if len(m.Roles) != 2 || m.Roles[0] != "7000329" || m.Roles[1] != "7000302" {
		t.Fatalf("expected [booster role-a] (marker gone, unmanageable snapshot role skipped), got %v", m.Roles)
	}
}

// TestForeverJailHasNoReleaseTime pins "forever" as a jail with no end at
// all, not a far-off date: ReleaseAt nil is what the sweep, the release
// timer and evasion handling already read as "only a moderator ends this".
func TestForeverJailHasNoReleaseTime(t *testing.T) {
	for _, in := range []string{"forever", " Permanent ", "indefinite"} {
		if d, err := parseSentence(in); err != nil || d != foreverSentence {
			t.Fatalf("parseSentence(%q) = %v, %v; want forever", in, d, err)
		}
	}
	if d, err := parseSentence("3d"); err != nil || d != 72*time.Hour {
		t.Fatalf("parseSentence(3d) = %v, %v", d, err)
	}

	ops := newFakeOps()
	ops.setMember("7000001", "7000102", []string{"7000302"})
	p := newTestPlugin(ops, newFakeStore(), newFakeSettings(), newFakeAudit(), newFakePerms(), newFakeScheduler())
	if _, err := p.applyJail(context.Background(), "7000001", "7000301", jailTarget{userID: "7000102", roles: []string{"7000302"}}, foreverSentence, "7000319", "r"); err != nil {
		t.Fatal(err)
	}
	rec, ok, _ := p.store.GetJail(context.Background(), "7000001", "7000102")
	if !ok || rec.ReleaseAt != nil {
		t.Fatalf("want a tracked jail with no release time, got ok=%v ReleaseAt=%v", ok, rec.ReleaseAt)
	}
}

// TestReleaseUntracksEvenWhenTheCallerGaveUp pins the 2026-10-18 failure: a
// /roles release ran past the router's 2.5s window, its context was
// cancelled after the roles were already back, and the row delete at the
// end failed with "context canceled". The member was out, the command said
// it failed, and the surviving row turned a mod's next hand edit into a
// transfer. The Discord calls take no context, so a cancelled one here is
// the same position: roles restorable, caller gone.
func TestReleaseUntracksEvenWhenTheCallerGaveUp(t *testing.T) {
	ops := newFakeOps()
	ops.setMember("7000001", "7000102", []string{"7000301"})
	ops.roles["7000001"] = []*frole{{ID: "7000302"}, {ID: "7000301"}}
	store := newFakeStore()
	p := newTestPlugin(ops, store, newFakeSettings(), newFakeAudit(), newFakePerms(), newFakeScheduler())
	rec := JailRecord{GuildID: "7000001", UserID: "7000102", JailRoleID: "7000301", SnapshotRoleIDs: []string{"7000302"}}
	_ = store.InsertJail(context.Background(), rec)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := p.releaseJail(ctx, "7000001", "7000102", rec, core.ActorSystem); err != nil {
		t.Fatalf("releaseJail: %v", err)
	}
	if _, ok, _ := store.GetJail(context.Background(), "7000001", "7000102"); ok {
		t.Fatal("a release whose roles went back must not leave its row behind")
	}

	_ = store.InsertGrant(context.Background(), GrantRecord{GuildID: "7000001", UserID: "7000102", RoleID: "7000302"})
	if err := p.revokeGrant(ctx, "7000001", "7000102", "7000302", core.ActorSystem); err != nil {
		t.Fatalf("revokeGrant: %v", err)
	}
	if _, ok, _ := store.GetGrant(context.Background(), "7000001", "7000102", "7000302"); ok {
		t.Fatal("a revoke whose role came off must not leave its row behind")
	}
}
