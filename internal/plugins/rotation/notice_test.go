package rotation

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/6586x57890143/merlin/internal/discordguard"
	"github.com/6586x57890143/merlin/internal/scheduler"
	"github.com/6586x57890143/merlin/internal/settings"
)

// fakeNoticeStore is the in-memory NoticeStore. ClaimNotice keeps the real
// one's contract exactly: first caller for a given (rotation, instant) wins,
// everyone after gets false. That contract is the entire idempotency
// mechanism, so a fake that always said true would test nothing.
type fakeNoticeStore struct {
	mu       sync.Mutex
	claimed  map[string]bool
	claimErr error
	pruned   []time.Time
}

func newFakeNoticeStore() *fakeNoticeStore {
	return &fakeNoticeStore{claimed: map[string]bool{}}
}

func (f *fakeNoticeStore) ClaimNotice(_ context.Context, rotationID int64, noticeFor time.Time) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.claimErr != nil {
		return false, f.claimErr
	}
	key := noticeKey(rotationID, noticeFor)
	if f.claimed[key] {
		return false, nil
	}
	f.claimed[key] = true
	return true, nil
}

func noticeKey(rotationID int64, noticeFor time.Time) string {
	return fmt.Sprintf("%d@%s", rotationID, noticeFor.UTC().Format(time.RFC3339Nano))
}

func (f *fakeNoticeStore) ReleaseNotice(_ context.Context, rotationID int64, noticeFor time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.claimed, noticeKey(rotationID, noticeFor))
	return nil
}

func (f *fakeNoticeStore) PruneNotices(_ context.Context, before time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pruned = append(f.pruned, before)
	return nil
}

// noticeRC is a rotating channel configured to warn 10 minutes ahead of a
// daily rotation.
func noticeRC() settings.RotationChannel {
	rc := finiteRetentionRC()
	rc.ID = 1
	rc.NoticeLeadMinutes = 10
	return rc
}

// testClock is a movable now, so a test can walk through more than one
// rotation cycle instead of only ever observing a single instant.
type testClock struct{ t time.Time }

func (c *testClock) now() time.Time          { return c.t }
func (c *testClock) advance(d time.Duration) { c.t = c.t.Add(d) }

func setupNotice(t *testing.T, rc settings.RotationChannel) (*fakeOps, *fakeScheduler, *fakeNoticeStore, *Plugin) {
	t.Helper()
	fs := newFakeSettings()
	if err := fs.UpsertRotationChannel(context.Background(), rc); err != nil {
		t.Fatalf("seed settings: %v", err)
	}
	ops := newFakeOps()
	sched := newFakeScheduler()
	notices := newFakeNoticeStore()
	testNoticeClock = &testClock{t: fixedNow}

	p := &Plugin{
		ops:              func(string) DiscordChannelOps { return ops },
		dryRun:           func(string) bool { return false },
		settings:         fs,
		sched:            sched,
		notices:          notices,
		log:              testLogger(),
		voice:            testVoice(),
		now:              testNoticeClock.now,
		sweepRegistered:  map[string]bool{},
		noticeRegistered: map[string]bool{},
		registeredJobs:   map[string]time.Duration{},
	}
	return ops, sched, notices, p
}

// testNoticeClock is reset by every setupNotice call, so tests do not share
// a clock between them.
var testNoticeClock = &testClock{t: fixedNow}

func rotationKey(rc settings.RotationChannel) string {
	return scheduler.JobKey(rc.GuildID, rotationJobName(rc.ID))
}

