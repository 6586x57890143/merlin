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

func TestParseFlexibleDurationRejectsInvalid(t *testing.T) {
	cases := []string{"", "abc", "0h", "0d", "-3d", "90m", "d", "h"}
	for _, c := range cases {
		if _, err := ParseFlexibleDuration(c); err == nil {
			t.Errorf("expected %q to be rejected", c)
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
	for _, in := range []string{"1h", "24h", "3d", "72h", "10d"} {
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
