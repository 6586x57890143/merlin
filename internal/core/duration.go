package core

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// ParseFlexibleDuration parses a whole number optionally suffixed with "d"
// (days), "h" (hours) or "m" (minutes), e.g. "3d", "72h", "90m", or a bare
// "72" (hours, for backward compatibility with this bot's old hours-only
// inputs). This is the only duration shape used anywhere a mod configures a
// schedule, retention window, or notice lead.
//
// Minutes are accepted because rotation intervals are stored and enforced
// to the minute (migration 0016). They were not, for a while after that
// change: the option descriptions offered "90m", the storage column held
// minutes, the floor was checked in minutes, and this parser still rejected
// anything with an "m" on it, so the one input the feature existed to
// support failed with "isn't a valid duration". Whether a given setting
// *allows* sub-hour values is the caller's business (rotation enforces a
// one hour floor in validateRotationChannel); parsing them is this
// function's.
func ParseFlexibleDuration(s string) (time.Duration, error) {
	s = strings.TrimSpace(strings.ToLower(s))
	if s == "" {
		return 0, fmt.Errorf("duration must not be empty: use a whole number of minutes, hours or days, e.g. \"90m\", \"72h\" or \"3d\"")
	}

	unit := "h"
	numPart := s
	if last := s[len(s)-1]; last == 'd' || last == 'h' || last == 'm' {
		unit = string(last)
		numPart = s[:len(s)-1]
	}

	n, err := strconv.Atoi(numPart)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("%q isn't a valid duration: use a positive whole number, optionally followed by d (days), h (hours) or m (minutes), e.g. \"3d\", \"72h\" or \"90m\"", s)
	}

	switch unit {
	case "d":
		return time.Duration(n) * 24 * time.Hour, nil
	case "m":
		return time.Duration(n) * time.Minute, nil
	default:
		return time.Duration(n) * time.Hour, nil
	}
}

// FormatDuration renders d back in the same shape ParseFlexibleDuration
// accepts, picking whichever unit reads cleanest: whole days if d divides
// evenly into 24-hour chunks, whole hours if it divides evenly into hours,
// otherwise minutes.
//
// The minute case is not decoration. Since intervals became minute-precise,
// a 90-minute rotation rendered through an hours-only formatter reads as
// "1h", which is wrong in an audit record and wrong in a confirmation
// message, and wrong in the direction that makes somebody think their
// configuration did not take.
func FormatDuration(d time.Duration) string {
	switch {
	case d >= 24*time.Hour && d%(24*time.Hour) == 0:
		return fmt.Sprintf("%dd", int(d/(24*time.Hour)))
	case d >= time.Hour && d%time.Hour == 0:
		return fmt.Sprintf("%dh", int(d/time.Hour))
	default:
		return fmt.Sprintf("%dm", int(d/time.Minute))
	}
}
