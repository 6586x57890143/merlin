package statistics

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/6586x57890143/merlin/internal/core"
)

// fakeScheduler records registrations; the job is driven by hand.
type fakeScheduler struct {
	jobs map[string]func(context.Context) error
}

func newFakeScheduler() *fakeScheduler {
	return &fakeScheduler{jobs: map[string]func(context.Context) error{}}
}
func (f *fakeScheduler) Register(key string, _ core.CronSpec, fn func(context.Context) error) error {
	f.jobs[key] = fn
	return nil
}
func (f *fakeScheduler) Unregister(key string) error          { delete(f.jobs, key); return nil }
func (f *fakeScheduler) RunNow(context.Context, string) error { return nil }
func (f *fakeScheduler) Seed(context.Context, string, time.Time) error {
	return nil
}
func (f *fakeScheduler) NextDue(context.Context, string) (time.Time, bool, error) {
	return time.Time{}, false, nil
}

// history builds a channel's messages, newest first, n per hour over hours
// hours ending just before until, alternating two authors.
func history(until time.Time, hours, perHour int) []*discordgo.Message {
	var out []*discordgo.Message
	for h := 1; h <= hours; h++ {
		hour := until.Add(-time.Duration(h) * time.Hour)
		for i := perHour - 1; i >= 0; i-- {
			author := "u1"
			if i%2 == 1 {
				author = "u2"
			}
			out = append(out, msgAt(hour.Add(time.Duration(i)*time.Minute), i, author, author))
		}
	}
	return out
}

// TestBackfillFillsTheHoursBeforeCounting: the importer lands exactly the
// window it was asked for, by the hour, and stops at from. The live
// boundary is exclusive: nothing at or after until is counted.
func TestBackfillFillsTheHoursBeforeCounting(t *testing.T) {
	live := windowStart.Add(10 * time.Hour)
	src := &fakeSource{
		channels: []*discordgo.Channel{textChannel("c1", "general")},
		msgs:     map[string][]*discordgo.Message{"c1": history(live.Add(time.Hour), 12, 4)},
	}
	store := newFakeStore()
	p := newTestPlugin(store, src)
	p.sched = newFakeScheduler()
	_ = store.MarkLive(context.Background(), "g1", live)

	n, err := p.queueBackfill(context.Background(), "g1", windowStart.Add(4*time.Hour), live)
	if err != nil || n != 1 {
		t.Fatalf("queue: n=%d err=%v", n, err)
	}
	if err := p.backfill(context.Background(), "g1"); err != nil {
		t.Fatalf("backfill: %v", err)
	}

	// Hours 4..9 after windowStart, 4 messages each: 24, split evenly.
	if got := store.total("g1", ""); got != 24 {
		t.Fatalf("expected the six hours inside the window, got %d messages", got)
	}
	if store.total("g1", "u1") != 12 || store.total("g1", "u2") != 12 {
		t.Fatalf("split wrong: u1=%d u2=%d", store.total("g1", "u1"), store.total("g1", "u2"))
	}
	if _, ok := store.hourly[bucketKey{"g1", "c1", "u1", live}]; ok {
		t.Fatal("the hour counting began must not be backfilled")
	}
	pending, _ := store.PendingBackfill(context.Background(), "g1")
	if len(pending) != 0 {
		t.Fatalf("channel should be done, got %+v", pending)
	}
	if store.users["g1:u1"].Name != "u1" {
		t.Fatal("authors seen during backfill should be named")
	}
	if store.channels["g1:c1"] != "general" {
		t.Fatal("queueing should record the channel name")
	}
}

