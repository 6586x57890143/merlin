package contest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
)

// The entry list freezes when voting opens, so the read that builds it has
// to be the last thing before the claim rather than whatever a tick happened
// to leave behind.
//
// This is the live failure it was found as. A contest ran for two minutes:
// the forum opened at 01:28:26, one member posted at 01:29:36, a second
// posted at 01:30:09, and the snapshot pushed at 01:30:20 carried one entry.
// Eleven seconds is well inside the one-minute tick, so the second post had
// never been synced, and /contest advance goes straight into advance(),
// which builds its push out of p.store.Submissions and never re-reads the
// forum. The second entry was dropped from the contest it had been entered
// in, and stayed dropped, because vote is where the list stops moving.
func TestAdvanceToVoteReadsTheForumOneLastTime(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	store, ops, sched, audit := newFakeStore(), newFakeOps(), newFakeSched(), &fakeAudit{}
	c := seedLive(t, store, PhaseSubmit, base)
	seedForum(ops, c, 0)

	// The tick that ran a minute ago saw one post.
	ops.threads = []*discordgo.Channel{entryThread(ops, "100", "u1")}
	p := newTestPlugin(t, store, ops, sched, audit, "")
	p.now = func() time.Time { return base }
	if err := p.syncSubmissions(context.Background(), c); err != nil {
		t.Fatalf("first sync: %v", err)
	}

	// A second member posts in the seconds before the operator advances, so
	// no tick has ever read it.
	ops.threads = append(ops.threads, entryThread(ops, "200", "u2"))

	if err := p.advance(context.Background(), c); err != nil {
		t.Fatalf("advance: %v", err)
	}

	subs, err := store.Submissions(context.Background(), "c1")
	if err != nil {
		t.Fatalf("submissions: %v", err)
	}
	got := make(map[string]bool, len(subs))
	for _, s := range subs {
		got[s.ThreadID] = true
	}
	if !got["100"] || !got["200"] {
		t.Fatalf("entry list froze without the last post: got %v, want both 100 and 200", got)
	}
}

// The sync has to run before AdvancePhase, not after it.
//
// syncSubmissions only does its withdraw pass while c.Phase is PhaseSubmit,
// deliberately: once voting starts a deleted post must not retroactively
// discard the votes cast for it. So a sync moved to after the claim still
// picks up late posts and silently stops honouring late withdrawals, which
// is the half of this that looks fine in the gallery and is wrong in the
// tally. Pinned because the ordering reads like a detail.
func TestAdvanceToVoteStillHonoursALastMinuteWithdrawal(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	store, ops, sched, audit := newFakeStore(), newFakeOps(), newFakeSched(), &fakeAudit{}
	c := seedLive(t, store, PhaseSubmit, base)
	seedForum(ops, c, 0)

	ops.threads = []*discordgo.Channel{
		entryThread(ops, "100", "u1"),
		entryThread(ops, "200", "u2"),
	}
	p := newTestPlugin(t, store, ops, sched, audit, "")
	p.now = func() time.Time { return base }
	if err := p.syncSubmissions(context.Background(), c); err != nil {
		t.Fatalf("first sync: %v", err)
	}

	// u2 deletes their post just before the contest closes.
	ops.threads = ops.threads[:1]

	if err := p.advance(context.Background(), c); err != nil {
		t.Fatalf("advance: %v", err)
	}

	subs, err := store.Submissions(context.Background(), "c1")
	if err != nil {
		t.Fatalf("submissions: %v", err)
	}
	for _, s := range subs {
		if s.ThreadID == "200" && s.WithdrawnAt == nil {
			t.Fatal("a post deleted before voting opened is still a live entry: the sync ran after the claim")
		}
	}
}

// A forum read that fails must not stop the contest closing. The stale list
// this fix exists to prevent is strictly smaller than a contest wedged in
// its submission phase: the Scheduler would retry, fail the same way, and
// trip maxConsecutiveFailures while voting never opened. Same policy tick()
// already applies to the same call.
func TestAFailedFinalSyncStillClosesSubmissions(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	store, ops, sched, audit := newFakeStore(), newFakeOps(), newFakeSched(), &fakeAudit{}
	c := seedLive(t, store, PhaseSubmit, base)
	seedForum(ops, c, 0)

	ops.threadsErr = errors.New("429 slow down")
	p := newTestPlugin(t, store, ops, sched, audit, "")
	p.now = func() time.Time { return base }

	if err := p.advance(context.Background(), c); err != nil {
		t.Fatalf("advance: %v", err)
	}
	live, err := store.LiveContest(context.Background(), "g1")
	if err != nil {
		t.Fatalf("live contest: %v", err)
	}
	if live.Phase != PhaseVote {
		t.Fatalf("contest stuck in %q after a failed forum read, want %q", live.Phase, PhaseVote)
	}
}
