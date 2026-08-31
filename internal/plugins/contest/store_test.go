package contest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/6586x57890143/merlin/internal/dbtest"
)

// The fakes in fakes_test.go let the phase machine and the forum sync be
// driven without a database, which is the right seam for those. What they
// cannot check is the half of this store's contract that Postgres itself
// enforces: the partial unique index that makes one entry per member a
// database rule rather than a hopeful comment, the conditional UPDATE that
// two overlapping ticks race on, the ON CONFLICT that turns a re-read forum
// post into a refreshed CDN link, and the CHECK constraint standing between
// a hand-edited row and a phase this code cannot render.
//
// Skips rather than fails without TEST_DATABASE_URL, so `go test ./...`
// still works with no setup. CI runs it against postgres:16-alpine.

func testStore(t *testing.T) (Store, string) {
	t.Helper()
	pool := dbtest.Pool(t)
	// A fresh guild id per test, since dbtest's database is shared across
	// them and a leftover contest would be picked up by LiveContest.
	guildID := "g-" + newID()
	return NewPostgresStore(pool), guildID
}

// thread ids are Discord snowflakes in production, so they are unique across
// every contest in every guild. dbtest shares one database between tests, so
// fixtures have to be unique too: reusing a literal "t1" would let one test
// resurrect another test's withdrawn row through the thread_id upsert, which
// production cannot do and which says nothing about this code.
func threadID(c Contest, n string) string { return c.ID + "-" + n }

func seed(t *testing.T, s Store, guildID string, phase Phase) Contest {
	t.Helper()
	base := time.Now().UTC().Truncate(time.Second)
	slug, err := newSlug()
	if err != nil {
		t.Fatalf("newSlug: %v", err)
	}
	c := Contest{
		ID: newID(), GuildID: guildID, Slug: slug,
		Title: "neon cats", Theme: "cats, but neon", Phase: phase,
		SubmitAt: base.Add(time.Hour), VoteAt: base.Add(2 * time.Hour),
		ResultsAt: base.Add(3 * time.Hour), MaxVotes: 3,
		AnnounceChannelID: "announce-1", CreatedBy: "mod-1",
	}
	if err := s.CreateContest(context.Background(), c); err != nil {
		t.Fatalf("CreateContest: %v", err)
	}
	return c
}

func TestPostgresContestRoundTrips(t *testing.T) {
	s, guildID := testStore(t)
	ctx := context.Background()
	want := seed(t, s, guildID, PhaseAnnounce)

	got, err := s.LiveContest(ctx, guildID)
	if err != nil {
		t.Fatalf("LiveContest: %v", err)
	}
	if got.ID != want.ID || got.Title != want.Title || got.Theme != want.Theme ||
		got.MaxVotes != want.MaxVotes || got.CreatedBy != want.CreatedBy {
		t.Errorf("round trip lost something:\n got %+v\nwant %+v", got, want)
	}
	if !got.SubmitAt.Equal(want.SubmitAt) || !got.ResultsAt.Equal(want.ResultsAt) {
		t.Errorf("deadlines drifted: %v %v", got.SubmitAt, got.ResultsAt)
	}

	if err := s.SetForumChannel(ctx, want.ID, "forum-1"); err != nil {
		t.Fatalf("SetForumChannel: %v", err)
	}
	if err := s.SetAnnounceMessage(ctx, want.ID, "announce-2", "msg-1"); err != nil {
		t.Fatalf("SetAnnounceMessage: %v", err)
	}
	got, _ = s.LiveContest(ctx, guildID)
	if got.ForumChannelID != "forum-1" || got.AnnounceMessageID != "msg-1" {
		t.Errorf("forum/announce not recorded: %+v", got)
	}

	guilds, err := s.GuildsWithLiveContests(ctx)
	if err != nil {
		t.Fatalf("GuildsWithLiveContests: %v", err)
	}
	var found bool
	for _, g := range guilds {
		if g == guildID {
			found = true
		}
	}
	if !found {
		t.Error("a live contest's guild was not listed")
	}
}

func TestPostgresNoContestIsNotAnError(t *testing.T) {
	s, guildID := testStore(t)
	if _, err := s.LiveContest(context.Background(), guildID); !errors.Is(err, ErrNoLiveContest) {
		t.Errorf("LiveContest on an empty guild = %v, want ErrNoLiveContest", err)
	}
	if _, err := s.LatestContest(context.Background(), guildID); !errors.Is(err, ErrNoLiveContest) {
		t.Errorf("LatestContest on an empty guild = %v, want ErrNoLiveContest", err)
	}
}

