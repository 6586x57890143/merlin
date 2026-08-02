package roles

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
)

func bulkPlugin(ops *fakeOps, store *fakeStore, perms *fakePerms) *Plugin {
	return newTestPlugin(ops, store, newFakeSettings(), newFakeAudit(), perms, newFakeScheduler())
}

// --- option collection ---

func TestCollectJailUserIDsReadsEveryFilledSlotInOrder(t *testing.T) {
	opts := map[string]*discordgo.ApplicationCommandInteractionDataOption{
		"user":     {Name: "user", Value: "u1"},
		"user3":    {Name: "user3", Value: "u3"},
		"user5":    {Name: "user5", Value: "u5"},
		"duration": {Name: "duration", Value: "1h"},
	}
	got := collectJailUserIDs(opts)
	if !slices.Equal(got, []string{"u1", "u3", "u5"}) {
		t.Errorf("collectJailUserIDs = %v, want [u1 u3 u5]", got)
	}
}

// Picking the same member in two slots is a slip. Letting it through would
// jail them from the first slot and then report them "already jailed" by
// their own command, which reads as a failure.
func TestCollectJailUserIDsDropsDuplicates(t *testing.T) {
	opts := map[string]*discordgo.ApplicationCommandInteractionDataOption{
		"user":  {Name: "user", Value: "u1"},
		"user2": {Name: "user2", Value: "u1"},
		"user3": {Name: "user3", Value: "u2"},
	}
	got := collectJailUserIDs(opts)
	if !slices.Equal(got, []string{"u1", "u2"}) {
		t.Errorf("collectJailUserIDs = %v, want [u1 u2] — the duplicate should collapse", got)
	}
}

func TestCollectJailUserIDsIgnoresUnfilledSlots(t *testing.T) {
	opts := map[string]*discordgo.ApplicationCommandInteractionDataOption{
		"user":  {Name: "user", Value: "u1"},
		"user2": {Name: "user2", Value: ""},
	}
	if got := collectJailUserIDs(opts); !slices.Equal(got, []string{"u1"}) {
		t.Errorf("collectJailUserIDs = %v, want [u1]", got)
	}
}

// --- jailMany ---

func TestJailManyJailsEveryTargetAndTracksThemAll(t *testing.T) {
	ops := newFakeOps()
	targets := make([]jailTarget, 0, 3)
	for _, id := range []string{"u1", "u2", "u3"} {
		ops.setMember("g1", id, []string{"role-a"})
		targets = append(targets, jailTarget{userID: id, roles: []string{"role-a"}})
	}
	store := newFakeStore()
	p := bulkPlugin(ops, store, newFakePerms())

	res := p.jailMany(context.Background(), "g1", "jail-role", targets, time.Hour, "mod1", "raid")

	if len(res.jailed) != 3 {
		t.Fatalf("jailed %v, want all three", res.jailed)
	}
	for _, id := range []string{"u1", "u2", "u3"} {
		rec, ok, _ := store.GetJail(context.Background(), "g1", id)
		if !ok {
			t.Errorf("%s was jailed with no record tracking them — nothing would ever release them", id)
			continue
		}
		if !slices.Equal(rec.SnapshotRoleIDs, []string{"role-a"}) {
			t.Errorf("%s snapshot = %v, want [role-a]", id, rec.SnapshotRoleIDs)
		}
		member, _ := ops.GuildMember("g1", id)
		if !slices.Contains(member.Roles, "jail-role") {
			t.Errorf("%s did not end up holding the jail marker: %v", id, member.Roles)
		}
	}
}

