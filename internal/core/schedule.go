package core

import (
	"fmt"
	"time"
)

// Schedule computes a recurring job's next due instant, strictly after a
// given anchor (typically its last successful run) — always an absolute,
// fixed-clock instant, never a countdown recomputed from "time remaining."
// This is what lets internal/scheduler support both flat intervals ("every
// 3 days") and calendar-anchored recurrences ("daily at 17:00 UTC", "weekly
// on Wednesday at 00:00 UTC") under one model: the Scheduler itself never
// needs a type switch to know which kind of rule produced the next
// instant, only that Next/String/Validate all exist.
type Schedule interface {
	// Next returns the next instant this schedule is due, strictly after
	// after (never equal to it, so a job whose anchor lands exactly on a
	// scheduled instant waits for the following occurrence, not an
	// immediate refire on the same tick).
	Next(after time.Time) time.Time
	// String renders the schedule for user-facing display (/rotation list,
	// audit messages) — every Schedule is self-describing so display code
	// never needs a type switch either.
	String() string
	// Validate reports whether the schedule's own parameters make sense
	// (e.g. a positive interval, an hour in 0-23) — checked once at
	// Scheduler.Register time, not on every due-check.
	Validate() error
	// TypicalPeriod is a representative recurrence period used only to
	// scale down the Scheduler's anti-thundering-herd jitter for schedules
	// shorter than it (see jitterFor in internal/scheduler) — never used
	// for the actual due-check.
	TypicalPeriod() time.Duration
}

// IntervalSchedule recurs a fixed duration after the previous occurrence —
// the schedule this bot has always supported ("every 24h", "every 3 days").
type IntervalSchedule struct {
	Interval time.Duration
}

func (s IntervalSchedule) Next(after time.Time) time.Time { return after.Add(s.Interval) }
func (s IntervalSchedule) String() string                 { return FormatDuration(s.Interval) }
func (s IntervalSchedule) TypicalPeriod() time.Duration   { return s.Interval }

func (s IntervalSchedule) Validate() error {
	if s.Interval <= 0 {
		return fmt.Errorf("interval must be positive, got %v", s.Interval)
	}
	return nil
}

// CalendarSchedule recurs at a fixed time of day, in UTC — either every day
// (Weekday == nil) or once a week on a specific day (Weekday set). Unlike
// IntervalSchedule, occurrences are anchored to wall-clock time rather than
// "N units since the last run," so a late or missed tick never drifts
// future occurrences off the intended time of day.
type CalendarSchedule struct {
	Weekday   *time.Weekday // nil = daily; set = weekly on this day
	HourUTC   int           // 0-23
	MinuteUTC int           // 0-59
}

// Next returns the next instant, strictly after after, at HourUTC:MinuteUTC
// UTC on the target day (every day, or the target weekday). Walks forward
// one day at a time rather than computing a closed-form offset: this runs
// at most once per job registration, never in a hot loop, so obviously
// correct beats clever here — worst case is 8 iterations (7 to land on the
// right weekday, plus 1 for the strictly-after check).
func (s CalendarSchedule) Next(after time.Time) time.Time {
	after = after.UTC()
	candidate := time.Date(after.Year(), after.Month(), after.Day(), s.HourUTC, s.MinuteUTC, 0, 0, time.UTC)
	for !candidate.After(after) || (s.Weekday != nil && candidate.Weekday() != *s.Weekday) {
		candidate = candidate.AddDate(0, 0, 1)
	}
	return candidate
}

func (s CalendarSchedule) String() string {
	t := fmt.Sprintf("%02d:%02d", s.HourUTC, s.MinuteUTC)
	if s.Weekday == nil {
		return fmt.Sprintf("daily at %s UTC", t)
	}
	return fmt.Sprintf("weekly on %s at %s UTC", *s.Weekday, t)
}

func (s CalendarSchedule) TypicalPeriod() time.Duration {
	if s.Weekday == nil {
		return 24 * time.Hour
	}
	return 7 * 24 * time.Hour
}

func (s CalendarSchedule) Validate() error {
	if s.HourUTC < 0 || s.HourUTC > 23 {
		return fmt.Errorf("hour must be 0-23, got %d", s.HourUTC)
	}
	if s.MinuteUTC < 0 || s.MinuteUTC > 59 {
		return fmt.Errorf("minute must be 0-59, got %d", s.MinuteUTC)
	}
	if s.Weekday != nil && (*s.Weekday < time.Sunday || *s.Weekday > time.Saturday) {
		return fmt.Errorf("weekday out of range: %d", *s.Weekday)
	}
	return nil
}
