package contest

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// --- the shape says enough to judge and not enough to redeem ---------------

// fakeToken is the stand-in for the part of a prize link that is the actual
// prize, and it is deliberately low entropy and self-describing.
//
// The fixtures here used to be realistic-looking keys, which is the obvious
// thing to reach for when the function under test classifies keys, and CI's
// gitleaks job flagged three of them as generic-api-key. It was right to:
// a string that looks like a credential in a diff is one a reviewer has to
// stop and think about, and suppressing that with a gitleaks:allow comment
// would trade a permanent "trust me" marker for nothing. codeShape only
// reads structure -- three groups of five, a host, a character class -- so
// a placeholder exercises exactly the same paths and reads better besides.
const fakeToken = "placeholder-not-a-real-code"

func TestCodeShapeSaysTheShapeAndNeverTheCode(t *testing.T) {
	for _, tc := range []struct {
		name, code, want string
	}{
		{"nothing pledged", "", ""},
		{"steam key", "AAAAA-BBBBB-CCCCC", "steam-shaped key, 17 characters"},
		{"gift link", "https://discord.gift/" + fakeToken, "link to discord.gift"},
		{"bare host link", "discord.gift/" + fakeToken, "link to discord.gift"},
		{"opaque token", "aBc123XyZ", "9 characters, letters and digits"},
		{"junk", "lol get rekt idiot", "4 words of prose"},
		{"one rude word", "cope", "4 characters, letters"},
	} {
		if got := codeShape(tc.code); got != tc.want {
			t.Errorf("%s: codeShape(%q) = %q, want %q", tc.name, tc.code, got, tc.want)
		}
	}

	// The half that matters. A link's host names the kind of thing it is; the
	// token after the slash is the entire prize, and it must not survive into
	// a string a moderator reads.
	for _, code := range []string{
		"https://discord.gift/" + fakeToken,
		"discord.gift/" + fakeToken,
		"https://store.steampowered.com/account/registerkey?code=" + fakeToken,
	} {
		shape := codeShape(code)
		for _, leak := range []string{fakeToken, "registerkey"} {
			if strings.Contains(shape, leak) {
				t.Errorf("codeShape(%q) = %q, which carries %q", code, shape, leak)
			}
		}
	}
}

// --- a pledge nobody has ruled on does not exist to anybody ---------------

func seedPledge(t *testing.T, store *fakeStore, contestID string, pr Prize) Prize {
	t.Helper()
	pr.ID, pr.ContestID = "prize-1", contestID
	if pr.DonorID == "" {
		pr.DonorID, pr.DonorName = "u9", "dana"
	}
	if pr.Title == "" {
		pr.Title = "a steam key"
	}
	if err := store.AddPrize(context.Background(), pr); err != nil {
		t.Fatalf("seed prize: %v", err)
	}
	return pr
}

// The gallery is the surface this whole queue exists to protect: /contest
// prize is TierPublic, so without the filter the page carries whatever any
// member typed, under their own name, moments after they typed it.
func TestAPendingPledgeIsNotPushedToTheGallery(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	store, ops, sched, audit := newFakeStore(), newFakeOps(), newFakeSched(), &fakeAudit{}
	seedLive(t, store, PhaseSubmit, base)
	seedPledge(t, store, "c1", Prize{Title: "FREE NITRO CLICK HERE"})

	var pushed []int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			var snap struct {
				Prizes []struct{} `json:"prizes"`
			}
			_ = json.NewDecoder(r.Body).Decode(&snap)
			pushed = append(pushed, len(snap.Prizes))
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	p := newTestPlugin(t, store, ops, sched, audit, srv.URL)
	p.now = func() time.Time { return base.Add(90 * time.Minute) }
	if err := p.tick(context.Background(), "g1"); err != nil {
		t.Fatalf("tick: %v", err)
	}

	if len(pushed) == 0 {
		t.Fatal("nothing was pushed at all, so this asserts nothing")
	}
	for _, n := range pushed {
		if n != 0 {
			t.Errorf("an unreviewed pledge reached the public gallery: %d prizes pushed", n)
		}
	}
}

// The other half, and the one that costs real money if it is wrong: an
// unreviewed code reaching a winner is the queue having been for nothing.
func TestAPendingPledgeIsNotAwarded(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	store, ops, sched, audit := newFakeStore(), newFakeOps(), newFakeSched(), &fakeAudit{}
	c := seedLive(t, store, PhaseVote, base)
	seedPledge(t, store, "c1", Prize{SecretSealed: []byte("sealed")})

	p := newTestPlugin(t, store, ops, sched, audit, "")
	p.now = func() time.Time { return base }
	subs := []Submission{{ID: "s1", ContestID: "c1", UserID: "u1", ThreadID: "t1", Author: "ana"}}
	p.awardPrizes(context.Background(), c, subs, []resultView{{ID: "s1", Votes: 4, Rank: 1}})

	got, _ := store.Prizes(context.Background(), "c1")
	if got[0].AwardedAt != nil {
		t.Error("an unreviewed pledge was handed to a winner")
	}
	if !got[0].HasSecret() {
		t.Error("the code was wiped for a delivery that never happened")
	}
	if len(ops.sentTo("dm-u1")) != 0 {
		t.Error("a winner was DMed a prize nobody had approved")
	}
}