// One member failing must not abort the batch or silently vanish from the
// report. A mod who reads "jailed 4" when only 3 were is worse off than one
// who reads "jailed 3, 1 failed".
func TestJailManyReportsPerTargetOutcomesAndKeepsGoing(t *testing.T) {
	ops := newFakeOps()
	for _, id := range []string{"u1", "u2", "u3"} {
		ops.setMember("g1", id, nil)
	}
	store := newFakeStore()

	// u2 is already jailed by an earlier action.
	existing := JailRecord{
		GuildID: "g1", UserID: "u2", SnapshotRoleIDs: []string{"real-role"},
		JailRoleID: "jail-role", JailedAt: fixedNow.Add(-time.Hour),
	}
	if err := store.InsertJail(context.Background(), existing); err != nil {
		t.Fatalf("InsertJail: %v", err)
	}

	p := bulkPlugin(ops, store, newFakePerms())
	targets := []jailTarget{{userID: "u1"}, {userID: "u2"}, {userID: "u3"}}
	res := p.jailMany(context.Background(), "g1", "jail-role", targets, time.Hour, "mod1", "")

	if !slices.Equal(res.jailed, []string{"u1", "u3"}) {
		t.Errorf("jailed = %v, want [u1 u3]", res.jailed)
	}
	if !slices.Equal(res.alreadyIn, []string{"u2"}) {
		t.Errorf("alreadyIn = %v, want [u2]", res.alreadyIn)
	}
	// The pre-existing record must be untouched — re-jailing would overwrite
	// its snapshot with the stripped state.
	rec, _, _ := store.GetJail(context.Background(), "g1", "u2")
	if !slices.Equal(rec.SnapshotRoleIDs, []string{"real-role"}) {
		t.Errorf("an already-jailed member's snapshot was overwritten: %v", rec.SnapshotRoleIDs)
	}
}

// --- rank partitioning ---

func TestPartitionByRankSkipsProtectedTargetsAndKeepsTheRest(t *testing.T) {
	perms := newFakePerms()
	perms.protected["admin-user"] = true
	p := bulkPlugin(newFakeOps(), newFakeStore(), perms)

	targets := []jailTarget{{userID: "u1"}, {userID: "admin-user"}, {userID: "u2"}}
	allowed, res := p.partitionByRank("g1", actorMember("mod1"), targets)

	got := make([]string, len(allowed))
	for i, a := range allowed {
		got[i] = a.userID
	}
	if !slices.Equal(got, []string{"u1", "u2"}) {
		t.Errorf("allowed = %v, want [u1 u2]", got)
	}
	if !slices.Equal(res.protected, []string{"admin-user"}) {
		t.Errorf("protected = %v, want [admin-user]", res.protected)
	}
}

// An unresolvable guild state is not "they're an ordinary member". It has to
// land in failed, not silently in allowed.
func TestPartitionByRankFailsClosedWhenTheCheckCannotBeMade(t *testing.T) {
	perms := newFakePerms()
	perms.moderateErr = fmt.Errorf("no session state available")
	p := bulkPlugin(newFakeOps(), newFakeStore(), perms)

	allowed, res := p.partitionByRank("g1", actorMember("mod1"), []jailTarget{{userID: "u1"}})
	if len(allowed) != 0 {
		t.Errorf("allowed %v despite an unresolvable rank check", allowed)
	}
	if len(res.failed) != 1 {
		t.Errorf("failed = %v, want the unresolvable target reported", res.failed)
	}
}

// The reason partitionByRank runs before resolveJailRole: resolving can
// *create* the Jailed role and write an overwrite to every channel. A batch
// the actor may not touch at all must leave nothing behind.
func TestFullyRefusedBatchCreatesNoJailRole(t *testing.T) {
	ops := newFakeOps()
	ops.setMember("g1", "admin-user", []string{"admin-role"})
	perms := newFakePerms()
	perms.protected["admin-user"] = true
	p := bulkPlugin(ops, newFakeStore(), perms)

	targets, _ := p.resolveTargets("g1", []string{"admin-user"})
	allowed, res := p.partitionByRank("g1", actorMember("mod1"), targets)
	if len(allowed) != 0 {
		t.Fatal("the protected target was not filtered out")
	}
	if len(res.protected) != 1 {
		t.Fatalf("protected = %v", res.protected)
	}
	if roles, _ := ops.GuildRoles("g1"); len(roles) != 0 {
		t.Errorf("a fully-refused batch created guild roles: %v", roles)
	}
}

// --- membersWithRole ---

