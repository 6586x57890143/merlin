package roles

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
)

// Sentencing an account that is not in the guild.
//
// The whole feature is resolveTargets tolerating one specific Discord error
// plus applyJail skipping a strip it has nothing to strip; everything after
// that is the machinery a rejoining evader already goes through. These
// tests pin the three places that could quietly go wrong: the double-check
// that separates a real absent account from a typo, the fact that nothing
// is written to Discord for somebody who is not there, and that the
// sentence actually lands when they arrive.

// TestResolveTargetsAcceptsAbsentAccountAfterCheckingItExists is the
// double-check itself. Unknown Member alone must not be enough: it is the
// same answer a mistyped snowflake gets.
func TestResolveTargetsAcceptsAbsentAccountAfterCheckingItExists(t *testing.T) {
	ops := newFakeOps()
	ops.setMember("g1", "here", []string{"role-a"})
	ops.setUser("elsewhere") // A real account, not in this guild.
	// "typo" is registered nowhere: no member, no user.

	p := newTestPlugin(ops, newFakeStore(), newFakeSettings(), newFakeAudit(), newFakePerms(), newFakeScheduler())

	targets, failed := p.resolveTargets("g1", []string{"here", "elsewhere", "typo"})

	if len(targets) != 2 {
		t.Fatalf("expected the member and the absent account as targets, got %+v", targets)
	}
	if targets[0].absent || !slices.Equal(targets[0].roles, []string{"role-a"}) {
		t.Fatalf("present member resolved wrong: %+v", targets[0])
	}
	if !targets[1].absent || len(targets[1].roles) != 0 {
		t.Fatalf("absent account resolved wrong: %+v", targets[1])
	}
	if len(failed) != 1 {
		t.Fatalf("expected the unknown snowflake to fail, got %v", failed)
	}
	// The member who is here must not cost a user lookup: the guild fetch
	// already answered.
	if !slices.Equal(ops.userCalls, []string{"elsewhere", "typo"}) {
		t.Fatalf("user lookups ran on the wrong IDs: %v", ops.userCalls)
	}
}

// TestResolveTargetsDoesNotTreatATransientFailureAsAbsence pins the
// direction that matters. A rate limit says nothing about whether somebody
// is in the guild, and recording a sentence that strips nothing against a
// member who is standing right there is the fail-open version of this.
func TestResolveTargetsDoesNotTreatATransientFailureAsAbsence(t *testing.T) {
	ops := newFakeOps()
	ops.memberFetchErrFor = map[string]error{"u1": transientErr()}
	ops.setUser("u1")

	p := newTestPlugin(ops, newFakeStore(), newFakeSettings(), newFakeAudit(), newFakePerms(), newFakeScheduler())

	targets, failed := p.resolveTargets("g1", []string{"u1"})
	if len(targets) != 0 || len(failed) != 1 {
		t.Fatalf("a 500 must fail, not resolve as absent: targets=%+v failed=%v", targets, failed)
	}
	if len(ops.userCalls) != 0 {
		t.Fatalf("a 500 must not reach the user lookup at all, got %v", ops.userCalls)
	}
}

// TestJailManyRecordsAnAbsentAccountWithoutTouchingDiscord: the sentence
// exists, nobody was edited, and the outcome is reported as pending rather
// than as a jail that happened.
func TestJailManyRecordsAnAbsentAccountWithoutTouchingDiscord(t *testing.T) {
	ops := newFakeOps()
	ops.setUser("u1")
	store := newFakeStore()
	p := newTestPlugin(ops, store, newFakeSettings(), newFakeAudit(), newFakePerms(), newFakeScheduler())

	res := p.jailMany(context.Background(), "g1", "jail-role",
		[]jailTarget{{userID: "u1", absent: true}}, time.Hour, "mod1", "raid warning")

	if len(res.pending) != 1 || res.pending[0] != "u1" {
		t.Fatalf("expected u1 reported as pending, got %+v", res)
	}
	if len(res.jailed) != 0 {
		t.Fatalf("an absent account must not report as jailed: %+v", res)
	}
	if len(ops.memberEditCalls) != 0 {
		t.Fatalf("expected no member edit for somebody who is not here, got %v", ops.memberEditCalls)
	}
	rec, ok, err := store.GetJail(context.Background(), "g1", "u1")
	if err != nil || !ok {
		t.Fatalf("expected the sentence recorded: ok=%v err=%v", ok, err)
	}
	if len(rec.SnapshotRoleIDs) != 0 {
		t.Fatalf("an absent account held nothing here; snapshot must be empty, got %v", rec.SnapshotRoleIDs)
	}
	if rec.JailRoleID != "jail-role" {
		t.Fatalf("recorded against the wrong marker: %q", rec.JailRoleID)
	}
}