// --- approving is the moment a pledge becomes public ----------------------

func TestApprovingAPledgePublishesIt(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	store, ops, sched, audit := newFakeStore(), newFakeOps(), newFakeSched(), &fakeAudit{}
	seedLive(t, store, PhaseSubmit, base)
	pr := seedPledge(t, store, "c1", Prize{Title: "a steam key", CodeShape: "steam-shaped key, 17 characters"})

	var pushedPrizes []int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			var snap struct {
				Prizes []struct{} `json:"prizes"`
			}
			_ = json.NewDecoder(r.Body).Decode(&snap)
			pushedPrizes = append(pushedPrizes, len(snap.Prizes))
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	p := newTestPlugin(t, store, ops, sched, audit, srv.URL)
	p.now = func() time.Time { return base }
	s, _ := stubSession()

	id := reviewPrefix + "approve:" + pr.ID
	p.handleReviewButton(context.Background(), s, componentClick(id), id)

	got, _ := store.Prizes(context.Background(), "c1")
	if !got[0].Approved || got[0].ReviewedBy != "mod-1" {
		t.Fatalf("the decision was not recorded: %+v", got[0])
	}
	if len(pushedPrizes) == 0 || pushedPrizes[len(pushedPrizes)-1] != 1 {
		t.Errorf("the approved pledge never reached the gallery: pushes = %v", pushedPrizes)
	}
	if len(ops.sentTo("announce-1")) != 1 {
		t.Errorf("announce posts = %d, want exactly 1", len(ops.sentTo("announce-1")))
	}
}

// --- rejecting keeps the record and destroys only the code ----------------

func TestRejectingAPledgeWipesTheCodeAndKeepsTheRow(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	store, ops, sched, audit := newFakeStore(), newFakeOps(), newFakeSched(), &fakeAudit{}
	seedLive(t, store, PhaseSubmit, base)
	pr := seedPledge(t, store, "c1", Prize{Title: "FREE NITRO", SecretSealed: []byte("sealed")})

	p := newTestPlugin(t, store, ops, sched, audit, "")
	p.now = func() time.Time { return base }
	s, _ := stubSession()

	id := reviewPrefix + "reject:" + pr.ID
	p.handleReviewButton(context.Background(), s, componentClick(id), id)

	got, _ := store.Prizes(context.Background(), "c1")
	if len(got) != 1 {
		t.Fatalf("rejecting deleted the row, so there is no record of the decision: %+v", got)
	}
	if !got[0].Rejected() {
		t.Errorf("the rejection was not recorded: %+v", got[0])
	}
	if got[0].HasSecret() {
		t.Error("merlin is still holding the code of a pledge she turned down")
	}
	if len(ops.sentTo("dm-u9")) != 1 {
		t.Errorf("donor DMs = %d, want 1: a rejection nobody is told about is a disappearance",
			len(ops.sentTo("dm-u9")))
	}
	if len(ops.sentTo("announce-1")) != 0 {
		t.Error("a rejected pledge was announced to the server anyway")
	}

	// The claim is what makes the buttons safe to leave on screen: the
	// message a mod is looking at can be older than the queue it describes,
	// and two mods can be working it at once.
	p.handleReviewButton(context.Background(), s, componentClick(id), id)
	if n := len(ops.sentTo("dm-u9")); n != 1 {
		t.Errorf("a stale button re-ran the rejection: donor DMs = %d", n)
	}
}

// A bounced DM is the ordinary case, not a failure. The decision is already
// recorded by the time it is attempted and must survive it.
func TestARejectionSurvivesAClosedDM(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	store, ops, sched, audit := newFakeStore(), newFakeOps(), newFakeSched(), &fakeAudit{}
	seedLive(t, store, PhaseSubmit, base)
	pr := seedPledge(t, store, "c1", Prize{SecretSealed: []byte("sealed")})
	ops.dmFails = true

	p := newTestPlugin(t, store, ops, sched, audit, "")
	p.now = func() time.Time { return base }
	s, _ := stubSession()

	id := reviewPrefix + "reject:" + pr.ID
	p.handleReviewButton(context.Background(), s, componentClick(id), id)

	got, _ := store.Prizes(context.Background(), "c1")
	if !got[0].Rejected() || got[0].HasSecret() {
		t.Errorf("a closed DM undid the rejection: %+v", got[0])
	}
}
