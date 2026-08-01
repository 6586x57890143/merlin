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
}

// GlobalConfig is the full loaded configuration. Discord and Database are
// populated exclusively from environment variables (see loader.go) and are
// tagged yaml:"-" so a config file can never set or override a secret.
type GlobalConfig struct {
	Guilds   map[string]GuildConfig `yaml:"guilds" validate:"dive"`
	LogLevel string                 `yaml:"log_level"`

	Discord  DiscordConfig  `yaml:"-"`
	Database DatabaseConfig `yaml:"-"`
}

type DiscordConfig struct {
	Token string
	AppID string
}

type DatabaseConfig struct {
	DSN string
}