func seedMembers(ops *fakeOps, guildID, roleID string, n int, from int) {
	for i := range n {
		ops.setMember(guildID, fmt.Sprintf("u%04d", from+i), []string{roleID})
	}
}

func TestMembersWithRoleFindsOnlyHolders(t *testing.T) {
	ops := newFakeOps()
	seedMembers(ops, "g1", "raider", 3, 1)
	ops.setMember("g1", "u9999", []string{"regular"})

	p := bulkPlugin(ops, newFakeStore(), newFakePerms())
	matches, complete, err := p.membersWithRole("g1", "raider")
	if err != nil {
		t.Fatalf("membersWithRole: %v", err)
	}
	if !complete {
		t.Error("a small guild should have been enumerated completely")
	}
	if len(matches) != 3 {
		t.Fatalf("matched %d members, want 3: %+v", len(matches), matches)
	}
	for _, m := range matches {
		if m.userID == "u9999" {
			t.Error("matched a member who does not hold the role")
		}
	}
}

// The cursor has to advance, or a guild larger than one page loops on the
// same page forever (or, worse, silently returns only the first page).
func TestMembersWithRolePagesBeyondOnePage(t *testing.T) {
	ops := newFakeOps()
	// memberPageSize is 1000; seed enough to force a second page while
	// staying under the match cap by giving most of them a different role.
	seedMembers(ops, "g1", "other", memberPageSize, 1)
	seedMembers(ops, "g1", "raider", 5, 5000)

	p := bulkPlugin(ops, newFakeStore(), newFakePerms())
	matches, complete, err := p.membersWithRole("g1", "raider")
	if err != nil {
		t.Fatalf("membersWithRole: %v", err)
	}
	if !complete {
		t.Error("scan reported incomplete on a guild well inside the page budget")
	}
	if len(matches) != 5 {
		t.Errorf("matched %d, want 5 — members past the first page were missed", len(matches))
	}
	if ops.memberListCalls < 2 {
		t.Errorf("only %d page fetch(es); pagination never advanced", ops.memberListCalls)
	}
}

func TestMembersWithRoleStopsOncePastTheCap(t *testing.T) {
	ops := newFakeOps()
	seedMembers(ops, "g1", "raider", maxBulkJailTargets+20, 1)

	p := bulkPlugin(ops, newFakeStore(), newFakePerms())
	matches, _, err := p.membersWithRole("g1", "raider")
	if err != nil {
		t.Fatalf("membersWithRole: %v", err)
	}
	if len(matches) != maxBulkJailTargets+1 {
		t.Errorf("collected %d matches; enumeration should stop one past the cap, which is all the caller needs", len(matches))
	}
}

// Without the intent (or on any other listing failure) the answer must be an
// error, never an empty match set that reads as "nobody holds this role".
func TestMembersWithRoleReportsListingFailure(t *testing.T) {
	ops := newFakeOps()
	ops.memberListErr = transientErr()
	p := bulkPlugin(ops, newFakeStore(), newFakePerms())

	if _, _, err := p.membersWithRole("g1", "raider"); err == nil {
		t.Fatal("a failed member listing must not look like an empty role")
	}
}

// --- target guards ---

func TestJailRoleRefusesEveryone(t *testing.T) {
	p := bulkPlugin(newFakeOps(), newFakeStore(), newFakePerms())
	// @everyone's role ID is the guild's own ID.
	err := p.validateJailRoleTarget("g1", "g1")
	if err == nil {
		t.Fatal("jailing @everyone was allowed; that jails the whole server including whoever could undo it")
	}
	if !strings.Contains(err.Error(), "@everyone") {
		t.Errorf("refusal does not explain why: %v", err)
	}
}

func TestJailRoleRefusesRoleTheBotCannotManage(t *testing.T) {
	perms := newFakePerms()
	perms.unmanageable["staff"] = true
	p := bulkPlugin(newFakeOps(), newFakeStore(), perms)

	if err := p.validateJailRoleTarget("g1", "staff"); err == nil {
		t.Fatal("allowed jailing a role the bot can't strip; members would keep it and the jail would not do its job")
	}
}

