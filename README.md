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
- **Gateway intents**: `GUILDS` and `GUILD_MEMBERS`. `MESSAGE_CONTENT` is
  never requested.
  - `GUILD_MEMBERS` is privileged, so it must also be ticked as **"Server
    Members Intent"** under Bot in Discord's Developer Portal (a self-serve
    toggle below 100 servers; Discord approval above that). Ticking it there
    is all you need to do — the bot asks for it by default.
  - It is what re-jails a member the moment they rejoin, rather than on the
    next `roles-sweep` tick. Jail survives a rejoin either way, so the intent
    narrows the window from "at most a minute" to "immediately"; it does not
    create the protection.
  - If the portal toggle is off, Discord refuses the connection and the bot
    **exits at startup with that explanation** rather than running on
    silently. To run without it, set `MERLIN_DISABLE_GUILD_MEMBERS_INTENT=1`
    and rely on the sweep.
  - This used to be opt-in via `MERLIN_ENABLE_GUILD_MEMBERS_INTENT`, which
    was a trap: ticking the portal toggle looked like it should be enough,
    changed nothing on its own, and nothing reported the mismatch. That
    variable is no longer read.

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
   stored on the VPS. It pins the deployed image to the commit SHA in
   `deployed-tag.env` and keeps the tag it replaced in `previous-tag.env`,
   which is what makes the rollback in the Runbook a one-liner.

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

## Runbook

Four things go wrong in production. Each has one procedure.

### 1. The bot is doing something destructive and must stop now

Fastest first — each is stronger and slower than the one above it.

```
/config pause paused:true
```
Refuses every rotation, archive deletion, jail, and role change in that
server. Takes effect on the next attempted action, no restart. Read and
inspect commands keep working, so you can still see what it thinks is going
on. Scheduled jobs stay *due* — nothing is skipped permanently, it runs on
the first tick after you unpause. Reverse with `paused:false`.

If Discord itself is unreachable or the database is the problem, do it on the
host instead:

```sh
cd /home/deploy/merlin
echo 'MERLIN_PAUSE_ALL_WRITES=1' >> .env
docker compose -f docker-compose.prod.yml up -d bot
```
Same stop, process-wide, every guild, independent of the database. Remove the
line and restart to release it.

Last resort, if the above can't be reached: `docker compose -f
docker-compose.prod.yml stop bot`. This also stops the scheduler, so anything
that comes due while it's down fires on the next tick after it comes back.

### 2. A bad deploy went out

Deploys are pinned to the commit SHA, and the previous one is kept on the
host.

```sh
cd /home/deploy/merlin
cat deployed-tag.env      # what's running now
cat previous-tag.env      # what it replaced

cp previous-tag.env deployed-tag.env
set -a; . ./deployed-tag.env; set +a
docker compose -f docker-compose.prod.yml up -d
```

To go back further, any commit SHA on `main` that CI built is a valid tag:
`echo MERLIN_IMAGE_TAG=<sha> > deployed-tag.env` and re-run the last two
lines. Note this rolls back *code only* — a deploy that ran a migration has
already changed the schema, and Go builds don't un-apply it. Check whether
the range you're rolling back over touched `internal/storage/migrations/`
before assuming a code rollback is sufficient.

### 3. The database is lost or corrupted

`pgbackup` writes a daily `pg_dump` to `/home/deploy/merlin/backups/`,
keeping 14 days, outside the `pgdata` volume. Everything the bot knows that
isn't reconstructible from Discord lives there: rotation config, permission
policy, jail records with the role snapshots needed to restore members, and
the audit trail.

```sh
cd /home/deploy/merlin
ls -la backups/                                   # newest last

docker compose -f docker-compose.prod.yml stop bot
docker compose -f docker-compose.prod.yml exec -T postgres \
  pg_restore -U "$POSTGRES_USER" -d merlin --clean --if-exists \
  < backups/merlin-YYYYMMDD-HHMMSS.dump
docker compose -f docker-compose.prod.yml start bot
```

Stop the bot first: restoring under a running bot races the scheduler against
a half-restored schema. Migrations re-run automatically on the restart and
are idempotent, so restoring an older dump and letting the bot catch the
schema up is fine.

**Rehearse this before you need it.** Restore a dump into a scratch database
and confirm it comes back — an untested restore is not a backup.

### 4. Something is wrong and you don't know what

Start inside Discord:

```
/config status
```
One embed: is the database reachable, is any scheduled job failing, is the
server paused or in dry-run, and do the configured audit-log/status channels
and mod roles still exist. That last one matters because a deleted audit-log
channel is otherwise silent — the audit trail just stops appearing.

Then the logs:

```sh
docker compose -f docker-compose.prod.yml logs -f --tail=200 bot
```

To raise verbosity, set `LOG_LEVEL=debug` in `.env` and restart the bot
(`docker compose -f docker-compose.prod.yml up -d bot`). It overrides
`config.yaml`, which is mounted read-only.

`/scheduler list` shows every registered job with last-run, next-due, and
consecutive-failure count — usually the fastest way to tell "wedged job" from
"nothing was due yet."

For "the bot did something and I don't know why", the `action_journal` table
records every destructive Discord call it attempted, including the ones
refused by the rate cap or circuit breaker before any audit entry was
written. Rows kept 30 days:

```sh
docker compose -f docker-compose.prod.yml exec -T postgres \
  psql -U "$POSTGRES_USER" -d merlin -c \
  "SELECT started_at, op, target_id, state, error FROM action_journal
   ORDER BY started_at DESC LIMIT 50;"
```

A row still `pending` long after `started_at` is a call that never returned —
the process died mid-mutation. It's the first thing worth checking after an
unexplained restart.

### Rehearsing a rotation before trusting it

Channel rotation and the archive sweep permanently delete channels. Before
the first live rotation on a real server:

```
/config dryrun enabled:true
```

Rotations and sweeps then make their full decision and write to
`#bird-audit-log` describing exactly what they *would* have done, and change
nothing. Let a full rotation interval and a sweep window pass, read the audit
log, then `/config dryrun enabled:false`.

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

**Jail survives leaving and rejoining.** Discord drops every role a member
holds when they leave, so without this a jailed member could shed the Jailed
role — and with it every channel restriction — simply by leaving and coming
back, while Merlin's own record still said they were jailed. The sweep
compares each active jail against the member's `JoinedAt`: a join later than
the jail began is a rejoin, and the marker goes back on. A member whose
marker is gone *without* a later join is treated as a deliberate manual
release by a mod, exactly as before. Re-applying never touches the stored
role snapshot or the release time — leaving neither serves the sentence nor
extends it.

## Package layout

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
- `internal/discordguard` — the chokepoint every destructive Discord call
  passes through, enforcing the pause and dry-run controls above. Plugins get
  a guild-bound view of it in place of the raw session.
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
