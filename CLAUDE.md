# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

`merlin` is a production-grade, modular Discord bot (Go, `discordgo` v0.29+, Go 1.27). Full design doc: [`spec.MD`](./spec.MD). Read it before any non-trivial change, especially §4 (security model), §4a (command/permission framework), and §9 (milestone plan). The project is built for a high-free-speech community with real exposure to weaponized/mass reporting, so **fail-safe behavior and defense in depth are first-class requirements**, not polish.

## Commands

```sh
go build ./...                              # compile everything
go vet ./...
golangci-lint run                           # matches CI's golangci-lint v2.13.1
go test ./...  -cover                       # full suite
go test ./... -race -cover -covermode=atomic  # matches CI exactly (needs CGO_ENABLED=1 / a C toolchain)
go test ./internal/plugins/rotation/... -run TestRotateFullCycle -v   # single package / single test
govulncheck ./...

go test ./... -coverprofile=coverage.out && scripts/coverage-floor.sh coverage.out   # the CI coverage gate
```

Every package under `internal/plugins/` must hold at least 70% statement coverage (`scripts/coverage-floor.sh`, run by CI). Plugins that predate the floor are pinned in that script at the coverage they had and may only go up; anything new gets the full 70% with no way to opt out but an edit somebody reviews.

`internal/settings` and `internal/storage` also have Postgres-backed tests (`internal/dbtest` is the shared harness). They skip themselves, rather than failing, when `TEST_DATABASE_URL` isn't set, so `go test ./...` still runs everywhere with zero setup; CI sets it via a `postgres:16-alpine` service on the `lint-test` job. To run them locally: `docker compose up -d postgres` (or any Postgres reachable at the DSN below), then `TEST_DATABASE_URL=postgres://merlin:changeme@localhost:5432/merlin?sslmode=disable go test ./internal/settings/... ./internal/storage/...` (substitute your own `.env` credentials if you changed them from `.env.example`'s defaults).

Local run:
```sh
cp .env.example .env                 # DISCORD_BOT_TOKEN, DISCORD_APP_ID, MERLIN_BOOTSTRAP_ADMIN_USER_ID, ...
cp config.example.yaml config.yaml   # bootstrap-only: just log_level as of Milestone 4
docker compose up --build            # bot + Postgres
# or, natively (Postgres is a hard runtime requirement; migrations run automatically at startup):
go run ./cmd/bot
```

CI (`.github/workflows/ci.yml`) runs on every push/PR: `go vet`, `golangci-lint`, `go test -race -cover`, `govulncheck`, `gitleaks` secret scan, and a Docker build. On push to `main` only, it also builds+pushes a multi-arch (amd64/arm64) image to GHCR and deploys to the VPS over SSH via `docker-compose.prod.yml`. There is no separate lint config file beyond `golangci-lint`'s defaults; check `.github/workflows/ci.yml` for the pinned version if a rule looks unfamiliar.

## Architecture

### Plugin model: modular by package, monolithic by binary

Every feature is a Go package implementing `core.Plugin` (`internal/core/registry.go`): `Name() string`, `Init(deps Deps) error`, `Start(ctx) error`, `Shutdown(ctx) error`. Plugins are compiled into one binary and registered in `cmd/bot/main.go`, with **no dynamic/hot-loading, ever** (spec.MD Design Principle 1). `Init` runs for every plugin (in registration order) before any `Start`; `Shutdown` runs in reverse start order. `Registry` (`internal/core/registry.go`) wraps every lifecycle call in `recover()`: a panicking plugin fails startup/shutdown loudly, it never limps along or takes the process down silently.

Plugins never import or call each other directly. They only communicate via:
- **`Deps`** (`internal/core/registry.go`): the fixed set of shared services injected at `Init`: `Session`, `Bus`, `Config`, `Perms`, `Commands`, `Audit`, `Logger`, `DB`, `Scheduler`. A plugin that needs something narrower than the full `internal/settings.Store` (e.g. `rotation`, `adminconfig`) takes it as an explicit constructor parameter instead (`rotation.New(settingsStore)`), not through `Deps`. See the narrow-interface pattern below.
- **`core.EventBus`** (`internal/core/eventbus.go`): pub/sub by `EventType` (`core.EventReady`, `core.EventConfigChanged`, `core.EventChannelRotated`, ...). `Publish` runs every subscriber synchronously on the publisher's goroutine, each under its own `recover()`, so handlers must not block for long, and must never call `Publish`/`Subscribe` reentrantly while holding a lock that could deadlock against the bus's own (it copies the subscriber slice under `RLock` before dispatching, precisely to avoid that).

### Narrow interface seams (the dominant pattern in this codebase)

Concrete stores (`internal/settings.Store`, `internal/config.Loader`) are never imported directly by every consumer. Instead each consumer package defines its own minimal interface for exactly what it needs, and the concrete type satisfies it structurally with zero glue code: `core.GuildAuthData`, `rotation.SettingsProvider`, `adminconfig.SettingsAdmin`, `scheduler.statusChannelResolver`, `audit.channelResolver`, `rotation.DiscordChannelOps`/`ArchiveStore`. This exists so (a) packages never form import cycles (e.g. `internal/settings` must import `internal/core` for `EventBus`, so `internal/core` can never import `internal/settings` back; it depends on the interface `GuildAuthData` instead), and (b) every plugin can be unit-tested against a tiny in-memory fake instead of live Postgres. When adding a new consumer of settings/config, define a narrow interface in the consumer's own package rather than depending on the concrete store.

### Command & Permission Framework (spec.MD §4a: read this before adding any command)

