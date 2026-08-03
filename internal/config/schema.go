package config

import "log/slog"

// GlobalConfig is process-level bootstrap only (spec.MD §4a): secrets and
// the handful of values needed before the bot can even open a Discord
// session or reach Postgres. Everything guild-scoped (mod roles, admins,
// permission whitelists, rotation config) lives in internal/settings,
// DB-backed and configured entirely through Discord commands, never
// hand-edited here. That split is deliberate: this file is read once at
// startup and requires host/file access to change; internal/settings is
// mutable at runtime by guild admins with no host access at all.
type GlobalConfig struct {
	LogLevel string `yaml:"log_level" validate:"omitempty,oneof=debug info warn error"`

	Discord  DiscordConfig  `yaml:"-"`
	Database DatabaseConfig `yaml:"-"`
	// BootstrapAdminUserID always satisfies core.TierAdmin, in every guild,
	// regardless of internal/settings state: the one identity that
	// guarantees a wiped or not-yet-configured guild's settings can never
	// permanently lock the operator out of /config. Env-sourced only, same
	// as the other secrets/identifiers below.
	BootstrapAdminUserID string `yaml:"-"`
	// PauseAllWrites is the host-level emergency stop: every destructive
	// Discord call is refused process-wide while it is set. Env-sourced and
	// applied at startup rather than hot-reloaded, because it is the lever
	// an operator reaches for when the bot is actively doing damage, so it
	// must not depend on parsing a config file or on a reachable database.
	// The per-guild equivalents (/config pause, /config dryrun) live in
	// internal/settings for everything short of that.
	PauseAllWrites bool `yaml:"-"`
	// GuildMembersIntent requests Discord's privileged GUILD_MEMBERS gateway
	// intent, letting the roles plugin re-apply a jail the instant an evader
	// rejoins rather than on the next one-minute sweep.
	//
	// On by default, and the default changed deliberately. It used to be
	// opt-in via MERLIN_ENABLE_GUILD_MEMBERS_INTENT, which put the operator in
	// the position of enabling "Server Members Intent" in the Developer Portal
	// (the only step that looks like it should matter) and getting no
	// behavior change, because the bot never asked for the intent. Nothing
	// reported the mismatch. Requesting it by default makes the portal toggle
	// mean what it appears to mean; a deployment that cannot enable it there
	// sets MERLIN_DISABLE_GUILD_MEMBERS_INTENT and keeps the sweep fallback.
	//
	// Asking for an intent the portal has not granted makes Discord reject the
	// connection, so this must be paired with the readiness check in
	// cmd/bot/main.go. See waitForReady, which turns that rejection into a
	// startup failure naming the toggle instead of a silent reconnect loop.
	GuildMembersIntent bool `yaml:"-"`
	// OnboardingDM lets Merlin send a guild's owner a one-time DM pointing at
	// /config setup when she joins a server nobody has configured yet.
	//
	// Off by default, and opt-in via MERLIN_ENABLE_ONBOARDING_DM. That is the
	// opposite default from GuildMembersIntent above, deliberately, and the
	// reasoning that made opt-in a bug there does not transfer. That bug had
	// three parts: an operator performing a visible action in a UI outside
	// this bot (ticking "Server Members Intent"), that action being the only
	// step that looks like it should matter, and nothing anywhere reporting
	// that it had no effect. None of them hold here. There is no external
	// toggle, nothing claims Merlin will DM anyone, and no granted capability
	// is being silently declined.
	//
	// What is left is a message Merlin originates unprompted to a person who
	// has not interacted with her, which is the category where "off unless
	// asked for" is the conservative default rather than the surprising one.
	// The failure mode is also mild and self-correcting: an operator who
	// expected the DM and did not get it goes and runs /config setup, which
	// is precisely what the DM would have told them to do.
	//
	// Enabling it later DMs the owner of every still-unconfigured guild on
	// the next restart, since onboarding_nudge_sent_at is only written after
	// a successful send. That is correct, and worth knowing before flipping
	// it on a bot that is already in several servers.
	OnboardingDM bool `yaml:"-"`
}

// Level maps LogLevel onto slog. The value is validated at load time, so an
// unrecognized string here can only mean an empty LogLevel.
func (c GlobalConfig) Level() slog.Level {
	switch c.LogLevel {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

type DiscordConfig struct {
	Token string
	AppID string
}

type DatabaseConfig struct {
	DSN string
}