// TestPendingSentenceAppliesOnArrival is the payoff: the row on its own is
// the mechanism, and the same path that re-jails an evader puts the marker
// on somebody arriving for the first time.
func TestPendingSentenceAppliesOnArrival(t *testing.T) {
	ops := newFakeOps()
	ops.setUser("u1")
	store := newFakeStore()
	p := newTestPlugin(ops, store, newFakeSettings(), newFakeAudit(), newFakePerms(), newFakeScheduler())

	if res := p.jailMany(context.Background(), "g1", "jail-role",
		[]jailTarget{{userID: "u1", absent: true}}, time.Hour, "mod1", ""); len(res.pending) != 1 {
		t.Fatalf("setup: expected a pending sentence, got %+v", res)
	}

	// They join: a member now exists, with a JoinedAt after the sentence and
	// the guild's own starter role, and no marker.
	ops.setMemberJoined("g1", "u1", []string{"newbie"}, fixedNow.Add(time.Minute))
	p.HandleMemberJoin(context.Background(), "g1", "u1")

	m, err := ops.GuildMember("g1", "u1")
	if err != nil {
		t.Fatalf("fetch member: %v", err)
	}
	if !slices.Equal(m.Roles, []string{"jail-role"}) {
		t.Fatalf("expected the marker applied on arrival, got %v", m.Roles)
	}
	if _, ok, _ := store.GetJail(context.Background(), "g1", "u1"); !ok {
		t.Fatal("the sentence must still be tracked after it lands")
	}
}

// TestPendingSentenceSurvivesUntilItsEndIfNobodyArrives: nothing to apply,
// nothing to strip, and the row is still there for whenever they show up.
func TestPendingSentenceSurvivesUntilItsEndIfNobodyArrives(t *testing.T) {
	ops := newFakeOps()
	ops.setUser("u1")
	store := newFakeStore()
	p := newTestPlugin(ops, store, newFakeSettings(), newFakeAudit(), newFakePerms(), newFakeScheduler())

	p.jailMany(context.Background(), "g1", "jail-role",
		[]jailTarget{{userID: "u1", absent: true}}, time.Hour, "mod1", "")

	if err := p.reapplyEvadedJails(context.Background(), "g1"); err != nil {
		t.Fatalf("sweep over an absent member: %v", err)
	}
	if _, ok, _ := store.GetJail(context.Background(), "g1", "u1"); !ok {
		t.Fatal("the sweep must not drop a sentence just because nobody has arrived")
	}
	if len(ops.memberEditCalls) != 0 {
		t.Fatalf("nothing to edit, got %v", ops.memberEditCalls)
	}
}

// TestTransferMovesAnAbsentSentenceWithoutAnEdit: jail somebody who is not
// here, then send them on vacation instead. Only the row moves.
func TestTransferMovesAnAbsentSentenceWithoutAnEdit(t *testing.T) {
	ops := newFakeOps()
	ops.setUser("u1")
	ops.roles["g1"] = []*discordgo.Role{{ID: "jail-role"}, {ID: "island"}}
	settings := newFakeSettings()
	settings.vacationRole["g1"] = "island"
	store := newFakeStore()
	p := newTestPlugin(ops, store, settings, newFakeAudit(), newFakePerms(), newFakeScheduler())

	p.jailMany(context.Background(), "g1", "jail-role",
		[]jailTarget{{userID: "u1", absent: true}}, time.Hour, "mod1", "")
	res := p.jailMany(context.Background(), "g1", "island",
		[]jailTarget{{userID: "u1", absent: true}}, 2*time.Hour, "mod1", "")

	if len(res.transferred) != 1 {
		t.Fatalf("expected the pending sentence moved to the island, got %+v", res)
	}
	rec, ok, _ := store.GetJail(context.Background(), "g1", "u1")
	if !ok || rec.JailRoleID != "island" {
		t.Fatalf("row did not follow the move: ok=%v rec=%+v", ok, rec)
	}
	if len(ops.memberEditCalls) != 0 {
		t.Fatalf("nobody to edit, got %v", ops.memberEditCalls)
	}
}
