package config

// GuildConfig holds per-guild settings. Validated on every load/reload so a
// malformed edit is rejected rather than silently applied (spec.MD "config
// changes are audited, not silent").
type GuildConfig struct {
	GuildID           string   `yaml:"guild_id" validate:"required"`
	ModRoleIDs        []string `yaml:"mod_role_ids" validate:"required,min=1,dive,required"`
	AdminUserIDs      []string `yaml:"admin_user_ids"`
	AuditLogChannelID string   `yaml:"audit_log_channel_id" validate:"required"`
	StatusChannelID   string   `yaml:"status_channel_id" validate:"required"`

	RotatingChannels []RotationConfig `yaml:"rotating_channels" validate:"dive"`
}

// RotationConfig configures periodic "Refresh" rotation for one channel
// (spec.MD §6): the channel is renamed into a hidden archive on an interval
// (or on demand, via /admin run-now) and replaced with a clean one, reducing
// the window of retained history a bad-faith actor can trawl through,
// without losing a moderation trail.
type RotationConfig struct {
	ChannelID         string `yaml:"channel_id" validate:"required"`
	IntervalHours     int    `yaml:"interval_hours" validate:"required,min=1"`
	ArchiveCategoryID string `yaml:"archive_category_id" validate:"required"`
	// ArchiveVisibility is "mod_only" (only configured mod roles can see the
	// archive) or "whitelist" (mod roles plus ArchiveWhitelistRoleIDs/
	// ArchiveWhitelistUserIDs). spec.MD marks "export_then_delete" as future
	// work — not yet supported.
	ArchiveVisibility string `yaml:"archive_visibility" validate:"required,oneof=mod_only whitelist"`
	// ArchiveWhitelistRoleIDs/ArchiveWhitelistUserIDs are only used when
	// ArchiveVisibility is "whitelist" — additional roles/users granted view
	// access to the archive category beyond the guild's mod roles (which
	// always retain access regardless of visibility mode).
	ArchiveWhitelistRoleIDs []string `yaml:"archive_whitelist_role_ids"`
	ArchiveWhitelistUserIDs []string `yaml:"archive_whitelist_user_ids"`
	// RetentionDays is the minimum time an archived channel is kept before
	// permanent deletion. Omit entirely to keep archives forever (never
	// swept) — a literal 0 is rejected so "forever" is always a deliberate
	// omission, never a number that reads as "zero days" (spec.MD warns
	// against ever defaulting retention to 0). "Forever" is an intentional,
	// operator-requested escape hatch: nothing automatically caps its
	// unbounded growth toward Discord's 500-channel guild limit, since that
	// would defeat the point of "forever" — treat it as an ongoing manual
	// operational responsibility, not something this bot protects against.
	RetentionDays *int `yaml:"retention_days" validate:"omitempty,min=1"`

	Sticky StickyConfig `yaml:"sticky"`
}

// StickyConfig controls whether a named template's messages are reposted
// and pinned in the fresh channel after rotation.
type StickyConfig struct {
	Enabled  bool   `yaml:"enabled"`
	Template string `yaml:"template"`
}

// GlobalConfig is the full loaded configuration. Discord and Database are
// populated exclusively from environment variables (see loader.go) and are
// tagged yaml:"-" so a config file can never set or override a secret.
type GlobalConfig struct {
	Guilds          map[string]GuildConfig    `yaml:"guilds" validate:"dive"`
	StickyTemplates map[string]StickyTemplate `yaml:"sticky_templates"`
	LogLevel        string                    `yaml:"log_level"`

	Discord  DiscordConfig  `yaml:"-"`
	Database DatabaseConfig `yaml:"-"`
}

// StickyTemplate is a named, reusable set of messages posted (in order) and
// pinned in a freshly-rotated channel. Content lives in config, not a DB
// table — it's mod-edited and rarely changes, and the loader already
// hot-reloads on SIGHUP.
type StickyTemplate struct {
	Messages []string `yaml:"messages" validate:"required,min=1,dive,required"`
}

type DiscordConfig struct {
	Token string
	AppID string
}

type DatabaseConfig struct {
	DSN string
}
