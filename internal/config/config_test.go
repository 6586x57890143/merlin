package config

import (
	"log/slog"
	"testing"
)

func TestGlobalConfigLevel(t *testing.T) {
	cases := map[string]slog.Level{
		"debug": slog.LevelDebug,
		"info":  slog.LevelInfo,
		"warn":  slog.LevelWarn,
		"error": slog.LevelError,
		"":      slog.LevelInfo,
	}
	for in, want := range cases {
		if got := (GlobalConfig{LogLevel: in}).Level(); got != want {
			t.Errorf("LogLevel %q: got %v, want %v", in, got, want)
		}
	}
}

func TestIsTruthy(t *testing.T) {
	for _, in := range []string{"1", "true", "TRUE", "yes", "on", " true "} {
		if !isTruthy(in) {
			t.Errorf("isTruthy(%q) = false, want true", in)
		}
	}
	// Anything unrecognized must leave the emergency stop disengaged, so a
	// typo can never silently pause every destructive action.
	for _, in := range []string{"", "0", "false", "no", "off", "ture", "maybe"} {
		if isTruthy(in) {
			t.Errorf("isTruthy(%q) = true, want false", in)
		}
	}
}
