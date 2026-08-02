package scheduler

import (
	"context"
	"testing"
	"time"
)

// When the bot is removed from a guild, that guild's jobs must stop being
// scheduled. Left registered they fail every REST call forever, climb toward
// the consecutive-failure alert, and then try to deliver that alert to a
// status channel in the guild the bot can no longer reach.
func TestUnregisterGuildRemovesOnlyThatGuildsJobs(t *testing.T) {
	s := New(newFakeStore(), fakeSettings{}, testLogger())
	noop := func(ctx context.Context) error { return nil }
	spec := CronSpec{Schedule: IntervalSchedule{Interval: time.Hour}}

	for _, key := range []string{
		JobKey("gone", "rotation:1"),
		JobKey("gone", "rotation:2"),
		JobKey("gone", "rotation-sweep"),
		JobKey("staying", "rotation:1"),
		JobKey("staying", "roles-sweep"),
	} {
		if err := s.Register(key, spec, noop); err != nil {
			t.Fatalf("Register(%s): %v", key, err)
		}
	}

	if dropped := s.UnregisterGuild("gone"); dropped != 3 {
		t.Errorf("dropped %d jobs, want 3", dropped)
	}

	if got := len(s.jobsForGuild("gone")); got != 0 {
		t.Errorf("%d job(s) still registered for the departed guild, want 0", got)
	}
	if got := len(s.jobsForGuild("staying")); got != 2 {
		t.Errorf("%d job(s) left for the untouched guild, want 2", got)
	}
}

// JobHealth is what /config status reads to answer "is anything wedged?"
// without shell access, so it has to count a job with any consecutive
// failures as failing, and count an unreadable job as failing too, since a
// health check that can't see a job must not call it healthy.
func TestJobHealthCountsFailingAndUnreadableJobs(t *testing.T) {
	store := newFakeStore()
	s := New(store, fakeSettings{}, testLogger())
	noop := func(ctx context.Context) error { return nil }
	spec := CronSpec{Schedule: IntervalSchedule{Interval: time.Hour}}

	for _, name := range []string{"rotation:1", "rotation:2", "rotation-sweep"} {
		if err := s.Register(JobKey("g1", name), spec, noop); err != nil {
			t.Fatalf("Register(%s): %v", name, err)
		}
	}

	total, failing, err := s.JobHealth(context.Background(), "g1")
	if err != nil {
		t.Fatalf("JobHealth: %v", err)
	}
	if total != 3 || failing != 0 {
		t.Fatalf("healthy guild: total=%d failing=%d, want 3 and 0", total, failing)
	}

	// One job has started failing.
	key := JobKey("g1", "rotation:1")
	if _, err := store.RecordFailure(context.Background(), key, time.Now().UTC()); err != nil {
		t.Fatalf("RecordFailure: %v", err)
	}

	total, failing, err = s.JobHealth(context.Background(), "g1")
	if err != nil {
		t.Fatalf("JobHealth: %v", err)
	}
	if total != 3 || failing != 1 {
		t.Errorf("total=%d failing=%d, want 3 and 1", total, failing)
	}
}

// A guild ID that is a prefix of another must not take the other's jobs with
// it: the key separator is the only thing keeping "123" from matching
// "1234:rotation:1".
func TestUnregisterGuildDoesNotMatchPrefixGuilds(t *testing.T) {
	s := New(newFakeStore(), fakeSettings{}, testLogger())
	noop := func(ctx context.Context) error { return nil }
	spec := CronSpec{Schedule: IntervalSchedule{Interval: time.Hour}}

	if err := s.Register(JobKey("123", "rotation-sweep"), spec, noop); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := s.Register(JobKey("1234", "rotation-sweep"), spec, noop); err != nil {
		t.Fatalf("Register: %v", err)
	}

	if dropped := s.UnregisterGuild("123"); dropped != 1 {
		t.Errorf("dropped %d, want 1", dropped)
	}
	if got := len(s.jobsForGuild("1234")); got != 1 {
		t.Errorf("guild 1234 lost its job to guild 123's teardown")
	}
}
