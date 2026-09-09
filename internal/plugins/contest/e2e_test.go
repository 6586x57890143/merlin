package contest

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/6586x57890143/merlin/internal/secret"
)

// One contest, start to finish, with a group of members rather than a fixture.
//
// Every other test in this package holds the contest still and pokes one part
// of it: a phase transition, a permission write, a withdrawal. That is the
// right shape for a regression, and it is why the bugs that survived to
// production were all in the seams between those parts -- a link that was
// alive at every step and dead by the time anybody looked, a permission write
// that was correct in isolation and erased the one beside it.
//
// So this drives the real Plugin, with the real store interface, the real
// phase machine and a real HTTP server standing in for the Worker, and only
// the clock is fake. It is the closest thing to running a contest that does
// not need a Discord token.

// member is one of the virtual test group.
type member struct {
	id   string
	name string
}

var testGroup = []member{
	{"u-ana", "ana"}, {"u-bo", "bo"}, {"u-cal", "cal"}, {"u-dee", "dee"},
	{"u-eli", "eli"}, {"u-fay", "fay"}, {"u-gus", "gus"}, {"u-hal", "hal"},
	{"u-ivy", "ivy"}, {"u-jo", "jo"}, {"u-kit", "kit"}, {"u-lou", "lou"},
}

// post puts one member's entry in the forum: a thread they own, with a
// starter message carrying a drawing.
func post(ops *fakeOps, threadID string, m member, title string) *discordgo.Channel {
	ops.messages[threadID] = []*discordgo.Message{{
		ID: threadID, Content: title + ", by " + m.name,
		Author: &discordgo.User{ID: m.id, Username: m.name},
		Attachments: []*discordgo.MessageAttachment{{
			ID:  threadID + "-a",
			URL: "https://cdn.discordapp.com/attachments/1/" + threadID + "/art.png?ex=deadbeef",
		}},
	}}
	return &discordgo.Channel{
		ID: threadID, GuildID: "g1", ParentID: "forum-1", OwnerID: m.id, Name: title,
	}
}

// fakeWorker is the gallery: it remembers the last snapshot merlin pushed and
// hands back a tally on close, which is what the real Worker does over the
// same three endpoints.
type fakeWorker struct {
	*httptest.Server
	pushes []snapshot
	tally  map[string]int
	closed bool
}

func newFakeWorker(t *testing.T, tally map[string]int) *fakeWorker {
	t.Helper()
	w := &fakeWorker{tally: tally}
	w.Server = httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer bot-token" {
			http.Error(rw, "who are you", http.StatusUnauthorized)
			return
		}
		switch {
		case r.Method == http.MethodPut:
			body, _ := io.ReadAll(r.Body)
			var snap snapshot
			if err := json.Unmarshal(body, &snap); err != nil {
				http.Error(rw, "bad snapshot", http.StatusBadRequest)
				return
			}
			w.pushes = append(w.pushes, snap)
			_, _ = rw.Write([]byte(`{"ok":true}`))
		case strings.HasSuffix(r.URL.Path, "/close"):
			w.closed = true
			_ = json.NewEncoder(rw).Encode(w.tally)
		case strings.HasSuffix(r.URL.Path, "/stats"):
			_, _ = rw.Write([]byte(`{"voters":9,"votes":21}`))
		default:
			http.Error(rw, "no", http.StatusNotFound)
		}
	}))
	t.Cleanup(w.Close)
	return w
}

func (w *fakeWorker) last() snapshot { return w.pushes[len(w.pushes)-1] }

