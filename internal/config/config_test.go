package config

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
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

// loadWith writes a minimal config file and loads it with the environment a
// deployed host would have, so these exercise reload() itself rather than
// re-implementing the line under test.
func loadWith(t *testing.T, env map[string]string) *GlobalConfig {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("log_level: info\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	t.Setenv("DISCORD_BOT_TOKEN", "test-token")
	t.Setenv("MERLIN_BOOTSTRAP_ADMIN_USER_ID", "1")
	for k, v := range env {
		t.Setenv(k, v)
	}
	l, err := NewLoader(path, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("NewLoader: %v", err)
	}
	cfg := l.Global()
	return &cfg
}

// The privileged GUILD_MEMBERS intent is now requested by default, and the
// default changed because the opt-in was itself the bug: enabling "Server
// Members Intent" in the Developer Portal (the only step that looks like it
// should matter) changed nothing, because the bot never asked for the
// intent. A jailed member who left and rejoined kept full access until the
// next sweep, and nothing anywhere reported the mismatch.
func TestGuildMembersIntentDefaultsOn(t *testing.T) {
	cases := []struct {
		name    string
		disable string
		want    bool
	}{
		{"unset", "", true},
		{"explicitly disabled", "1", false},
		{"disabled, spelled true", "true", false},
		{"disabled, spelled yes", "yes", false},
		// Anything unrecognized leaves the protection in place, matching how
		// the emergency stop can only ever be engaged deliberately.
		{"a typo does not disable it", "flase", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := loadWith(t, map[string]string{"MERLIN_DISABLE_GUILD_MEMBERS_INTENT": c.disable})
			if cfg.GuildMembersIntent != c.want {
				t.Errorf("GuildMembersIntent = %v, want %v", cfg.GuildMembersIntent, c.want)
			}
		})
	}
}

// The superseded variable must not quietly re-disable the intent for a host
// whose .env still carries it from before the default changed.
func TestSupersededEnableVariableNoLongerSuppressesTheIntent(t *testing.T) {
	for _, v := range []string{"", "0", "1"} {
		cfg := loadWith(t, map[string]string{"MERLIN_ENABLE_GUILD_MEMBERS_INTENT": v})
		if !cfg.GuildMembersIntent {
			t.Errorf("MERLIN_ENABLE_GUILD_MEMBERS_INTENT=%q turned the intent off", v)
		}
	}
}