// The conditional UPDATE is what stops two overlapping ticks both announcing
// the same transition. Exactly one caller may win a given move.
func TestPostgresOnlyOneCallerWinsAPhaseAdvance(t *testing.T) {
	s, guildID := testStore(t)
	ctx := context.Background()
	c := seed(t, s, guildID, PhaseAnnounce)

	won, err := s.AdvancePhase(ctx, c.ID, PhaseAnnounce, PhaseSubmit)
	if err != nil || !won {
		t.Fatalf("first advance = %v, %v, want won", won, err)
	}
	won, err = s.AdvancePhase(ctx, c.ID, PhaseAnnounce, PhaseSubmit)
	if err != nil {
		t.Fatalf("second advance: %v", err)
	}
	if won {
		t.Error("two callers both won the same phase advance, so it would be announced twice")
	}

	got, _ := s.LiveContest(ctx, guildID)
	if got.Phase != PhaseSubmit {
		t.Errorf("phase = %s, want submit", got.Phase)
	}
}

// A cancelled contest is not live and not the latest; a finished one is not
// live but is still the latest, because /contest link has to answer for it.
func TestPostgresLiveAndLatestDisagreeCorrectly(t *testing.T) {
	s, guildID := testStore(t)
	ctx := context.Background()
	c := seed(t, s, guildID, PhaseVote)

	if _, err := s.AdvancePhase(ctx, c.ID, PhaseVote, PhaseResults); err != nil {
		t.Fatalf("advance: %v", err)
	}
	if _, err := s.LiveContest(ctx, guildID); !errors.Is(err, ErrNoLiveContest) {
		t.Error("a finished contest still reads as live, so its tick job would never be dropped")
	}
	if got, err := s.LatestContest(ctx, guildID); err != nil || got.ID != c.ID {
		t.Errorf("LatestContest = %+v, %v, want the finished contest", got, err)
	}

	if _, err := s.AdvancePhase(ctx, c.ID, PhaseResults, PhaseCancelled); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if _, err := s.LatestContest(ctx, guildID); !errors.Is(err, ErrNoLiveContest) {
		t.Error("a cancelled contest is still being offered by LatestContest")
	}
}

// The CHECK constraint is the backstop against a hand-edited row holding a
// phase this code cannot render.
func TestPostgresRefusesAnUnknownPhase(t *testing.T) {
	s, guildID := testStore(t)
	c := Contest{
		ID: newID(), GuildID: guildID, Slug: "x" + newID(),
		Title: "bad", Phase: Phase("elimination"),
		SubmitAt: time.Now(), VoteAt: time.Now(), ResultsAt: time.Now(),
		CreatedBy: "mod-1",
	}
	if err := s.CreateContest(context.Background(), c); err == nil {
		t.Error("the database accepted a phase nothing in this package can render")
	}
}

func TestPostgresResultsAndTallyErrorRoundTrip(t *testing.T) {
	s, guildID := testStore(t)
	ctx := context.Background()
	c := seed(t, s, guildID, PhaseVote)

	if err := s.SetTallyError(ctx, c.ID, "d1 is having a moment"); err != nil {
		t.Fatalf("SetTallyError: %v", err)
	}
	got, _ := s.LiveContest(ctx, guildID)
	if got.TallyError != "d1 is having a moment" {
		t.Errorf("tally error = %q", got.TallyError)
	}

	blob, err := marshalResults([]resultView{{ID: "s1", Votes: 9, Rank: 1}})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	closedAt := time.Now().UTC().Truncate(time.Second)
	if err := s.SetResults(ctx, c.ID, blob, closedAt); err != nil {
		t.Fatalf("SetResults: %v", err)
	}
	got, _ = s.LiveContest(ctx, guildID)
	rs, err := unmarshalResults(got.Results)
	if err != nil || len(rs) != 1 || rs[0].Votes != 9 {
		t.Fatalf("results = %+v, %v", rs, err)
	}
	// Recording a tally clears the last failure, or /contest status would
	// keep reporting an error that has since been resolved.
	if got.TallyError != "" {
		t.Errorf("tally error survived a successful close: %q", got.TallyError)
	}
	if got.ClosedAt == nil || !got.ClosedAt.Equal(closedAt) {
		t.Errorf("closed_at = %v, want %v", got.ClosedAt, closedAt)
	}
}

