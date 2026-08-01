# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

`merlin` is a production-grade, modular Discord bot (Go, `discordgo` v0.29+, Go 1.25). Full design doc: [`spec.MD`](./spec.MD) — read it before any non-trivial change, especially §4 (security model), §4a (command/permission framework), and §9 (milestone plan). The project is built for a high-free-speech community with real exposure to weaponized/mass reporting, so **fail-safe behavior and defense in depth are first-class requirements**, not polish.

## Commands

```sh
go build ./...                              # compile everything
go vet ./...
golangci-lint run                           # matches CI's golangci-lint v2.12.2
go test ./...  -cover                       # full suite
go test ./... -race -cover -covermode=atomic  # matches CI exactly (needs CGO_ENABLED=1 / a C toolchain)
go test ./internal/plugins/rotation/... -run TestRotateFullCycle -v   # single package / single test
govulncheck ./...
```

Local run:
```sh
cp .env.example .env                 # DISCORD_BOT_TOKEN, DISCORD_APP_ID, MERLIN_BREAK_GLASS_ADMIN_USER_ID, ...
cp config.example.yaml config.yaml   # bootstrap-only: just log_level as of Milestone 4
docker compose up --build            # bot + Postgres
# or, natively (Postgres is a hard runtime requirement; migrations run automatically at startup):
go run ./cmd/bot
```

CI (`.github/workflows/ci.yml`) runs on every push/PR: `go vet`, `golangci-lint`, `go test -race -cover`, `govulncheck`, `gitleaks` secret scan, and a Docker build. On push to `main` only, it also builds+pushes a multi-arch (amd64/arm64) image to GHCR and deploys to the VPS over SSH via `docker-compose.prod.yml`. There is no separate lint config file beyond `golangci-lint`'s defaults — check `.github/workflows/ci.yml` for the pinned version if a rule looks unfamiliar.

## Architecture

### Plugin model — modular by package, monolithic by binary

Every feature is a Go package implementing `core.Plugin` (`internal/core/registry.go`): `Name() string`, `Init(deps Deps) error`, `Start(ctx) error`, `Shutdown(ctx) error`. Plugins are compiled into one binary and registered in `cmd/bot/main.go` — **no dynamic/hot-loading, ever** (spec.MD Design Principle 1). `Init` runs for every plugin (in registration order) before any `Start`; `Shutdown` runs in reverse start order. `Registry` (`internal/core/registry.go`) wraps every lifecycle call in `recover()` — a panicking plugin fails startup/shutdown loudly, it never limps along or takes the process down silently.

Plugins never import or call each other directly. They only communicate via:
- **`Deps`** (`internal/core/registry.go`) — the fixed set of shared services injected at `Init`: `Session`, `Bus`, `Config`, `Perms`, `Commands`, `Audit`, `Logger`, `DB`, `Scheduler`. A plugin that needs something narrower than the full `internal/settings.Store` (e.g. `rotation`, `adminconfig`) takes it as an explicit constructor parameter instead (`rotation.New(settingsStore)`), not through `Deps` — see the narrow-interface pattern below.
- **`core.EventBus`** (`internal/core/eventbus.go`) — pub/sub by `EventType` (`core.EventReady`, `core.EventConfigChanged`, `core.EventChannelRotated`, ...). `Publish` runs every subscriber synchronously on the publisher's goroutine, each under its own `recover()` — handlers must not block for long, and must never call `Publish`/`Subscribe` reentrantly while holding a lock that could deadlock against the bus's own (it copies the subscriber slice under `RLock` before dispatching, precisely to avoid that).

### Narrow interface seams (the dominant pattern in this codebase)

Concrete stores (`internal/settings.Store`, `internal/config.Loader`) are never imported directly by every consumer. Instead each consumer package defines its own minimal interface for exactly what it needs, and the concrete type satisfies it structurally with zero glue code: `core.GuildAuthData`, `rotation.SettingsProvider`, `adminconfig.SettingsAdmin`, `scheduler.statusChannelResolver`, `audit.channelResolver`, `rotation.DiscordChannelOps`/`ArchiveStore`. This exists so (a) packages never form import cycles (e.g. `internal/settings` must import `internal/core` for `EventBus`, so `internal/core` can never import `internal/settings` back — it depends on the interface `GuildAuthData` instead), and (b) every plugin can be unit-tested against a tiny in-memory fake instead of live Postgres. When adding a new consumer of settings/config, define a narrow interface in the consumer's own package rather than depending on the concrete store.