`core.CommandRouter` (`internal/core/commands.go`) is the **single owner** of slash-command registration and dispatch. This replaced an ad-hoc per-plugin `session.AddHandler`/`ApplicationCommandBulkOverwrite` approach that had a live collision bug (two plugins each bulk-overwriting would silently wipe out each other's commands) and no discoverability story. Rules that follow from this, binding for all new plugins:

- **One top-level command per plugin** (`/rotation`, `/scheduler`, `/config`, ...), not a central `/admin` dumping ground. The only sanctioned exception is `/config` (`internal/plugins/adminconfig`), because mod roles/admins/permission whitelists don't belong to any single feature plugin.
- Register via `Commands.RegisterCommand(pluginName, cmd)` + `Commands.Handle(topLevelName, path, PermSpec, handler)` during `Init`. Never call `session.AddHandler` or `discordgo.ApplicationCommand*` directly. `pluginName` (almost always the plugin's own `Name()`) is what lets the router check per-guild plugin enable/disable before dispatch.
- **`PermSpec{Tier, Action}` is mandatory** for every leaf. `Tier` is `TierPublic` / `TierMod` / `TierAdmin`; its zero value (`tierUnset`) is deliberately invalid and rejected at `Finalize()`: a forgotten tier fails the build at startup, it never silently defaults to public. Any tier above `TierPublic` also requires a non-empty `Action`.
- `Authorize` (`internal/core/permissions.go`) checks four layers in order, coarsest first (spec.MD §4/§4a): (0) is the owning plugin enabled in this guild (`core.PluginGate`, `/config plugins set`), checked by `CommandRouter` before Authorize even runs; (1) deny, via `ActionPolicy.DenyRoleIDs/DenyUserIDs` (`/config permissions deny`), wins over everything below except the bootstrap admin, which nothing can deny; (2) tier, the guild's `ActionPolicy.RequiredTier` override if set (`/config permissions set-tier`), else the command's own `PermSpec.Tier`; `TierAdmin` passes for a DB-listed admin, the bootstrap identity, or anyone holding Discord's own Administrator permission bit (`member.Permissions`, already on every interaction, no extra API call); `TierMod` deliberately has no permission-bit shortcut, only DB-listed mod roles or admins; (3) allow, the additive per-action whitelist (`/config permissions allow`), independent of tier. Mutating any of admins/mod-roles/tier-overrides/allow/deny/plugin-toggle is itself `TierAdmin`-only, never `TierMod`, so a mod can never escalate. That invariant is enforced against the tier-override mechanism too: `/config permissions set-tier` refuses to lower `config.mutate` below `TierAdmin` (`adminconfig.validateTierChange`), since a guild that set it to `TierMod` would be letting any mod run `/config admins add @themselves`, the same class of self-inflicted lockout as disabling `adminconfig`, and guarded the same way.
- Only `TierMod`/`TierAdmin` overrides are honored; a stored `RequiredTier` of `TierPublic` is ignored. `set-tier` never offers it, so one could only arrive from a corrupt row, a hand-edited DB, or a future import path, and honoring it would strip every check off a privileged action. Loosening Admin→Mod is a deliberate feature; loosening anything to "no check at all" is the one direction a stored value must never move a command.
- The effective tier is resolved **before** the `TierPublic` shortcut, not after. Short-circuiting on the compiled-in `spec.Tier` would silently ignore a guild's `set-tier` override on a public action, a fail-open in the one function that must fail closed. The reverse can't happen: `set-tier` only offers Mod/Admin, so an override can raise a public command's bar but never lower a privileged one to public.
- Discord's own `default_member_permissions` is **deliberately left unset** on Mod/Admin commands (see spec.MD §4a): the internal checks above are the sole real gate, so they can't be bypassed by a mismatched Discord permission bit.
- `adminconfig` (owning `/config`) can never be disabled via `/config plugins set`, guarded explicitly in its handler, since disabling it would permanently lock a guild out of ever re-enabling anything.
- Commands register **per-guild** (via `RegisterGuild`, called reactively from a `GuildCreate` handler in `cmd/bot/main.go`), not globally, giving instant availability with no propagation delay. Because Discord stores those registrations and they survive restarts, **a guild whose `GuildCreate` handling failed still answers every command**, which makes that handler a uniquely deceptive place to bail out early. It used to `return` when `settingsStore.Refresh` failed, skipping both plugins' `SyncGuild`; the guild had never been through a settings mutator so it was never queued for retry either, and nothing re-ran for the life of the process. The visible result was a server where `/roles jail` worked perfectly and jails silently never expired and were escapable by rejoining, because `roles-sweep` had never been registered. The handler now marks the guild stale (`settings.Store.MarkStale`, feeding `RetryStale`) and continues with everything that doesn't need settings; the roles sweep needs none. Only rotation is skipped, since it *derives* which jobs should exist from settings and reconciling against fail-closed defaults would read as "this guild rotates nothing" and unregister real work; `RetryStale` publishes `EventConfigChanged` on recovery, which is what reconciles it.
- Every dispatch (`dispatchCommand`/`dispatchAutocomplete`) runs under `recover()`; `discordgo`'s own event dispatch has none.
- Any option whose valid values come from bot state (job keys, action names, ...) gets Discord autocomplete *plus* a plain `list` subcommand as a redundant, no-typing discovery path. Any channel/role/user-valued option uses the native `Channel`/`Role`/`User` option type, never a raw string ID. Autocomplete must return at most `25` choices, since Discord rejects a longer response outright, leaving the user with *no* suggestions rather than a truncated list.
- **Discord's hard response limits are correctness constraints, not polish.** A handler has 3 seconds to respond at all: anything doing real work (`/scheduler run-now`, `/roles jail`'s first-ever run, `/roles configure sync-channels`) must `core.DeferResponse` first and finish with `core.FollowUpOK`/`FollowUpErr`, or a fully successful operation still surfaces as "the application did not respond." Likewise an embed field over 1024 bytes (`core.TruncateEmbedField`) fails the whole message rather than being trimmed, and repeated `UpdateEmbedWithComponents` edits must replace attachments rather than append them.
- Use `core.LeafArgs(i)` inside handlers to read the invoked subcommand's arguments by name. Don't re-walk `i.ApplicationCommandData().Options` yourself; this exists specifically because that walk was independently reimplemented in three places before being consolidated.

### Self-serve, DB-backed config, not `config.yaml`

`config.yaml`/`.env` are bootstrap-only: Discord token/App ID, DB DSN, log level, and one **bootstrap admin user ID** (`MERLIN_BOOTSTRAP_ADMIN_USER_ID`) that always satisfies `TierAdmin` in every guild regardless of DB state, so a wiped/misconfigured guild's settings can never permanently lock the operator out. Everything guild-scoped (mod roles, admins, permission whitelists, rotation settings) lives in Postgres via `internal/settings.Store`, in-memory cached per guild, refreshed on every mutation, and invalidated bot-wide via `core.EventConfigChanged`. Admins configure all of it through Discord commands (`/config`, `/rotation configure`), never by editing files on the host. `/config setup` (`internal/plugins/adminconfig/setup.go`) is a paginated wizard (audit-log channel, status channel, mod role, admins) that **creates nothing on its own**: each step offers a native picker for what the guild already has, plus an optional button to create the default (`#bird-audit-log`, `#bird-status`, a `merlin Mod` role). Every step re-derives its state from live settings at render time, so there's no wizard session to expire, two admins can drive it concurrently, and re-running it is a status review rather than a redo. `/config import` is an explicit, one-time YAML→DB migration path for pre-Milestone-4 deployments, never automatic.

### Scheduler (`internal/scheduler`)

Generic cron core other plugins register recurring jobs with. Recurrence is a `core.Schedule` (`internal/core/schedule.go`), not a raw cron expression or a bare interval: `next_due = spec.Schedule.Next(last_run)`, persisted per job key `guildID:name` in Postgres, so a restart never resets or double-fires a rotation window. Two implementations: `IntervalSchedule{Interval}` ("every 24h"/"every 3 days", the original and still most common shape) and `CalendarSchedule{Weekday, HourUTC, MinuteUTC}` (wall-clock-anchored: daily when `Weekday` is nil, weekly when set, so a late or missed tick never drifts a "daily at 17:00 UTC" job off its intended time of day the way re-deriving from elapsed time would). `Scheduler.Register` never type-switches on which kind it got; both satisfy the same `Next`/`String`/`Validate`/`TypicalPeriod` interface. Named tunables live in `internal/scheduler/scheduler.go`'s const block (`tickInterval`, `maxConsecutiveFailures`, `backoffBase`/`backoffMax`, `maxJitter`, `jobTimeout`, `stateWriteTimeout`); extend that block rather than adding new inline literals. Every run is bounded by `jobTimeout` under a base context cancelled at `Shutdown`, and the last-run/failure bookkeeping that follows it deliberately runs on a *detached* context (`context.WithoutCancel`): writing through the same context that just killed the run would drop the failure, leaving a wedged job looking permanently healthy: never backing off, never tripping the `maxConsecutiveFailures` alert. Jobs whose existence can change at runtime (rotation channels added/removed via `/rotation configure`) must call `Scheduler.Unregister` to stay in sync; see rotation's `reconcile` pattern below. A job freshly registered for something a user just configured (not a restart) that shouldn't fire on the very next tick (e.g. a rotation channel someone just added) can call `Scheduler.Seed(ctx, jobKey, now)` right after `Register` to mark it as if it had just run, deferring its first real fire by one full schedule period (see rotation's `deferFirstRotation`); a job the Scheduler has never seen is otherwise treated as immediately due, which is correct for jobs like sweep jobs but was a real bug for rotation itself (a newly-added channel used to rotate on the very next tick instead of waiting a full interval).

### Channel Rotation (`internal/plugins/rotation`)

Implements spec.MD §6. Config is entirely DB-backed; because a guild's set of rotating channels can change at runtime, job registration is **not** a static Init-time loop: `reconcile(guildID)` (in `rotation.go`) diffs current settings against currently-registered Scheduler jobs (register new/changed-interval, unregister removed) and is invoked both from `SyncGuild` (called by `cmd/bot/main.go` on every `GuildCreate`) and reactively via a `core.EventConfigChanged` subscription set up in `Init`. A settings mutation already triggers reconcile through the event; don't also call `SyncGuild` explicitly after one (that was a real redundant-call bug fixed here once already).

Each rotation cycle re-derives "is this step already done?" from live Discord/Postgres state, so a crash mid-rotation self-heals on the next scheduled retry without duplicate channels or reposted stickies. That re-derivation has to cover the *whole* cycle, including the steps after the flip: an attempt that archived the old channel and then failed (recording the archive, retargeting the config) leaves the old channel renamed, so `archivedChannelOrigin` recovers its pre-archive name from the archive-name format plus its category, and the retry resumes at bookkeeping instead of rotating a second time. Replacement-channel matching is likewise name **plus** category rather than name alone: Discord allows duplicate channel names, and matching on name alone let an unrelated same-named channel elsewhere in the guild pass as an already-revealed replacement, at which point rotation archived a live channel and retargeted itself onto a stranger's. It flips visibility **new channel first** (create and populate the replacement before touching the original), trading the literal step order in spec.MD for a stronger safety property: the worst failure case is a brief moment where both channels are visible, never a moment with no live channel at all. Archived channels are swept for permanent deletion by a separate hourly `rotation-sweep` job per guild, once past `retention_hours` (nil = keep forever, never swept); a mod manually moving an archived channel out of its archive category is treated as an implicit "keep this one."

