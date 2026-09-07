package contest

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"
)

// signedURL is a Discord attachment link as the CDN hands it out: the expiry
// and the issue time are hex unix seconds, which is what makes the deadline
// readable from the URL rather than guessed from a clock.
func signedURL(id string, expires time.Time) string {
	return "https://cdn.discordapp.com/attachments/1/" + id + "/art.png" +
		"?ex=" + strconv.FormatInt(expires.Unix(), 16) +
		"&is=" + strconv.FormatInt(expires.Add(-24*time.Hour).Unix(), 16) +
		"&hm=abc"
}

func TestURLExpiryReadsTheSignature(t *testing.T) {
	// The real link off the live gallery, which expired eleven hours after
	// its contest ended: ex=6a9e16ce is 2026-09-07 01:43:42 UTC.
	got, ok := urlExpiry("https://cdn.discordapp.com/attachments/1/2/k7j7245.jpeg" +
		"?ex=6a9e16ce&is=6a9cc54e&hm=c3650b12")
	if !ok {
		t.Fatal("a real signed Discord URL was not readable")
	}
	if want := time.Unix(0x6a9e16ce, 0).UTC(); !got.Equal(want) {
		t.Errorf("expiry = %v, want %v", got, want)
	}

	// Anything unreadable is "no expiry", never "expired". A URL merlin
	// cannot parse is not necessarily one that has died, and calling it dead
	// would re-read the whole forum on every tick forever.
	for _, bad := range []string{
		"",
		"https://cdn.discordapp.com/attachments/1/2/art.png",
		"https://cdn.discordapp.com/attachments/1/2/art.png?ex=",
		"https://cdn.discordapp.com/attachments/1/2/art.png?ex=notahexnumber",
		"https://cdn.discordapp.com/attachments/1/2/art.png?ex=0",
		"https://example.com/art.png",
		"://nonsense",
	} {
		if _, ok := urlExpiry(bad); ok {
			t.Errorf("urlExpiry(%q) claimed to know an expiry", bad)
		}
	}
}

// The deadline that matters is the soonest across the whole contest: one
// entry going dark is a broken gallery, so the refresh cannot wait for the
// average or the last.
func TestSoonestExpiryIsTheFirstOneToDie(t *testing.T) {
	base := time.Unix(1_700_000_000, 0).UTC()
	subs := []Submission{
		{MediaURLs: []string{signedURL("a", base.Add(20*time.Hour))}},
		{MediaURLs: []string{
			signedURL("b", base.Add(30*time.Hour)),
			signedURL("c", base.Add(2*time.Hour)), // the one that matters
		}},
		{MediaURLs: []string{"https://example.com/not-discord.png"}},
		{}, // a text-only entry
	}
	got, ok := soonestExpiry(subs)
	if !ok {
		t.Fatal("no expiry found among entries that carry one")
	}
	if want := base.Add(2 * time.Hour); !got.Equal(want) {
		t.Errorf("soonest = %v, want %v", got, want)
	}

	if _, ok := soonestExpiry([]Submission{{MediaURLs: []string{"https://example.com/x.png"}}}); ok {
		t.Error("claimed an expiry for entries that carry none")
	}
}

// The whole point of reading the signature: refresh because the link is about
// to die, not because half a day has gone by.
func TestRefreshIsDrivenByTheLinksNotTheClock(t *testing.T) {
	base := time.Unix(1_700_000_000, 0).UTC()
	store, ops, sched, audit := newFakeStore(), newFakeOps(), newFakeSched(), &fakeAudit{}
	c := seedLive(t, store, PhaseVote, base)

	// One entry whose art is good for another twenty hours.
	if err := store.UpsertSubmission(context.Background(), Submission{
		ID: "s1", ContestID: c.ID, UserID: "u1", ThreadID: "t1",
		MediaURLs: []string{signedURL("a", base.Add(20*time.Hour))},
	}); err != nil {
		t.Fatalf("seed submission: %v", err)
	}

	now := base
	p := newTestPlugin(t, store, ops, sched, audit, "")
	p.now = func() time.Time { return now }

	if p.dueForRefresh(context.Background(), c) {
		t.Error("refreshed a link with twenty hours left, which is most of its life")
	}

	// Still not, at eight hours out: outside the margin.
	now = base.Add(12 * time.Hour)
	if p.dueForRefresh(context.Background(), c) {
		t.Error("refreshed a link with eight hours left")
	}

	// Inside the margin, it goes.
	now = base.Add(20*time.Hour - refreshMargin + time.Minute)
	if !p.dueForRefresh(context.Background(), c) {
		t.Fatal("did not refresh a link inside the margin, so the gallery would go dark")
	}

	// And having just gone, it does not go again on the next tick, however
	// urgent the URLs look: a refresh is one REST call per entry.
	now = now.Add(tickInterval)
	if p.dueForRefresh(context.Background(), c) {
		t.Error("refreshed twice within the floor, so a nearly-expired contest re-reads " +
			"its whole forum every minute")
	}
	now = now.Add(refreshFloor)
	if !p.dueForRefresh(context.Background(), c) {
		t.Error("never refreshed again after the floor elapsed")
	}
}