// The heads-up fires once, inside the lead window, and says how long is
// left. Members' first notice of a rotation used to be the channel already
// being gone.
func TestNoticeFiresInsideTheLeadWindow(t *testing.T) {
	rc := noticeRC()
	ops, sched, _, p := setupNotice(t, rc)
	sched.nextDue[rotationKey(rc)] = fixedNow.Add(6 * time.Minute)

	if err := p.postDueNotices(context.Background(), "g1"); err != nil {
		t.Fatalf("postDueNotices: %v", err)
	}

	msgs := ops.messages[rc.ChannelID]
	if len(msgs) != 1 {
		t.Fatalf("messages posted = %d, want 1", len(msgs))
	}
	// The heads-up is an embed now, matching the intro notice posted into
	// this same channel when the rotation lands, so the text is a
	// description rather than message content.
	if len(ops.complexSends) != 1 || ops.complexSends[0].Embed == nil {
		t.Fatalf("heads-up was not sent as an embed: %+v", ops.complexSends)
	}
	body := ops.complexSends[0].Embed.Description

	// Deliberately "6 minutes", not the "6m" this asserted before. The
	// countdown moved from core.FormatDuration to humanDuration when the
	// heads-up became an embed, so that the two messages rotation posts into
	// one channel describe time the same way; the compact form is the
	// admin-surface register. This assertion is the contract, so it is
	// changed knowingly rather than relaxed to match whatever the code does.
	if !strings.Contains(body, "6 minutes") {
		t.Errorf("notice does not say how long is left: %q", body)
	}
	if strings.ContainsAny(body, "{}") {
		t.Errorf("notice leaked a placeholder: %q", body)
	}
	if len(ops.complexSends[0].Files) == 0 {
		t.Error("heads-up embed has no files, so its mood thumbnail renders as a broken frame")
	}
}

// The job runs every minute and the lead window is many minutes wide, so
// without the claim every single tick inside the window would post again.
// Being told six times that a channel is about to wipe reads as a broken
// bot, and this is the check that makes it structurally impossible.
func TestNoticeFiresOnlyOncePerRotation(t *testing.T) {
	rc := noticeRC()
	ops, sched, _, p := setupNotice(t, rc)
	due := fixedNow.Add(8 * time.Minute)
	sched.nextDue[rotationKey(rc)] = due

	for range 5 {
		if err := p.postDueNotices(context.Background(), "g1"); err != nil {
			t.Fatalf("postDueNotices: %v", err)
		}
	}

	if got := len(ops.messages[rc.ChannelID]); got != 1 {
		t.Errorf("posted %d notices for one rotation, want 1", got)
	}
}

// A different rotation instant is a different notice, or the channel would
// be warned once and then never again.
func TestTheNextRotationGetsItsOwnNotice(t *testing.T) {
	rc := noticeRC()
	ops, sched, _, p := setupNotice(t, rc)

	sched.nextDue[rotationKey(rc)] = fixedNow.Add(5 * time.Minute)
	if err := p.postDueNotices(context.Background(), "g1"); err != nil {
		t.Fatalf("postDueNotices: %v", err)
	}
	// A day later, with the clock moved forward too, the next rotation is
	// approaching. Moving only the due instant would put it outside the lead
	// window and prove nothing.
	testNoticeClock.advance(24 * time.Hour)
	sched.nextDue[rotationKey(rc)] = testNoticeClock.now().Add(5 * time.Minute)
	if err := p.postDueNotices(context.Background(), "g1"); err != nil {
		t.Fatalf("postDueNotices: %v", err)
	}

	if got := len(ops.messages[rc.ChannelID]); got != 2 {
		t.Errorf("posted %d notices across two rotations, want 2", got)
	}
}

func TestNoticeStaysQuietWhenItShould(t *testing.T) {
	cases := []struct {
		name    string
		lead    int
		nextDue func() (time.Time, bool)
		why     string
	}{
		{
			name:    "rotation is further out than the lead",
			lead:    10,
			nextDue: func() (time.Time, bool) { return fixedNow.Add(3 * time.Hour), true },
			why:     "warning three hours early is not a warning, it is noise",
		},
		{
			name: "no lead configured",
			lead: 0,
			// Deliberately inside what would be the window if a lead
			// existed, so this can only pass by honouring the setting.
			nextDue: func() (time.Time, bool) { return fixedNow.Add(2 * time.Minute), true },
			why:     "0 means the admin turned the heads-up off",
		},
		{
			name:    "rotation is overdue",
			lead:    10,
			nextDue: func() (time.Time, bool) { return time.Time{}, false },
			why:     "an overdue job fires on the next tick, so a countdown would be wrong in the one direction that matters",
		},
		{
			name:    "rotation already passed the due instant",
			lead:    10,
			nextDue: func() (time.Time, bool) { return fixedNow.Add(-time.Minute), true },
			why:     "counting down to something in the past reads as a stuck bot",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rc := noticeRC()
			rc.NoticeLeadMinutes = c.lead
			ops, sched, _, p := setupNotice(t, rc)
			if due, ok := c.nextDue(); ok {
				sched.nextDue[rotationKey(rc)] = due
			}

			if err := p.postDueNotices(context.Background(), "g1"); err != nil {
				t.Fatalf("postDueNotices: %v", err)
			}
			if got := len(ops.messages[rc.ChannelID]); got != 0 {
				t.Errorf("posted %d notices: %s", got, c.why)
			}
		})
	}
}

