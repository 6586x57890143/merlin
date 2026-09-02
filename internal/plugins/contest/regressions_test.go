package contest

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
)

// entryThread is a forum post by one member, plus the starter message
// readThread will find behind it.
func entryThread(ops *fakeOps, id, owner string) *discordgo.Channel {
	ops.messages[id] = []*discordgo.Message{{
		ID: id, Content: "here it is",
		Author: &discordgo.User{ID: owner, Username: owner},
	}}
	return &discordgo.Channel{
		ID: id, GuildID: "g1", ParentID: "forum-1", OwnerID: owner, Name: "entry " + id,
	}
}

func seedLive(t *testing.T, store *fakeStore, phase Phase, base time.Time) Contest {
	t.Helper()
	c := liveContest(phase, base)
	if err := store.CreateContest(context.Background(), c); err != nil {
		t.Fatalf("seed contest: %v", err)
	}
	return c
}

// --- the entry list is what Discord says exists, not what merlin processed --

func TestATransientReadFailureDoesNotWithdrawALiveEntry(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	store, ops, sched, audit := newFakeStore(), newFakeOps(), newFakeSched(), &fakeAudit{}
	c := seedLive(t, store, PhaseSubmit, base)

	ops.threads = []*discordgo.Channel{entryThread(ops, "100", "u1")}
	p := newTestPlugin(t, store, ops, sched, audit, "")
	p.now = func() time.Time { return base }
	if err := p.syncSubmissions(context.Background(), c); err != nil {
		t.Fatalf("first sync: %v", err)
	}

	// The post is still there; the starter-message read is what fails, which
	// is what a 429 on a busy contest looks like from here.
	delete(ops.messages, "100")
	if err := p.syncSubmissions(context.Background(), c); err != nil {
		t.Fatalf("second sync: %v", err)
	}

	subs, err := store.Submissions(context.Background(), "c1")
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if len(subs) != 1 {
		t.Fatalf("entries = %d, want 1: a failed read withdrew a live entry, "+
			"which is untracking on failed rather than on gone", len(subs))
	}
}

func TestAnArchivedForumPostIsStillAnEntry(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	store, ops, sched, audit := newFakeStore(), newFakeOps(), newFakeSched(), &fakeAudit{}
	c := seedLive(t, store, PhaseSubmit, base)

	ops.threads = []*discordgo.Channel{entryThread(ops, "100", "u1")}
	p := newTestPlugin(t, store, ops, sched, audit, "")
	p.now = func() time.Time { return base }
	if err := p.syncSubmissions(context.Background(), c); err != nil {
		t.Fatalf("first sync: %v", err)
	}

	// Nobody replied for a few days, so Discord archived the post. It is
	// still sitting in the forum looking exactly like an entry.
	ops.archived = ops.threads
	ops.threads = nil
	if err := p.syncSubmissions(context.Background(), c); err != nil {
		t.Fatalf("second sync: %v", err)
	}

	if ops.archivedAskedFor != c.ForumChannelID {
		t.Errorf("asked %q for archived threads, want the forum %q",
			ops.archivedAskedFor, c.ForumChannelID)
	}
	subs, err := store.Submissions(context.Background(), "c1")
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if len(subs) != 1 {
		t.Fatalf("entries = %d, want 1: an archived post read as a deleted one", len(subs))
	}
}

func TestDeletingAndRepostingInOneTickKeepsTheNewEntry(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	store, ops, sched, audit := newFakeStore(), newFakeOps(), newFakeSched(), &fakeAudit{}
	c := seedLive(t, store, PhaseSubmit, base)

	ops.threads = []*discordgo.Channel{entryThread(ops, "100", "u1")}
	p := newTestPlugin(t, store, ops, sched, audit, "")
	p.now = func() time.Time { return base }
	if err := p.syncSubmissions(context.Background(), c); err != nil {
		t.Fatalf("first sync: %v", err)
	}

	// Same member, new post, old one gone: both happen between two ticks, so
	// one sync has to retire the old row before inserting the new one or the
	// insert lands on contest_submissions_one_live_idx.
	ops.threads = []*discordgo.Channel{entryThread(ops, "200", "u1")}
	if err := p.syncSubmissions(context.Background(), c); err != nil {
		t.Fatalf("second sync: %v", err)
	}

	subs, err := store.Submissions(context.Background(), "c1")
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if len(subs) != 1 || subs[0].ThreadID != "200" {
		t.Fatalf("entries = %+v, want exactly the replacement post 200", subs)
	}
}

func TestMaxEntriesActuallyCapsTheEntryList(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	store, ops, sched, audit := newFakeStore(), newFakeOps(), newFakeSched(), &fakeAudit{}
	c := seedLive(t, store, PhaseSubmit, base)

	// Every entry by a different member, so the one-per-member rule is not
	// what does the trimming. IDs are fixed width so string order (which is
	// what snowflake order is) matches numeric order.
	for i := range maxEntries + 20 {
		id := strconv.Itoa(100000 + i)
		ops.threads = append(ops.threads, entryThread(ops, id, "u"+id))
	}

	p := newTestPlugin(t, store, ops, sched, audit, "")
	p.now = func() time.Time { return base }
	if err := p.syncSubmissions(context.Background(), c); err != nil {
		t.Fatalf("sync: %v", err)
	}

	subs, err := store.Submissions(context.Background(), "c1")
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if len(subs) != maxEntries {
		t.Fatalf("entries = %d, want the cap of %d", len(subs), maxEntries)
	}
	// The cap keeps the first to post, not an arbitrary slice of the list.
	if subs[0].ThreadID != "100000" {
		t.Errorf("earliest kept entry is %q, want 100000: the cap dropped the wrong end",
			subs[0].ThreadID)
	}
}

