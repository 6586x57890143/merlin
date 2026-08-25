package config

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"slices"
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
	// onReload holds the hooks OnReload registered, run after every
	// successful reload so values applied once at startup can follow the
	// file instead of freezing at their boot-time value.
	onReload []func(*GlobalConfig)
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

	// Secrets/identifiers are env-only and re-applied on every reload, never
	// read from YAML, so a hot-reloaded config file can never leak or
	// override them.
	next.Discord.Token = os.Getenv("DISCORD_BOT_TOKEN")
	next.Discord.AppID = os.Getenv("DISCORD_APP_ID")
	next.Database.DSN = os.Getenv("DATABASE_URL")
	next.BootstrapAdminUserID = os.Getenv("MERLIN_BOOTSTRAP_ADMIN_USER_ID")
	next.PauseAllWrites = isTruthy(os.Getenv("MERLIN_PAUSE_ALL_WRITES"))
	// Opt-out rather than opt-in: see GlobalConfig.GuildMembersIntent. The
	// superseded MERLIN_ENABLE_GUILD_MEMBERS_INTENT is deliberately not read
	// any more: an existing .env that sets it now gets the behavior it was
	// asking for regardless, and one that doesn't gets it too, which was the
	// entire point of changing the default.
	next.GuildMembersIntent = !isTruthy(os.Getenv("MERLIN_DISABLE_GUILD_MEMBERS_INTENT"))
	// Opt-in, unlike the intent above. See GlobalConfig.OnboardingDM for why
	// the defaults point in opposite directions.
	next.OnboardingDM = isTruthy(os.Getenv("MERLIN_ENABLE_ONBOARDING_DM"))
	// Opt-in, like the onboarding DM and unlike the members intent. See
	// GlobalConfig.MessageContentIntent for why the defaults differ.
	next.MessageContentIntent = isTruthy(os.Getenv("MERLIN_ENABLE_MESSAGE_CONTENT_INTENT"))
	next.SecretKey = os.Getenv("MERLIN_SECRET_KEY")
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
	hooks := slices.Clone(l.onReload)
	l.mu.Unlock()

	// Outside the lock: a hook is arbitrary caller code, and the obvious
	// thing for one to do is read the config it was just handed, which would
	// deadlock against the RLock in Global.
	for _, fn := range hooks {
		fn(&next)
	}
	return nil
}

// OnReload registers fn to run after every successful reload, and never
// after a failed one, so a hook only ever sees a config that passed
// validation.
//
// This exists because a value read once at startup silently stops tracking
// the file it came from. LogLevel was exactly that: parsed, validated, used
// to set the log level during startup, and then never consulted again, so
// SIGHUP reloaded it into a struct nobody read and raising verbosity
// mid-incident still meant a restart. A restart is the one thing you do not
// want while reproducing a live problem.
//
// Hooks run in registration order on the goroutine that reloaded, which is
// Watch's, so a slow hook delays later hooks and nothing else.
func (l *Loader) OnReload(fn func(*GlobalConfig)) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.onReload = append(l.onReload, fn)
}

// isTruthy accepts the spellings an operator under pressure is likely to
// reach for. Anything unrecognized, including a typo, leaves the pause
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
