# merlin

A production-grade, modular Discord bot for "The Melting Pot." This README
covers running, deploying, and navigating the codebase. For architecture,
security model, and the reasoning behind any design choice, see
[`spec.MD`](./spec.MD) — that's the one place rationale lives; this file
just links to it rather than repeating it.

## Bot permissions & intents

- **OAuth2 scopes**: `bot`, `applications.commands`.
- **Bot role permission bits** — what the bot's own Discord role can do,
  requested via the invite URL's `permissions` parameter (Discord creates/
  updates a managed role for the bot matching this bitmask — re-authorizing
  the same invite link later updates that role in place, no need to
  remove/re-add the bot):
  - `Manage Channels` (bit `16`) — create/rename/move/delete channels and
    edit permission overwrites, needed for channel rotation (Milestone 3)
    and for `/roles`'s per-channel jail-visibility overwrites (Milestone 9).
  - `Manage Roles` (bit `268435456`) — needed for `/config setup`
    (Milestone 4) to create a default "Merlin Mod" role when a guild has
    none configured yet; also used by `/roles` (Milestone 9) to create/
    reuse a shared "Jailed" marker role and to strip/restore/grant/revoke
    member roles — no new bit needed, this one already covers it.
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
  these govern which *members* can invoke a command): every command
  declares a `Public`/`Mod`/`Admin` tier, checked centrally before any
  handler runs, plus a per-guild-configurable policy layer (tier overrides,
  per-person grants/blocks, plugin on/off) — see spec.MD §4/§4a for the full
  model and `/config`'s subcommands. Who counts as "mod"/"admin" is
  configured via `/config mod-roles`/`/config admins` — see "First-time
  setup" below.
- **Gateway intents**: `GUILDS` only. `GUILD_MEMBERS` and `MESSAGE_CONTENT`
  are privileged intents requiring Discord approval at scale, and neither is
  requested until a specific plugin genuinely needs it.

## Local development

```sh
cp .env.example .env            # fill in DISCORD_BOT_TOKEN, DISCORD_APP_ID,
                                 # and MERLIN_BOOTSTRAP_ADMIN_USER_ID (your
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
cp .env.example .env                  # fill in real values, incl. MERLIN_BOOTSTRAP_ADMIN_USER_ID
cp config.example.yaml config.yaml    # bootstrap-only (log_level) — see "First-time setup"
```

The `deploy` SSH user must be able to run `docker`/`docker compose` without an
interactive sudo prompt (e.g. a member of the `docker` group).

## Scheduler

`internal/scheduler` is the generic cron core other plugins register
recurring jobs with — persisted last-run state, retry with backoff, and a
status-channel alert past repeated failures (spec.MD §5). `/scheduler
run-now` (autocomplete over registered job keys) and `/scheduler list`
cover manual triggering and inspection for every registered job, including
rotation's.

## Channel rotation

`internal/plugins/rotation` implements spec.MD §6's "Refresh": a configured
channel is periodically given a clean history while preserving a moderation
trail. Configured per guild via `/rotation configure add|edit|remove|sticky`
and inspected with `/rotation list` — see spec.MD §6 for the full rotation
process, visibility/retention semantics, and archive sweep behavior.

## Role management

`internal/plugins/roles` implements jail (snapshot-and-strip a member's
roles and channel access for a period, then restore both automatically or on
demand — `/roles jail|release`) and timed single-role grants
(`/roles grant|revoke`), with `/roles list` to inspect a member's active
jail/grants. Jail denies every channel except a guild-configured allowlist
(`/roles configure allow-channel|disallow-channel|list-channels`), enforced
via one shared "Jailed" role's own permission overwrites rather than
per-member overwrites, so jailing scales to any server size at a fixed
Discord API cost.

## Scheduler

- `internal/core` — plugin registry/lifecycle, event bus, tiered+whitelist
  permissions (`permissions.go`), the single command router/dispatcher
  (`commands.go`, spec.MD §4a), shared Discord session.
- `internal/config` — process-bootstrap-only config as of Milestone 4: log
  level, Discord token/App ID, DB DSN, and the bootstrap admin user ID.
  No guild/role/channel config here anymore.
- `internal/settings` — DB-backed, per-guild config (mod roles, admins,
  permission whitelists, rotation settings), in-memory cached and
  invalidated on every mutation via `core.EventConfigChanged`; the thing
  `/config` and `/rotation configure` actually read/write.
- `internal/storage` — Postgres connection pool, migration runner
  (`storage.Migrate`, applied automatically at startup), and SQL migrations.
- `internal/scheduler` — cron core (see above); itself a `core.Plugin` and
  the concrete implementation behind `Deps.Scheduler`.
- `internal/audit` — minimal `core.AuditWriter`: DB insert + `#bird-audit-log`
  embed, behind `Deps.Audit`.
- `internal/plugins/ping` — reference plugin exercising the full lifecycle.
- `internal/plugins/rotation` — channel rotation (see above).
- `internal/plugins/roles` — jail + timed role grants (see above).
- `internal/plugins/adminconfig` — the cross-cutting `/config` command tree
  (admins/mod-roles/permissions/setup/import) — the one exception to "one
  top-level command per plugin," since these concepts don't belong to any
  single feature plugin (spec.MD §4a).

Every plugin implements the `core.Plugin` interface and is registered at
startup in `cmd/bot/main.go` — no dynamic/hot-loading, per `spec.MD`
Design Principle 1.

## First-time setup

Once the bot is invited (see the invite link above) and running:

1. **Invite the bot** (a server admin must do this). If nothing is
   configured yet, it DMs the guild owner once pointing at `/config setup`
   — see spec.MD §4a for why the owner, specifically.
2. **Run `/config setup`** as the owner, any other Administrator, or the
   bootstrap admin. Creates `#bird-audit-log`, `#bird-status`, and a mod
   role for whatever's missing; safe to re-run any time as a status check.
3. **`/config admins add`** / **`/config mod-roles add`** to bring in
   others beyond the Administrator/bootstrap paths.
4. **`/rotation configure add`** for any channel you want periodically
   refreshed.
5. Optionally fine-tune access per action with `/config permissions
   set-tier|allow|deny` or turn a whole plugin off with `/config plugins
   set` — see spec.MD §4a for the full model.
