package scheduler

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// TestStartedSchedulerFiresAtTheDueInstantWithoutATick: once Start has run,
// a job fires from its own timer at nextDue, not from the thirty second
// backstop poll. The interval here is far shorter than tickInterval, so a
// second run arriving quickly can only have come from the timer.
func TestStartedSchedulerFiresAtTheDueInstantWithoutATick(t *testing.T) {
	s := New(newFakeStore(), fakeSettings{}, testLogger())
	const interval = 150 * time.Millisecond
	stamps := make(chan time.Time, 8)
	if err := s.Register(JobKey("g1", "fast"), CronSpec{Schedule: IntervalSchedule{Interval: interval}}, func(ctx context.Context) error {
		stamps <- time.Now()
		return nil
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = s.Shutdown(context.Background()) }()

	next := func() time.Time {
		select {
		case at := <-stamps:
			return at
		case <-time.After(2 * time.Second):
			t.Fatal("job did not fire; only the tick would have, thirty seconds from now")
			return time.Time{}
		}
	}
	first, second := next(), next()
	if gap := second.Sub(first); gap < interval {
		t.Fatalf("second run fired %v after the first, before the %v interval", gap, interval)
	}
}

// TestShutdownAndUnregisterStopTheTimer: a stopped scheduler and a removed
// job must not fire again from a timer armed earlier.
func TestShutdownAndUnregisterStopTheTimer(t *testing.T) {
	s := New(newFakeStore(), fakeSettings{}, testLogger())
	var kept, removed int32
	const interval = 50 * time.Millisecond
	_ = s.Register(JobKey("g1", "kept"), CronSpec{Schedule: IntervalSchedule{Interval: interval}}, func(context.Context) error {
		atomic.AddInt32(&kept, 1)
		return nil
	})
	_ = s.Register(JobKey("g1", "removed"), CronSpec{Schedule: IntervalSchedule{Interval: interval}}, func(context.Context) error {
		atomic.AddInt32(&removed, 1)
		return nil
	})
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitForCount(t, &kept, 1)
	waitForCount(t, &removed, 1)

	_ = s.Unregister(JobKey("g1", "removed"))
	waitForCount(t, &kept, 3)
	if got := atomic.LoadInt32(&removed); got != 1 {
		t.Fatalf("unregistered job kept firing from its timer: %d runs", got)
	}

	if err := s.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	after := atomic.LoadInt32(&kept)
	time.Sleep(5 * interval)
	if got := atomic.LoadInt32(&kept); got != after {
		t.Fatalf("job fired after Shutdown: %d -> %d", after, got)
	}
}
