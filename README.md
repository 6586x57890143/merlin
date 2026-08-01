# merlin

A production-grade, modular Discord bot for "The Melting Pot." See
[`spec.MD`](./spec.MD) for the full design doc. This README covers what's
built so far: Milestone 0 (scaffold, CI, Docker, `/ping`), Milestone 1
(core plugin registry, event bus, permissions layer, config loader),
Milestone 2 (scheduler / cron core), Milestone 3 (channel rotation), and
Milestone 4 (command framework, tiered permissions, self-serve DB-backed
config — spec.MD §4a).

## Bot permissions & intents

- **OAuth2 scopes**: `bot`, `applications.commands`.
- **Bot role permission bits** — what the bot's own Discord role can do,
  requested via the invite URL's `permissions` parameter (Discord creates/
  updates a managed role for the bot matching this bitmask — re-authorizing
  the same invite link later updates that role in place, no need to
  remove/re-add the bot):
  - `Manage Channels` (bit `16`) — create/rename/move/delete channels and
    edit permission overwrites, needed for channel rotation (Milestone 3).
  - `Manage Roles` (bit `268435456`) — needed for `/config setup`
    (Milestone 4) to create a default "Merlin Mod" role when a guild has
    none configured yet; also required for any future faction-role
    assignment (Milestone 5).
  - Least-privilege, per spec.MD §4: never `Administrator`; this list only
    grows when a landed milestone genuinely needs a new bit, and this
    section (plus the invite link below) is updated in the same PR.

  **Current invite link** (scopes + the bits above, `16 | 268435456 = 268435472`):
  ```
  https://discord.com/api/oauth2/authorize?client_id=1533094679560847460&scope=bot%20applications.commands&permissions=268435472
  ```
  Have a server admin click this link and re-authorize whenever the
  permission bits change — it updates the bot's existing role rather than
  creating a duplicate.

- **Command-level gates** (separate from the bot's own permissions above —
  these govern which *members* can invoke a command): as of Milestone 4
  (spec.MD §4a) every command declares a mandatory `core.PermSpec{Tier,
  Action}` — `Public`, `Mod`, or `Admin` — checked centrally by
  `core.CommandRouter` before any handler runs. `Admin` passes for a
  DB-listed admin, the break-glass identity, *or* anyone holding Discord's
  own Administrator permission in that guild (spec.MD §4c) — most notably
  the guild owner, who always has it, so they can self-serve `/config setup`
  with zero prior bot configuration. A guild can further customize any
  action per `/config permissions set-tier|grant|revoke|block|unblock`
  (spec.MD §4c): override its effective tier, additively grant a specific
  role/user access without making them a full mod, or explicitly block one
  — blocks win over everything else except the break-glass identity, which
  nothing can block. A whole plugin can also be toggled off per guild via
  `/config plugins disable` (checked before any of the above). Discord's own
  `DefaultMemberPermissions` is deliberately left unset on Mod/Admin
  commands (see spec.MD §4a for why) — the internal checks above are the
  sole real gate. Who counts as "mod" (still purely DB-driven, no
  permission-bit shortcut — mods are a tool, not a way to reconfigure the
  bot) or "admin" beyond the paths above is configured via
  `/config mod-roles`/`/config admins` — see "First-time setup" below.
- **Gateway intents**: `GUILDS` only. `GUILD_MEMBERS` and `MESSAGE_CONTENT`
  are privileged intents requiring Discord approval at scale, and neither is
  requested until a specific plugin genuinely needs it.

## Local development

```sh
cp .env.example .env            # fill in DISCORD_BOT_TOKEN, DISCORD_APP_ID,
                                 # and MERLIN_BREAK_GLASS_ADMIN_USER_ID (your
                                 # own Discord user ID)
cp config.example.yaml config.yaml   # bootstrap-only as of Milestone 4 — just log_level
docker compose up --build
```

Guild/role/channel config no longer lives in `config.yaml` (Milestone 4,
spec.MD §4a) — it's configured entirely through Discord commands once the
bot is running. See "First-time setup" below.