// TestANewPostIsPickedUpWithoutWaitingForATick covers the gateway handler
// that exists purely for latency. It shares the one-entry-per-member rule
// with the sync, so the two are checked against the same fake index.
func TestANewPostIsPickedUpWithoutWaitingForATick(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	store, ops, sched, audit := newFakeStore(), newFakeOps(), newFakeSched(), &fakeAudit{}
	seedLive(t, store, PhaseSubmit, base)

	p := newTestPlugin(t, store, ops, sched, audit, "")
	p.now = func() time.Time { return base }

	post := func(id, owner string) {
		th := entryThread(ops, id, owner)
		p.HandleThreadCreate(nil, &discordgo.ThreadCreate{Channel: th})
	}

	post("100", "u1")
	subs, err := store.Submissions(context.Background(), "c1")
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if len(subs) != 1 || subs[0].ThreadID != "100" {
		t.Fatalf("entries = %+v, want the new post recorded straight away", subs)
	}

	// A second post by the same member is refused and told why, rather than
	// replacing the first or colliding with the one-live-entry index.
	post("200", "u1")
	subs, _ = store.Submissions(context.Background(), "c1")
	if len(subs) != 1 || subs[0].ThreadID != "100" {
		t.Fatalf("entries = %+v, want the first post to still be the entry", subs)
	}
	if len(ops.sent["200"]) == 0 {
		t.Error("nothing said in the second post, so the member has no idea it did not count")
	}

	// A post merlin cannot read is called out in the thread rather than
	// dropped: an empty post and an unreadable one look identical here.
	ops.messages["300"] = []*discordgo.Message{{
		ID: "300", Author: &discordgo.User{ID: "u3", Username: "u3"},
	}}
	p.HandleThreadCreate(nil, &discordgo.ThreadCreate{
		Channel: &discordgo.Channel{ID: "300", GuildID: "g1", ParentID: "forum-1", OwnerID: "u3"},
	})
	if len(ops.sent["300"]) == 0 {
		t.Error("an unreadable post was dropped silently, which is the one thing it must not be")
	}
}

// --- the expensive half of a sync is rate limited -------------------------

func TestVotingDoesNotReReadEveryEntryEveryTick(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	store, ops, sched, audit := newFakeStore(), newFakeOps(), newFakeSched(), &fakeAudit{}
	seedLive(t, store, PhaseVote, base)
	ops.threads = []*discordgo.Channel{
		entryThread(ops, "100", "u1"),
		entryThread(ops, "200", "u2"),
	}

	p := newTestPlugin(t, store, ops, sched, audit, "")
	now := base.Add(2*time.Hour + time.Minute) // in vote, nowhere near results
	p.now = func() time.Time { return now }

	for range 5 {
		if err := p.tick(context.Background(), "g1"); err != nil {
			t.Fatalf("tick: %v", err)
		}
		now = now.Add(tickInterval)
	}
	if ops.messageReads != len(ops.threads) {
		t.Fatalf("starter-message reads = %d over five ticks, want %d (one sync): "+
			"the refresh rate limit is guarding the push instead of the reads behind it",
			ops.messageReads, len(ops.threads))
	}

	// Past refreshInterval it does run again, or the CDN links go stale.
	now = now.Add(refreshInterval)
	if err := p.tick(context.Background(), "g1"); err != nil {
		t.Fatalf("tick after the refresh window: %v", err)
	}
	if ops.messageReads != 2*len(ops.threads) {
		t.Fatalf("starter-message reads = %d, want %d: the links never get refreshed",
			ops.messageReads, 2*len(ops.threads))
	}
}

// --- nobody voted --------------------------------------------------------