func TestTwelveMembersRunAWholeContest(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	store, ops, sched, audit := newFakeStore(), newFakeOps(), newFakeSched(), &fakeAudit{}
	seedGate(store, ops)

	// ana wins on votes, bo is second, cal third. The rest drew nothing but
	// zeroes, which is the ordinary case and has to rank without crashing.
	worker := newFakeWorker(t, map[string]int{})

	sealer, err := secret.New(testKey())
	if err != nil {
		t.Fatalf("sealer: %v", err)
	}
	p := New(store, func(string) DiscordOps { return ops }, fixedVoice{"a line"}, sealer,
		worker.URL, "bot-token", "link-key")
	p.audit = audit
	p.log = quietLog()
	p.sched = sched

	now := base
	p.now = func() time.Time { return now }
	sess, _ := stubSession()

	// --- a mod starts it ---------------------------------------------------

	p.handleNew(context.Background(), sess, interaction("new",
		strOpt("title", "Neon Cats"), strOpt("theme", "cats, but neon"),
		strOpt("announce-for", "1h"), strOpt("submit-for", "48h"), strOpt("vote-for", "24h"),
		intOpt("picks", 3)))

	c, err := store.LiveContest(context.Background(), "g1")
	if err != nil {
		t.Fatalf("no contest was created: %v", err)
	}
	if c.Phase != PhaseAnnounce {
		t.Fatalf("phase = %s, want announce", c.Phase)
	}
	if !sched.has("g1:contest-tick") {
		t.Fatal("no tick job, so nothing would ever advance the contest")
	}

	// The forum is gated from the instant it exists, which is the property
	// the whole gate rests on.
	created := ops.created[0].PermissionOverwrites
	if denyOn(created, "g1", discordgo.PermissionOverwriteTypeRole)&discordgo.PermissionViewChannel == 0 {
		t.Error("the forum was created visible to accounts that have not passed the gate")
	}

	// --- two people pledge prizes while it is still being announced --------

	p.handlePrizeModal(context.Background(), sess, modalSubmit(prizeModalPrefix+c.ID, map[string]string{
		prizeFieldTitle: "a steam key", prizeFieldCode: "STEAM-AAAA-BBBB",
	}), prizeModalPrefix+c.ID)
	p.handlePrizeModal(context.Background(), sess, modalSubmit(prizeModalPrefix+c.ID, map[string]string{
		prizeFieldTitle: "a drawing of your cat",
	}), prizeModalPrefix+c.ID)
	prizes, err := store.Prizes(context.Background(), c.ID)
	if err != nil || len(prizes) != 2 {
		t.Fatalf("prizes = %d (%v), want 2", len(prizes), err)
	}

	// --- and a mod rules on both before either is public -------------------
	//
	// Driven through the real button, not the store, because the queue being
	// wired to the command router is half of what this test is for. Neither
	// pledge exists to anybody outside this queue until these two clicks.
	if len(ops.sentTo(c.AnnounceChannelID)) != 1 {
		t.Fatalf("an unreviewed pledge reached the announce channel: %d posts", len(ops.sentTo(c.AnnounceChannelID)))
	}
	for _, pr := range prizes {
		p.handleReviewButton(context.Background(), sess, componentClick(reviewPrefix+"approve:"+pr.ID),
			reviewPrefix+"approve:"+pr.ID)
	}
	prizes, _ = store.Prizes(context.Background(), c.ID)
	for _, pr := range prizes {
		if !pr.Approved {
			t.Fatalf("pledge %s did not survive the review queue", pr.Title)
		}
	}

	// --- submissions open --------------------------------------------------

	now = base.Add(time.Hour + time.Second)
	if err := p.tick(context.Background(), "g1"); err != nil {
		t.Fatalf("open submissions: %v", err)
	}
	c, _ = store.LiveContest(context.Background(), "g1")
	if c.Phase != PhaseSubmit {
		t.Fatalf("phase = %s, want submit", c.Phase)
	}

	// --- the group posts ---------------------------------------------------

	// Fourteen threads from twelve members: kit posts twice, and lou deletes
	// their post partway through the window.
	for n, m := range testGroup {
		ops.threads = append(ops.threads, post(ops, "t"+strconv.Itoa(100+n), m, m.name+"'s cat"))
	}
	ops.threads = append(ops.threads, post(ops, "t900", testGroup[10], "kit's second go"))

	now = base.Add(2 * time.Hour)
	if err := p.tick(context.Background(), "g1"); err != nil {
		t.Fatalf("sync entries: %v", err)
	}

	subs, err := store.Submissions(context.Background(), c.ID)
	if err != nil {
		t.Fatalf("submissions: %v", err)
	}
	if len(subs) != len(testGroup) {
		t.Fatalf("entries = %d, want %d: one live entry per member, and kit posted twice",
			len(subs), len(testGroup))
	}
	seen := map[string]bool{}
	for _, s := range subs {
		if seen[s.UserID] {
			t.Errorf("%s has two live entries", s.UserID)
		}
		seen[s.UserID] = true
	}

	// lou withdraws by deleting the post, which is the only way to withdraw
	// and the reason there is no /contest withdraw to keep in step.
	ops.threads = ops.threads[:len(ops.threads)-2] // lou's post and kit's spare
	now = base.Add(3 * time.Hour)
	if err := p.tick(context.Background(), "g1"); err != nil {
		t.Fatalf("sync after a withdrawal: %v", err)
	}
	subs, _ = store.Submissions(context.Background(), c.ID)
	if len(subs) != len(testGroup)-1 {
		t.Fatalf("entries after a withdrawal = %d, want %d", len(subs), len(testGroup)-1)
	}

	// --- voting opens ------------------------------------------------------

	now = base.Add(49*time.Hour + time.Second)
	if err := p.tick(context.Background(), "g1"); err != nil {
		t.Fatalf("open voting: %v", err)
	}
	c, _ = store.LiveContest(context.Background(), "g1")
	if c.Phase != PhaseVote {
		t.Fatalf("phase = %s, want vote", c.Phase)
	}
	if len(worker.pushes) == 0 {
		t.Fatal("voting opened without the gallery ever being told what to show")
	}

	// The entry list freezes when voting starts. Somebody losing must not be
	// able to delete their post and take their voters' ballots with them.
	frozen := len(worker.last().Entries)
	ops.threads = ops.threads[:1]
	now = base.Add(50 * time.Hour)
	if err := p.tick(context.Background(), "g1"); err != nil {
		t.Fatalf("tick during voting: %v", err)
	}
	subs, _ = store.Submissions(context.Background(), c.ID)
	if len(subs) != frozen {
		t.Errorf("entries = %d during voting, want %d frozen: a deleted post retroactively "+
			"discarded the votes cast for it", len(subs), frozen)
	}

	// Nothing merlin pushes may carry a raw Discord ID or a prize code. This
	// is the privacy promise the whole Worker boundary exists to make, and it
	// has to hold on a real snapshot rather than a hand-built one.
	body, err := json.Marshal(worker.last())
	if err != nil {
		t.Fatalf("marshal snapshot: %v", err)
	}
	for _, m := range testGroup {
		if strings.Contains(string(body), m.id) {
			t.Errorf("the snapshot carries %s's raw Discord ID", m.name)
		}
	}
	if strings.Contains(string(body), "STEAM-AAAA-BBBB") {
		t.Error("the snapshot carries a prize code")
	}

	// --- the votes come in, and the contest ends ---------------------------

	byUser := map[string]string{}
	for _, s := range subs {
		byUser[s.UserID] = s.ID
	}
	worker.tally = map[string]int{
		byUser["u-ana"]: 9,
		byUser["u-bo"]:  6,
		byUser["u-cal"]: 4,
	}

	now = base.Add(73*time.Hour + time.Second)
	if err := p.tick(context.Background(), "g1"); err != nil {
		t.Fatalf("finish: %v", err)
	}
	if !worker.closed {
		t.Error("the contest finished without the ballot ever being frozen")
	}

	done, err := store.LatestContest(context.Background(), "g1")
	if err != nil {
		t.Fatalf("latest: %v", err)
	}
	if done.Phase != PhaseResults {
		t.Fatalf("phase = %s, want results", done.Phase)
	}

	results, err := unmarshalResults(done.Results)
	if err != nil {
		t.Fatalf("results: %v", err)
	}
	if len(results) == 0 || results[0].ID != byUser["u-ana"] || results[0].Rank != 1 {
		t.Fatalf("ana did not win with the most votes: %+v", results)
	}
	if results[1].ID != byUser["u-bo"] || results[2].ID != byUser["u-cal"] {
		t.Errorf("the podium is not in vote order: %+v", results[:3])
	}

	// --- the prize reaches the winner, and only then is it wiped -----------

	if ops.dmCount() == 0 {
		t.Fatal("nobody was DMed a prize")
	}
	after, _ := store.Prizes(context.Background(), c.ID)
	var awarded, stillSealed int
	for _, pr := range after {
		if pr.AwardedTo != nil {
			awarded++
		}
		if pr.HasSecret() {
			stillSealed++
		}
	}
	if awarded != 2 {
		t.Errorf("prizes awarded = %d, want 2", awarded)
	}
	if stillSealed != 0 {
		t.Error("a delivered prize code was left sealed in the database")
	}
	for _, row := range audit.all() {
		if strings.Contains(row, "STEAM-AAAA-BBBB") {
			t.Fatal("a prize code reached the audit log")
		}
	}

	// The winner's thread is pinned, so the forum reads correctly to somebody
	// who arrives after the fact.
	if len(ops.pinned) == 0 {
		t.Error("the winning entry was not pinned")
	}

	// --- and the gallery does not rot --------------------------------------

	// A finished contest keeps its tick for the refresh window, because the
	// CDN links in that last push expire in about a day.
	if !sched.has("g1:contest-tick") {
		t.Error("the tick was dropped at close, so the results gallery would go dead in a day")
	}
	now = base.Add(73*time.Hour + artRefreshWindow + time.Hour)
	if err := p.tick(context.Background(), "g1"); err != nil {
		t.Fatalf("tick past the window: %v", err)
	}
	p.SyncGuild(context.Background(), "g1")
	if sched.has("g1:contest-tick") {
		t.Error("the tick outlived the refresh window, costing REST calls forever")
	}

	// The last thing the gallery was told is that the contest is over, or it
	// goes on inviting votes on a contest that has ended.
	if worker.last().Phase != string(PhaseResults) {
		t.Errorf("the gallery was last told phase %q", worker.last().Phase)
	}
}