// One live entry per member, enforced by the partial unique index rather
// than by the statement. This is the rule that makes "one each" true even if
// two ticks and a gateway handler all try at once.
func TestPostgresRefusesASecondLiveEntryFromOneMember(t *testing.T) {
	s, guildID := testStore(t)
	ctx := context.Background()
	c := seed(t, s, guildID, PhaseSubmit)

	first := Submission{ID: newID(), ContestID: c.ID, UserID: "u1", ThreadID: threadID(c, "1"), Title: "one"}
	if err := s.UpsertSubmission(ctx, first); err != nil {
		t.Fatalf("first entry: %v", err)
	}
	second := Submission{ID: newID(), ContestID: c.ID, UserID: "u1", ThreadID: threadID(c, "2"), Title: "two"}
	if err := s.UpsertSubmission(ctx, second); err == nil {
		t.Fatal("a member got two live entries, so one each is not actually enforced")
	}

	// Withdrawing the first frees the slot, which is what makes deleting a
	// post and posting again work.
	if err := s.WithdrawMissing(ctx, c.ID, []string{threadID(c, "2")}, time.Now()); err != nil {
		t.Fatalf("withdraw: %v", err)
	}
	if err := s.UpsertSubmission(ctx, second); err != nil {
		t.Fatalf("re-entry after withdrawing: %v", err)
	}
	subs, err := s.Submissions(ctx, c.ID)
	if err != nil || len(subs) != 1 || subs[0].ThreadID != threadID(c, "2") {
		t.Fatalf("entries = %+v, %v", subs, err)
	}
}

// Re-reading a forum post is how a stale Discord CDN link gets refreshed, so
// the upsert has to update rather than collide, and has to un-withdraw a row
// whose thread came back.
func TestPostgresUpsertRefreshesAnEntryInPlace(t *testing.T) {
	s, guildID := testStore(t)
	ctx := context.Background()
	c := seed(t, s, guildID, PhaseSubmit)

	sub := Submission{
		ID: newID(), ContestID: c.ID, UserID: "u1", ThreadID: threadID(c, "1"),
		Title: "one", Author: "ana", Kind: "image", MediaURL: "https://cdn/a.png?ex=old",
	}
	if err := s.UpsertSubmission(ctx, sub); err != nil {
		t.Fatalf("insert: %v", err)
	}

	sub.ID = newID() // a fresh read makes a fresh id, and must not make a row
	sub.Title = "one, retitled"
	sub.MediaURL = "https://cdn/a.png?ex=fresh"
	if err := s.UpsertSubmission(ctx, sub); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	subs, err := s.Submissions(ctx, c.ID)
	if err != nil || len(subs) != 1 {
		t.Fatalf("entries = %+v, %v", subs, err)
	}
	if subs[0].MediaURL != "https://cdn/a.png?ex=fresh" || subs[0].Title != "one, retitled" {
		t.Errorf("entry was not refreshed in place: %+v", subs[0])
	}
}

