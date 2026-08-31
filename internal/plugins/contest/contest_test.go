package contest

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/6586x57890143/merlin/internal/scheduler"
	"github.com/6586x57890143/merlin/internal/secret"
)

func liveContest(phase Phase, base time.Time) Contest {
	return Contest{
		ID: "c1", GuildID: "g1", Slug: "abcdefghijklmnop",
		Title: "neon cats", Phase: phase, MaxVotes: 2,
		SubmitAt:          base.Add(time.Hour),
		VoteAt:            base.Add(2 * time.Hour),
		ResultsAt:         base.Add(3 * time.Hour),
		ForumChannelID:    "forum-1",
		AnnounceChannelID: "announce-1",
		CreatedBy:         "mod-1",
	}
}

// --- the phase machine ----------------------------------------------------

func TestPhasesAdvanceInOrderAndAnnounceOnce(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	store, ops, sched, audit := newFakeStore(), newFakeOps(), newFakeSched(), &fakeAudit{}
	c := liveContest(PhaseAnnounce, base)
	if err := store.CreateContest(context.Background(), c); err != nil {
		t.Fatalf("seed contest: %v", err)
	}

	p := newTestPlugin(t, store, ops, sched, audit, "")
	now := base
	p.now = func() time.Time { return now }

	// Nothing is due yet, so the tick must change nothing at all.
	if err := p.tick(context.Background(), "g1"); err != nil {
		t.Fatalf("early tick: %v", err)
	}
	if got, _ := store.LiveContest(context.Background(), "g1"); got.Phase != PhaseAnnounce {
		t.Fatalf("phase moved early: %s", got.Phase)
	}

	now = base.Add(time.Hour + time.Second)
	if err := p.tick(context.Background(), "g1"); err != nil {
		t.Fatalf("tick to submit: %v", err)
	}
	got, _ := store.LiveContest(context.Background(), "g1")
	if got.Phase != PhaseSubmit {
		t.Fatalf("phase = %s, want submit", got.Phase)
	}
	if n := len(ops.sentTo("announce-1")); n != 1 {
		t.Fatalf("announcements after one transition = %d, want 1", n)
	}

	// A second tick inside the same window must not announce again: the
	// conditional phase claim is the whole reason two overlapping ticks
	// cannot double-post.
	if err := p.tick(context.Background(), "g1"); err != nil {
		t.Fatalf("repeat tick: %v", err)
	}
	if n := len(ops.sentTo("announce-1")); n != 1 {
		t.Fatalf("announcements after a repeat tick = %d, want 1", n)
	}
}

func TestSubmissionsOpenAndCloseFlipsThePostingOverwrite(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	store, ops, sched, audit := newFakeStore(), newFakeOps(), newFakeSched(), &fakeAudit{}
	if err := store.CreateContest(context.Background(), liveContest(PhaseAnnounce, base)); err != nil {
		t.Fatalf("seed: %v", err)
	}
	p := newTestPlugin(t, store, ops, sched, audit, "")
	now := base.Add(time.Hour + time.Second)
	p.now = func() time.Time { return now }

	if err := p.tick(context.Background(), "g1"); err != nil {
		t.Fatalf("open: %v", err)
	}
	now = base.Add(2*time.Hour + time.Second)
	if err := p.tick(context.Background(), "g1"); err != nil {
		t.Fatalf("close: %v", err)
	}

	if len(ops.overwrit) != 2 {
		t.Fatalf("permission writes = %d, want 2", len(ops.overwrit))
	}
	if ops.overwrit[0] != 0 {
		t.Errorf("opening submissions denied %d, want 0", ops.overwrit[0])
	}
	if ops.overwrit[1] != postingPerms {
		t.Errorf("closing submissions denied %d, want %d", ops.overwrit[1], postingPerms)
	}
}

func TestNoEntriesEndsCleanlyRatherThanCrowningNobody(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	store, ops, sched, audit := newFakeStore(), newFakeOps(), newFakeSched(), &fakeAudit{}
	if err := store.CreateContest(context.Background(), liveContest(PhaseVote, base)); err != nil {
		t.Fatalf("seed: %v", err)
	}
	p := newTestPlugin(t, store, ops, sched, audit, "")
	p.now = func() time.Time { return base.Add(4 * time.Hour) }

	if err := p.tick(context.Background(), "g1"); err != nil {
		t.Fatalf("finish: %v", err)
	}
	got, _ := store.LatestContest(context.Background(), "g1")
	if got.Phase != PhaseResults {
		t.Fatalf("phase = %s, want results", got.Phase)
	}
	if len(ops.pinned) != 0 {
		t.Errorf("pinned something with no entries: %v", ops.pinned)
	}
}

