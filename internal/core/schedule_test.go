package core

import (
	"testing"
	"time"
)

func TestIntervalScheduleNext(t *testing.T) {
	s := IntervalSchedule{Interval: 3 * time.Hour}
	after := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	want := after.Add(3 * time.Hour)
	if got := s.Next(after); !got.Equal(want) {
		t.Fatalf("Next() = %v, want %v", got, want)
	}
}

func TestIntervalScheduleValidate(t *testing.T) {
	tests := []struct {
		name    string
		s       IntervalSchedule
		wantErr bool
	}{
		{"positive", IntervalSchedule{Interval: time.Hour}, false},
		{"zero", IntervalSchedule{Interval: 0}, true},
		{"negative", IntervalSchedule{Interval: -time.Hour}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.s.Validate()
			if (err != nil) != tt.wantErr {
				t.Fatalf("Validate() = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestIntervalScheduleStringAndTypicalPeriod(t *testing.T) {
	s := IntervalSchedule{Interval: 72 * time.Hour}
	if got, want := s.String(), "3d"; got != want {
		t.Fatalf("String() = %q, want %q", got, want)
	}
	if got, want := s.TypicalPeriod(), 72*time.Hour; got != want {
		t.Fatalf("TypicalPeriod() = %v, want %v", got, want)
	}
}

func TestCalendarScheduleNextDailyLaterToday(t *testing.T) {
	s := CalendarSchedule{HourUTC: 17, MinuteUTC: 0}
	after := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	want := time.Date(2026, 1, 1, 17, 0, 0, 0, time.UTC)
	if got := s.Next(after); !got.Equal(want) {
		t.Fatalf("Next() = %v, want %v", got, want)
	}
}

func TestCalendarScheduleNextDailyAlreadyPassedTodayRollsToTomorrow(t *testing.T) {
	s := CalendarSchedule{HourUTC: 5, MinuteUTC: 0}
	after := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	want := time.Date(2026, 1, 2, 5, 0, 0, 0, time.UTC)
	if got := s.Next(after); !got.Equal(want) {
		t.Fatalf("Next() = %v, want %v", got, want)
	}
}

// TestCalendarScheduleNextExactBoundaryIsNotDue verifies Next is strictly
// after its input: a job whose anchor lands exactly on the scheduled
// instant must wait for the following occurrence, not refire immediately
// on the same tick (see the Schedule interface doc comment).
func TestCalendarScheduleNextExactBoundaryIsNotDue(t *testing.T) {
	s := CalendarSchedule{HourUTC: 0, MinuteUTC: 0}
	after := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	want := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	if got := s.Next(after); !got.Equal(want) {
		t.Fatalf("Next() at exact boundary = %v, want next day's occurrence %v", got, want)
	}
}

func TestCalendarScheduleNextWeeklySameDayLater(t *testing.T) {
	wed := time.Wednesday
	s := CalendarSchedule{Weekday: &wed, HourUTC: 0, MinuteUTC: 0}
	// 2026-01-07 is a Wednesday.
	after := time.Date(2026, 1, 7, 10, 0, 0, 0, time.UTC)
	want := time.Date(2026, 1, 14, 0, 0, 0, 0, time.UTC)
	if got := s.Next(after); !got.Equal(want) {
		t.Fatalf("Next() = %v, want %v", got, want)
	}
}

func TestCalendarScheduleNextWeeklyWraparound(t *testing.T) {
	wed := time.Wednesday
	s := CalendarSchedule{Weekday: &wed, HourUTC: 0, MinuteUTC: 0}
	// 2026-01-03 is a Saturday; the next Wednesday is 2026-01-07.
	after := time.Date(2026, 1, 3, 12, 0, 0, 0, time.UTC)
	want := time.Date(2026, 1, 7, 0, 0, 0, 0, time.UTC)
	if got := s.Next(after); !got.Equal(want) {
		t.Fatalf("Next() = %v, want %v", got, want)
	}
}

func TestCalendarScheduleStringDailyAndWeekly(t *testing.T) {
	daily := CalendarSchedule{HourUTC: 17, MinuteUTC: 5}
	if got, want := daily.String(), "daily at 17:05 UTC"; got != want {
		t.Fatalf("String() = %q, want %q", got, want)
	}

	wed := time.Wednesday
	weekly := CalendarSchedule{Weekday: &wed, HourUTC: 0, MinuteUTC: 0}
	if got, want := weekly.String(), "weekly on Wednesday at 00:00 UTC"; got != want {
		t.Fatalf("String() = %q, want %q", got, want)
	}
}

func TestCalendarScheduleTypicalPeriod(t *testing.T) {
	daily := CalendarSchedule{HourUTC: 0, MinuteUTC: 0}
	if got, want := daily.TypicalPeriod(), 24*time.Hour; got != want {
		t.Fatalf("daily TypicalPeriod() = %v, want %v", got, want)
	}

	wed := time.Wednesday
	weekly := CalendarSchedule{Weekday: &wed, HourUTC: 0, MinuteUTC: 0}
	if got, want := weekly.TypicalPeriod(), 7*24*time.Hour; got != want {
		t.Fatalf("weekly TypicalPeriod() = %v, want %v", got, want)
	}
}

func TestCalendarScheduleValidate(t *testing.T) {
	badWeekday := time.Weekday(9)
	goodWeekday := time.Friday

	tests := []struct {
		name    string
		s       CalendarSchedule
		wantErr bool
	}{
		{"valid daily", CalendarSchedule{HourUTC: 12, MinuteUTC: 30}, false},
		{"valid weekly", CalendarSchedule{Weekday: &goodWeekday, HourUTC: 0, MinuteUTC: 0}, false},
		{"hour too low", CalendarSchedule{HourUTC: -1, MinuteUTC: 0}, true},
		{"hour too high", CalendarSchedule{HourUTC: 24, MinuteUTC: 0}, true},
		{"minute too low", CalendarSchedule{HourUTC: 0, MinuteUTC: -1}, true},
		{"minute too high", CalendarSchedule{HourUTC: 0, MinuteUTC: 60}, true},
		{"weekday out of range", CalendarSchedule{Weekday: &badWeekday, HourUTC: 0, MinuteUTC: 0}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.s.Validate()
			if (err != nil) != tt.wantErr {
				t.Fatalf("Validate() = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

// Both concrete types must satisfy Schedule — compile-time check.
var (
	_ Schedule = IntervalSchedule{}
	_ Schedule = CalendarSchedule{}
)
