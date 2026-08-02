package config

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"

	"github.com/go-playground/validator/v10"
	"gopkg.in/yaml.v3"
)

// Loader owns the current GlobalConfig and knows how to (re)load it from a
// YAML file plus environment variables. Reads are safe for concurrent use
// via RLock; Watch (platform-specific) triggers reload without downtime.
type Loader struct {
	mu   sync.RWMutex
	path string
	cur  *GlobalConfig
	log  *slog.Logger
}

func NewLoader(path string, log *slog.Logger) (*Loader, error) {
	l := &Loader{path: path, log: log}
	if err := l.reload(); err != nil {
		return nil, err
	}
	return l, nil
}

func (l *Loader) reload() error {
	raw, err := os.ReadFile(l.path)
	if err != nil {
		return fmt.Errorf("read config: %w", err)
	}
	var next GlobalConfig
	if err := yaml.Unmarshal(raw, &next); err != nil {
		return fmt.Errorf("parse config: %w", err)
	}
	if err := validator.New().Struct(&next); err != nil {
		return fmt.Errorf("validate config: %w", err)
	}

	// Secrets/identifiers are env-only and re-applied on every reload — never
	// read from YAML, so a hot-reloaded config file can never leak or
	// override them.
	next.Discord.Token = os.Getenv("DISCORD_BOT_TOKEN")
	next.Discord.AppID = os.Getenv("DISCORD_APP_ID")
	next.Database.DSN = os.Getenv("DATABASE_URL")
	next.BootstrapAdminUserID = os.Getenv("MERLIN_BOOTSTRAP_ADMIN_USER_ID")
	next.PauseAllWrites = isTruthy(os.Getenv("MERLIN_PAUSE_ALL_WRITES"))
	// Opt-out rather than opt-in: see GlobalConfig.GuildMembersIntent. The
	// superseded MERLIN_ENABLE_GUILD_MEMBERS_INTENT is deliberately not read
	// any more — an existing .env that sets it now gets the behavior it was
	// asking for regardless, and one that doesn't gets it too, which was the
	// entire point of changing the default.
	next.GuildMembersIntent = !isTruthy(os.Getenv("MERLIN_DISABLE_GUILD_MEMBERS_INTENT"))
	// LOG_LEVEL overrides the YAML value when set. On a deployed host .env is
	// already the file an operator edits; config.yaml is a read-only mount,
	// so requiring a file change to raise verbosity mid-incident would be the
	// harder path for no benefit. Validated after the override so a typo in
	// either source fails startup rather than silently reverting to Info.
	if v := os.Getenv("LOG_LEVEL"); v != "" {
		next.LogLevel = v
	}
	if err := validator.New().Struct(&next); err != nil {
		return fmt.Errorf("validate config: %w", err)
	}
	if next.Discord.Token == "" {
		return errors.New("DISCORD_BOT_TOKEN not set")
	}
	if next.BootstrapAdminUserID == "" {
		return errors.New("MERLIN_BOOTSTRAP_ADMIN_USER_ID not set: without it, a guild with no settings configured yet has no way to run /config at all")
	}

	l.mu.Lock()
	l.cur = &next
	l.mu.Unlock()
	return nil
}

// isTruthy accepts the spellings an operator under pressure is likely to
// reach for. Anything unrecognized — including a typo — leaves the pause
// disengaged, so the emergency stop can only ever be turned on deliberately.
func isTruthy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// Global returns a snapshot of the full current configuration.
func (l *Loader) Global() GlobalConfig {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return *l.cur
}