**The retention deadline is re-derived on every sweep, never read from a frozen column** (`archiveDeadline` in `sweep.go`). `rotation_archives.delete_after` is written once at archive time and nothing ever updates it, so treating it as authoritative meant a retention change only applied to *future* archives: an admin who widened retention (or switched to `retention_forever`) still watched the sweep permanently delete existing archives on the old, earlier schedule, and channel deletion has no undo. Tightening it leaked in the other direction, keeping content past the new window, which is the promise the whole feature exists to make. The sweep now lists a guild's archives and computes `ArchivedAt + the slot's current RetentionHours` per row, via `rotation_archives.rotation_id` (migration 0013): the stable `settings_rotation_channels.id`, not `source_channel_id`, which can't serve as the link because `rotate()` retargets a slot's `channel_id` every cycle (the same reason `archive_category_id` had to be denormalized in migration 0008). `delete_after` survives only as the fallback for pre-0013 rows and for archives whose slot was removed via `/rotation configure remove`, which promises existing archives are left untouched.

**`rotation-sweep` is only registered where it has something to sweep** (`reconcileSweepJob`): a guild with at least one rotating channel, or one with archives still pending. It used to be registered for every guild on `GuildCreate` regardless: a permanent-deletion job armed in servers that never opted into rotation, firing within one tick of startup since a job with no run history is immediately due, and harmless only because the table it read happened to be empty. Pending archives keep it alive after the last rotating channel is removed, so a retention window that's still owed is still enforced; a failed archive lookup leaves the current registration untouched rather than guessing in either direction. The top-of-`rotate()` reads (fetching the live channel and the guild's channel list) run concurrently via a plain `sync.WaitGroup`; the reveal/archive swap itself is deliberately *not* parallelized (see `execute.go`'s comment at that exact spot): it would reintroduce a window with **no** correctly-named live channel at all, a strictly worse failure mode than today's brief dual-visible window, for well under a second of saved latency.

### Audit and status rendering (`internal/audit`, `adminconfig/status.go`)

The audit log is read by a human under time pressure, and until recently it was not written for one: every entry was `ColorSuccess` (including permanent channel deletion), the title was the raw dotted action string, the actor was a bare snowflake, and automated actions were attributed to the literal word `system` with nothing saying what that meant.

- **`core.ActorSystem`** is the sentinel every automated call site passes (`rotation`, `roles`), and **`core.FormatActor`** renders it as "merlin (automatic)". The stored `audit_log.actor_id` keeps the sentinel: the row is the machine-readable copy, the embed is the human one.
- **`core.MentionUser`/`MentionChannel`/`MentionRole`** are used **at the call site**, so the durable row is readable too, rather than adding a render-time layer or changing `core.AuditWriter`'s signature (which would touch `core/registry.go` and both `fakes_test.go`). A mention still contains the raw snowflake, so a future audit search matches exactly as before. All three return `""` for `""`, which is load bearing: the permission audit lines build `role=%s user=%s` where exactly one is ever set, and `<@&>` renders as literal garbage.
- **`audit.actions`** maps an action to a human title and a severity colour, backed by a mechanical `humanize` fallback (`config.mod_role_added` → "Config: mod role added"). A table alone can drift from call sites; a mechanical de-dot alone never drifts but reads worse. The pair degrades to *readable*, never blank and never the raw key, which makes a missing row a polish gap rather than a bug. Unlisted actions get `ColorInfo`: never claim success for something the code does not recognise, never cry wolf either. `*.dryrun` actions wear `MoodIdle`, matching `/config pause`'s precedent that a bot doing what it was told has not failed.
- **The audit post stays on the raw session, not `discordguard`**, and this is deliberate: the guard refuses writes when a guild is paused or in dry-run, so routing audit through it would silence the trail exactly when somebody hit the emergency stop, and would guarantee the `rotation.dryrun`/`archive.dryrun` records (which exist *because* the guild is in dry-run) were never posted. `AllowedMentions` is therefore zeroed explicitly, as `scheduler.alert` does.
- **`scheduler.alert` takes `(jobKey, failures, cause error)`, not a preformatted string**, so the test hook and the real path cannot describe the same failure differently, and it posts an embed with the error in a `TruncateEmbedField`-bounded field. That last part is a bug fix, not polish: it was a `Content` string with no truncation, so an error over 2000 bytes made the send fail outright and the wedged-job alert reached nobody, which is the single failure the mechanism exists to prevent.
- **`/config status` names roles and admins** (`<@&id>`/`<@id>`, capped at `maxMentionsListed` with "and N more") rather than counting them, matching the channel lines on the same screen. Its severity is tracked as the body is built, not recovered by scanning the finished text for `❌`/`⚠️`/`⛔`: that read the control signal for the response colour out of prose this bot does not author (interpolated Discord and database errors), so any error string containing a warning glyph turned a healthy server amber. The emoji are cues for the reader now, not state. The body is `core.TruncateEmbedDescription`'d for the same reason: it interpolates unbounded error text into a single 4096-byte description, and over-limit rejects the whole message, so `/config status` would fail precisely when it had something long and important to report.

**Audit-failure policy**: an audit-embed post failure (e.g. `#bird-audit-log` not configured or deleted) must never fail the operation that triggered it: the actual action already succeeded, and the durable audit record write happens independently of the embed post inside `audit.Writer.Record`. Every call site logs-and-continues on a non-nil `Record` error; this was a real bug in `rotation/execute.go` (an audit failure was making the Scheduler treat a successful rotation as failed) fixed by matching the policy every other call site already used.

### Mood icons (`internal/core/embeds.go`)

Six drawings of merlin (`assets/merlin_{ok,error,warn,info,notice,idle}.png`), shown as the embed **thumbnail**. Not the author icon: Discord renders that at around 24px and circle-crops it, which turns a detailed square sprite into a smudge with its corners cut off.

**The mood is derived from the embed's colour** (`moodForColor`), not passed in. That is what let all ~113 existing `RespondOK/Err/Info/Warn` call sites pick up an icon without being touched: the colour already encodes exactly this distinction, and a second argument saying the same thing again is only an opportunity for the two to disagree. `core.WithMood` overrides it for the cases the palette cannot express, which today is `MoodIdle` on `/config pause` and `/config dryrun`: both report as warnings, and a paused bot is doing what it was told rather than failing.

**Attachments are derived from the finished embed** (`embedFiles`/`EmbedFiles`), never hand-listed. An `attachment://` URL with no matching upload renders as a broken frame, and nothing about the code that built the embed would look wrong, so reading the URLs back off the embed is what stops the two drifting apart. Any non-interaction sender (a DM, a channel post) must use `core.EmbedFiles(embed)` rather than listing `AvatarFile()` itself. `NewLandmarkEmbed` deliberately clears the thumbnail: the banner is already carrying the visual weight. **`NewEmbed` sets no footer and no timestamp**: together they drew a second "merlin, today at 14:32" line right under the one Discord already puts above every message the bot sends, so the identity now comes from the mood thumbnail and the palette alone. That is also why the avatar is no longer attached unconditionally (`referencesAvatar`): with nothing pointing at it, uploading it on every response was bytes and an extra entry in Discord's attachment list for an image nobody could see.

The icons were cut from a single 1024x1536 sheet (kept at `assets/source/`, not embedded) by throwaway tooling outside the repo module. Two details that mattered: the trim uses per-row/column ink counts rather than a plain bounding box, because one faint speck near a cell corner pinned the alert sprite's box to the whole cell and made that one icon render visibly smaller than the other five; and the alpha threshold was measured rather than guessed, since the sprites sit on a glow that fades to nothing and the resulting bounds are stable only above `0x4000`.

### merlin's voice (`internal/voice`)

Every member-facing message merlin writes herself comes from here. Not string literals at call sites, for two reasons: a bot that says the identical sentence every rotation reads as furniture rather than a character, and, more importantly, **some of what she says is load bearing**.

The split is the whole design. **Lines are data** (`lines/*.yaml`, `go:embed`ed) so they can be reviewed as writing; **the contract is code** (`keys.go`) so it cannot drift. Each `Key` declares a `Register` (playful for ambient surfaces, plain for moderation outcomes), its **required placeholders**, a length limit, and a compiled-in `fallback`.

**The required-placeholder rule is the point, and it cuts both ways.** Every `rotation.intro.full` line must contain `{cadence}` *and* `{retention}`, because that notice is the server's published retention policy and the code has already reported the cadence as though it were the deletion window once (see `rotation/templates.go`'s comment). Personality varies the wording; it can never vary the facts. The reverse matters just as much now that disclosure is configurable (see "Rotation disclosure modes" below): `loadCatalog` rejects any placeholder outside a key's `required` set, so a line that slipped `{retention}` into `rotation.intro.cadence` fails to boot rather than publishing a deletion window the guild deliberately withheld. That guarantee holds only while `spec.optional` stays empty on those keys, which is why the field is documented as deliberately unused rather than deleted. `loadCatalog` refuses to boot on a line missing one, exactly as `CommandRouter.Finalize()` refuses to boot on a command with no declared tier. It also rejects unknown placeholders (`{cadance}`), unbalanced braces (a malformed placeholder otherwise passes the "has none" check and posts a literal brace), fewer than `minLinesPerKey` (8) lines per key, a line duplicated within a key (which passes the count while quietly costing a permutation and doubling that line's odds), and the same em dashes/curly quotes CI rejects repo-wide. Each playful key deliberately spans calm to blunt rather than settling on one tone, grouped by band in the YAML so the balance is visible to whoever adds to it; `moderation.*` spans warmth only, never wit. Selection is uniform across the whole key, because a person is not in a consistent mood either. `Validate` is exported because it is the contract: **any future generator has to pass its output through it**, which is what makes "add an LLM later" a change of source rather than a second unchecked path into a public channel.

Consumers depend on `voice.Source`, never `*Speaker`, so that swap stays a constructor change. `Line` never returns an error (every call site is about to post something, and an error there just means silence); it falls back to the spec's compiled-in line, and returns `""` only if even that cannot render, which callers treat as "say nothing" rather than posting visible braces. Selection is random with **guaranteed** no-immediate-repeat per guild+key: one re-roll, then a deterministic step, because a plain re-roll still repeats about one time in twenty five with five lines and the one place people notice is a channel they are watching. `WithRand` injects the RNG, mirroring the Scheduler's `now func() time.Time`.

`PERSONA.md` is the brief the lines are written against, and is what a generator would get as its system prompt. Operator-supplied sticky messages are deliberately **not** routed through any of this: those are the guild's own words, posted verbatim in order.

Admin surfaces (`/config`, `/rotation configure`) are not in the catalog at all. They are read by someone making a decision, often under time pressure, where warmth costs scanning speed.