// Claiming before posting means a storage failure costs a notice, not a
// duplicate. The other order double-warns whenever the write fails.
func TestAFailedClaimPostsNothing(t *testing.T) {
	rc := noticeRC()
	ops, sched, notices, p := setupNotice(t, rc)
	sched.nextDue[rotationKey(rc)] = fixedNow.Add(5 * time.Minute)
	notices.claimErr = errors.New("database is down")

	if err := p.postDueNotices(context.Background(), "g1"); err != nil {
		t.Fatalf("a failed claim aborted the whole pass: %v", err)
	}
	if got := len(ops.messages[rc.ChannelID]); got != 0 {
		t.Errorf("posted %d notices despite failing to claim one", got)
	}
}

// Dry-run and pause refuse the send before Discord is touched at all, so
// the claim they consumed was spent on a message that provably never went
// out. Keeping it would mean an operator who clears dry-run with minutes
// still on the clock gets silence for a rotation the bot has already filed
// as warned, which is the exact shape of the bug this whole feature exists
// to avoid on the other side.
func TestASkippedSendGivesItsClaimBack(t *testing.T) {
	rc := noticeRC()
	ops, sched, notices, p := setupNotice(t, rc)
	due := fixedNow.Add(5 * time.Minute)
	sched.nextDue[rotationKey(rc)] = due

	ops.failWith["ChannelMessageSend"] = fmt.Errorf("message.send: %w", discordguard.ErrDryRun)

	if err := p.postDueNotices(context.Background(), "g1"); !discordguard.Skipped(err) {
		t.Fatalf("postDueNotices returned %v, want a skip so the job reports success", err)
	}
	if got := len(ops.messages[rc.ChannelID]); got != 0 {
		t.Fatalf("dry-run posted %d notices", got)
	}
	if notices.claimed[noticeKey(rc.ID, due)] {
		t.Fatal("the claim survived a send that never reached Discord, so this rotation can never be warned about")
	}

	// Dry-run cleared, still inside the window: the notice goes out.
	delete(ops.failWith, "ChannelMessageSend")
	if err := p.postDueNotices(context.Background(), "g1"); err != nil {
		t.Fatalf("postDueNotices: %v", err)
	}
	if got := len(ops.messages[rc.ChannelID]); got != 1 {
		t.Errorf("posted %d notices after dry-run was cleared, want 1", got)
	}
}

// The opposite case, and the reason the release is narrow. A send that
// genuinely errored may have reached Discord with only the response lost, so
// its claim stands: this feature would rather miss a notice than tell a
// channel twice that it is about to be wiped.
func TestAGenuinelyFailedSendKeepsItsClaim(t *testing.T) {
	rc := noticeRC()
	ops, sched, notices, p := setupNotice(t, rc)
	due := fixedNow.Add(5 * time.Minute)
	sched.nextDue[rotationKey(rc)] = due

	ops.failWith["ChannelMessageSend"] = errors.New("500 internal server error")

	if err := p.postDueNotices(context.Background(), "g1"); err != nil {
		t.Fatalf("one channel's failure aborted the whole pass: %v", err)
	}
	if !notices.claimed[noticeKey(rc.ID, due)] {
		t.Error("released a claim for a send that may well have landed, risking a duplicate warning")
	}
}

// A job that exists only where it has work, matching reconcileSweepJob.
func TestNoticeJobExistsOnlyWhereALeadIsSet(t *testing.T) {
	rc := noticeRC()
	_, sched, _, p := setupNotice(t, rc)
	key := scheduler.JobKey("g1", noticeJobName)

	p.reconcileNoticeJob("g1")
	if !sched.registered[key] {
		t.Fatal("no notice job registered for a guild that wants notices")
	}

	// The admin turns it off.
	rc.NoticeLeadMinutes = 0
	if err := p.settings.(*fakeSettings).UpsertRotationChannel(context.Background(), rc); err != nil {
		t.Fatalf("update settings: %v", err)
	}
	p.reconcileNoticeJob("g1")
	if sched.registered[key] {
		t.Error("notice job still registered after every lead was turned off")
	}
}

