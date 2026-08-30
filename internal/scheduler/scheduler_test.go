package scheduler

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// testClock is a mutex-guarded, manually-advanced clock so tests can control
// due-ness deterministically instead of racing real wall-clock time.
type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func newTestClock() *testClock {
	return &testClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
}

// fakeStore is an in-memory JobStateStore so scheduler logic can be unit
// tested without a live Postgres.
type fakeStore struct {
	mu     sync.Mutex
	states map[string]JobState
}

func newFakeStore() *fakeStore {
	return &fakeStore{states: make(map[string]JobState)}
}

// fakeSettings is a no-op statusChannelResolver; none of the scheduler unit
// tests exercise alert routing via a real channel (TestThresholdFailuresAlertOnce
// injects s.alertFunc directly instead), so an always-empty resolver is fine.
type fakeSettings struct{}

func (fakeSettings) StatusChannelID(guildID string) string { return "" }

func (f *fakeStore) Get(ctx context.Context, jobKey string) (JobState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.states[jobKey], nil
}

func (f *fakeStore) RecordSuccess(ctx context.Context, jobKey string, at time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.states[jobKey] = JobState{LastRun: at, HasLastRun: true, LastAttempt: at, ConsecutiveFailures: 0}
	return nil
}

func (f *fakeStore) RecordFailure(ctx context.Context, jobKey string, at time.Time) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	st := f.states[jobKey]
	st.LastAttempt = at
	st.ConsecutiveFailures++
	f.states[jobKey] = st
	return st.ConsecutiveFailures, nil
}

func waitForCount(t *testing.T, counter *int32, want int32) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if atomic.LoadInt32(counter) >= want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d runs, got %d", want, atomic.LoadInt32(counter))
}