### Temporary Role Management (`internal/plugins/roles`)

Jail (snapshot-and-strip a member's roles, restore automatically or on demand) and timed single-role grants, mirroring rotation's shape end to end: a plugin-owned Postgres store (`role_jails`/`role_grants`, migration 0011) for runtime state (not `internal/settings.Store`, which is guild *configuration* only), and a per-guild `roles-sweep` Scheduler job (every 1 minute, not rotation's hourly cadence: jail/grant durations are realistically minutes-to-hours, not day-scale) that re-derives idempotency from live Discord state rather than trusting stored assumptions.

- **Jail is role-based, not per-member channel overwrites.** `/roles jail` replaces a member's roles with a single shared "Jailed" marker role (auto-created/reused per guild, mirroring rotation's archive-category pattern) plus any role `Permissions.CanManageRole` says the bot can't touch (kept in place, reported, never silently dropped, per spec.MD §4 item 4, previously written but never wired up until this plugin). Channel visibility is enforced entirely by the *Jailed role's own* per-channel permission overwrites (`jailchannels.go`): every channel gets a deny overwrite (View Channel, plus Connect for voice) except a guild-configured allowlist (`/roles configure allow-channel`, backed by `settings_guild.jail_allowed_channel_ids`). This means jailing/releasing a member is always exactly one `GuildMemberEdit` call, regardless of guild size; the alternative (per-member overwrites on every channel) would cost O(channel count) API calls per jail/release and doesn't scale.
- New channels don't automatically inherit the deny overwrite (no gateway listener for channel creation, by design; see `jailchannels.go`'s doc comment); `/roles configure sync-channels` recomputes every channel against the current allowlist and is also run automatically the first time the Jailed role is created for a guild.
- **The jail mutation records before it strips** (`applyJail`). Ordering is the whole correctness argument: stripping first and then failing to record left a member holding nothing but the marker with nothing tracking them: no sweep would release them, no `/roles release` would find a record, and it looks from the outside like an ordinary jail that never ends. Recording first can only fail the other way, and a record for an un-stripped member self-heals on the next sweep via the confused-deputy check below (marker absent ⇒ already handled ⇒ untrack, restore nothing); the explicit rollback just makes that immediate. Relatedly, the cached Jailed-role ID is dropped only on Discord's `ErrCodeUnknownRole` (`core.HasDiscordErrorCode`), never on any "unknown resource": the same `GuildMemberEdit` call reports Unknown *Member* when the target left, which says nothing about the role.
- **Confused-deputy re-check**: both the sweep job and on-demand `/roles release`/`revoke` re-fetch the member fresh before acting; if a mod already manually restored/changed roles (jail marker or granted role no longer present), that's treated as an implicit "already handled": stop tracking, don't fight the manual override. Same rescue-hatch pattern as `rotation.sweepOne`.
- **Jail survives leaving and rejoining** (`reapplyEvadedJails`, `rejoinedSinceJail` in `jail.go`). Discord does not preserve roles across a rejoin, so a jailed member who left and came back returned with no marker role, and therefore none of the channel denials the Jailed role carries, while `role_jails` still said they were jailed and nothing was checking. `member.JoinedAt` (free with the REST fetch the sweep already makes) is the discriminator: a join later than `JailedAt` is a rejoin and the marker goes back on; a missing marker *without* a later join is the mod-released case the confused-deputy rule above protects, and stays honored. That's why the check is `JoinedAt`-based rather than "marker missing ⇒ re-apply", which would have destroyed the rescue hatch. `rejoinGrace` absorbs clock skew between Discord's `JoinedAt` and this bot's own `JailedAt` stamp, so a member jailed seconds after joining isn't mistaken for an evader. Re-applying deliberately does **not** rewrite the stored snapshot (it holds the member's real pre-jail roles and is the only copy; overwriting it with what they hold on rejoin, i.e. nothing, is the same class of bug as the `ON CONFLICT DO UPDATE` jail race) or `ReleaseAt` (how long someone is jailed is a moderator's decision; leaving neither serves nor extends it). The bounded `ActiveJails` query (`maxActiveJailChecks`, newest-first) keeps the per-sweep REST cost proportional to concurrent jails. The privileged `GUILD_MEMBERS` intent additionally registers a `GuildMemberAdd` handler (`HandleMemberJoin`) that closes the ≤1-tick window to near-instant. It is requested **by default** (`MERLIN_DISABLE_GUILD_MEMBERS_INTENT` opts out); it was opt-in via `MERLIN_ENABLE_GUILD_MEMBERS_INTENT` and that was itself a bug: an operator who ticked "Server Members Intent" in the Developer Portal, the only step that looks like it should matter, got no behavior change because the bot never asked for the intent, and nothing anywhere reported the mismatch. Asking by default makes the portal toggle mean what it appears to mean. It has to be paired with `core.WatchReady`: `session.Open()` returns as soon as the identify is *sent*, so a portal toggle that isn't set produces close code 4014, which discordgo neither surfaces nor gives up on; it reconnect-loops while startup logs "merlin is running", leaving a process that holds no gateway connection and silently does nothing. `WatchReady` must be armed *before* `Open` (discordgo dispatches from inside it), and its timeout is deliberately fatal.
- Grant escalates (any role the bot can manage), jail only restricts, so grant/revoke are `TierAdmin`, jail/release are `TierMod`.
- **Bulk jail** (`bulkjail.go`): `/roles jail` takes up to five members via separate native `User` options (`collectJailUserIDs`; Discord has no multi-user option type, and spec.MD §4a forbids taking IDs as raw strings), and `/roles jail-role` jails every holder of one role. Both funnel into `jailMany`, so single and bulk can't diverge in behaviour, only in *reporting*, where one member keeps the original precise wording and its `roles.jail` audit action, and a batch gets one `roles.jail_bulk` entry (per-member audit records would post one embed each, blowing the guild's `message.send` budget for no gain; per-member state stays in `role_jails`). `jail-role` is a separate leaf, not an option on `jail`: Discord requires required options before optional ones, so folding it in would push `duration` ahead of the far more common single-member path, and a separate leaf gets its own `PermSpec`: `TierAdmin`/`roles.jail_role`, by the same blast-radius reasoning that already isolates `configure_jail_channels`, lowerable per guild via `set-tier`. `partitionByRank` runs the per-target `CanModerate` check **before** `resolveJailRole`, because resolving can *create* the role and write an overwrite to every channel: a batch the actor outranks nobody in must leave nothing behind. The `maxBulkJailTargets` cap is about reversibility, not Discord's limits: jail and release share the guild's `member.edit` budget, so a batch big enough to drain it would block the releases undoing it; over the cap the command refuses rather than jailing an arbitrary subset. `membersWithRole` pages the member list (Discord has no "members with role" endpoint, and this needs `GUILD_MEMBERS`), stops one past the cap, and returns `complete=false` rather than letting a truncated scan read as "this is everyone". The cap is checked on the **raw** match count before self/bot exclusion, or trimming two names could bring an apparent 200 down to 50.

### AI Moderation (`internal/plugins/aimod`)