// A shorter lifetime than the one this code was written against must not
// break the gallery. The old fixed interval refreshed on its own schedule and
// would have gone on doing so straight past the new expiry.
func TestAShorterLinkLifetimeIsFollowed(t *testing.T) {
	base := time.Unix(1_700_000_000, 0).UTC()
	store, ops, sched, audit := newFakeStore(), newFakeOps(), newFakeSched(), &fakeAudit{}
	c := seedLive(t, store, PhaseVote, base)

	// Suppose Discord starts issuing four-hour links.
	if err := store.UpsertSubmission(context.Background(), Submission{
		ID: "s1", ContestID: c.ID, UserID: "u1", ThreadID: "t1",
		MediaURLs: []string{signedURL("a", base.Add(4*time.Hour))},
	}); err != nil {
		t.Fatalf("seed submission: %v", err)
	}

	now := base
	p := newTestPlugin(t, store, ops, sched, audit, "")
	p.now = func() time.Time { return now }

	// An hour in, the link has three hours left, which is inside the margin.
	// A twelve hour timer would not have fired for another eleven.
	now = base.Add(time.Hour)
	if !p.dueForRefresh(context.Background(), c) {
		t.Error("a four hour link was not refreshed inside the margin: the refresh is " +
			"still following a clock rather than the link")
	}
}

// A store that cannot be read is not permission to stop refreshing. The cost
// of refreshing unnecessarily is one pass over the forum; the cost the other
// way is a gallery of broken images with nothing left to fix it.
func TestAnUnreadableEntryListStillRefreshes(t *testing.T) {
	base := time.Unix(1_700_000_000, 0).UTC()
	store, ops, sched, audit := newFakeStore(), newFakeOps(), newFakeSched(), &fakeAudit{}
	c := seedLive(t, store, PhaseVote, base)
	store.subsErr = errors.New("database went away")

	p := newTestPlugin(t, store, ops, sched, audit, "")
	p.now = func() time.Time { return base }

	if !p.dueForRefresh(context.Background(), c) {
		t.Error("a failed entry read stopped the refresh, which is the direction that " +
			"leaves a gallery broken with nothing scheduled to fix it")
	}
}

// The finished-contest window is what bounds the cost of all this, so it has
// to actually end.
func TestTheRefreshWindowEndsAndTheJobGoes(t *testing.T) {
	base := time.Unix(1_700_000_000, 0).UTC()
	store, ops, sched, audit := newFakeStore(), newFakeOps(), newFakeSched(), &fakeAudit{}
	closed := base
	c := liveContest(PhaseResults, base)
	c.ClosedAt = &closed
	if err := store.CreateContest(context.Background(), c); err != nil {
		t.Fatalf("seed: %v", err)
	}
	ops.threads = append(ops.threads, entryThread(ops, "t1", "u1"))

	now := base
	p := newTestPlugin(t, store, ops, sched, audit, "")
	p.now = func() time.Time { return now }

	if _, ok := p.closedNeedingArt(context.Background(), "g1"); !ok {
		t.Fatal("a contest that closed a moment ago is not being kept alive")
	}
	now = base.Add(artRefreshWindow - time.Hour)
	if _, ok := p.closedNeedingArt(context.Background(), "g1"); !ok {
		t.Error("the window ended early")
	}
	now = base.Add(artRefreshWindow + time.Hour)
	if _, ok := p.closedNeedingArt(context.Background(), "g1"); ok {
		t.Error("the window never ends, so every contest a guild ever ran keeps costing " +
			"REST calls forever")
	}
}