func TestTickRunsJobOnlyWhenDue(t *testing.T) {
	store := newFakeStore()
	clock := newTestClock()
	s := New(store, fakeSettings{}, testLogger())
	s.now = clock.Now

	jobKey := JobKey("g1", "test")
	interval := time.Hour
	var runs int32
	if err := s.Register(jobKey, CronSpec{Schedule: IntervalSchedule{Interval: interval}}, func(ctx context.Context) error {
		atomic.AddInt32(&runs, 1)
		return nil
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	// Never attempted: due immediately.
	s.tick(context.Background())
	waitForCount(t, &runs, 1)

	// Right after a successful run, not due again.
	s.tick(context.Background())
	time.Sleep(20 * time.Millisecond)
	if got := atomic.LoadInt32(&runs); got != 1 {
		t.Fatalf("expected still 1 run right after success, got %d", got)
	}

	jitter := jitterFor(jobKey, interval)

	// Just short of interval+jitter: still not due.
	clock.Advance(interval + jitter - time.Second)
	s.tick(context.Background())
	time.Sleep(20 * time.Millisecond)
	if got := atomic.LoadInt32(&runs); got != 1 {
		t.Fatalf("fired before due: got %d runs", got)
	}

	// Past due: fires.
	clock.Advance(2 * time.Second)
	s.tick(context.Background())
	waitForCount(t, &runs, 2)
}

func TestPersistedLastRunSurvivesRestart(t *testing.T) {
	store := newFakeStore()
	clock := newTestClock()
	jobKey := JobKey("g1", "restart-test")

	s1 := New(store, fakeSettings{}, testLogger())
	s1.now = clock.Now
	if err := s1.Register(jobKey, CronSpec{Schedule: IntervalSchedule{Interval: time.Hour}}, func(ctx context.Context) error { return nil }); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := s1.RunNow(context.Background(), jobKey); err != nil {
		t.Fatalf("RunNow: %v", err)
	}

	// Simulate a process restart: a fresh Scheduler instance sharing the
	// same backing store must not treat the job as never-attempted.
	s2 := New(store, fakeSettings{}, testLogger())
	s2.now = clock.Now
	if err := s2.Register(jobKey, CronSpec{Schedule: IntervalSchedule{Interval: time.Hour}}, func(ctx context.Context) error { return nil }); err != nil {
		t.Fatalf("Register: %v", err)
	}

	due, err := s2.isDue(context.Background(), s2.jobs[jobKey])
	if err != nil {
		t.Fatalf("isDue: %v", err)
	}
	if due {
		t.Fatal("expected job not due immediately after restart; last_run should have survived")
	}
}

func TestBackoffForGrowsAndCaps(t *testing.T) {
	prev := time.Duration(0)
	for i := 1; i < maxConsecutiveFailures; i++ {
		d := backoffFor(i)
		if d <= prev {
			t.Fatalf("backoff not increasing at failure %d: got %v, prev %v", i, d, prev)
		}
		if d > backoffMax {
			t.Fatalf("backoff exceeds cap at failure %d: %v", i, d)
		}
		prev = d
	}
}

func TestThresholdFailuresAlertOnce(t *testing.T) {
	store := newFakeStore()
	clock := newTestClock()
	s := New(store, fakeSettings{}, testLogger())
	s.now = clock.Now

	var alerts []string
	s.alertFunc = func(ctx context.Context, jobKey string, failures int, cause error) error {
		alerts = append(alerts, fmt.Sprintf("%s failed %d times: %v", jobKey, failures, cause))
		return nil
	}

	jobKey := JobKey("g1", "failing")
	boom := errors.New("boom")
	if err := s.Register(jobKey, CronSpec{Schedule: IntervalSchedule{Interval: time.Hour}}, func(ctx context.Context) error { return boom }); err != nil {
		t.Fatalf("Register: %v", err)
	}

	for i := 1; i <= maxConsecutiveFailures; i++ {
		if err := s.RunNow(context.Background(), jobKey); err == nil {
			t.Fatalf("attempt %d: expected job error", i)
		}
	}

	if len(alerts) != 1 {
		t.Fatalf("expected exactly 1 alert at the failure threshold, got %d: %v", len(alerts), alerts)
	}
}

func TestPerJobLockPreventsConcurrentDoubleRun(t *testing.T) {
	store := newFakeStore()
	s := New(store, fakeSettings{}, testLogger())

	jobKey := JobKey("g1", "slow")
	started := make(chan struct{})
	var startedOnce sync.Once
	release := make(chan struct{})
	var concurrent int32

	if err := s.Register(jobKey, CronSpec{Schedule: IntervalSchedule{Interval: time.Hour}}, func(ctx context.Context) error {
		if atomic.AddInt32(&concurrent, 1) > 1 {
			t.Error("job ran concurrently with itself")
		}
		startedOnce.Do(func() { close(started) })
		<-release
		atomic.AddInt32(&concurrent, -1)
		return nil
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	errCh := make(chan error, 1)
	go func() { errCh <- s.RunNow(context.Background(), jobKey) }()

	<-started
	if err := s.RunNow(context.Background(), jobKey); err == nil {
		t.Fatal("expected a concurrent RunNow to fail while the job is already running")
	}
	close(release)
	if err := <-errCh; err != nil {
		t.Fatalf("first RunNow: %v", err)
	}
}

func TestJitterDeterministicPerJobKey(t *testing.T) {
	a := jitterFor("g1:job", time.Hour)
	b := jitterFor("g1:job", time.Hour)
	if a != b {
		t.Fatalf("jitter not deterministic: %v vs %v", a, b)
	}
	if a < 0 || a > maxJitter {
		t.Fatalf("jitter out of bounds: %v", a)
	}
}

func TestRunNowIgnoresScheduleAndUpdatesLastRun(t *testing.T) {
	store := newFakeStore()
	clock := newTestClock()
	s := New(store, fakeSettings{}, testLogger())
	s.now = clock.Now

	jobKey := JobKey("g1", "manual")
	var runs int32
	if err := s.Register(jobKey, CronSpec{Schedule: IntervalSchedule{Interval: 24 * time.Hour}}, func(ctx context.Context) error {
		atomic.AddInt32(&runs, 1)
		return nil
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	if err := s.RunNow(context.Background(), jobKey); err != nil {
		t.Fatalf("RunNow: %v", err)
	}
	if got := atomic.LoadInt32(&runs); got != 1 {
		t.Fatalf("expected 1 run, got %d", got)
	}

	st, err := store.Get(context.Background(), jobKey)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !st.HasLastRun || !st.LastRun.Equal(clock.Now()) {
		t.Fatalf("expected last_run persisted at %v, got %+v", clock.Now(), st)
	}
}

func TestRegisterValidation(t *testing.T) {
	noop := func(ctx context.Context) error { return nil }

	tests := []struct {
		name    string
		jobKey  string
		spec    CronSpec
		fn      func(ctx context.Context) error
		wantErr bool
	}{
		{"empty key", "", CronSpec{Schedule: IntervalSchedule{Interval: time.Hour}}, noop, true},
		{"zero interval", "g1:job", CronSpec{}, noop, true},
		{"negative interval", "g1:job", CronSpec{Schedule: IntervalSchedule{Interval: -time.Hour}}, noop, true},
		{"nil fn", "g1:job", CronSpec{Schedule: IntervalSchedule{Interval: time.Hour}}, nil, true},
		{"valid", "g1:job", CronSpec{Schedule: IntervalSchedule{Interval: time.Hour}}, noop, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := New(newFakeStore(), fakeSettings{}, testLogger())
			err := s.Register(tt.jobKey, tt.spec, tt.fn)
			if (err != nil) != tt.wantErr {
				t.Fatalf("Register(%q): err=%v, wantErr=%v", tt.jobKey, err, tt.wantErr)
			}
		})
	}
}

func TestRegisterDuplicateJobKeyFails(t *testing.T) {
	s := New(newFakeStore(), fakeSettings{}, testLogger())
	noop := func(ctx context.Context) error { return nil }
	if err := s.Register("g1:job", CronSpec{Schedule: IntervalSchedule{Interval: time.Hour}}, noop); err != nil {
		t.Fatalf("first Register: %v", err)
	}
	if err := s.Register("g1:job", CronSpec{Schedule: IntervalSchedule{Interval: time.Hour}}, noop); err == nil {
		t.Fatal("expected duplicate registration to fail")
	}
}

func TestRunNowUnknownJobFails(t *testing.T) {
	s := New(newFakeStore(), fakeSettings{}, testLogger())
	if err := s.RunNow(context.Background(), "g1:nope"); err == nil {
		t.Fatal("expected RunNow on an unregistered job to fail")
	}
}

// TestJitterSpreadsAcrossItsFullRange is a regression test for a silent
// 32-bit overflow: jitterFor folded a uint32 hash into a nanosecond bound,
// and nanoseconds outgrow uint32 at 4.3 seconds, so every job's jitter
// landed in a ~4s window no matter how large maxJitter was, and the
// thundering-herd spread it exists to provide barely existed. With a
// 24h interval the bound is maxJitter (2 minutes), and a healthy spread must
// reach well past those first four seconds.
func TestJitterSpreadsAcrossItsFullRange(t *testing.T) {
	const (
		jobs        = 200
		wantMinimum = 30 * time.Second // far beyond the ~4s the overflow allowed
	)
	var maxSeen time.Duration
	buckets := make(map[int]bool)
	for i := range jobs {
		j := jitterFor(JobKey("guild"+strconv.Itoa(i), "rotation:1"), 24*time.Hour)
		if j < 0 || j >= maxJitter {
			t.Fatalf("jitter %v out of range [0, %v)", j, maxJitter)
		}
		maxSeen = max(maxSeen, j)
		buckets[int(j/(10*time.Second))] = true
	}
	if maxSeen < wantMinimum {
		t.Errorf("largest jitter across %d jobs was %v, want at least %v; the range is being truncated", jobs, maxSeen, wantMinimum)
	}
	if len(buckets) < 6 {
		t.Errorf("jitter clustered into %d of 12 ten-second buckets, want it spread across the range", len(buckets))
	}
}

// TestJitterZeroForShortIntervals keeps the bound's edge honest: an interval
// too short to carve a tenth out of shouldn't produce a jitter at all,
// rather than a modulo-by-zero panic.
func TestJitterZeroForShortIntervals(t *testing.T) {
	for _, interval := range []time.Duration{0, -time.Hour, 5 * time.Nanosecond} {
		if got := jitterFor("g1:job", interval); got != 0 {
			t.Errorf("jitterFor(interval=%v) = %v, want 0", interval, got)
		}
	}
}

// TestExecuteRecordsFailureAfterJobTimeout covers the bookkeeping detach: a
// job killed by its own timeout must still have that failure persisted.
// Writing it through the expired context instead would drop it, leaving the
// job looking permanently healthy: never backing off, never alerting, and
// re-running into the same wall every tick.
func TestExecuteRecordsFailureAfterJobTimeout(t *testing.T) {
	store := newFakeStore()
	clock := newTestClock()
	s := New(store, fakeSettings{}, testLogger())
	s.now = clock.Now

	jobKey := JobKey("g1", "hangs")
	if err := s.Register(jobKey, CronSpec{Schedule: IntervalSchedule{Interval: time.Hour}}, func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the job's context is already dead by the time it runs
	if err := s.RunNow(ctx, jobKey); err == nil {
		t.Fatal("expected the cancelled job to report an error")
	}

	st, err := store.Get(context.Background(), jobKey)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if st.ConsecutiveFailures != 1 {
		t.Fatalf("expected the failure to be recorded despite the dead context, got %d consecutive failures", st.ConsecutiveFailures)
	}
}

// TestAutocompleteJobCapsAtDiscordLimit guards a hard API limit: an
// autocomplete response over 25 choices is rejected outright, so a guild
// with many rotating channels would get no suggestions at all rather than a
// truncated list.
func TestAutocompleteJobCapsAtDiscordLimit(t *testing.T) {
	s := New(newFakeStore(), fakeSettings{}, testLogger())
	for i := range maxAutocompleteChoices + 10 {
		key := JobKey("g1", "rotation:"+strconv.Itoa(i))
		if err := s.Register(key, CronSpec{Schedule: IntervalSchedule{Interval: time.Hour}}, func(ctx context.Context) error { return nil }); err != nil {
			t.Fatalf("Register: %v", err)
		}
	}

	choices := s.autocompleteJob(context.Background(), &discordgo.InteractionCreate{
		Interaction: &discordgo.Interaction{GuildID: "g1"},
	}, "job", "")
	if len(choices) != maxAutocompleteChoices {
		t.Fatalf("got %d autocomplete choices, want the Discord cap of %d", len(choices), maxAutocompleteChoices)
	}
}

// TestJobsForGuildIsolatesGuilds guards the prefix matching every per-guild
// view depends on: /scheduler list and run-now must never surface, or run,
// another server's job.
func TestJobsForGuildIsolatesGuilds(t *testing.T) {
	s := New(newFakeStore(), fakeSettings{}, testLogger())
	noop := func(ctx context.Context) error { return nil }
	for _, key := range []string{
		JobKey("g1", "rotation:1"),
		JobKey("g1", "rotation-sweep"),
		JobKey("g2", "rotation:1"),
		JobKey("g10", "rotation:1"), // a guild ID that has g1 as a prefix
	} {
		if err := s.Register(key, CronSpec{Schedule: IntervalSchedule{Interval: time.Hour}}, noop); err != nil {
			t.Fatalf("Register(%s): %v", key, err)
		}
	}

	got := s.jobsForGuild("g1")
	if len(got) != 2 {
		t.Fatalf("expected exactly g1's 2 jobs, got %d: %v", len(got), got)
	}
	for _, j := range got {
		if !strings.HasPrefix(j.key, "g1:") {
			t.Errorf("jobsForGuild(g1) returned %q", j.key)
		}
	}
}

// TestUnregisterStopsFutureRunsMidFlight documents the contract reconcile
// depends on when a mod removes a rotating channel: an in-flight run
// finishes untouched, but no later tick may pick the job up again.
func TestUnregisterStopsFutureRunsMidFlight(t *testing.T) {
	store := newFakeStore()
	clock := newTestClock()
	s := New(store, fakeSettings{}, testLogger())
	s.now = clock.Now

	jobKey := JobKey("g1", "removed")
	var runs int32
	if err := s.Register(jobKey, CronSpec{Schedule: IntervalSchedule{Interval: time.Hour}}, func(ctx context.Context) error {
		atomic.AddInt32(&runs, 1)
		return nil
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	s.tick(context.Background())
	waitForCount(t, &runs, 1)

	if err := s.Unregister(jobKey); err != nil {
		t.Fatalf("Unregister: %v", err)
	}
	clock.Advance(48 * time.Hour)
	s.tick(context.Background())
	time.Sleep(20 * time.Millisecond)

	if got := atomic.LoadInt32(&runs); got != 1 {
		t.Fatalf("expected the unregistered job never to run again, got %d runs", got)
	}
	if err := s.RunNow(context.Background(), jobKey); err == nil {
		t.Fatal("expected RunNow on an unregistered job to fail")
	}
}

// A job that keeps failing must keep backing off. Past the alert threshold
// the backoff used to stop and the schedule take over, but last_run only
// moves on success, so a wedged job's next-due instant sits permanently in
// the past: "back to its normal cadence" meant every tick forever, whatever
// the schedule said. A weekly review that timed out once ran 534 more times
// in ten hours that way, each one a billed model call.
func TestWedgedJobKeepsBackingOff(t *testing.T) {
	start := time.Date(2026, 8, 30, 4, 0, 0, 0, time.UTC)
	sched := IntervalSchedule{Interval: 7 * 24 * time.Hour}
	st := JobState{
		// Seeded a week ago and never successful since: exactly the shape
		// the calibration job was in.
		LastRun:             start.Add(-7 * 24 * time.Hour),
		HasLastRun:          true,
		LastAttempt:         start,
		ConsecutiveFailures: maxConsecutiveFailures + 1,
	}

	if jobIsDue(st, sched, 0, start.Add(tickInterval)) {
		t.Error("a wedged job was due again one tick after failing")
	}
	if !jobIsDue(st, sched, 0, start.Add(backoffMax+time.Minute)) {
		t.Error("a wedged job never became due again; backoff must cap, not stop retrying")
	}
}