Milestone 10. Reads messages and checks them against Discord's own Community
Guidelines, removing or rewriting what breaches them. Requested intent is
`MESSAGE_CONTENT`, **opt-in** via `MERLIN_ENABLE_MESSAGE_CONTENT_INTENT` (the
opposite default from `GUILD_MEMBERS`, because it is every message in every
guild and spec.MD's least-privilege section names it specifically). Without it
the plugin registers its commands, scans nothing, and says so in `/aimod
status` rather than appearing to work.

- **The ladder, and the one rule that must not move.** Rung 0 is a free skip
  filter (bot/webhook author, exempt channel/role, too short, content-hash
  dedupe); rung 1 is a deliberately tiny regex table for things that are
  unambiguous *and* urgent (leaked bot token, phishing/IP-grabber domains, an
  SSN); rung 2 is a cheap model over a 1.5s micro-batch of up to 20 messages;
  rung 3 is a better model on one message with the full policy file and five
  lines of channel context. **A rung-2 hit can only ever `flag`.** Deleting or
  rewriting requires rung 3 to have confirmed above `actThreshold`. That single
  rule is what keeps a nano model's false positives off a free-speech server,
  and it is also why the expensive tier stays affordable: it only ever sees the
  ~1% that already tripped. A failed or unparseable deep pass acts on nothing;
  treating an outage as a confirmation would let Discord being down delete
  messages.
- **The policy catalogue is data, validated like `internal/voice`'s.** Ten
  `policy/*.yaml` files, `go:embed`ed, each carrying `violations` **and**
  `not_violations`, both with a `minListItems` floor. The second list is the
  half that matters here: Discord's own explainers mostly state no exceptions,
  so those lines are derived from the definitions' own qualifiers ("with the
  intention to cause harm", "intentional actions meant to cause distress").
  Without them a general-purpose model reads "hate speech" as "rude" and this
  plugin becomes the thing it was built to protect the server from. A file
  missing a boundary fails `LoadPolicies` at startup, exactly as a voice line
  missing a required placeholder does. `child_safety` is not disableable, the
  same guard shape as `adminconfig` refusing to disable itself.
- **`child_safety` is the one bucket where humour is not a defence**, and it
  has to say so in its own file because every other force in this package
  pushes the other way: `systemPreamble` tells the model dark humour is
  ordinary here and to report nothing when a message is ambiguous, and a joke
  framing is exactly what makes one ambiguous. Discord's policy has no such
  exception. The gap that exposed this was narrower than the posture, though:
  every `violations` line described content *involving* a minor (sharing,
  soliciting, roleplay, grooming), so a first-person boast about sexual
  interest matched nothing and a model following the file correctly cleared
  it. Stated interest is now in `short` too, since that single line is all
  rung 2 ever sees and nothing reaches the deep pass unflagged. The added
  `not_violations` lines are load bearing in the other direction: on a blunt
  server "you're a pedo" is an ordinary insult, and a bucket that started
  removing those would be the over-broad censor this catalogue exists to
  prevent. Relatedly, `validateCalibration` refuses any example that tells the
  filter to stand down on this bucket: the weekly reviewer is asked to hunt
  for over-strictness, is told irony is the default reading, and on
  `CalibrationAuto` applies its own answer, so one such example would reach
  every prompt from then on. That is turning the bucket off by a route
  `EffectiveAction` does not guard. Examples that *tighten* it stay allowed.
  The guard is a backstop, not the mechanism: `calibrationPreamble` carries
  the same carve-out the policy file does (the irony-is-the-default-reading
  rule is scoped to exclude this one bucket) and routes a genuine overreach
  to a `too_strict` **finding** instead, which a moderator reads in
  `/aimod calibrate show` and answers by editing the policy file. So the
  observation survives and only the unattended 4am self-application is
  blocked. `TestCalibrationPromptCarriesTheSameChildSafetyRule` is what stops
  the const and the YAML drifting apart, since a reviewer reporting the
  filter as too strict for enforcing a policy it must enforce is the worst
  version of this mechanism.
- **Two-stage token use is the whole cost design.** The fast prompt carries one
  `short` line per *enforced* bucket and nothing else (~350 tokens, amortized
  across the batch); the full policy file is sent only to the deep pass, only
  for the one flagged bucket. Turning a bucket off is therefore a cost saving
  as well as a policy choice. Every request pins `temperature: 0`, so a
  classifier answers the same twice; the `provider: {zdr: true,
  data_collection: "deny", require_parameters: true}` block is per gateway,
  and see "Two gateways" below for why that is now a statement with an
  exception rather than an invariant.
- **Budget and its failure directions.** `usage.cost` comes back on every
  OpenRouter response (and on an OrcaRouter one as `usage.cost_usd`, but only
  when `X-OrcaRouter-Include-Cost` asked for it; `chatOnce` folds the two into
  one field the moment the body is parsed, so nothing downstream knows there
  are two gateways), so the per-guild daily cap is an exact figure rather
  than an estimate. Usage is booked for any response carrying it, *including*
  one whose body failed to parse: the money left the account either way, and a
  budget that only counts successes is one a misbehaving model walks through.
  An unreadable spend row fails **closed** (`checkBudget` returns exhausted),
  because assuming zero spent turns an unreachable database into an uncapped
  budget. Over the cap the plugin degrades to rungs 0-1 and posts one audit
  entry per day, never a silent downgrade.
- **Cost-drain abuse is a first-class attack, not spam.** The daily budget caps
  the bill; the damage it does not cap is the hours of unprotected server after
  somebody exhausts it, which is exactly what a person planning to post
  something reportable would arrange first. So `userMeter` adds per-member
  sliding-window ceilings, tighter on the deep rung (`maxUserDeep`) than the
  fast one because the two cost two orders of magnitude apart. A member over
  the deep ceiling is still **recorded as flagged**, never silently dropped, or
  the ceiling itself becomes a way to bury a real violation inside a flood.
- **Sanctions use jail, not Discord's timeout.** `roles.JailAutomatic` (added
  for this) is reached through the narrow `aimod.Jailer` interface wired in
  `cmd/bot/main.go`, so this package never imports `roles`. Duration scales
  with the policy file's own `severity` and doubles per prior sanction in
  `repeatWindow`, capped. A timeout is the fallback for when jail is genuinely
  unavailable. The sanction row is written **whether or not the jail lands**,
  because it is what the next offence counts; losing it to a brief Discord
  outage would quietly reset somebody's history. Reversed incidents do not
  count, so one false positive cannot lengthen every future sentence.
- **Automatic actions must never be aimable at staff.** `JailAutomatic` runs
  `CanModerate` with a nil actor (which refuses every admin-equivalent target
  and allows everyone else, precisely the rule an automated caller needs) and
  `timeoutMember` refuses the owner and anyone holding Administrator, Moderate
  Members or Manage Messages, failing closed on roles it cannot resolve. The
  `/aimod moderate-me` opt-in list waives *only* the rank check and is **purely
  additive**: nothing reads it to decide whether to protect anyone, anybody can
  add themselves, and only the bootstrap operator can add somebody else. The
  bootstrap identity is never sanctionable, listed or not.
- **Rewrite is delete-and-repost through a channel webhook** wearing the
  author's name and avatar, because a bot cannot edit another user's message.
  The webhook is resolved *before* the delete, so a webhook failure does not
  silently downgrade a rewrite into a removal. `discordguard.WebhookExecute`
  zeroes `AllowedMentions`, which matters more here than anywhere else in that
  file: the reposted text is member-authored and moments old. The
  "edited by merlin" marker is not optional; the repost carries somebody's name
  and is not what they wrote.
- **The incident is recorded before the message is touched** (same ordering
  argument as `roles.applyJail`): the other order leaves the message gone with
  no copy of it, nothing for `/aimod undo`, and nothing to show the member.
  Evidence retention is per guild and **re-derived on every prune** by joining
  `aimod_config`, never frozen into a per-row column: that is the
  `rotation_archives.delete_after` mistake pointing the other way, and here the
  direction that has to work is *shortening* the window.
- **Config is cached** (`cachingStore`, `configTTL`) because this is the only
  hot path in the codebase: without it a busy guild pays a Postgres round trip
  per message. Every setter invalidates; a new setter added to `Store` and not
  listed there serves a stale config for up to the TTL, which is most of why
  the TTL exists at all.

### Rung 1.5: the local triage model (`internal/plugins/aimod/triage.go`)

A per-guild logistic regression over hashed character n-grams that decides
whether a message is worth a model call at all. Roughly a hundred lines of
`math` and no new dependency.

**It approximates rung 2 rather than judging policy, and that framing is the
safety argument.** Rung 2 is already the gate for everything below it: nothing
reaches the deep pass unflagged. So this rung predicts *the fast pass's own
output* and its only decision is whether to spend that call. It can skip or
pass through. It can never flag, remove, rewrite or sanction, which is the same
rule that keeps rung 2 off the delete path, and it means the rung adds its own
error rate on top of rung 2's without introducing a new class of miss.

**Training is online, from verdicts, with nothing retained.** The label is what
the fast pass just decided, and the text is the copy already in hand; both are
discarded when the batch returns. There is no training set and no table of
examples, which is deliberate: a nightly pass over stored messages would mean
storing messages, and `aimod_incidents` holds only what was acted on (no
negatives) and prunes its content on `evidence_hours` anyway. `aimod_triage`
holds a block of gradient-updated float32s and nothing a member wrote is
recoverable from it, so continuous learning extends this plugin's retention by
zero bytes. This is the one place the milestone plan was wrong and the code
deviates from it on purpose.

**Five things stand between the model and a missed violation**, and each fails
toward scanning:
- `triageWarmup` (500 examples) before it may skip anything, so a fresh guild,
  a new deployment and a restored backup all behave exactly as before.
- `triageSkipThreshold` (0.02) is not "probably fine", it is "the model has
  essentially never seen the fast pass flag anything like this".
- `neverSkipPattern` vetoes the child-safety vocabulary outright, checked
  *before* the model is consulted so no amount of confidence routes around it.
  Deliberately over-inclusive: a false positive costs one call that would have
  happened anyway, and it is not a detector, since nothing acts on a match.
- `triagePosWeight` (12) is what stops the model collapsing to always-clean.
  About 1% of messages are flagged, so the loss is minimised by answering
  "clean" to everything, and that model is right 99% of the time while skipping
  every violation on the server.
- `triageSampleRate` (5% of would-be skips) is scanned anyway. **This is not a
  hedge.** A model that skips a region of its input stops receiving labels from
  it, so its picture freezes on the day it started skipping and later drift is
  invisible precisely where it is trusted most. Sampling keeps labels flowing
  from the skipped region and is the only honest measure of the miss rate,
  which `/aimod status` reports as "missed N of M sampled".

**`triage_mode` is off/shadow/on, defaulting to shadow**, the same
suggest-before-auto shape as `calibration_mode` and for the same reason: this
changes what gets looked at, so a guild watches it work rather than discovering
it. Shadow scores and learns and changes nothing, which is what makes the
`/aimod status` figures evidence an admin can act on. The counters behind them
(`considered`/`wouldSkip`) are incremented in every mode for exactly that
reason.

The skip sits ahead of `userMeter.allowScan` in `HandleMessage`, and behind the
dedupe lookup. Ahead of the meter because a message that was never scanned must
not draw on the member's scan ceiling, or ordinary chatter would exhaust the
quota that exists for content nobody has judged; a sampled message is exempt
too, since the audit must not spend somebody else's protection. Behind the
dedupe because that answer is remembered fact and this one is a guess, and a
guess must never override a verdict already reached on identical text.