// The same run with nobody voting. Every entry ties at zero, and sortResults
// tiebreaks on a random entry ID, so falling through to the winners path would
// DM a sealed prize code to an arbitrary entrant and then wipe the ciphertext
// -- the one irreversible thing this plugin does.
func TestAWholeContestWithNoVotesCrownsNobody(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	store, ops, sched, audit := newFakeStore(), newFakeOps(), newFakeSched(), &fakeAudit{}
	seedGate(store, ops)
	worker := newFakeWorker(t, map[string]int{})

	sealer, err := secret.New(testKey())
	if err != nil {
		t.Fatalf("sealer: %v", err)
	}
	p := New(store, func(string) DiscordOps { return ops }, fixedVoice{"a line"}, sealer,
		worker.URL, "bot-token", "link-key")
	p.audit, p.log, p.sched = audit, quietLog(), sched

	now := base
	p.now = func() time.Time { return now }
	sess, _ := stubSession()

	p.handleNew(context.Background(), sess, interaction("new",
		strOpt("title", "quiet one"), strOpt("announce-for", "0")))
	c, err := store.LiveContest(context.Background(), "g1")
	if err != nil {
		t.Fatalf("no contest: %v", err)
	}
	p.handlePrizeModal(context.Background(), sess, modalSubmit(prizeModalPrefix+c.ID, map[string]string{
		prizeFieldTitle: "a steam key", prizeFieldCode: "STEAM-CCCC-DDDD",
	}), prizeModalPrefix+c.ID)
	// Approved, so that "nothing was awarded" below is about nobody voting
	// rather than about the pledge never having been reviewed.
	pending, _ := store.Prizes(context.Background(), c.ID)
	p.handleReviewButton(context.Background(), sess, componentClick(reviewPrefix+"approve:"+pending[0].ID),
		reviewPrefix+"approve:"+pending[0].ID)

	for n, m := range testGroup[:3] {
		ops.threads = append(ops.threads, post(ops, "t"+strconv.Itoa(200+n), m, m.name+"'s go"))
	}
	now = base.Add(time.Hour)
	if err := p.tick(context.Background(), "g1"); err != nil {
		t.Fatalf("sync: %v", err)
	}

	// Straight through submit and vote with not one ballot cast.
	now = base.Add(49 * time.Hour)
	if err := p.tick(context.Background(), "g1"); err != nil {
		t.Fatalf("open voting: %v", err)
	}
	now = base.Add(74 * time.Hour)
	if err := p.tick(context.Background(), "g1"); err != nil {
		t.Fatalf("finish: %v", err)
	}

	done, _ := store.LatestContest(context.Background(), "g1")
	if done.Phase != PhaseResults {
		t.Fatalf("phase = %s, want results", done.Phase)
	}
	if ops.dmCount() != 0 {
		t.Error("a prize was DMed to an arbitrary entrant in a contest nobody voted in")
	}
	after, _ := store.Prizes(context.Background(), c.ID)
	for _, pr := range after {
		if pr.AwardedTo != nil {
			t.Error("a prize was awarded with no votes cast")
		}
		if !pr.HasSecret() {
			t.Error("the prize code was wiped without being delivered, so it is gone for good")
		}
	}
}
