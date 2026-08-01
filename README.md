# merlin

A production-grade, modular Discord bot for "The Melting Pot." See
[`spec.MD`](./spec.MD) for the full design doc. This README covers what's
built so far: Milestone 0 (scaffold, CI, Docker, `/ping`) + Milestone 1
(core plugin registry, event bus, permissions layer, config loader).

## Bot permissions & intents

This build requests the minimum needed for `/ping` and the core skeleton —
nothing destructive yet:

- **OAuth2 scopes**: `bot`, `applications.commands`.
- **Permission bits**: none (no elevated bits requested). Future milestones
  will add `Manage Channels`, `Manage Roles`, `Manage Messages`, and
  `View Audit Log` as the rotation/factions/reporting plugins land — see
  `spec.MD` §4.
- **Gateway intents**: `GUILDS` only. `GUILD_MEMBERS` and `MESSAGE_CONTENT`
  are privileged intents requiring Discord approval at scale, and neither is
  requested until a specific plugin genuinely needs it.

## Local development

```sh
cp .env.example .env            # fill in DISCORD_BOT_TOKEN, DISCORD_APP_ID
cp config.example.yaml config.yaml   # fill in your guild/role/channel IDs
docker compose up --build
```

Or run natively (requires a reachable Postgres):

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

## Architecture

- `internal/core` — plugin registry/lifecycle, event bus, permissions,
  shared Discord session.
- `internal/config` — per-guild YAML config + env-sourced secrets, with
  SIGHUP hot-reload for non-secret values on Linux.
- `internal/storage` — Postgres connection pool + migrations.
- `internal/plugins/ping` — reference plugin exercising the full lifecycle.

Every plugin implements the `core.Plugin` interface and is registered at
startup in `cmd/bot/main.go` — no dynamic/hot-loading, per `spec.MD`
Design Principle 1.
