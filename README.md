# merlin

A production-grade, modular Discord bot for "The Melting Pot." See
[`spec.MD`](./spec.MD) for the full design doc. This README covers what's
built so far: Milestone 0 (scaffold, CI, Docker, `/ping`), Milestone 1
(core plugin registry, event bus, permissions layer, config loader), and
Milestone 2 (scheduler / cron core).

## Bot permissions & intents

This build requests the minimum needed for `/ping` and the core skeleton —
nothing destructive yet:

- **OAuth2 scopes**: `bot`, `applications.commands`.
- **Permission bits**: `/admin run-now` requires `Manage Server` as its
  Discord-side gate (layer 1/4 of spec.MD §4's authorization model); the
  internal mod/admin allow-list (layer 2) is still checked in-code before
  anything runs. Future milestones will add `Manage Channels`,
  `Manage Roles`, `Manage Messages`, and `View Audit Log` as the
  rotation/factions/reporting plugins land — see `spec.MD` §4.
- **Gateway intents**: `GUILDS` only. `GUILD_MEMBERS` and `MESSAGE_CONTENT`
  are privileged intents requiring Discord approval at scale, and neither is
  requested until a specific plugin genuinely needs it.

## Local development

```sh
cp .env.example .env            # fill in DISCORD_BOT_TOKEN, DISCORD_APP_ID
cp config.example.yaml config.yaml   # fill in your guild/role/channel IDs
docker compose up --build
```

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
cp .env.example .env                  # fill in real values
cp config.example.yaml config.yaml    # fill in real guild/role/channel IDs
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

A mod/admin-only `/admin run-now <job>` command (layered authorization: an
explicit `DefaultMemberPermissions` bit plus the internal allow-list check)
triggers any registered job immediately, for testing.

## Architecture

- `internal/core` — plugin registry/lifecycle, event bus, permissions,
  shared Discord session.
- `internal/config` — per-guild YAML config + env-sourced secrets, with
  SIGHUP hot-reload for non-secret values on Linux.
- `internal/storage` — Postgres connection pool, migration runner
  (`storage.Migrate`, applied automatically at startup), and SQL migrations.
- `internal/scheduler` — cron core (see above); itself a `core.Plugin` and
  the concrete implementation behind `Deps.Scheduler`.
- `internal/plugins/ping` — reference plugin exercising the full lifecycle.

Every plugin implements the `core.Plugin` interface and is registered at
startup in `cmd/bot/main.go` — no dynamic/hot-loading, per `spec.MD`
Design Principle 1.