Weights live in `aimod_triage` rather than on `aimod_config`, the same split
the tip jar uses: they are runtime state written every `triageSaveEvery` (500)
examples and on `Shutdown`, and the config row behind `cachingStore` is read on
every message. Persistence matters because this bot redeploys on every push to
main, and a model that never saved would be cold most of the time. A stored
blob of the wrong length is refused rather than reinterpreted: it means the
table size changed between releases, and old weights against a new hash space
give a model that is confidently wrong about everything, where starting again
is only a warmup during which nothing is skipped.

### Two gateways (`internal/plugins/aimod/provider.go`)

`providerSpec` is a table of two: OrcaRouter, the default, and OpenRouter, the
fallback. Not an interface with two implementations, because the differences
are five fields and a base URL, and a type with methods would mean reading two
files to learn what the second one does differently. The gateway is carried on
the unexported `chatRequest.spec`, so it never reaches the wire, and **nil
means OpenRouter on `c.base`**, which is what every request meant before there
was a second gateway and is what keeps the tests that predate one honest.

**Which gateway a guild is on is derived, never stored** (`route`): OrcaRouter
if it holds an `orca_key_sealed`, OpenRouter otherwise. A provider column
beside a key column is two facts that must agree, and what a disagreement
produces is a guild whose scanning silently stopped pointing at a gateway it
has no credential for. The same reasoning runs through the whole change:
`/aimod configure key` reads the gateway off the key's own prefix (with an
explicit option only as the escape hatch for a gateway that changes what its
keys look like), and the tip jar reads its chain off its address.

**OrcaRouter is free, and that is the point**: `orcarouter/free` routes by
difficulty across the free lineup at $0 per token, limited by request rate
rather than balance. Three things follow that are correctness issues rather
than cosmetics. It is OpenAI-shaped, so it takes singular `model` and not the
`models` fallback array, and a request carrying neither is refused outright.
Its cost arrives as `usage.cost_usd` only when a header asks, and a gateway
whose every call reads as free turns the daily cap into no cap at all.
And its 429 is not the ordinary kind: a free window refills at its boundary
(the minute bucket, or the day bucket at 00:00 UTC) rather than easing back,
so `Retry-After` can be hours and the usual answer of waiting a moment and
retrying the same gateway is the one thing certainly wrong. The lowest tier's
oversized-prompt refusal reports *identically* and never passes on a retry at
all, so `freeRateLimited` refuses to retry either and the deep rung falls over
instead.

**Only the deep rung falls over** (`escalate`). It is the rung whose verdict
can delete or rewrite, its gateway is a free tier, and there is no OrcaRouter
equivalent of `require_parameters`, so a model that quietly ignores the JSON
schema is a real failure mode there. That arrives as an error exactly as a
rate limit does, and no attempt is made to tell them apart: both mean this
gateway produced no verdict, and a failed deep pass acts on nothing, which is
safe and also useless. The fast rung deliberately gets none of this, since it
can only flag, everything it flags is re-read anyway, and re-running a twenty
message batch at OpenRouter prices is the bill the default gateway exists to
avoid.

**The privacy loss is disclosed rather than papered over.** OrcaRouter
documents no per-request equivalent of the `provider` block, so `strictPrefs`
is false for it and `chatOnce` *strips* the block rather than sending one that
may be silently ignored: a guarantee the surrounding code still reads as
holding is worse than one never claimed. `/aimod status` names the active
gateway and states plainly, via `privacyLine`, that ZDR is not enforceable on
it and how to move back.

### The tip jar (`internal/plugins/aimod/funding.go`, `erc20.go`)

`/aimod funding` is a per-guild donation wallet shown next to a fuel gauge for
the gateway credit that pays for scanning. **The bot holds no key and moves
no money, and that is forced rather than chosen**: OpenRouter removed the
programmatic crypto purchase endpoint (`POST /api/v1/credits/coinbase` now
returns `410 Gone`) and their Auto Top-Up charges a saved card, never a wallet;
OrcaRouter's crypto top-up is a NOWPayments checkout, and NOWPayments having an
API is not a reason to custody anybody's donations. No credential turns crypto
into credits unattended, so custodying would buy nothing while putting the
money behind a Discord bot's threat model. What is automated is the
*visibility*: balances, burn rate, runway, and a once-per-day
`aimod.funding_low` audit entry telling the operator to go click the checkout.

The gauge reads whichever gateway the guild is routed to. `Client.KeyInfo`
(`GET /key`) for OpenRouter, `Client.OrcaBalance` for OrcaRouter, which is two
OpenAI-shaped billing calls (`/dashboard/billing/subscription` for the ceiling,
`/usage` for the spend, **in cents**) subtracted into the *same* `KeyInfo`
struct. Mapping onto the existing struct rather than adding a second one is
what lets `bar`, `runway`, `checkCredit`, `noticeFunding` and both renderers
stay gateway-agnostic, nil-pointer "no limit set" branch included. A free-tier
guild has no balance to draw at all, and both surfaces say so rather than
rendering an empty bar: what runs out there is a request rate.

**An address selects a family, not a chain** (`familyFor`). A `0x` address is
the same account on Base, Ethereum, Polygon, Arbitrum and BNB Chain, because
one private key controls all five, so "which chain is this address on" has no
answer to derive. merlin reads every rail in the family and sums them, which
deletes the derive-versus-store dilemma rather than solving it: nothing about
the chain is stored, nothing is guessed, and a donor sends on whichever listed
network is cheapest for them. `T...` is TRON, and a 32 byte base58 address is
Solana. The three forms cannot collide, since a TRON address decodes to 25
bytes and a Solana one to 32, which needs at least 43 base58 characters.

**`fundingRail` and `providerSpec.topUpChains` are deliberately separate,
because they are facts of different kinds.** A rail is a property of a
blockchain: stable, verifiable from here, and every contract in the table was
checked live against its own `symbol()` and `decimals()` before being added.
That check is not ceremony; it caught BNB Chain's **18** decimals, where the
usual assumption of 6 would have reported a one dollar donation as a trillion,
and the table already had to dodge Base's USDbC and Polygon's USDC.e, the
latter reporting the symbol `USDC` identically to the native token. What a
gateway's checkout *accepts*, by contrast, is somebody else's merchant
configuration: it can change without notice and this bot cannot observe it. So
everything rendered from `topUpChains` is dated (`topUpCheckedOn`) and hedged,
and everything rendered from a rail is not. The previous version conflated the
two and told donors that Ethereum mainnet USDC would be lost, which
OpenRouter's checkout takes perfectly well: their flow runs on Coinbase
Commerce's onchain payment protocol, which settles supported tokens across the
Ethereum, Polygon and Base ecosystems. OrcaRouter's NOWPayments checkout has
only been seen offering TRON, and NOWPayments supporting ~350 assets is a fact
about the platform rather than about that merchant's configuration.

**A partial read must never be booked.** `Balances` reads a family's rails
concurrently and is all or nothing, because a sum missing one unreachable rail
is indistinguishable from a smaller balance, and `pollFunding` reads a fall as
the operator withdrawing to buy credits. One dead RPC would therefore record a
withdrawal that never happened and then book the recovery as a donation nobody
made. An error costs one fifteen minute retry. `set-address` runs the same
read for the same reason: a baseline banked while one chain was unreachable
would report that chain's existing holdings as a gift on the next poll.

`aimod_funding.balances` (migration 0027) stores the per-rail breakdown behind
the total, written only by a complete poll, because `/aimod funding show` is
`TierPublic` and must not fan a dozen RPC calls out on a member's command. The
public view renders only rails holding something (a family has up to nine, and
eight zeroes would bury the useful line), each linked to that chain's explorer,
plus a whole-address explorer link so anybody can audit the jar without
trusting merlin's arithmetic. For an EVM address that link is deliberately
Etherscan's multi-chain view: a single per-chain explorer would show one of
five ledgers and imply the other four were empty. The network list carries full
weight and the caveat below it does not, since with several chains accepted
"which networks are OK" is the actionable fact.

TRON and every EVM chain share one code path: TronGrid answers Ethereum's
JSON-RPC for reads, so the same `eth_call`, the same `balanceOf` selector, the
same left-padded argument and the same `math/big` parse all stand. What TRON
needed was a base58check decoder (`tronAddressToHex`, on `math/big` and
`crypto/sha256`) for the `T...` form, applied to the **token contract as well
as the wallet** since both fields want Ethereum-shaped addresses. Solana is the
one rail read differently, with `getTokenAccountsByOwner` rather than
`eth_call`, marked by a single bool on the rail rather than a second client
type since the timeout, the body cap and the HTTP-200-carries-a-JSON-RPC-error
trap are all identical. Its accounts are **summed**, because an owner can hold
several token accounts for one mint and reading the first would under report
money somebody genuinely sent.

Address validation is deliberately unequal and says so. A TRON address carries
a checksum, so `set-address` genuinely catches a mistyped one. A Solana address
is a raw ed25519 public key with **no** checksum, so a typo that still decodes
to 32 bytes is accepted and cannot be caught short of asking the chain; an EVM
address is not EIP-55 checked either, since a fully lowercase one is valid.
`TestSolanaAddressValidation` pins that asymmetry so nobody later assumes
parity between the families.