// A Worker that cannot be reached at close is the one failure that must not
// advance the contest: claiming results with no tally would leave it finished
// with no winner and nothing scheduled to try again.
func TestAFailedTallyLeavesTheContestInVoteAndSaysWhy(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	store, ops, sched, audit := newFakeStore(), newFakeOps(), newFakeSched(), &fakeAudit{}
	c := liveContest(PhaseVote, base)
	if err := store.CreateContest(context.Background(), c); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := store.UpsertSubmission(context.Background(), Submission{
		ID: "s1", ContestID: "c1", UserID: "u1", ThreadID: "t1", Title: "one", Author: "ana",
	}); err != nil {
		t.Fatalf("seed submission: %v", err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "d1 is having a moment", http.StatusInternalServerError)
	}))
	defer srv.Close()

	p := newTestPlugin(t, store, ops, sched, audit, srv.URL)
	p.now = func() time.Time { return base.Add(4 * time.Hour) }

	if err := p.tick(context.Background(), "g1"); err == nil {
		t.Fatal("a failed close returned nil, so the scheduler would never retry")
	}
	got, _ := store.LiveContest(context.Background(), "g1")
	if got.Phase != PhaseVote {
		t.Fatalf("phase = %s, want vote: a contest must not finish without a tally", got.Phase)
	}
	if got.TallyError == "" {
		t.Error("nothing recorded on the contest, so /contest status could not report it")
	}
}

