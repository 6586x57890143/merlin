// Package scripts is the on/off switch for scripts: small, hyper-specific,
// per-server behaviour that lives below a plugin (a Go file in the plugin's
// package with a table of data and a hook into work the plugin already does).
// Every script is off until an admin turns it on, and while it is on it is
// deliberately stronger than the guild's admins: the only sanctioned way to
// stop one is turning it off here. The definitions themselves are code, so
// no admin surface can change what a script does, only whether it runs.
//
// Plugins take Store as a constructor parameter, the same narrow-interface
// shape as rotation.SettingsProvider. It is its own table rather than a
// column on settings_guild because nothing reads it on a hot path: a sweep
// asks once a minute, a command once per click.
package scripts

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Warning is the sentence every plugin's enable path leads with. One copy,
// so the next script cannot soften it.
const Warning = "Scripts are server-specific behaviour that overrides normal admin authority while on. " +
	"merlin will undo what an admin does to whatever this script protects, without asking. " +
	"The only way to stop it is turning the script off."

// Store answers and records whether a script is on in a guild.
type Store interface {
	// Enabled reports whether script is on in guildID. Callers treat an
	// error as off: a script is extra behaviour, and an unreadable switch
	// must mean doing nothing rather than doing something an admin turned
	// off.
	Enabled(ctx context.Context, guildID, script string) (bool, error)
	SetEnabled(ctx context.Context, guildID, script string, on bool) error
}

type pgStore struct{ pool *pgxpool.Pool }

// NewPostgresStore backs Store with scripts_enabled (migration 0036): a row
// present means on, so the zero state of every guild is off.
func NewPostgresStore(pool *pgxpool.Pool) Store { return &pgStore{pool: pool} }

func (s *pgStore) Enabled(ctx context.Context, guildID, script string) (bool, error) {
	var on bool
	if err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM scripts_enabled WHERE guild_id = $1 AND script = $2)`,
		guildID, script).Scan(&on); err != nil {
		return false, fmt.Errorf("scripts: read %s: %w", script, err)
	}
	return on, nil
}

func (s *pgStore) SetEnabled(ctx context.Context, guildID, script string, on bool) error {
	var err error
	if on {
		_, err = s.pool.Exec(ctx, `INSERT INTO scripts_enabled (guild_id, script) VALUES ($1, $2) ON CONFLICT DO NOTHING`, guildID, script)
	} else {
		_, err = s.pool.Exec(ctx, `DELETE FROM scripts_enabled WHERE guild_id = $1 AND script = $2`, guildID, script)
	}
	if err != nil {
		return fmt.Errorf("scripts: set %s=%v: %w", script, on, err)
	}
	return nil
}
