package core

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// ParseFlexibleDuration parses a whole number followed by an explicit unit:
// "3d"/"3 days", "72h"/"72 hours", "90m"/"90 minutes". This is the only
// duration shape used anywhere a mod configures a schedule, retention
// window, or notice lead.
//
// The unit is REQUIRED, and that is the entire point of this function
// existing rather than a call to time.ParseDuration. A bare number used to
// be accepted as hours. That reads as a harmless convenience and is anything
// but: a retention window entered as "3" meaning three days became three
// hours, and the first visible sign was archived channels being permanently
// deleted a day early. Nothing recoverable, no warning, and the value looked
// correct in every list that echoed it back. Since intervals became
// minute-precise the same ambiguity got worse rather than better: a notice
// lead of "10" is far more likely to mean ten minutes than ten hours.
//
// "3" is genuinely ambiguous to a human, so this asks instead of guessing.
// Refusing is the safe failure here because every consumer of the value
// either destroys content or restricts a member when it elapses, so there is
// no reading that is harmless to get wrong. The error names the readings
// back rather than only rejecting.
//
// Requiring a unit should not make the input fussy, so spelled-out and
// spaced forms parse too: 3d, 3 d, 3day, 3 days, 72h, 72 hr, 72hrs,
// 72 hours, 90m, 90 min, 90 minutes, case-insensitive. Whether a given
// setting *allows* sub-hour values is the caller's business (rotation
// enforces a one hour floor in validateRotationChannel); parsing them is
// this function's.
func ParseFlexibleDuration(s string) (time.Duration, error) {
	s = strings.TrimSpace(strings.ToLower(s))
	if s == "" {
		return 0, fmt.Errorf("duration must not be empty: give a number and a unit, e.g. %q, %q or %q", "3d", "72h", "90m")
	}

	numPart, unitPart := splitNumberAndUnit(s)
	if unitPart == "" {
		return 0, fmt.Errorf("%q doesn't say what unit you mean: write %q for days, %q for hours or %q for minutes",
			s, numPart+"d", numPart+"h", numPart+"m")
	}

	var per time.Duration
	switch unitPart {
	case "d", "day", "days":
		per = 24 * time.Hour
	case "h", "hr", "hrs", "hour", "hours":
		per = time.Hour
	case "m", "min", "mins", "minute", "minutes":
		per = time.Minute
	default:
		return 0, fmt.Errorf("%q isn't a unit this bot understands: use d (days), h (hours) or m (minutes), e.g. %q, %q or %q",
			unitPart, "3d", "72h", "90m")
	}

	n, err := strconv.Atoi(numPart)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("%q isn't a valid duration: use a positive whole number followed by d (days), h (hours) or m (minutes), e.g. %q, %q or %q",
			s, "3d", "72h", "90m")
	}
	return time.Duration(n) * per, nil
}

// splitNumberAndUnit divides s into its leading digits and the rest, with any
// separating whitespace dropped ("3 days" -> "3", "days"). Either half may
// come back empty; the caller decides what that means, so a bare number and
// an unrecognized unit can be reported differently.
func splitNumberAndUnit(s string) (numPart, unitPart string) {
	i := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	return s[:i], strings.TrimSpace(s[i:])
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