func TestAWorkingTallyRanksAndAnnounces(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	store, ops, sched, audit := newFakeStore(), newFakeOps(), newFakeSched(), &fakeAudit{}
	if err := store.CreateContest(context.Background(), liveContest(PhaseVote, base)); err != nil {
		t.Fatalf("seed: %v", err)
	}
	for _, s := range []Submission{
		{ID: "s1", ContestID: "c1", UserID: "u1", ThreadID: "t1", Title: "one", Author: "ana"},
		{ID: "s2", ContestID: "c1", UserID: "u2", ThreadID: "t2", Title: "two", Author: "bo"},
	} {
		if err := store.UpsertSubmission(context.Background(), s); err != nil {
			t.Fatalf("seed submission: %v", err)
		}
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/close") {
			_ = json.NewEncoder(w).Encode(Tally{"s1": 3, "s2": 9})
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
	rs, err := unmarshalResults(got.Results)
	if err != nil {
		t.Fatalf("results: %v", err)
	}
	if rs[0].ID != "s2" || rs[0].Rank != 1 || rs[0].Votes != 9 {
		t.Fatalf("top result = %+v, want s2 rank 1 on 9", rs[0])
	}
	// The winning thread is pinned so the forum carries the outcome too.
	if len(ops.pinned) != 1 || !strings.Contains(ops.pinned[0], "t2") {
		t.Errorf("pinned = %v, want the winning thread", ops.pinned)
	}
}

// --- the tick job registration rule ---------------------------------------

func TestTheTickJobExistsOnlyWhileAContestIsLive(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	store, ops, sched, audit := newFakeStore(), newFakeOps(), newFakeSched(), &fakeAudit{}
	p := newTestPlugin(t, store, ops, sched, audit, "")
	p.now = func() time.Time { return base }
	key := scheduler.JobKey("g1", "contest-tick")

	p.SyncGuild(context.Background(), "g1")
	if sched.has(key) {
		t.Fatal("registered a tick for a guild that has never run a contest")
	}

	if err := store.CreateContest(context.Background(), liveContest(PhaseVote, base)); err != nil {
		t.Fatalf("seed: %v", err)
	}
	p.SyncGuild(context.Background(), "g1")
	if !sched.has(key) {
		t.Fatal("no tick registered for a live contest")
	}

	// Finishing it must take the job away again.
	p.now = func() time.Time { return base.Add(4 * time.Hour) }
	if err := p.tick(context.Background(), "g1"); err != nil {
		t.Fatalf("finish: %v", err)
	}
	if sched.has(key) {
		t.Error("tick job survived the contest it existed for")
	}
}

// A database blip must not unregister a running contest's job, which is the
// one direction of this decision that silently stops real work.
func TestAFailedLookupLeavesTheTickRegistrationAlone(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	store, ops, sched, audit := newFakeStore(), newFakeOps(), newFakeSched(), &fakeAudit{}
	if err := store.CreateContest(context.Background(), liveContest(PhaseVote, base)); err != nil {
		t.Fatalf("seed: %v", err)
	}
	p := newTestPlugin(t, store, ops, sched, audit, "")
	p.now = func() time.Time { return base }
	p.SyncGuild(context.Background(), "g1")

	store.liveErr = errors.New("connection refused")
	p.SyncGuild(context.Background(), "g1")

	if !sched.has(scheduler.JobKey("g1", "contest-tick")) {
		t.Error("a transient database error unregistered a live contest's tick")
	}
}

// --- forum reading --------------------------------------------------------

func TestOneEntryPerMemberKeepsTheOldestPost(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	store, ops, sched, audit := newFakeStore(), newFakeOps(), newFakeSched(), &fakeAudit{}
	c := liveContest(PhaseSubmit, base)
	if err := store.CreateContest(context.Background(), c); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Discord returns threads newest first, and snowflake ids sort
	// chronologically, so "200" is the later post by the same member.
	ops.threads = []*discordgo.Channel{
		{ID: "200", ParentID: "forum-1", OwnerID: "u1", Name: "second go"},
		{ID: "100", ParentID: "forum-1", OwnerID: "u1", Name: "first go"},
		{ID: "300", ParentID: "forum-1", OwnerID: "u2", Name: "somebody else"},
	}
	for _, id := range []string{"100", "200", "300"} {
		ops.messages[id] = []*discordgo.Message{{
			ID: id, Content: "here it is",
			Author: &discordgo.User{ID: "u" + id[:1], Username: "someone"},
		}}
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
	if len(subs) != 2 {
		t.Fatalf("entries = %d, want 2", len(subs))
	}
	for _, s := range subs {
		if s.ThreadID == "200" {
			t.Error("kept the second post by a member instead of their first")
		}
	}
}

// Deleting your forum post is how you withdraw, so an entry whose thread has
// gone must stop counting on the next sync.
func TestADeletedThreadWithdrawsTheEntry(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	store, ops, sched, audit := newFakeStore(), newFakeOps(), newFakeSched(), &fakeAudit{}
	c := liveContest(PhaseSubmit, base)
	if err := store.CreateContest(context.Background(), c); err != nil {
		t.Fatalf("seed: %v", err)
	}
	ops.threads = []*discordgo.Channel{{ID: "100", ParentID: "forum-1", OwnerID: "u1", Name: "mine"}}
	ops.messages["100"] = []*discordgo.Message{{
		ID: "100", Content: "art", Author: &discordgo.User{ID: "u1", Username: "ana"},
	}}

	p := newTestPlugin(t, store, ops, sched, audit, "")
	p.now = func() time.Time { return base }
	if err := p.syncSubmissions(context.Background(), c); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	if subs, _ := store.Submissions(context.Background(), "c1"); len(subs) != 1 {
		t.Fatalf("entries after first sync = %d, want 1", len(subs))
	}

	ops.threads = nil
	if err := p.syncSubmissions(context.Background(), c); err != nil {
		t.Fatalf("second sync: %v", err)
	}
	if subs, _ := store.Submissions(context.Background(), "c1"); len(subs) != 0 {
		t.Fatalf("entries after the post was deleted = %d, want 0", len(subs))
	}
}

// An unreadable post and an empty one look identical over the wire, and the
// unreadable case is the likely one. Guessing "empty" would silently drop
// somebody's entry, which is the failure this whole path exists to avoid.
func TestAnUnreadablePostIsReportedRatherThanCountedEmpty(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	store, ops := newFakeStore(), newFakeOps()
	c := liveContest(PhaseSubmit, base)
	ops.messages["100"] = []*discordgo.Message{{
		ID: "100", Author: &discordgo.User{ID: "u1", Username: "ana"},
	}}

	p := newTestPlugin(t, store, ops, newFakeSched(), &fakeAudit{}, "")
	p.now = func() time.Time { return base }
	_, err := p.readThread(context.Background(), c,
		&discordgo.Channel{ID: "100", ParentID: "forum-1", OwnerID: "u1", Name: "mine"})
	if !errors.Is(err, ErrNoMessageContent) {
		t.Fatalf("err = %v, want ErrNoMessageContent", err)
	}
}

func TestAttachmentsAndLinksAreClassified(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	store, ops := newFakeStore(), newFakeOps()
	c := liveContest(PhaseSubmit, base)
	p := newTestPlugin(t, store, ops, newFakeSched(), &fakeAudit{}, "")

	ops.messages["1"] = []*discordgo.Message{{
		ID: "1", Author: &discordgo.User{ID: "u1", Username: "ana"},
		Attachments: []*discordgo.MessageAttachment{
			{URL: "https://cdn.discordapp.com/a.png", ContentType: "image/png", Filename: "a.png"},
		},
	}}
	sub, err := p.readThread(context.Background(), c, &discordgo.Channel{ID: "1", OwnerID: "u1", Name: "art"})
	if err != nil {
		t.Fatalf("image: %v", err)
	}
	if sub.Kind != "image" || sub.MediaURL == "" {
		t.Errorf("image entry = %+v", sub)
	}

	ops.messages["2"] = []*discordgo.Message{{
		ID: "2", Author: &discordgo.User{ID: "u2", Username: "bo"},
		Content: "listen to this https://soundcloud.com/x/y please",
	}}
	sub, err = p.readThread(context.Background(), c, &discordgo.Channel{ID: "2", OwnerID: "u2", Name: "track"})
	if err != nil {
		t.Fatalf("link: %v", err)
	}
	if sub.Kind != "link" || sub.Link != "https://soundcloud.com/x/y" {
		t.Errorf("link entry = %+v", sub)
	}
}

func TestKindOfFallsBackToTheExtension(t *testing.T) {
	for _, tc := range []struct{ ct, name, want string }{
		{"image/png", "a.png", "image"},
		{"audio/mpeg", "a.mp3", "audio"},
		{"video/mp4", "a.mp4", "video"},
		{"", "track.flac", "audio"},
		{"application/octet-stream", "art.webp", "image"},
		{"application/octet-stream", "thing.zip", "other"},
		{"", "notes.txt", "text"},
	} {
		if got := kindOf(tc.ct, tc.name); got != tc.want {
			t.Errorf("kindOf(%q, %q) = %q, want %q", tc.ct, tc.name, got, tc.want)
		}
	}
}

func TestForumNameIsAValidChannelName(t *testing.T) {
	for in, want := range map[string]string{
		"Neon Cats":          "contest-neon-cats",
		"  weird   !!! name": "contest-weird-name",
		"2026 art jam":       "contest-2026-art-jam",
		"!!!":                "contest",
	} {
		if got := forumName(in); got != want {
			t.Errorf("forumName(%q) = %q, want %q", in, got, want)
		}
	}
	long := forumName(strings.Repeat("a", 300))
	if len(long) > 100 {
		t.Errorf("forumName produced %d characters, Discord's cap is 100", len(long))
	}
}

// --- prizes ---------------------------------------------------------------

func TestAPrizeCodeIsWipedOnlyAfterItIsDelivered(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	sealer, err := secret.New(testKey())
	if err != nil {
		t.Fatalf("sealer: %v", err)
	}
	sealed, err := sealer.Seal("STEAM-KEY-1234")
	if err != nil {
		t.Fatalf("seal: %v", err)
	}

	run := func(dmFails bool) []Prize {
		t.Helper()
		store, ops, sched, audit := newFakeStore(), newFakeOps(), newFakeSched(), &fakeAudit{}
		ops.dmFails = dmFails
		c := liveContest(PhaseResults, base)
		if err := store.CreateContest(context.Background(), c); err != nil {
			t.Fatalf("seed: %v", err)
		}
		if err := store.AddPrize(context.Background(), Prize{
			ID: "p1", ContestID: "c1", DonorID: "u9", DonorName: "dana",
			Title: "a steam key", SecretSealed: sealed,
		}); err != nil {
			t.Fatalf("seed prize: %v", err)
		}

		p := newTestPlugin(t, store, ops, sched, audit, "")
		p.sealer = sealer
		p.now = func() time.Time { return base }

		subs := []Submission{{ID: "s1", ContestID: "c1", UserID: "u1", ThreadID: "t1", Author: "ana"}}
		p.awardPrizes(context.Background(), c, subs, []resultView{{ID: "s1", Votes: 4, Rank: 1}})

		// The code must never reach the durable audit row, whichever way
		// delivery went.
		for _, row := range audit.all() {
			if strings.Contains(row, "STEAM-KEY-1234") {
				t.Fatalf("a prize code reached the audit log: %s", row)
			}
		}
		got, err := store.Prizes(context.Background(), "c1")
		if err != nil {
			t.Fatalf("read prizes: %v", err)
		}
		return got
	}

	delivered := run(false)
	if delivered[0].AwardedTo == nil || *delivered[0].AwardedTo != "u1" {
		t.Fatalf("prize not awarded: %+v", delivered[0])
	}
	if delivered[0].HasSecret() {
		t.Error("the code survived a successful delivery")
	}

	kept := run(true)
	if !kept[0].HasSecret() {
		t.Error("a failed DM wiped the code, so /contest claim has nothing to hand over")
	}
	if kept[0].AwardedTo == nil {
		t.Error("a failed DM lost the pairing, so nothing records who it belongs to")
	}
}

func TestSnapshotNeverCarriesAPrizeCodeOrARawDiscordID(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	p := newTestPlugin(t, newFakeStore(), newFakeOps(), newFakeSched(), &fakeAudit{}, "https://w.test")
	c := liveContest(PhaseVote, base)

	snap := p.snapshotOf(c,
		[]Submission{{ID: "s1", UserID: "111222333444555666", ThreadID: "t1", Author: "ana", Title: "one"}},
		[]Prize{{ID: "p1", DonorID: "u9", DonorName: "dana", Title: "a key",
			SecretSealed: []byte("ciphertext-here")}})

	blob, err := json.Marshal(snap)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	body := string(blob)
	for _, forbidden := range []string{"ciphertext-here", "111222333444555666", "secret", "sealed"} {
		if strings.Contains(body, forbidden) {
			t.Errorf("snapshot contains %q:\n%s", forbidden, body)
		}
	}
	if snap.Entries[0].ByHash == "" {
		t.Error("the entry carries no hash, so the Worker cannot refuse a self-vote")
	}
}

// --- identity ---------------------------------------------------------------

// The Worker computes this same hash in JavaScript, from the Discord ID that
// OAuth just proved, and refuses a self-vote by comparing it against the
// by_hash merlin put on each entry. So the two implementations have to agree
// byte for byte, and this pins the exact bytes: a change on either side fails
// here rather than on a live contest that quietly lets somebody vote for
// their own work. scripts/check-contest.mjs asserts the same vector from the
// other end, under the same key.
func TestTokenGoldenVector(t *testing.T) {
	w := newWorkerClient("https://w.test", "bot", "link-key")
	if got := w.Hash("111222333444555666"); got != goldenHash {
		t.Errorf("Hash = %q, want %q. If this changed deliberately, update the same vector in scripts/check-contest.mjs", got, goldenHash)
	}
}

// The gallery link carries no credential at all: who you are is settled by
// Discord OAuth on the page. So one link works for everybody and is safe to
// paste in the announcement, and a forwarded link gets the recipient their
// own vote rather than somebody else's.
func TestThePageLinkCarriesNoCredential(t *testing.T) {
	w := newWorkerClient("https://w.test/", "bot", "link-key")
	got := w.PageURL("abcdefghijklmnop")
	if got != "https://w.test/c/abcdefghijklmnop" {
		t.Errorf("PageURL = %q", got)
	}
	for _, bad := range []string{"#", "?", "t="} {
		if strings.Contains(got, bad) {
			t.Errorf("PageURL = %q, which carries something that looks like a credential", got)
		}
	}
	if nilW := (*workerClient)(nil); nilW.PageURL("x") != "" {
		t.Error("an unconfigured worker produced a link")
	}
}

// --- ranking --------------------------------------------------------------

func TestRankSharesARankOnATieAndIsStable(t *testing.T) {
	subs := []Submission{{ID: "a"}, {ID: "b"}, {ID: "c"}, {ID: "d"}}
	got := rank(subs, Tally{"a": 5, "b": 5, "c": 9, "d": 0})

	want := []resultView{
		{ID: "c", Votes: 9, Rank: 1},
		{ID: "a", Votes: 5, Rank: 2},
		{ID: "b", Votes: 5, Rank: 2},
		{ID: "d", Votes: 0, Rank: 4},
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("rank[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}

	// Same input, same output: nothing about the order may depend on map
	// iteration or on when this ran.
	for i := 0; i < 20; i++ {
		if again := rank(subs, Tally{"a": 5, "b": 5, "c": 9, "d": 0}); again[1].ID != got[1].ID {
			t.Fatal("rank is not stable across runs")
		}
	}
}

// --- durations ------------------------------------------------------------

func TestPhaseDurationsAreBounded(t *testing.T) {
	arg := func(v string) map[string]*discordgo.ApplicationCommandInteractionDataOption {
		return map[string]*discordgo.ApplicationCommandInteractionDataOption{
			"submit-for": {Name: "submit-for", Type: discordgo.ApplicationCommandOptionString, Value: v},
		}
	}

	if got, err := optDuration(nil, "submit-for", defaultSubmitFor, false); err != nil || got != defaultSubmitFor {
		t.Errorf("missing option = %v, %v, want the default", got, err)
	}
	if got, err := optDuration(arg("90m"), "submit-for", defaultSubmitFor, false); err != nil || got != 90*time.Minute {
		t.Errorf("90m = %v, %v", got, err)
	}
	if _, err := optDuration(arg("30s"), "submit-for", defaultSubmitFor, false); err == nil {
		t.Error("accepted a phase shorter than the floor")
	}
	if _, err := optDuration(arg("400d"), "submit-for", defaultSubmitFor, false); err == nil {
		t.Error("accepted a phase longer than the cap")
	}
	if _, err := optDuration(arg("banana"), "submit-for", defaultSubmitFor, false); err == nil {
		t.Error("accepted a duration that does not parse")
	}

	zero := map[string]*discordgo.ApplicationCommandInteractionDataOption{
		"announce-for": {Name: "announce-for", Type: discordgo.ApplicationCommandOptionString, Value: "0"},
	}
	if got, err := optDuration(zero, "announce-for", defaultAnnounceFor, true); err != nil || got != 0 {
		t.Errorf("announce-for 0 = %v, %v, want zero and no error", got, err)
	}
}

func TestDeadlineAndLiveTrackThePhase(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	c := liveContest(PhaseAnnounce, base)
	for _, tc := range []struct {
		phase Phase
		want  time.Time
		has   bool
		live  bool
	}{
		{PhaseAnnounce, c.SubmitAt, true, true},
		{PhaseSubmit, c.VoteAt, true, true},
		{PhaseVote, c.ResultsAt, true, true},
		{PhaseResults, time.Time{}, false, false},
		{PhaseCancelled, time.Time{}, false, false},
	} {
		c.Phase = tc.phase
		got, has := c.Deadline()
		if has != tc.has || (has && !got.Equal(tc.want)) {
			t.Errorf("%s: Deadline = %v, %v", tc.phase, got, has)
		}
		if c.Live() != tc.live {
			t.Errorf("%s: Live = %v, want %v", tc.phase, c.Live(), tc.live)
		}
	}
}

func TestSlugsAreUnguessableAndDistinct(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		s, err := newSlug()
		if err != nil {
			t.Fatalf("newSlug: %v", err)
		}
		if len(s) != 26 {
			t.Fatalf("slug %q is %d characters, want 26 (128 bits of base32)", s, len(s))
		}
		if strings.ToLower(s) != s {
			t.Fatalf("slug %q is not lowercase, so the Worker's route would not match it", s)
		}
		if seen[s] {
			t.Fatal("newSlug repeated itself")
		}
		seen[s] = true
	}
}

// Golden vector, shared with scripts/check-contest.mjs under the same key.
// See TestTokenGoldenVector for why it is pinned rather than round-tripped.
const goldenHash = "TO2O8a5j_WwoeAVZamINPQ"

// Once voting opens the entry list is frozen. Letting a deleted post
// retroactively withdraw an entry would silently discard every vote already
// cast for it, so somebody losing could delete their post and take their
// voters' ballots with them.
func TestDeletingAPostDuringVotingDoesNotRemoveTheEntry(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	store, ops := newFakeStore(), newFakeOps()
	c := liveContest(PhaseSubmit, base)
	if err := store.CreateContest(context.Background(), c); err != nil {
		t.Fatalf("seed: %v", err)
	}
	ops.threads = []*discordgo.Channel{{ID: "100", ParentID: "forum-1", OwnerID: "u1", Name: "mine"}}
	ops.messages["100"] = []*discordgo.Message{{
		ID: "100", Content: "art", Author: &discordgo.User{ID: "u1", Username: "ana"},
	}}

	p := newTestPlugin(t, store, ops, newFakeSched(), &fakeAudit{}, "")
	p.now = func() time.Time { return base }
	if err := p.syncSubmissions(context.Background(), c); err != nil {
		t.Fatalf("sync while open: %v", err)
	}

	c.Phase = PhaseVote
	ops.threads = nil
	if err := p.syncSubmissions(context.Background(), c); err != nil {
		t.Fatalf("sync while voting: %v", err)
	}
	subs, _ := store.Submissions(context.Background(), "c1")
	if len(subs) != 1 {
		t.Fatalf("entries after a mid-vote delete = %d, want the entry kept", len(subs))
	}
}
