package core

import (
	"testing"
	"time"
)

func TestParseFlexibleDurationDays(t *testing.T) {
	d, err := ParseFlexibleDuration("3d")
	if err != nil {
		t.Fatalf("ParseFlexibleDuration: %v", err)
	}
	if d != 72*time.Hour {
		t.Fatalf("expected 72h for \"3d\", got %v", d)
	}
}

func TestParseFlexibleDurationHours(t *testing.T) {
	d, err := ParseFlexibleDuration("72h")
	if err != nil {
		t.Fatalf("ParseFlexibleDuration: %v", err)
	}
	if d != 72*time.Hour {
		t.Fatalf("expected 72h, got %v", d)
	}
}

func TestParseFlexibleDurationBareNumberDefaultsToHours(t *testing.T) {
	d, err := ParseFlexibleDuration("24")
	if err != nil {
		t.Fatalf("ParseFlexibleDuration: %v", err)
	}
	if d != 24*time.Hour {
		t.Fatalf("expected a bare number to be interpreted as hours, got %v", d)
	}
}

func TestParseFlexibleDurationCaseInsensitiveAndTrimmed(t *testing.T) {
	d, err := ParseFlexibleDuration("  2D  ")
	if err != nil {
		t.Fatalf("ParseFlexibleDuration: %v", err)
	}
	if d != 48*time.Hour {
		t.Fatalf("expected 48h, got %v", d)
	}
}

// Minutes parse. This assertion used to be the opposite, and that was the
// bug: migration 0016 made rotation intervals minute-precise, the command
// descriptions started advertising "90m", and this parser still rejected
// it, so the single input the change existed to support failed with
// "isn't a valid duration". The test was passing the whole time, because it
// was asserting the old contract.
func TestParseFlexibleDurationMinutes(t *testing.T) {
	for in, want := range map[string]time.Duration{
		"90m":  90 * time.Minute,
		"1m":   time.Minute,
		"600m": 600 * time.Minute,
		" 45M": 45 * time.Minute,
	} {
		got, err := ParseFlexibleDuration(in)
		if err != nil {
			t.Errorf("ParseFlexibleDuration(%q): %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("ParseFlexibleDuration(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestParseFlexibleDurationRejectsInvalid(t *testing.T) {
	cases := []string{"", "abc", "0h", "0d", "0m", "-3d", "-90m", "d", "h", "m", "1.5h", "90 m"}
	for _, c := range cases {
		if _, err := ParseFlexibleDuration(c); err == nil {
			t.Errorf("expected %q to be rejected", c)
		}
	}
}

// Whether a *setting* may be sub-hour is the caller's business: rotation
// enforces its own one-hour floor in validateRotationChannel. Parsing is
// separate from policy, and conflating the two is what left the two halves
// of the minute-precision change disagreeing.
func TestFormatDurationUsesMinutesWhenHoursWouldLie(t *testing.T) {
	for d, want := range map[time.Duration]string{
		90 * time.Minute:  "90m",
		30 * time.Minute:  "30m",
		150 * time.Minute: "150m",
		time.Hour:         "1h",
		18 * time.Hour:    "18h",
		72 * time.Hour:    "3d",
	} {
		if got := FormatDuration(d); got != want {
			t.Errorf("FormatDuration(%v) = %q, want %q", d, got, want)
		}
	}
}

func TestFormatDurationWholeDays(t *testing.T) {
	if got := FormatDuration(72 * time.Hour); got != "3d" {
		t.Fatalf("expected \"3d\", got %q", got)
	}
}

func TestFormatDurationNonWholeDays(t *testing.T) {
	if got := FormatDuration(18 * time.Hour); got != "18h" {
		t.Fatalf("expected \"18h\", got %q", got)
	}
}

func TestFormatDurationRoundTripsWithParse(t *testing.T) {
	for _, in := range []string{"1h", "24h", "3d", "72h", "10d", "90m", "45m"} {
		d, err := ParseFlexibleDuration(in)
		if err != nil {
			t.Fatalf("ParseFlexibleDuration(%q): %v", in, err)
		}
		out := FormatDuration(d)
		d2, err := ParseFlexibleDuration(out)
		if err != nil {
			t.Fatalf("ParseFlexibleDuration(FormatDuration(%q)=%q): %v", in, out, err)
		}
		if d != d2 {
			t.Fatalf("round-trip mismatch for %q: %v != %v (via %q)", in, d, d2, out)
		}
	}
}
