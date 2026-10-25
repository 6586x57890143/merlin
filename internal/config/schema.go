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
	// BootstrapAdminUserIDs each always satisfy core.TierAdmin, in every
	// guild, regardless of internal/settings state: the break-glass
	// identities that guarantee a wiped or not-yet-configured guild's
	// settings can never permanently lock the operators out of /config.
	// MERLIN_BOOTSTRAP_ADMIN_USER_ID, comma separated; the first is the
	// operator a surface names when it names one (the /support advert).
	// Env-sourced only, same as the other secrets/identifiers below.
	BootstrapAdminUserIDs []string `yaml:"-"`
	// PauseAllWrites is the host-level emergency stop: every destructive
	// Discord call is refused process-wide while it is set. Env-sourced and
	// applied at startup rather than hot-reloaded, because it is the lever
	// an operator reaches for when the bot is actively doing damage, so it
	// must not depend on parsing a config file or on a reachable database.
	// The per-guild equivalents (/config pause, /config dryrun) live in
	// internal/settings for everything short of that.
	PauseAllWrites bool `yaml:"-"`
	// OnboardingDM lets merlin send a guild's owner a one-time DM pointing at
	// /config setup when she joins a server nobody has configured yet.
	//
	// Off by default, and opt-in via MERLIN_ENABLE_ONBOARDING_DM. What is left is a message merlin originates unprompted to a person who
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
	// SecretKey is a base64 32-byte key used to encrypt per-guild
	// third-party API keys at rest (today: OpenRouter, in
	// internal/plugins/aimod). Unset means those keys cannot be stored at
	// all, and the command that would store one refuses rather than falling
	// back to plaintext.
	SecretKey string `yaml:"-"`

	// Per-rail overrides for the tip jar, each pointing at an endpoint and
	// the token contract read there. Empty means the compiled-in defaults,
	// which is the normal case: an operator sets these only to move a chain
	// off a public RPC or to point a rail at a different token.
	//
	// An endpoint and a token address travel together because that pair is
	// what a rail is: an endpoint on one chain with a token address from
	// another reports a zero balance rather than an error, which is the worst
	// way for a misconfiguration to present. Environment only, never
	// guild-settable: pointing a server's donations at an arbitrary contract
	// is not an admin's decision to make.
	//
	// Maps rather than a field per rail, because the tip jar reads a dozen of
	// them and a struct field per rail would mean this file, the loader and
	// the plugin's constructor all growing every time one is added.
	// FundingRPCURLs is keyed by chain ("base"), since every rail on one chain
	// shares an endpoint; FundingContracts is keyed by rail ("base:USDC").
	FundingRPCURLs   map[string]string `yaml:"-"`
	FundingContracts map[string]string `yaml:"-"`

	// The contest plugin's Cloudflare Worker: the gallery and voting page.
	// Env rather than per-guild settings because one deployment means one
	// Worker, the same reasoning that keeps the funding rails here.
	//
	// ContestWorkerURL empty is a supported state, not a misconfiguration:
	// the Discord half of a contest (the forum, the phases, the prizes, the
	// announcements) runs without a Worker, and /contest status says plainly
	// that there is nowhere to browse or vote. ContestLinkKey hashes Discord
	// IDs before they cross to Cloudflare, so the vote ledger holds no
	// Discord identities; it has to match the Worker's own LINK_KEY secret,
	// which the Worker uses to hash the account OAuth just confirmed.
	ContestWorkerURL   string `yaml:"-"`
	ContestWorkerToken string `yaml:"-"`
	ContestLinkKey     string `yaml:"-"`
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