### Command & Permission Framework (spec.MD §4a — read this before adding any command)

`core.CommandRouter` (`internal/core/commands.go`) is the **single owner** of slash-command registration and dispatch. This replaced an ad-hoc per-plugin `session.AddHandler`/`ApplicationCommandBulkOverwrite` approach that had a live collision bug (two plugins each bulk-overwriting would silently wipe out each other's commands) and no discoverability story. Rules that follow from this, binding for all new plugins:

- **One top-level command per plugin** (`/rotation`, `/scheduler`, `/config`, ...), not a central `/admin` dumping ground. The only sanctioned exception is `/config` (`internal/plugins/adminconfig`), because mod roles/admins/permission whitelists don't belong to any single feature plugin.
- Register via `Commands.RegisterCommand(pluginName, cmd)` + `Commands.Handle(topLevelName, path, PermSpec, handler)` during `Init` — never call `session.AddHandler` or `discordgo.ApplicationCommand*` directly. `pluginName` (almost always the plugin's own `Name()`) is what lets the router check per-guild plugin enable/disable before dispatch.
- **`PermSpec{Tier, Action}` is mandatory** for every leaf. `Tier` is `TierPublic` / `TierMod` / `TierAdmin`; its zero value (`tierUnset`) is deliberately invalid and rejected at `Finalize()` — a forgotten tier fails the build at startup, it never silently defaults to public. Any tier above `TierPublic` also requires a non-empty `Action`.
- `Authorize` (`internal/core/permissions.go`) checks four layers in order, coarsest first (spec.MD §4c): (0) is the owning plugin enabled in this guild (`core.PluginGate`, `/config plugins enable|disable`) — checked by `CommandRouter` before Authorize even runs; (1) deny — `ActionPolicy.DenyRoleIDs/DenyUserIDs` (`/config permissions block`), wins over everything below except the break-glass admin, which nothing can deny; (2) tier — the guild's `ActionPolicy.RequiredTier` override if set (`/config permissions set-tier`), else the command's own `PermSpec.Tier`; `TierAdmin` passes for a DB-listed admin, the break-glass identity, or anyone holding Discord's own Administrator permission bit (`member.Permissions`, already on every interaction — no extra API call); `TierMod` deliberately has no permission-bit shortcut, only DB-listed mod roles or admins; (3) allow — the additive per-action whitelist (`/config permissions grant`), independent of tier. Mutating any of admins/mod-roles/tier-overrides/allow/deny/plugin-toggle is itself `TierAdmin`-only, never `TierMod`, so a mod can never escalate.
- Discord's own `default_member_permissions` is **deliberately left unset** on Mod/Admin commands (see spec.MD §4a) — the internal checks above are the sole real gate, so they can't be bypassed by a mismatched Discord permission bit.
- `adminconfig` (owning `/config`) can never be disabled via `/config plugins disable` — guarded explicitly in its handler, since disabling it would permanently lock a guild out of ever re-enabling anything.
- Commands register **per-guild** (via `RegisterGuild`, called reactively from a `GuildCreate` handler in `cmd/bot/main.go`), not globally — instant availability, no propagation delay.
- Every dispatch (`dispatchCommand`/`dispatchAutocomplete`) runs under `recover()` — `discordgo`'s own event dispatch has none.
- Any option whose valid values come from bot state (job keys, action names, ...) gets Discord autocomplete *plus* a plain `list` subcommand as a redundant, no-typing discovery path. Any channel/role/user-valued option uses the native `Channel`/`Role`/`User` option type, never a raw string ID.
- Use `core.LeafArgs(i)` inside handlers to read the invoked subcommand's arguments by name — don't re-walk `i.ApplicationCommandData().Options` yourself; this exists specifically because that walk was independently reimplemented in three places before being consolidated.

### Self-serve, DB-backed config — not `config.yaml`

`config.yaml`/`.env` are bootstrap-only: Discord token/App ID, DB DSN, log level, and one **break-glass admin user ID** (`MERLIN_BREAK_GLASS_ADMIN_USER_ID`) that always satisfies `TierAdmin` in every guild regardless of DB state, so a wiped/misconfigured guild's settings can never permanently lock the operator out. Everything guild-scoped (mod roles, admins, permission whitelists, rotation settings) lives in Postgres via `internal/settings.Store`, in-memory cached per guild, refreshed on every mutation, and invalidated bot-wide via `core.EventConfigChanged`. Admins configure all of it through Discord commands (`/config`, `/rotation configure`) — never by editing files on the host. `/config setup` auto-creates `#bot-audit-log`, `#bot-status`, and a mod role for whatever a guild is missing; `/config import` is an explicit, one-time YAML→DB migration path for pre-Milestone-4 deployments, never automatic.

### Scheduler (`internal/scheduler`)

Generic cron core other plugins register recurring jobs with (interval-based, not raw cron expressions — `next_due = last_run + interval`, persisted per job key `guildID:name` in Postgres, so a restart never resets or double-fires a rotation window). Named tunables live in `internal/scheduler/scheduler.go`'s const block (`tickInterval`, `maxConsecutiveFailures`, `backoffBase`/`backoffMax`, `maxJitter`) — extend that block rather than adding new inline literals. Jobs whose existence can change at runtime (rotation channels added/removed via `/rotation configure`) must call `Scheduler.Unregister` to stay in sync — see rotation's `reconcile` pattern below.

### Channel Rotation (`internal/plugins/rotation`)

Implements spec.MD §6. Config is entirely DB-backed; because a guild's set of rotating channels can change at runtime, job registration is **not** a static Init-time loop — `reconcile(guildID)` (in `rotation.go`) diffs current settings against currently-registered Scheduler jobs (register new/changed-interval, unregister removed) and is invoked both from `SyncGuild` (called by `cmd/bot/main.go` on every `GuildCreate`) and reactively via a `core.EventConfigChanged` subscription set up in `Init` — a settings mutation already triggers reconcile through the event; don't also call `SyncGuild` explicitly after one (that was a real redundant-call bug fixed here once already).

Each rotation cycle re-derives "is this step already done?" from live Discord/Postgres state, so a crash mid-rotation self-heals on the next scheduled retry without duplicate channels or reposted stickies. It flips visibility **new channel first** (create and populate the replacement before touching the original), trading the literal step order in spec.MD for a stronger safety property: the worst failure case is a brief moment where both channels are visible, never a moment with no live channel at all. Archived channels are swept for permanent deletion by a separate hourly `rotation-sweep` job per guild, once past `retention_days` (nil = keep forever, never swept); a mod manually moving an archived channel out of its archive category is treated as an implicit "keep this one."

**Audit-failure policy**: an audit-embed post failure (e.g. `#bot-audit-log` not configured or deleted) must never fail the operation that triggered it — the actual action already succeeded, and the durable audit record write happens independently of the embed post inside `audit.Writer.Record`. Every call site logs-and-continues on a non-nil `Record` error; this was a real bug in `rotation/execute.go` (an audit failure was making the Scheduler treat a successful rotation as failed) fixed by matching the policy every other call site already used.

### Security model (spec.MD §4, enforced in code, not just documented)

- Layered authorization: `core.CommandRouter` → plugin-enable gate → `Permissions.Authorize` (deny → tier → allow, see above) is the mandatory check on every command; `Permissions.CanManageRole` enforces the bot can never touch a Discord role positioned above its own top role.
- Fail safe, not fail silent: an unset `PermTier` fails `Finalize()` at startup; a missing top-level handler for a declared subcommand also fails `Finalize()`.
- Minimal logging: log IDs, not message content, by default — this matters doubly for rotation, whose whole point is *not* retaining content longer than necessary.
- Self-throttling independent of Discord's own rate limits (e.g. `channelCapHeadroom` in `rotation/execute.go` staying well clear of Discord's 500-channel guild cap) rather than relying solely on discordgo's rate-limit backoff.

## Conventions (from CONTRIBUTING.md)

- Small, reviewable PRs — one milestone (or a slice of one) per PR, per `spec.MD` §9's breakdown.
- `main` is protected: PR review + all CI checks required, no direct/force pushes.
- Run `go vet ./...`, `go test ./... -race -cover`, and `golangci-lint run` before opening a PR.