**Stablecoins from a fixed table, not arbitrary tokens** (a `ponytail:` note
marks the ceiling). Reading whatever a donor might send needs a token indexer
plus a price oracle, which is an API key and a dependency for something a donor
solves by swapping first. Overrides are `MERLIN_RPC_<CHAIN>` per chain and
`MERLIN_TOKEN_<CHAIN>_<ASSET>` per rail, scanned by prefix so adding a rail
needs no loader edit; the four superseded single-chain names still map onto
base and tron so a deployed `.env` keeps working. An endpoint and its token
contract must be set together: an endpoint on one chain with a token address
from another reports a zero balance rather than an error. Reading the balance is one `eth_call` with
the `balanceOf` selector and a left-padded address, parsed through `math/big`
(a `uint256` does not fit a `uint64`), so there is no web3 dependency. A
JSON-RPC error arrives with **HTTP 200**, so status-only checking would read a
rejected call as a zero balance.

**Only the guild owner or the bootstrap operator may set the address**
(`canSetFunding`), enforced in the handler because `PermSpec` cannot express
either identity, exactly as `/aimod moderate-user` does. `TierAdmin` is only
the coarse floor: a guild with five admins would otherwise have five accounts
able to silently repoint where donated money goes, and nothing sent on chain
can be recovered. It fails closed on an unresolvable guild. `clear` is
deliberately open to any admin, because it can only ever stop donations and
never redirect them. Every change writes an audit entry naming both addresses,
and the public view names who set it and leads with a warning for 24 hours.

**Donations are balance deltas, not parsed transactions.** The first poll after
an address is set is a *baseline* and counts nothing, or pointing the bot at a
wallet that already holds money reports a phantom gift on day one; the baseline
is written with the address in one statement, from the balance the `set-address`
command already read. A fall in balance is the operator buying credits, never a
negative donation. Two gifts inside one 15 minute poll read as one, which is
why the display says "donations" and never "donors". `aimod_funding` is a
separate table from `aimod_config` on purpose: the poller writes every 15
minutes and `aimod_config` is the hot-path row behind `cachingStore`, so these
setters are the only ones in the package that correctly need **no** cache
invalidation. The job is registered only where there is a wallet and is
deliberately **not** `Seed`ed, the opposite of the calibration job beside it: a
freshly set address should show a balance on the next tick, and its first fire
costs one public RPC call rather than a model bill.

**`funding/show` is the one command that answers the channel rather than the
invoker**, via `core.DeferResponsePublic`. Everything else in this bot is
ephemeral, which is right for a surface read to make a decision and wrong for
one whose purpose is to be seen by other people. It has to be a separate defer
rather than a flag on the follow-up, because Discord fixes ephemerality when
the interaction is acknowledged: deferring privately and editing in a public
answer silently stays private. Use it only where the answer contains nothing
the invoker would mind posting in the channel they ran it in, which is why the
mod-facing figures stay on `/aimod status`. `followUp` zeroes
`AllowedMentions` for the same reason `discordguard.GuildOps` does: embeds
cannot ping today, and that is what keeps it true if a `Content` line is ever
added to a public path. The field renders in three weights (fenced address,
bold chain warning, `subtext()` for guidance and provenance) because sending
on the wrong chain is the mistake people actually make, and the money does not
come back.

`funding.ask`/`thanks`/`low`/`dry` are in the voice catalog because
`funding/show` is `TierPublic`; the address, both balances, the runway and the
"funds go to whoever controls it" warning are **code-authored embed fields**,
since `voice.Line` selects at random and falls back silently, which is right
for a greeting and wrong for the sentence saying where somebody's money goes.
Member-facing durations render through `humanRunway` ("6 days"), the audit and
`/aimod status` ones through `core.FormatDuration` ("6d").

### The browser lab (`cmd/lab`, `internal/lab`, `web/lab`)

merlin's own packages compiled to WebAssembly, so two of this codebase's
promises stop requiring a Go toolchain to collect on. `internal/voice` is built
so its lines are data that can be reviewed as writing, and reviewing one meant
running the bot; the rotation notice is a guild's **published retention
policy**, and the only way to see what a channel would be told was to configure
it in a live server and wait.

**Nothing in the lab reimplements anything, and that is the entire design.** A
JavaScript simulator would be a second copy of the schedule arithmetic and the
disclosure rules, and a second copy drifts. What it would drift on is a
deletion window somebody had already published. So the page calls
`rotation.RetentionNotice`, `rotation.HeadsUpNotice`, `rotation.ValidateChannel`,
`core.IntervalSchedule.Next` and `voice.Validate`: the same functions the bot
calls, which is what justifies shipping a several-megabyte binary to a browser.

Those four `rotation` identifiers were exported *for* this, and the bot's own
call sites now route through them (`retentionNotice` is a one-line delegation),
so there is exactly one path and the preview cannot diverge from what gets
posted. Exporting was cheap because none of them needed anything from `Plugin`
beyond the speaker.

The split is `internal/lab` (no build tag, plain Go, tested natively) under
`cmd/lab` (`//go:build js && wasm`, pure JSON-in/JSON-out marshalling). Logic
that cannot be reached by `go test` is therefore about forty lines of glue, and
`scripts/check-lab.mjs` drives the real compiled blob in Node to cover even
that. CI builds `cmd/lab` for js/wasm on every push, because a signature moving
under the binding breaks no test and would otherwise rot silently.

Size is ~13.7MB raw and ~3.6MB gzipped, and it measured **identical** with and
without the `aimod` package: the Go runtime dominates and dead-code elimination
handles the rest, so scope here is not size-constrained. TinyGo would cut it
substantially and is deliberately not used, since it would add a second
toolchain and does not support everything the real packages rely on.

The build (`scripts/build-lab.sh`) copies `wasm_exec.js` out of the local
`GOROOT` rather than vendoring it: it is part of the runtime and must match the
compiler that produced the blob beside it, and a committed copy drifts silently
on a Go upgrade into a page that loads and then does nothing. Both it and
`lab.wasm` are gitignored.

**There is deliberately no authentication.** The repository is public, so every
line, policy file, pattern and rule the lab renders is already readable on
GitHub and gating the page would protect nothing. Discord OAuth specifically
would be worse than nothing here: the authorization-code flow needs a client
secret, which means a server, and merlin is outbound-only by design. Adding an
inbound HTTP surface to a bot whose threat model is weaponized reporting, in
order to guard public information, is the wrong trade. If the page ever does
need gating (keeping it out of search results, say), the answer is an identity
proxy in front of the static files, not auth code in this repository.

Also fixed on the way in: `internal/config`'s reload files were tagged
`windows` / `!windows`, so js/wasm matched the SIGHUP one and the whole tree
failed to compile for a browser. They are now `unix` / `!unix`, and the no-op
was renamed off the `_windows.go` suffix, because a GOOS filename suffix
carries an implicit constraint that ANDs with the explicit tag and would have
left js/wasm with no definition at all.

### Rotation disclosure modes

`settings_rotation_channels.disclosure` (migration 0018, default `full`) is how much a freshly rotated channel is told about its own rotation: `full` (cadence + archival window), `cadence`, `retention`, or `generic` (neither). Per channel rather than per guild, matching `retention_hours` itself. Set via `/rotation configure add|edit`, a fixed four-value `Choices` option rather than autocomplete, since §4a's autocomplete rule is about values that come from bot state and cannot be enumerated at compile time.

The four modes cross with bounded-vs-forever retention onto **six** voice keys (`rotation.intro.full`, `.full_forever`, `.cadence`, `.retention`, `.retention_forever`, `.generic`); `cadence` and `generic` say nothing about the archive and so need no forever variant. `rotation.introKey` returns the key **and** its vars together, deliberately: `voice.Line` falls back silently on a missing placeholder, so a selection split across two switches would degrade every notice for that mode to the compiled-in line with nothing saying why. `plainRetentionNotice`, the catalog-failed backstop, switches on the same mode for the same reason in the worse direction: a fallback that ignored the mode would publish exactly what the guild suppressed, at the one moment nobody is reading the logs.

**Three layers keep an illegal mode out**: `validateRotationChannel` rejects it at the point of mutation, `UpsertRotationChannel` normalizes the empty value to `full`, and a `CHECK` constraint on the column is the backstop against a hand-edited row. Unlike `archive_visibility` next door (bare TEXT, designed to grow per spec.MD §6), this is a closed set: a fifth value would need code in `templates.go` and lines in the catalog before it could mean anything. Anything unreadable resolves to `full`, since over-disclosing is a wording problem while under-disclosing would silently retract a promise the guild believes it is still making.

`archive_visibility: whitelist` is no longer offered by `/rotation configure`, because nothing but `/config import`'s legacy YAML path ever populated the whitelist arrays, so selecting it produced a mode that behaved identically to `mod_only`. `archiveOverwrites` still implements it and `validateRotationChannel` still **accepts** it: `edit` is a read-modify-write of the whole struct, so rejecting it would refuse an imported guild's every future edit over a field the admin never touched and can no longer select.

### Pre-rotation heads-up