Or run natively (Postgres is a hard runtime requirement — the scheduler
persists per-job last-run state there, and the bot exits at startup if
`DATABASE_URL` isn't set; migrations run automatically on every startup):

```sh
go run ./cmd/bot
```

## Testing

```sh
go vet ./...
go test ./... -race -cover
```

## Token rotation

See [`SECURITY.md`](./SECURITY.md) for the token-compromise runbook.

## Deployment

Every push to `main` that passes CI (`lint-test`, `secret-scan`, `docker-build`)
triggers two more CI jobs:

1. `push-image` builds the Docker image and pushes it to GHCR as
   `ghcr.io/6586x57890143/merlin:latest` and `:<commit-sha>`, using the
   workflow's own `GITHUB_TOKEN` — no separate registry secret.
2. `deploy` copies `docker-compose.prod.yml` to the VPS and runs
   `docker compose -f docker-compose.prod.yml pull && up -d` over SSH
   (`VPS_HOST`/`VPS_SSH_KEY` repo secrets), logging into GHCR with the same
   short-lived `GITHUB_TOKEN` so no long-lived registry credential is ever
   stored on the VPS.

`docker-compose.prod.yml` differs from the local `docker-compose.yml` only in
that `bot` pulls the prebuilt GHCR image instead of building from source.
Deploys never touch `.env` or `config.yaml` on the VPS — those hold real
secrets/guild config and must be created there once by hand:

```sh
# one-time setup on the VPS, in /home/deploy/merlin
cp .env.example .env                  # fill in real values, incl. MERLIN_BREAK_GLASS_ADMIN_USER_ID
cp config.example.yaml config.yaml    # bootstrap-only (log_level) — see "First-time setup"
```

The `deploy` SSH user must be able to run `docker`/`docker compose` without an
interactive sudo prompt (e.g. a member of the `docker` group).

## Scheduler

`internal/scheduler` is the generic cron core other time-based plugins
(Rotation, next) register recurring jobs with (spec.MD §5). Job state
(last-run timestamp, consecutive failures) is persisted per job key
(`guildID:name`) in Postgres, so "every N hours" survives a restart instead
of resetting its clock. Failing jobs retry with capped exponential backoff;
past 5 consecutive failures, the scheduler posts an alert to the guild's
configured `status_channel_id` and falls back to the job's normal interval
rather than retrying forever.

`/scheduler run-now` (Mod tier) triggers any registered job immediately,
for testing — the `job` option autocompletes over the invoking guild's
currently-registered job keys, so there's nothing to memorize or look up
in code. `/scheduler list` shows every registered job for the guild
(last-run, next-due, consecutive failures) as a plain, no-typing-required
alternative to autocomplete.

## Channel rotation

`internal/plugins/rotation` implements spec.MD §6's "Refresh": a configured
channel (e.g. `#general-chat`) is periodically given a clean history while
preserving a moderation trail, specifically to reduce the window of
retained content a bad-faith actor can trawl through for a retroactive
mass-report campaign. As of Milestone 4, it's configured per guild entirely
through `/rotation configure add|edit|remove|sticky` (Admin tier, using
Discord's native channel/role pickers instead of raw IDs) — `/rotation
list` shows every configured rotation for the guild. See spec.MD §6.

Each rotation cycle: creates a fully-configured replacement channel
**hidden** from members, populates it (sticky messages + a birdlike
transparency notice about the retention policy), then flips visibility
**new channel first** — only after the replacement is live does the old
channel get renamed into the hidden archive category. This deliberately
trades spec's literal step order for a stronger safety property: the worst
failure case is a brief moment where both channels are visible under the
same name, never a window where the guild has no live channel at all. Every
step re-derives "is this already done?" from live Discord/Postgres state,
so a crash mid-rotation self-heals on the next scheduled retry (via the
existing Scheduler backoff, unchanged) without creating duplicate channels
or reposting stickies.

`archive_visibility` is `mod_only` (only the guild's configured mod roles
can see the archive) or `whitelist` (mod roles plus extra
`archive_whitelist_role_ids`/`archive_whitelist_user_ids`) — mod roles
always retain access either way.

Archived channels are permanently deleted by a per-guild hourly sweep job
(`rotation-sweep`, registered like any other Scheduler job) once past their
`retention_days` — a small `rotation_archives` table tracks pending
deletions rather than teaching the Scheduler about one-shot jobs. Omit
`retention_days` entirely to keep an archive forever (never swept); this is
an intentional, unbounded escape hatch with no automatic cap, so treat it
as an ongoing manual responsibility, not something the bot protects against.
If a mod manually moves an archived channel out of its configured archive
category before the sweep runs, that's treated as an implicit "keep this
one" and it's never deleted.

`/scheduler run-now` (with autocomplete over registered job keys) already
covers manually triggering either the per-channel rotation job or the
`rotation-sweep` job — rotation itself adds no separate manual-trigger
command, just its own `/rotation configure`/`list` for settings.

## Architecture

- `internal/core` — plugin registry/lifecycle, event bus, tiered+whitelist
  permissions (`permissions.go`), the single command router/dispatcher
  (`commands.go`, spec.MD §4a), shared Discord session.
- `internal/config` — process-bootstrap-only config as of Milestone 4: log
  level, Discord token/App ID, DB DSN, and the break-glass admin user ID.
  No guild/role/channel config here anymore.
- `internal/settings` — DB-backed, per-guild config (mod roles, admins,
  permission whitelists, rotation settings), in-memory cached and
  invalidated on every mutation via `core.EventConfigChanged`; the thing
  `/config` and `/rotation configure` actually read/write.
- `internal/storage` — Postgres connection pool, migration runner
  (`storage.Migrate`, applied automatically at startup), and SQL migrations.
- `internal/scheduler` — cron core (see above); itself a `core.Plugin` and
  the concrete implementation behind `Deps.Scheduler`.
- `internal/audit` — minimal `core.AuditWriter`: DB insert + `#bot-audit-log`
  embed, behind `Deps.Audit`.
- `internal/plugins/ping` — reference plugin exercising the full lifecycle.
- `internal/plugins/rotation` — channel rotation (see above).
- `internal/plugins/adminconfig` — the cross-cutting `/config` command tree
  (admins/mod-roles/permissions/setup/import) — the one exception to "one
  top-level command per plugin," since these concepts don't belong to any
  single feature plugin (spec.MD §4a).

Every plugin implements the `core.Plugin` interface and is registered at
startup in `cmd/bot/main.go` — no dynamic/hot-loading, per `spec.MD`
Design Principle 1.

## First-time setup

Once the bot is invited (see the invite link above) and running:

1. **Invite the bot** to your guild via the link above (a server admin must
   do this). If nothing is configured yet, the bot DMs the guild **owner**
   once, pointing at `/config setup` (spec.MD §4c) — Discord has no API for
   "who actually invited the bot," so the owner is the best available,
   always-present proxy, and they always implicitly hold Discord's
   Administrator permission, so they can act on it immediately.
2. **Run `/config setup`** as the guild owner, anyone else with Discord's
   Administrator permission, or the break-glass admin (the Discord user
   whose ID you set as `MERLIN_BREAK_GLASS_ADMIN_USER_ID` — a fallback that
   always passes Admin tier regardless of DB state, mainly useful if no
   Administrator is available or as a recovery path). This auto-creates
   `#bot-audit-log`, `#bot-status`, and a "Merlin Mod" role for whatever
   isn't already configured, and always responds with a full status summary
   and next steps — it's safe and useful to re-run any time as a "how's my
   setup" check, not just once.
3. **Run `/config admins add`** to add specific bot-admins beyond whoever
   already qualifies via Discord's Administrator permission, and
   `/config mod-roles add` to designate mod role(s) — both Admin tier. Mods
   never get command-configuration access automatically (see step 5).
4. **Configure rotation** with `/rotation configure add` (Admin tier) for
   any channel you want periodically refreshed.
5. Optionally, fine-tune who can use a specific action beyond the defaults:
   **`/config permissions set-tier <action> mod`** lets mods use it too
   (rotation's configure actions are Admin-only out of the box);
   **`/config permissions grant <action> <role-or-user>`** additively grants
   one specific non-mod role/user access without making them a full mod;
   **`/config permissions block <action> <role-or-user>`** excludes one
   specific role/user even if their tier would otherwise allow it. A whole
   plugin can be turned off for the server with `/config plugins disable`.

Every command response is a color-coded, ephemeral embed (from Merlin's
brand palette — see `internal/core/embeds.go` and spec.MD §4b) visible only
to whoever ran the command.

`/config import` exists only for migrating a pre-Milestone-4 deployment's
`config.yaml` guild settings into the DB once — new deployments never need
it.