// TestBackfillResumesWithoutDoubleCounting is the idempotency argument: a
// slice that ends mid-channel leaves a cursor, and re-running from it lands
// on the same totals as one uninterrupted pass.
func TestBackfillResumesWithoutDoubleCounting(t *testing.T) {
	live := windowStart.Add(10 * time.Hour)
	src := &fakeSource{
		channels: []*discordgo.Channel{textChannel("c1", "general")},
		msgs:     map[string][]*discordgo.Message{"c1": history(live, 10, 30)},
	}
	store := newFakeStore()
	p := newTestPlugin(store, src)
	p.sched = newFakeScheduler()
	_ = store.MarkLive(context.Background(), "g1", live)
	if _, err := p.queueBackfill(context.Background(), "g1", windowStart, live); err != nil {
		t.Fatal(err)
	}

	// A clock that runs out after the first page: the slice ends with the
	// cursor written for whatever whole hours that page closed.
	calls := 0
	base := p.now()
	p.now = func() time.Time {
		calls++
		if calls > 2 {
			return base.Add(backfillSlice + time.Second)
		}
		return base
	}
	if err := p.backfill(context.Background(), "g1"); err != nil {
		t.Fatalf("first slice: %v", err)
	}
	pending, _ := store.PendingBackfill(context.Background(), "g1")
	if len(pending) != 1 || pending[0].Cursor == "" {
		t.Fatalf("expected a cursor after the first slice, got %+v", pending)
	}
	first := store.total("g1", "")
	if first == 0 || first == 300 {
		t.Fatalf("the first slice should have landed some whole hours, not %d", first)
	}

	p.now = func() time.Time { return base }
	if err := p.backfill(context.Background(), "g1"); err != nil {
		t.Fatalf("second slice: %v", err)
	}
	if got := store.total("g1", ""); got != 300 {
		t.Fatalf("resumed backfill should reach exactly 300, got %d", got)
	}
	pending, _ = store.PendingBackfill(context.Background(), "g1")
	if len(pending) != 0 {
		t.Fatal("channel should be done")
	}
}

// TestBackfillSkipsAnUnreadableChannelAndKeepsGoing: Missing Access is an
// answer, recorded on the row, and the next channel still gets read.
func TestBackfillSkipsAnUnreadableChannelAndKeepsGoing(t *testing.T) {
	live := windowStart.Add(2 * time.Hour)
	src := &fakeSource{
		channels:   []*discordgo.Channel{textChannel("a-secret", "secret"), textChannel("b-open", "open")},
		msgs:       map[string][]*discordgo.Message{"b-open": history(live, 2, 3)},
		unreadable: map[string]bool{"a-secret": true},
	}
	store := newFakeStore()
	p := newTestPlugin(store, src)
	p.sched = newFakeScheduler()
	_ = store.MarkLive(context.Background(), "g1", live)
	if _, err := p.queueBackfill(context.Background(), "g1", windowStart, live); err != nil {
		t.Fatal(err)
	}
	if err := p.backfill(context.Background(), "g1"); err != nil {
		t.Fatalf("backfill: %v", err)
	}
	sum, _ := store.BackfillStatus(context.Background(), "g1")
	if sum.Failed != 1 || sum.Done != 1 || sum.Pending != 0 {
		t.Fatalf("status: %+v", sum)
	}
	if store.total("g1", "") != 6 {
		t.Fatalf("the readable channel should still be counted: %d", store.total("g1", ""))
	}
}

// TestBackfillJobRegistersOnlyWhileThereIsWork: the same rule as every
// other per-guild job here.
func TestBackfillJobRegistersOnlyWhileThereIsWork(t *testing.T) {
	live := windowStart.Add(time.Hour)
	src := &fakeSource{channels: []*discordgo.Channel{textChannel("c1", "general")}, msgs: map[string][]*discordgo.Message{}}
	store := newFakeStore()
	p := newTestPlugin(store, src)
	sched := newFakeScheduler()
	p.sched = sched
	_ = store.MarkLive(context.Background(), "g1", live)

	p.SyncGuild(context.Background(), "g1")
	if _, ok := sched.jobs[backfillJobKey("g1")]; ok {
		t.Fatal("no backfill requested, no job")
	}
	if _, err := p.queueBackfill(context.Background(), "g1", windowStart, live); err != nil {
		t.Fatal(err)
	}
	job, ok := sched.jobs[backfillJobKey("g1")]
	if !ok {
		t.Fatal("queueing should register the job")
	}
	if err := job(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := sched.jobs[backfillJobKey("g1")]; ok {
		t.Fatal("a finished backfill should unregister its job")
	}
	p.ForgetGuild("g1")
}

// TestSnowflakeCursorArithmetic: the resume cursor is one above the message
// that closed the hour, so paging before it re-reads that message and
// nothing newer.
func TestSnowflakeCursorArithmetic(t *testing.T) {
	m := msgAt(windowStart, 5, "u", "u")
	id := messageID(m.ID)
	if id == 0 {
		t.Fatal("a real snowflake should parse")
	}
	if messageID("nope") != 0 {
		t.Fatal("garbage is 0")
	}
	next := strconv.FormatInt(id+1, 10)
	src := &fakeSource{msgs: map[string][]*discordgo.Message{"c": {m}}}
	got, _ := src.ChannelMessages("c", 100, next, "", "")
	if len(got) != 1 || got[0].ID != m.ID {
		t.Fatalf("before=id+1 should return the message itself, got %v", got)
	}
}
