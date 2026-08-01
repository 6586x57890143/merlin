package scheduler

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"
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
	s := New(store, testLogger())
	s.now = clock.Now

	jobKey := JobKey("g1", "test")
	interval := time.Hour
	var runs int32
	if err := s.Register(jobKey, CronSpec{Interval: interval}, func(ctx context.Context) error {
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

	s1 := New(store, testLogger())
	s1.now = clock.Now
	if err := s1.Register(jobKey, CronSpec{Interval: time.Hour}, func(ctx context.Context) error { return nil }); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := s1.RunNow(context.Background(), jobKey); err != nil {
		t.Fatalf("RunNow: %v", err)
	}

	// Simulate a process restart: a fresh Scheduler instance sharing the
	// same backing store must not treat the job as never-attempted.
	s2 := New(store, testLogger())
	s2.now = clock.Now
	if err := s2.Register(jobKey, CronSpec{Interval: time.Hour}, func(ctx context.Context) error { return nil }); err != nil {
		t.Fatalf("Register: %v", err)
	}

	due, err := s2.isDue(context.Background(), s2.jobs[jobKey])
	if err != nil {
		t.Fatalf("isDue: %v", err)
	}
	if due {
		t.Fatal("expected job not due immediately after restart — last_run should have survived")
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
	s := New(store, testLogger())
	s.now = clock.Now

	var alerts []string
	s.alertFunc = func(ctx context.Context, jobKey, msg string) error {
		alerts = append(alerts, msg)
		return nil
	}

	jobKey := JobKey("g1", "failing")
	boom := errors.New("boom")
	if err := s.Register(jobKey, CronSpec{Interval: time.Hour}, func(ctx context.Context) error { return boom }); err != nil {
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
	s := New(store, testLogger())

	jobKey := JobKey("g1", "slow")
	started := make(chan struct{})
	var startedOnce sync.Once
	release := make(chan struct{})
	var concurrent int32

	if err := s.Register(jobKey, CronSpec{Interval: time.Hour}, func(ctx context.Context) error {
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
	s := New(store, testLogger())
	s.now = clock.Now

	jobKey := JobKey("g1", "manual")
	var runs int32
	if err := s.Register(jobKey, CronSpec{Interval: 24 * time.Hour}, func(ctx context.Context) error {
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
		{"empty key", "", CronSpec{Interval: time.Hour}, noop, true},
		{"zero interval", "g1:job", CronSpec{}, noop, true},
		{"negative interval", "g1:job", CronSpec{Interval: -time.Hour}, noop, true},
		{"nil fn", "g1:job", CronSpec{Interval: time.Hour}, nil, true},
		{"valid", "g1:job", CronSpec{Interval: time.Hour}, noop, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := New(newFakeStore(), testLogger())
			err := s.Register(tt.jobKey, tt.spec, tt.fn)
			if (err != nil) != tt.wantErr {
				t.Fatalf("Register(%q): err=%v, wantErr=%v", tt.jobKey, err, tt.wantErr)
			}
		})
	}
}

func TestRegisterDuplicateJobKeyFails(t *testing.T) {
	s := New(newFakeStore(), testLogger())
	noop := func(ctx context.Context) error { return nil }
	if err := s.Register("g1:job", CronSpec{Interval: time.Hour}, noop); err != nil {
		t.Fatalf("first Register: %v", err)
	}
	if err := s.Register("g1:job", CronSpec{Interval: time.Hour}, noop); err == nil {
		t.Fatal("expected duplicate registration to fail")
	}
}

func TestRunNowUnknownJobFails(t *testing.T) {
	s := New(newFakeStore(), testLogger())
	if err := s.RunNow(context.Background(), "g1:nope"); err == nil {
		t.Fatal("expected RunNow on an unregistered job to fail")
	}
}
