package config

// GlobalConfig is process-level bootstrap only (spec.MD §4a): secrets and
// the handful of values needed before the bot can even open a Discord
// session or reach Postgres. Everything guild-scoped (mod roles, admins,
// permission whitelists, rotation config) lives in internal/settings,
// DB-backed and configured entirely through Discord commands — never
// hand-edited here. That split is deliberate: this file is read once at
// startup and requires host/file access to change; internal/settings is
// mutable at runtime by guild admins with no host access at all.
type GlobalConfig struct {
	LogLevel string `yaml:"log_level"`

	Discord  DiscordConfig  `yaml:"-"`
	Database DatabaseConfig `yaml:"-"`
	// BootstrapAdminUserID always satisfies core.TierAdmin, in every guild,
	// regardless of internal/settings state — the one identity that
	// guarantees a wiped or not-yet-configured guild's settings can never
	// permanently lock the operator out of /config. Env-sourced only, same
	// as the other secrets/identifiers below.
	BootstrapAdminUserID string `yaml:"-"`
}

type DiscordConfig struct {
	Token string
	AppID string
}

type DatabaseConfig struct {
	DSN string
}
