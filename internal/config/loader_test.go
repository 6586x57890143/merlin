package config

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func writeConfig(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

const validYAML = `
guilds:
  "g1":
    guild_id: "g1"
    mod_role_ids: ["r1"]
    audit_log_channel_id: "audit"
    status_channel_id: "status"
`

func TestNewLoaderRequiresToken(t *testing.T) {
	t.Setenv("DISCORD_BOT_TOKEN", "")
	path := writeConfig(t, validYAML)

	if _, err := NewLoader(path, testLogger()); err == nil {
		t.Fatal("expected error when DISCORD_BOT_TOKEN is unset")
	}
}

func TestNewLoaderValidationFailure(t *testing.T) {
	t.Setenv("DISCORD_BOT_TOKEN", "tok")
	// Missing required mod_role_ids.
	invalid := `
guilds:
  "g1":
    guild_id: "g1"
    audit_log_channel_id: "audit"
    status_channel_id: "status"
`
	path := writeConfig(t, invalid)

	if _, err := NewLoader(path, testLogger()); err == nil {
		t.Fatal("expected validation error for missing mod_role_ids")
	}
}

func TestLoaderGuildAndSecretsFromEnv(t *testing.T) {
	t.Setenv("DISCORD_BOT_TOKEN", "tok")
	t.Setenv("DISCORD_APP_ID", "app123")
	t.Setenv("DATABASE_URL", "postgres://x")
	path := writeConfig(t, validYAML)

	l, err := NewLoader(path, testLogger())
	if err != nil {
		t.Fatalf("NewLoader: %v", err)
	}

	gc, err := l.Guild("g1")
	if err != nil {
		t.Fatalf("Guild: %v", err)
	}
	if gc.AuditLogChannelID != "audit" {
		t.Fatalf("expected audit channel id 'audit', got %q", gc.AuditLogChannelID)
	}

	global := l.Global()
	if global.Discord.Token != "tok" || global.Discord.AppID != "app123" {
		t.Fatalf("expected secrets sourced from env, got %+v", global.Discord)
	}
	if global.Database.DSN != "postgres://x" {
		t.Fatalf("expected DSN from env, got %q", global.Database.DSN)
	}
}

func TestLoaderUnknownGuild(t *testing.T) {
	t.Setenv("DISCORD_BOT_TOKEN", "tok")
	path := writeConfig(t, validYAML)

	l, err := NewLoader(path, testLogger())
	if err != nil {
		t.Fatalf("NewLoader: %v", err)
	}
	if _, err := l.Guild("does-not-exist"); err == nil {
		t.Fatal("expected error for unconfigured guild")
	}
}