func TestAContestNobodyVotedInCrownsNobodyAndAwardsNothing(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	store, ops, sched, audit := newFakeStore(), newFakeOps(), newFakeSched(), &fakeAudit{}
	seedLive(t, store, PhaseVote, base)
	for _, s := range []Submission{
		{ID: "s1", ContestID: "c1", UserID: "u1", ThreadID: "t1", Title: "one", Author: "ana"},
		{ID: "s2", ContestID: "c1", UserID: "u2", ThreadID: "t2", Title: "two", Author: "bo"},
	} {
		if err := store.UpsertSubmission(context.Background(), s); err != nil {
			t.Fatalf("seed submission: %v", err)
		}
	}
	if err := store.AddPrize(context.Background(), Prize{
		ID: "p1", ContestID: "c1", DonorID: "d1", Title: "a key",
		SecretSealed: []byte("sealed"),
	}); err != nil {
		t.Fatalf("seed prize: %v", err)
	}

	// Every entry tied at zero: the Worker returns an empty tally.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/close") {
			_ = json.NewEncoder(w).Encode(Tally{})
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	p := newTestPlugin(t, store, ops, sched, audit, srv.URL)
	p.now = func() time.Time { return base.Add(4 * time.Hour) }
	if err := p.tick(context.Background(), "g1"); err != nil {
		t.Fatalf("finish: %v", err)
	}

	got, _ := store.LatestContest(context.Background(), "g1")
	if got.Phase != PhaseResults {
		t.Fatalf("phase = %s, want results", got.Phase)
	}
	prizes, err := store.Prizes(context.Background(), "c1")
	if err != nil {
		t.Fatalf("read prizes: %v", err)
	}
	if prizes[0].AwardedAt != nil {
		t.Error("a prize went to an entry with no votes, picked by entry-ID sort order")
	}
	if prizes[0].SecretSealed == nil {
		t.Error("the sealed code was wiped, and nothing brings it back")
	}
	for _, sends := range ops.sent {
		for _, m := range sends {
			for _, e := range m.Embeds {
				if strings.Contains(e.Title, "winners") {
					t.Errorf("announced winners for a contest nobody voted in: %q", e.Title)
				}
			}
		}
	}
}

// --- the tail of a contest -----------------------------------------------

func TestAContestThatGotNoEntriesStillTellsTheWorkerItIsOver(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	store, ops, sched, audit := newFakeStore(), newFakeOps(), newFakeSched(), &fakeAudit{}
	seedLive(t, store, PhaseVote, base)

	var pushedPhases []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			var snap struct {
				Phase string `json:"phase"`
			}
			_ = json.NewDecoder(r.Body).Decode(&snap)
			pushedPhases = append(pushedPhases, snap.Phase)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	p := newTestPlugin(t, store, ops, sched, audit, srv.URL)
	p.now = func() time.Time { return base.Add(4 * time.Hour) }
	if err := p.tick(context.Background(), "g1"); err != nil {
		t.Fatalf("finish: %v", err)
	}

	if len(pushedPhases) == 0 {
		t.Fatal("nothing was pushed, so the gallery goes on inviting votes on a finished contest")
	}
	if last := pushedPhases[len(pushedPhases)-1]; last != string(PhaseResults) {
		t.Errorf("last pushed phase = %q, want %q", last, PhaseResults)
	}
}

func TestAFailedForumCreateLeavesTheGuildAbleToTryAgain(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	store, ops, sched, audit := newFakeStore(), newFakeOps(), newFakeSched(), &fakeAudit{}
	c := seedLive(t, store, PhaseAnnounce, base)

	p := newTestPlugin(t, store, ops, sched, audit, "")
	p.now = func() time.Time { return base }

	// Stand in for the handler's own failure path: the row is committed and
	// the forum create then fails.
	ops.createFail = ErrForumFull
	if _, err := p.createForum(context.Background(), c, ""); err == nil {
		t.Fatal("forum create was supposed to fail")
	}
	if _, err := store.AdvancePhase(context.Background(), c.ID, c.Phase, PhaseCancelled); err != nil {
		t.Fatalf("retire the stub contest: %v", err)
	}

	if _, err := store.LiveContest(context.Background(), "g1"); err != ErrNoLiveContest {
		t.Fatal("a contest with no forum is still live, so /contest new refuses forever " +
			"and it ticks through every phase collecting nothing")
	}
}

func TestAPrizeStaysClaimableAfterTheNextContestStarts(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	store := newFakeStore()
	old := liveContest(PhaseResults, base)
	if err := store.CreateContest(context.Background(), old); err != nil {
		t.Fatalf("seed old contest: %v", err)
	}
	next := liveContest(PhaseSubmit, base.Add(time.Hour))
	next.ID, next.Slug = "c2", "qrstuvwxyz012345"
	if err := store.CreateContest(context.Background(), next); err != nil {
		t.Fatalf("seed next contest: %v", err)
	}

	won := "u1"
	at := base
	if err := store.AddPrize(context.Background(), Prize{
		ID: "p1", ContestID: old.ID, DonorID: "d1", Title: "a key",
		AwardedTo: &won, AwardedAt: &at, SecretSealed: []byte("sealed"),
	}); err != nil {
		t.Fatalf("seed prize: %v", err)
	}

	// The DM bounced, which is the ordinary case, and a mod has since
	// started the next contest.
	prizes, err := store.PrizesAwardedTo(context.Background(), "g1", won)
	if err != nil {
		t.Fatalf("claim lookup: %v", err)
	}
	if len(prizes) != 1 || prizes[0].ID != "p1" {
		t.Fatalf("claimable prizes = %+v, want the one from the finished contest", prizes)
	}

	// Somebody else's guild never sees it.
	other, err := store.PrizesAwardedTo(context.Background(), "g2", won)
	if err != nil {
		t.Fatalf("cross-guild lookup: %v", err)
	}
	if len(other) != 0 {
		t.Errorf("prizes leaked across guilds: %+v", other)
	}
}
