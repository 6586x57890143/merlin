package config

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
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

	// Secrets are env-only and re-applied on every reload — never read from
	// YAML, so a hot-reloaded config file can never leak or override them.
	next.Discord.Token = os.Getenv("DISCORD_BOT_TOKEN")
	next.Discord.AppID = os.Getenv("DISCORD_APP_ID")
	next.Database.DSN = os.Getenv("DATABASE_URL")
	if next.Discord.Token == "" {
		return errors.New("DISCORD_BOT_TOKEN not set")
	}

	l.mu.Lock()
	l.cur = &next
	l.mu.Unlock()
	return nil
}

// Guild returns the config for a specific guild, or an error if the guild
// isn't configured.
func (l *Loader) Guild(id string) (GuildConfig, error) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	gc, ok := l.cur.Guilds[id]
	if !ok {
		return GuildConfig{}, fmt.Errorf("no config for guild %s", id)
	}
	return gc, nil
}

// Global returns a snapshot of the full current configuration.
func (l *Loader) Global() GlobalConfig {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return *l.cur
}