func TestJailRoleAllowsAnOrdinaryRole(t *testing.T) {
	p := bulkPlugin(newFakeOps(), newFakeStore(), newFakePerms())
	if err := p.validateJailRoleTarget("g1", "raider"); err != nil {
		t.Fatalf("an ordinary manageable role was refused: %v", err)
	}
}

// --- self/bot exclusion ---

func TestExcludeSelfAndBotDropsBothAndKeepsOthers(t *testing.T) {
	p := bulkPlugin(newFakeOps(), newFakeStore(), newFakePerms())
	session := &discordgo.Session{State: discordgo.NewState()}
	session.State.User = &discordgo.User{ID: "bot-id"}

	targets := []jailTarget{{userID: "mod1"}, {userID: "bot-id"}, {userID: "u1"}}
	got := p.excludeSelfAndBot(targets, "mod1", session)

	if len(got) != 1 || got[0].userID != "u1" {
		t.Errorf("excludeSelfAndBot = %+v, want just u1 — jailing the actor or the bot is an accident, not an intent", got)
	}
}

// --- reporting ---

// The property that matters: nobody who was not jailed may be omitted. A
// summary that lists only successes lets a mod believe a raid was contained.
func TestSummaryAccountsForEveryNonJailedMember(t *testing.T) {
	res := bulkJailResult{
		jailed:       []string{"u1"},
		alreadyIn:    []string{"u2"},
		protected:    []string{"u3"},
		failed:       []string{"u4: boom"},
		unmanageable: 1,
	}
	out := summarizeBulkJail(res, time.Hour)
	for _, want := range []string{"u1", "u2", "u3", "u4", "kept at least one role"} {
		if !strings.Contains(out, want) {
			t.Errorf("summary omits %q, so its absence would go unnoticed:\n%s", want, out)
		}
	}
}

// An embed field over 1024 bytes fails the whole message rather than being
// trimmed, and a 50-member batch reaches that easily — the report has to
// survive the size it was designed for.
func TestSummaryStaysWithinDiscordsFieldLimit(t *testing.T) {
	var res bulkJailResult
	for i := range maxBulkJailTargets {
		res.jailed = append(res.jailed, fmt.Sprintf("12345678901234567%d", i))
	}
	if got := len(summarizeBulkJail(res, time.Hour)); got > 1024 {
		t.Errorf("summary is %d bytes; Discord rejects the entire message over 1024", got)
	}
}

func TestDescribeCountDistinguishesAFloorFromATotal(t *testing.T) {
	if got := describeCount(3, true); got != "3" {
		t.Errorf("describeCount(3, complete) = %q, want %q", got, "3")
	}
	if got := describeCount(12, false); got != "at least 12" {
		t.Errorf("an incomplete scan must not report its floor as a total, got %q", got)
	}
	if got := describeCount(maxBulkJailTargets+1, true); !strings.HasPrefix(got, "at least") {
		t.Errorf("a capped scan must not report its floor as a total, got %q", got)
	}
}

func TestMergeCombinesEveryCategory(t *testing.T) {
	a := bulkJailResult{jailed: []string{"u1"}, protected: []string{"p1"}, unmanageable: 1}
	b := bulkJailResult{jailed: []string{"u2"}, alreadyIn: []string{"a1"}, failed: []string{"f1"}, unmanageable: 2}
	got := a.merge(b)

	if !slices.Equal(got.jailed, []string{"u1", "u2"}) {
		t.Errorf("jailed = %v", got.jailed)
	}
	if !slices.Equal(got.protected, []string{"p1"}) || !slices.Equal(got.alreadyIn, []string{"a1"}) || !slices.Equal(got.failed, []string{"f1"}) {
		t.Errorf("merge dropped a category: %+v", got)
	}
	if got.unmanageable != 3 {
		t.Errorf("unmanageable = %d, want 3", got.unmanageable)
	}
	if got.attempted() != 5 {
		t.Errorf("attempted = %d, want 5", got.attempted())
	}
}
