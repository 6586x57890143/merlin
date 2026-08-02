package core

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// ParseFlexibleDuration parses a whole number of hours, optionally suffixed
// with "d" (days) or "h" (hours), e.g. "3d", "72h", or a bare "72" (hours,
// for backward compatibility with this bot's old hours-only inputs). This
// is the only duration shape used anywhere a mod configures a schedule or
// retention window (spec.MD §6): sub-hour granularity isn't supported,
// since nothing in this bot's scheduling actually resolves finer than an
// hour, so a value like "90m" is rejected as a likely typo rather than
// silently rounded.
func ParseFlexibleDuration(s string) (time.Duration, error) {
	s = strings.TrimSpace(strings.ToLower(s))
	if s == "" {
		return 0, fmt.Errorf("duration must not be empty: use a whole number of hours or days, e.g. \"72h\" or \"3d\"")
	}

	unit := "h"
	numPart := s
	if last := s[len(s)-1]; last == 'd' || last == 'h' {
		unit = string(last)
		numPart = s[:len(s)-1]
	}

	n, err := strconv.Atoi(numPart)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("%q isn't a valid duration: use a positive whole number, optionally followed by d (days) or h (hours), e.g. \"3d\" or \"72h\"", s)
	}

	if unit == "d" {
		return time.Duration(n) * 24 * time.Hour, nil
	}
	return time.Duration(n) * time.Hour, nil
}

// FormatDuration renders d back in the same shape ParseFlexibleDuration
// accepts, picking whichever unit reads cleanest: whole days if d divides
// evenly into 24-hour chunks, otherwise whole hours. d is assumed to
// already be a whole number of hours (every caller in this bot only ever
// stores interval/retention that way), so any sub-hour remainder is dropped
// rather than rendered, since it should never occur.
func FormatDuration(d time.Duration) string {
	hours := int(d / time.Hour)
	if hours > 0 && hours%24 == 0 {
		return fmt.Sprintf("%dd", hours/24)
	}
	return fmt.Sprintf("%dh", hours)
}