// A lead at or beyond the interval would leave a permanent "about to wipe"
// banner in the channel, at which point the words stop meaning anything.
func TestNoticeLeadMustBeShorterThanTheInterval(t *testing.T) {
	base := finiteRetentionRC()
	base.IntervalMinutes = 60

	for _, c := range []struct {
		lead    int
		wantErr bool
	}{
		{lead: 0, wantErr: false},
		{lead: 10, wantErr: false},
		{lead: 59, wantErr: false},
		{lead: 60, wantErr: true},
		{lead: 120, wantErr: true},
		{lead: -1, wantErr: true},
	} {
		rc := base
		rc.NoticeLeadMinutes = c.lead
		err := ValidateChannel(rc)
		if c.wantErr && err == nil {
			t.Errorf("lead of %d minutes against a 60 minute interval was accepted", c.lead)
		}
		if !c.wantErr && err != nil {
			t.Errorf("lead of %d minutes was rejected: %v", c.lead, err)
		}
	}
}

// A channel on generic disclosure still gets warned, but without a number.
// The countdown is the rotation schedule, which is exactly what that mode
// exists to withhold, so publishing it in the message immediately before the
// deliberately vague intro notice would make the setting pointless.
func TestGenericDisclosureWarnsWithoutACountdown(t *testing.T) {
	rc := noticeRC()
	rc.Disclosure = settings.DisclosureGeneric
	ops, sched, _, p := setupNotice(t, rc)
	sched.nextDue[rotationKey(rc)] = fixedNow.Add(6 * time.Minute)

	if err := p.postDueNotices(context.Background(), "g1"); err != nil {
		t.Fatalf("postDueNotices: %v", err)
	}

	if len(ops.complexSends) != 1 || ops.complexSends[0].Embed == nil {
		t.Fatalf("generic disclosure posted no heads-up embed: %+v", ops.complexSends)
	}
	body := ops.complexSends[0].Embed.Description
	for _, leak := range []string{"6 minutes", "6m", "minutes"} {
		if strings.Contains(body, leak) {
			t.Errorf("generic heads-up published the countdown (%q), which is the schedule the mode withholds: %q", leak, body)
		}
	}
	if body == "" {
		t.Error("generic heads-up said nothing at all; the warning itself is still wanted")
	}
}

// The two switches stay independent: disclosure decides how much is said,
// notice lead decides whether anything is said at all. Turning disclosure
// down must not quietly re-arm a heads-up an admin had switched off, and a
// lead of zero must still mean silence even in generic mode.
func TestNoticeLeadOffSilencesGenericDisclosureToo(t *testing.T) {
	rc := noticeRC()
	rc.Disclosure = settings.DisclosureGeneric
	rc.NoticeLeadMinutes = 0
	ops, sched, _, p := setupNotice(t, rc)
	sched.nextDue[rotationKey(rc)] = fixedNow.Add(6 * time.Minute)

	if err := p.postDueNotices(context.Background(), "g1"); err != nil {
		t.Fatalf("postDueNotices: %v", err)
	}
	if got := len(ops.messages[rc.ChannelID]); got != 0 {
		t.Errorf("posted %d notices with the lead switched off, want 0", got)
	}
}

// Under a minute out, the countdown rounds to zero and the message would read
// "resets in 0 minutes", which is wrong and visibly broken. Staying quiet is
// the failure mode this feature already prefers, and the claim must survive
// so the window is not spent on a message that was never sent.
func TestASubMinuteRotationGetsNoCountdown(t *testing.T) {
	rc := noticeRC()
	ops, sched, notices, p := setupNotice(t, rc)
	due := fixedNow.Add(20 * time.Second)
	sched.nextDue[rotationKey(rc)] = due

	if err := p.postDueNotices(context.Background(), "g1"); err != nil {
		t.Fatalf("postDueNotices: %v", err)
	}
	if got := len(ops.messages[rc.ChannelID]); got != 0 {
		t.Errorf("posted %d notices for a rotation under a minute away, want 0", got)
	}
	if notices.claimed[noticeKey(rc.ID, due)] {
		t.Error("a notice that was never sent consumed its claim")
	}
}