func TestPostgresWithdrawMissingOnlyTouchesThisContest(t *testing.T) {
	s, guildID := testStore(t)
	ctx := context.Background()
	a := seed(t, s, guildID, PhaseSubmit)
	if _, err := s.AdvancePhase(ctx, a.ID, PhaseSubmit, PhaseResults); err != nil {
		t.Fatalf("finish the first: %v", err)
	}
	b := seed(t, s, guildID, PhaseSubmit)

	for _, sub := range []Submission{
		{ID: newID(), ContestID: a.ID, UserID: "u1", ThreadID: threadID(a, "1"), Title: "old"},
		{ID: newID(), ContestID: b.ID, UserID: "u1", ThreadID: threadID(b, "1"), Title: "new"},
	} {
		if err := s.UpsertSubmission(ctx, sub); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	// Syncing the second contest's empty forum must not withdraw the first
	// contest's entries.
	if err := s.WithdrawMissing(ctx, b.ID, nil, time.Now()); err != nil {
		t.Fatalf("withdraw: %v", err)
	}
	if subs, _ := s.Submissions(ctx, a.ID); len(subs) != 1 {
		t.Error("syncing one contest withdrew another contest's entries")
	}
	if subs, _ := s.Submissions(ctx, b.ID); len(subs) != 0 {
		t.Error("withdraw did not run on its own contest")
	}
}

func TestPostgresPrizeLifecycle(t *testing.T) {
	s, guildID := testStore(t)
	ctx := context.Background()
	c := seed(t, s, guildID, PhaseAnnounce)

	p1 := Prize{
		ID: newID(), ContestID: c.ID, DonorID: "u9", DonorName: "dana",
		Title: "a steam key", Details: "for hollow knight",
		SecretSealed: []byte{1, 2, 3, 4},
	}
	p2 := Prize{ID: newID(), ContestID: c.ID, DonorID: "u8", Title: "50 usdc"}
	for _, p := range []Prize{p1, p2} {
		if err := s.AddPrize(ctx, p); err != nil {
			t.Fatalf("AddPrize: %v", err)
		}
	}

	got, err := s.Prizes(ctx, c.ID)
	if err != nil || len(got) != 2 {
		t.Fatalf("prizes = %+v, %v", got, err)
	}
	if !got[0].HasSecret() || got[1].HasSecret() {
		t.Errorf("secrets did not round trip: %+v", got)
	}

	// A donor may only pull their own pledge, and the check is part of the
	// delete rather than a step before it.
	ok, err := s.RemovePrize(ctx, c.ID, p1.ID, "somebody-else")
	if err != nil {
		t.Fatalf("RemovePrize: %v", err)
	}
	if ok {
		t.Fatal("somebody pulled a pledge that was not theirs")
	}

	// Awarded, delivered, wiped: the three steps in the order that keeps a
	// failed DM recoverable.
	when := time.Now().UTC().Truncate(time.Second)
	if err := s.MarkPrizeAwarded(ctx, p1.ID, "winner-1", when); err != nil {
		t.Fatalf("MarkPrizeAwarded: %v", err)
	}
	got, _ = s.Prizes(ctx, c.ID)
	if got[0].AwardedTo == nil || *got[0].AwardedTo != "winner-1" {
		t.Fatalf("award not recorded: %+v", got[0])
	}
	if !got[0].HasSecret() {
		t.Error("marking a prize awarded wiped the code before it was delivered")
	}
	if err := s.ClearPrizeSecret(ctx, p1.ID); err != nil {
		t.Fatalf("ClearPrizeSecret: %v", err)
	}
	got, _ = s.Prizes(ctx, c.ID)
	if got[0].HasSecret() {
		t.Error("the code survived being cleared")
	}

	// An awarded prize can no longer be pulled back.
	if ok, err := s.RemovePrize(ctx, c.ID, p1.ID, "u9"); err != nil || ok {
		t.Errorf("RemovePrize on an awarded prize = %v, %v, want refused", ok, err)
	}
}

func TestPostgresConfigDefaultsAndRoundTrips(t *testing.T) {
	s, guildID := testStore(t)
	ctx := context.Background()

	// An unconfigured guild is not an error: /contest new falls back to the
	// channel it was run in, which is what the mod already meant.
	cfg, err := s.GetConfig(ctx, guildID)
	if err != nil {
		t.Fatalf("GetConfig on an unconfigured guild: %v", err)
	}
	if cfg.DefaultMaxVotes != 3 || cfg.AnnounceChannelID != "" {
		t.Errorf("defaults = %+v", cfg)
	}

	cfg.AnnounceChannelID = "announce-9"
	cfg.ForumCategoryID = "cat-9"
	cfg.DefaultMaxVotes = 5
	if err := s.SetConfig(ctx, cfg); err != nil {
		t.Fatalf("SetConfig: %v", err)
	}
	cfg.DefaultMaxVotes = 1
	if err := s.SetConfig(ctx, cfg); err != nil {
		t.Fatalf("SetConfig again: %v", err)
	}

	got, err := s.GetConfig(ctx, guildID)
	if err != nil {
		t.Fatalf("GetConfig: %v", err)
	}
	if got.AnnounceChannelID != "announce-9" || got.ForumCategoryID != "cat-9" || got.DefaultMaxVotes != 1 {
		t.Errorf("config = %+v", got)
	}
}