`settings_rotation_channels.notice_lead_minutes` (migration 0017, default 10, 0 = off) is how long before a rotation the channel is warned. Validation keeps it strictly below the interval: a lead at or beyond it would leave a permanent "about to wipe" banner and the words would stop meaning anything. Under a minute out the notice is skipped without consuming its claim, since `remaining.Round(time.Minute)` renders "0 minutes" and the rotation fires on the next tick anyway: same reasoning as the overdue case, wrong in the one direction that matters.

The heads-up is an embed (`ColorWarning` → `MoodWarn`), not bare content. It used to be the one member-facing message that was not, while its sibling intro notice in the same channel minutes later was a branded embed; the pair now reads as escalate-then-settle against the intro's `ColorPrimary`/`MoodNotice`. Its countdown renders through `humanDuration` ("6 minutes"), not `core.FormatDuration` ("6m"): **every member-facing duration is prose, every admin/audit one is compact**, and those two messages land in the same channel minutes apart. A channel on `generic` disclosure gets `rotation.headsup.generic`, which carries no countdown at all, since a countdown *is* the rotation schedule. Lead and disclosure otherwise stay independent switches: disclosure decides how much is said, `notice: off` decides whether anything is.

One `rotation-notice` job per guild on a 1 minute schedule, registered only where some channel actually wants one (`reconcileNoticeJob`, same "a job exists only where it has work" rule as `reconcileSweepJob`, and it assumes the caller holds `p.mu` exactly as that one does; taking the lock again deadlocks, which is how it was first written). It asks `Scheduler.NextDue` rather than recomputing last-run plus interval, so the number in the message and the moment the rotation happens cannot drift apart.

**Idempotency is a database constraint, not an in-process check.** The job runs every minute and the lead window is many minutes wide, so without a claim every tick inside the window would post again. `rotation_notices` is keyed on `(rotation_id, notice_for)` where `notice_for` is the instant the notice is *for*, not when it was sent, so two overlapping runs compute the same key and `INSERT ... ON CONFLICT DO NOTHING` lets exactly one win. **Claim before posting**: the other order double-warns whenever the write fails, and being told six times that a channel is about to wipe reads as a broken bot, whereas a missed notice is invisible. An overdue rotation (`NextDue` says not-ok) gets no notice at all, since it fires on the next tick and a countdown would be wrong in the one direction that matters.

`core.ParseFlexibleDuration` accepts `m` as well as `h`/`d`, and `FormatDuration`/`humanDuration` render minutes when hours would lie. This was a real gap: migration 0016 made intervals minute-precise and the option descriptions started advertising `"90m"`, but the parser still rejected anything with an `m` on it, so the one input the change existed to enable failed with "isn't a valid duration". The test suite was green throughout, because a test asserted the old contract (`"90m"` must be rejected).

### Rotation interval and placement

Interval is stored as `settings_rotation_channels.interval_minutes` (migration 0016; it was `interval_hours`, and `/rotation configure` parsed a flexible duration then truncated it with `int(interval / time.Hour)`, silently rounding `90m` down to an hour). `minRotationInterval` (1h, in `configure.go`) is now a validation floor rather than a storage granularity, which is the right place for it, since the reason for a floor is the guild's 20/hour channel-create budget and member whiplash, not the column's type. Anything at or above the floor is allowed to the minute. `internal/settings/import.go` still parses `interval_hours` from legacy YAML (that file format is fixed history) and multiplies on the way in.

`restorePosition` (`execute.go`) puts the replacement back in the slot the original occupied, and **must run after the archive**: while both channels are in the category they compete for the index, and Discord breaks ties between equal positions by channel ID, so the newer replacement sorts below the channel it replaced. Setting `Position` at create time is undone the moment the old channel moves out. It is deliberately non-fatal and skipped on the resumed-after-archive path (where the old channel's position is its position *inside* the archive category): by that point the rotation is already correct, and returning an error would have the Scheduler retry `rotate()`, which creates a second replacement. Same log-and-continue policy as audit failures.

### Security model (spec.MD §4, enforced in code, not just documented)

- Layered authorization: `core.CommandRouter` → plugin-enable gate → `Permissions.Authorize` (deny → tier → allow, see above) is the mandatory check on every command; `Permissions.CanManageRole` enforces the bot can never touch a Discord role positioned above its own top role.
- **Rank check on restrictive actions** (`Permissions.CanModerate`, called by `/roles jail`): the tier layer asks "may you run this command", never "may you run it *against this person*", and that gap is exploitable in exactly one place. Jail is `TierMod` and strips every manageable role, so for an admin whose authority comes from Discord's Administrator bit, being jailed also strips their `TierAdmin` access to this bot, so a rogue mod could dismantle a guild's whole admin team one jail at a time with every individual action looking perfectly authorized. `CanModerate` refuses when the target is admin-equivalent (DB-listed, bootstrap, Discord Administrator, or guild owner) and the actor isn't, and refuses bootstrap as a target for *everyone*, matching the deny-list's own carve-out. Deliberately narrow: mod-on-mod moderation stays allowed, and admins remain peers. It fails closed: an unresolvable guild state refuses the jail rather than assuming the target is ordinary. Note `member.Permissions` can't be reused for this: Discord computes that bitmask only for the interaction's own member, so an arbitrary target's Administrator bit has to be re-derived from the guild's roles.
- Fail safe, not fail silent: an unset `PermTier` fails `Finalize()` at startup; a missing top-level handler for a declared subcommand also fails `Finalize()`.
- **Only untrack on "gone", never on "failed"** (`core.IsUnknownResource`): every place this bot drops its own tracking state because a Discord resource seems to have disappeared (`rotation.sweepOne`, `roles.releaseJail`, `roles.revokeGrant`) must first confirm Discord actually said the thing doesn't exist (a 100xx "Unknown ..." code), and otherwise fail so the next sweep retries. Treating a rate limit or 5xx as "gone" silently abandons pending work: an archived channel that never gets swept past its retention window, a jailed member nobody ever releases. Both failed exactly that way before this check existed, and neither is visible from the outside.
- **Nothing this bot posts is allowed to ping.** `discordguard.GuildOps.ChannelMessageSend`/`ChannelMessageSendComplex` overwrite `AllowedMentions` with the zero value, which marshals as `{"parse":[]}` because discordgo deliberately leaves `Parse` without `omitempty`. Plain `discordgo.ChannelMessageSend` omits the field entirely, which Discord reads as "parse everything in the content". This matters because the text is not always ours: rotation reposts operator-supplied sticky messages on every cycle, so one user mention pings that person on a schedule forever and a mentionable role pings its whole membership. `@everyone`/`@here` additionally need Mention Everyone, which the documented invite does not request but any admin can grant at any time, silently arming every sticky already configured. Suppressing at the guard rather than per call site is the same reasoning as `core.DenyEveryoneExceptBot`; a caller that genuinely needs to ping should add a named method taking an explicit allowlist, never reach past the guard to the raw session. `scheduler.alert` spells the same thing out locally because it sends on the raw session and interpolates arbitrary error text.
- **`everyoneCanRead` (`adminconfig/status.go`) answers "yes" when unsure, on purpose.** `/config setup`'s "create it for me" path applies `core.DenyEveryoneExceptBot`, but *picking an existing channel* deliberately changes no permissions on a channel the guild already uses, so an admin can point the audit log at a public channel and publish every jail and config change. Both `/config status` and the pick step warn. The check re-runs on every `/config status` rather than only at pick time because the answer drifts: a channel that was private when it was chosen stops being private the moment someone removes the overwrite, and nothing about the audit log continuing to fill up says otherwise. The two mistakes are not symmetric, so a spurious warning is the acceptable one.
- `GuildRoleDelete` prunes the deleted role from mod roles and from every action's allow/deny lists (`settings.Store.PruneDeletedRole`) and drops `roles`' cached jail-role ID (`roles.HandleRoleDeleted`), since that cache is held for the process lifetime and would otherwise fail every later jail against a dead ID. **There is deliberately no `GuildMemberRemove` handler**: a jailed member's `role_jails` row is the only copy of the roles they held before being jailed and is what re-applies the marker if they return inside their window, so dropping or flagging it on leave is exactly the evasion bug fixed once already (`evasion_test.go` asserts the row survives a departure).
- `config.Loader.OnReload` exists so a value applied once at startup can keep tracking the file. `LogLevel` was the motivating case: parsed, validated, used, then never read again, so SIGHUP reloaded it into a field nothing consulted and raising verbosity mid-incident still meant a restart. Hooks run outside the write lock (reading the config they are handed is the obvious thing for one to do, and would otherwise deadlock against `Global`) and never fire for a reload that failed validation.
- Minimal logging: log IDs, not message content, by default; this matters doubly for rotation, whose whole point is *not* retaining content longer than necessary.
- Self-throttling independent of Discord's own rate limits (e.g. `channelCapHeadroom` in `rotation/execute.go` staying well clear of Discord's 500-channel guild cap) rather than relying solely on discordgo's rate-limit backoff.

## Conventions (from CONTRIBUTING.md)

- Small, reviewable PRs: one milestone (or a slice of one) per PR, per `spec.MD` §9's breakdown.
- `main` is protected: PR review + all CI checks required, no direct/force pushes.
- Run `go vet ./...`, `go test ./... -race -cover`, and `golangci-lint run` before opening a PR.
