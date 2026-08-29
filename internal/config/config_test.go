package config

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"
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

// LogLevel used to be read exactly once, during startup, so SIGHUP parsed a
// new value into a struct nothing consulted again and raising verbosity to
// look at a live problem still meant a restart. OnReload is what lets a
// value keep tracking the file, so it has to fire on a real reload and hand
// over the new config, not the one captured at boot.
func TestOnReloadFiresWithTheNewConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("log_level: info\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	t.Setenv("DISCORD_BOT_TOKEN", "test-token")
	t.Setenv("MERLIN_BOOTSTRAP_ADMIN_USER_ID", "1")
	t.Setenv("LOG_LEVEL", "")

	l, err := NewLoader(path, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("NewLoader: %v", err)
	}

	var seen []string
	l.OnReload(func(c *GlobalConfig) { seen = append(seen, c.LogLevel) })

	// A hook registered after construction must not fire retroactively for
	// the load that already happened.
	if len(seen) != 0 {
		t.Fatalf("hook fired on registration: %v", seen)
	}

	if err := os.WriteFile(path, []byte("log_level: debug\n"), 0o600); err != nil {
		t.Fatalf("rewrite config: %v", err)
	}
	if err := l.reload(); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if len(seen) != 1 || seen[0] != "debug" {
		t.Fatalf("hook saw %v, want exactly [debug]", seen)
	}
	if got := l.Global().LogLevel; got != "debug" {
		t.Errorf("Global() still reports %q after reload", got)
	}
}

// A hook must never see a config that failed validation: the whole point of
// keeping the previous config on a bad edit is that nothing downstream acts
// on the broken one.
func TestOnReloadDoesNotFireOnAFailedReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("log_level: info\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	t.Setenv("DISCORD_BOT_TOKEN", "test-token")
	t.Setenv("MERLIN_BOOTSTRAP_ADMIN_USER_ID", "1")
	t.Setenv("LOG_LEVEL", "")

	l, err := NewLoader(path, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("NewLoader: %v", err)
	}
	fired := 0
	l.OnReload(func(*GlobalConfig) { fired++ })

	if err := os.WriteFile(path, []byte("log_level: not-a-level\n"), 0o600); err != nil {
		t.Fatalf("rewrite config: %v", err)
	}
	if err := l.reload(); err == nil {
		t.Fatal("an invalid log level was accepted")
	}
	if fired != 0 {
		t.Errorf("hook ran %d times for a reload that failed validation", fired)
	}
	if got := l.Global().LogLevel; got != "info" {
		t.Errorf("a failed reload changed the live config to %q", got)
	}
}

// A hook that reads the config it was handed is the obvious thing to write,
// and would deadlock if hooks ran while reload still held the write lock.
func TestOnReloadHookCanReadTheLoader(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("log_level: info\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	t.Setenv("DISCORD_BOT_TOKEN", "test-token")
	t.Setenv("MERLIN_BOOTSTRAP_ADMIN_USER_ID", "1")
	t.Setenv("LOG_LEVEL", "")

	l, err := NewLoader(path, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("NewLoader: %v", err)
	}
	l.OnReload(func(*GlobalConfig) { _ = l.Global() })

	done := make(chan error, 1)
	go func() { done <- l.reload() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("reload: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("reload deadlocked: hooks are running while the write lock is held")
	}
}

// The onboarding DM is off unless an operator asks for it, which is the
// opposite default from the GUILD_MEMBERS intent above and deliberately so:
// this is a message merlin originates unprompted to a person who has not
// interacted with her, not a capability whose absence breaks a feature.
func TestOnboardingDMIsOffUnlessAskedFor(t *testing.T) {
	if cfg := loadWith(t, nil); cfg.OnboardingDM {
		t.Error("the onboarding DM is on by default; a single-server deploy would DM the owner unasked")
	}

	cases := map[string]bool{
		"1": true, "true": true, "yes": true, "on": true,
		"": false, "0": false, "false": false,
		// A typo leaves it off, matching the direction every other
		// unrecognized value in this loader falls.
		"ture": false,
	}
	for value, want := range cases {
		cfg := loadWith(t, map[string]string{"MERLIN_ENABLE_ONBOARDING_DM": value})
		if cfg.OnboardingDM != want {
			t.Errorf("MERLIN_ENABLE_ONBOARDING_DM=%q gave OnboardingDM=%v, want %v", value, cfg.OnboardingDM, want)
		}
	}
}
