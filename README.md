# merlin

merlin is a Discord bot I wrote for **The Melting Pot**, a free-speech server that
attracts a fair amount of mass-reporting and brigading. Everything here started
as something that server actually needed, which is why the defaults lean
cautious: least privilege on the Discord side, nothing it posts can ping anyone
and anything destructive can be stopped from three different places.

It's one Go binary plus Postgres. Features are plugins compiled into the binary.
There's no hot loading and no runtime plugin fetching, on purpose.

If you want the design reasoning behind any of it, that lives in
[`spec.MD`](./spec.MD). This file is just how to run the thing.

## What it does

**Channel rotation.** Point it at a channel and every so often it swaps that
channel out for a fresh empty one under the same name, in the same place in the
sidebar. The old one gets renamed, moved into a hidden archive category and
deleted after a retention window you set (or kept forever if you'd rather). The
channel gets a heads-up before it happens and a short note afterwards explaining
what just happened. Handy if you run a channel where people would rather their
history not sit around indefinitely.

Archives are hidden from everyone except your mods and anyone with Discord's
Administrator bit. If you want another role to be able to read them, add it with
`/rotation configure allow-archive-role`. That grant is read-only, so the role
can view the channel and read the history back and nothing else: no posting, no
reactions, no threads. Permissions live on the archive category and every
archived channel is kept in sync with it, so a role you add or remove applies to
the archives that already exist, not just future ones. The bot also checks
hourly that nothing has been added to that category by hand.

**Jail.** `/roles jail @someone 2h` snapshots their roles, strips them and drops
a shared "Jailed" role on them instead. That role's channel overwrites decide
what a jailed member can still see, so jailing costs one API call no matter how
big the server is. They get their roles back automatically when the timer runs
out, or when a mod runs `/roles release`. Leaving and rejoining doesn't shake it
off. There's a bulk version and a `/roles jail-role` for when a raid shows up.

**Timed role grants.** `/roles grant @someone @role 24h`, and it comes off by
itself.

**Contests.** `/contest new title:neon cats` and merlin runs the whole thing:
an announce window while the prize pool fills, then a forum channel she creates
for the entries, then voting, then the winner. People enter by posting in the
forum like any other forum, so files are Discord's problem and not the bot's,
and deleting your post is how you withdraw. Voting happens on a web gallery
because forty pieces of art is the one thing Discord is genuinely bad at.
Everyone signs in with Discord there, so it is one vote per member of your
server and no one else, and the page is an unguessable link no search engine
indexes.

**Prizes.** `/contest prize` opens a form. Steam key, Nitro gift link, crypto,
anything. If it has a code, merlin keeps it encrypted and DMs it to the winner
the moment the contest ends, then wipes it. If it does not, she introduces the
two of you and stays out of it. She never holds money.

**Everything is configured in Discord.** No YAML editing on the host for
day-to-day stuff. `/config setup` walks you through the audit log channel,
status channel, mod role and admins. After that it's `/config admins`,
`/config mod-roles`, `/config permissions` and `/config plugins`. Every command
declares a Public/Mod/Admin tier. You can override tiers per server, allow or
deny specific people or roles per action, or switch a whole plugin off entirely.

**An audit trail and an off switch.** Anything the bot does lands in
`#bird-audit-log` as an embed and in Postgres as a row. `/config pause` refuses
every destructive action instantly, `/config dryrun` makes it describe what it
would have done without doing it, and there's a host-level env var for when
Discord or the database is the thing that's broken.

**AI moderation.** Optional, off until you turn it on, and it needs an
OpenRouter key you provide. It reads messages and checks them against
[Discord's own Community Guidelines](https://discord.com/guidelines), removing
or cleaning up the ones that breach them. The point is narrow: it is there to
stop the server being reported off the platform, not to police how people talk.
Rudeness, hostility, profanity, dark humour and opinions you dislike are not
violations and it is told so explicitly.

Each of the ten policy areas is a file in `internal/plugins/aimod/policy/`
saying both what the rule covers and where it stops, and you choose per area
whether merlin ignores it, flags it, cleans it up or removes it. Only child
safety is fixed on. Hate speech, gore, self-harm and spam are **off** by
default: those are questions about what kind of room you're running, and you
should switch them on deliberately rather than find out later.

It is built to be cheap for whoever is paying. Most messages never reach a
model at all (a skip filter and a small pattern list handle them for free);
what does is batched and read by a cheap model that can only ever *flag*;
nothing is deleted until a better model has re-read it against the full policy
text. You set a daily USD cap per server, and past it merlin falls back to the
free checks rather than spending more. `/aimod models show` prices your current
setup against your server's own measured traffic, and `/aimod models compare`
prices any other model before you switch.

Every action is explained to the member in a DM, recorded in the audit log, and
reversible with `/aimod undo` while the evidence window lasts. Start with
`/aimod configure mode flag` and watch it for a week before letting it act.

## Adding it to a server

Invite link (a server admin has to click it):

```
https://discord.com/api/oauth2/authorize?client_id=1533094679560847460&scope=bot%20applications.commands&permissions=285212688
```

That asks for `Manage Channels`, `Manage Roles` and `Move Members`, and nothing
else. Never `Administrator`. If the permission bits ever change, re-clicking the
same link updates the bot's existing role rather than adding a second one.

`Move Members` is only there so `/roles jail` can boot someone out of a voice
call at jail time. Permission overwrites don't apply to a voice session that's
already in progress, so without it a jailed member who was mid-call would stay
in it.

### If you're running AI moderation

`/aimod` needs more than the link above grants, so it has its own:

```
https://discord.com/api/oauth2/authorize?client_id=1533094679560847460&scope=bot%20applications.commands&permissions=1100333786128
```

That's the same three, plus `View Channel` and `Read Message History` (to see
messages at all, and to read the few lines of context before a flagged one),
`Manage Messages` (to delete one), `Manage Webhooks` (to repost a rewritten
message under its author's name), and `Moderate Members` (the timeout that the
sanction ladder falls back to when jail can't be applied).

Two links rather than one on purpose. The plugin is off until an admin turns it
on, and a deployment that never will shouldn't be handing the bot the ability to
delete messages and time people out. Use the narrow link unless you need this.

### Intents

`GUILDS` and `GUILD_VOICE_STATES` are unprivileged and always requested.

`GUILD_MEMBERS` is privileged, so if you're self-hosting you need to tick
**Server Members Intent** under Bot in the Discord Developer Portal. The bot
asks for it by default. If the toggle is off, Discord silently refuses the
gateway connection, so the bot gives up at startup and tells you which toggle to
flip rather than sitting there reconnecting forever.

What the intent buys you: a jailed member who rejoins gets re-jailed instantly
instead of within a minute, and roles that a server's Onboarding flow hands back
to a jailed member get stripped again right away. Both of those work without it,
just a minute slower. If you can't enable it, set
`MERLIN_DISABLE_GUILD_MEMBERS_INTENT=1` and live with the sweep.

`MESSAGE_CONTENT` is privileged too, and is **off unless you ask for it**:
`MERLIN_ENABLE_MESSAGE_CONTENT_INTENT=1` plus **Message Content Intent** ticked
in the same Developer Portal page. It's the opposite default from the one above
because it's a far larger ask (every message in every server, and Discord
reviews it above 100 guilds), and nothing but `/aimod` reads it. Without it that
plugin registers its commands, scans nothing, and says so in `/aimod status`
rather than looking like it's working.

Same failure mode as the other one if the portal toggle is missing: Discord
refuses the connection and the bot exits at startup naming both toggles instead
of reconnect-looping.

## First run

1. Invite the bot.
2. Run `/config setup` as the server owner, any Administrator or the bootstrap
   admin. It creates `#bird-audit-log`, `#bird-status` and a `merlin Mod` role
   for whatever's missing, and offers a picker for anything you already have. Safe
   to re-run whenever, it doubles as a status screen.
3. `/config admins add` and `/config mod-roles add` for everyone else.
4. `/rotation configure add` if you want a rotating channel.
5. `/config status` to check it's all wired up.

Before you trust rotation on a real channel, turn on `/config dryrun
enabled:true` and let a full interval pass. It'll write to the audit log
describing exactly what it would have deleted, and touch nothing. Channel
deletion has no undo, so this is worth the wait.

## Running it yourself

You need Docker and a Discord application. Postgres is not optional, the
scheduler keeps its state there and the bot won't start without it. Migrations
run automatically at startup.

```sh
git clone https://github.com/6586x57890143/merlin
cd merlin
cp .env.example .env                 # bot token, app ID, your own Discord user ID
cp config.example.yaml config.yaml   # just log_level
docker compose up --build
```

`MERLIN_BOOTSTRAP_ADMIN_USER_ID` in `.env` is your own Discord user ID. That
identity always counts as admin in every server regardless of what's in the
database, so a wiped or broken config can't lock you out permanently. It's the
one thing that isn't database-backed.

Natively, if you'd rather:

```sh
go run ./cmd/bot
```

Tests:

```sh
go vet ./...
go test ./... -race -cover
golangci-lint run
```

The Postgres-backed tests skip themselves when `TEST_DATABASE_URL` isn't set, so
a plain `go test ./...` works with no setup. To actually run them, bring up the
compose Postgres and point the variable at it.

## Deploying

Push to `main`, CI does the rest. `lint-test`, `secret-scan`, `prose` and
`docker-build` have to pass, then `push-image` builds a multi-arch image and
pushes it to GHCR, and `deploy` SSHes into the VPS and runs
`docker compose -f docker-compose.prod.yml pull && up -d`.

The deploy pins the running image to the commit SHA in `deployed-tag.env` and
keeps whatever it replaced in `previous-tag.env`, which is what makes rolling
back a three-line job.

`docker-compose.prod.yml` is the same as the local compose file except `bot`
pulls the prebuilt image instead of building from source. Deploys never touch
`.env` or `config.yaml` on the host, those hold real secrets and you create them
there once by hand:

```sh
# once, on the VPS, in /home/deploy/merlin
cp .env.example .env
cp config.example.yaml config.yaml
```

Repo secrets you'll need: `VPS_HOST` and `VPS_SSH_KEY`. GHCR auth uses the
workflow's own `GITHUB_TOKEN`, so there's no long-lived registry credential
sitting on the box. The SSH user needs to be able to run `docker compose`
without an interactive sudo prompt.

## When something breaks

**Make it stop.** `/config pause paused:true` refuses every rotation, archive
deletion, jail and role change in that server, effective on the next attempted
action. Read commands keep working. Scheduled jobs stay due rather than getting
skipped, so they run on the first tick after you unpause.

If Discord or the database is the problem, do it on the host instead:

```sh
cd /home/deploy/merlin
echo 'MERLIN_PAUSE_ALL_WRITES=1' >> .env
docker compose -f docker-compose.prod.yml up -d bot
```

**Roll back a bad deploy.**

```sh
cd /home/deploy/merlin
cat deployed-tag.env previous-tag.env

cp previous-tag.env deployed-tag.env
set -a; . ./deployed-tag.env; set +a
docker compose -f docker-compose.prod.yml up -d
```

Any commit SHA that CI built is a valid tag, so you can go further back by
writing one into `deployed-tag.env` yourself. This rolls back code only. If the
range you're skipping over added a migration, the schema has already moved and
won't move back on its own, so check `internal/storage/migrations/` first.

**Restore the database.** `pgbackup` drops a daily `pg_dump` into
`/home/deploy/merlin/backups/` and keeps two weeks, outside the `pgdata` volume.
That's where everything lives that Discord can't tell you: rotation config,
permission policy, jail records with the role snapshots needed to give people
their roles back and the audit trail.

```sh
docker compose -f docker-compose.prod.yml stop bot
docker compose -f docker-compose.prod.yml exec -T postgres \
  pg_restore -U "$POSTGRES_USER" -d merlin --clean --if-exists \
  < backups/merlin-YYYYMMDD-HHMMSS.dump
docker compose -f docker-compose.prod.yml start bot
```

Stop the bot first or the scheduler races a half-restored schema. Migrations
re-run on the way back up and are idempotent, so restoring an older dump is
fine. Rehearse this into a scratch database at some point, an untested restore
isn't a backup.

**Work out what happened.** `/config status` in Discord first: database
reachable, any failing jobs, paused or dry-run state and whether the configured
channels and roles still exist. Then `/scheduler list` for last-run, next-due and
failure counts per job, which is usually the quickest way to tell a wedged job
from one that simply wasn't due. Then the logs:

```sh
docker compose -f docker-compose.prod.yml logs -f --tail=200 bot
```

`LOG_LEVEL=debug` in `.env` plus a restart turns the volume up. It overrides
`config.yaml`, which is mounted read-only.

For "it did something and I don't know why", the `action_journal` table has every
destructive Discord call it attempted, including ones the rate cap or circuit
breaker refused before anything reached the audit log. Rows stick around 30 days.
A row still marked `pending` long after it started is a call that never returned,
which usually means the process died mid-write.

## Code layout

Every plugin implements `core.Plugin` and gets registered in `cmd/bot/main.go`.
Plugins never import each other, they talk through injected dependencies and an
event bus.

| Package | What's in it |
|---|---|
| `internal/core` | plugin lifecycle, event bus, permissions, the command router, embeds |
| `internal/config` | bootstrap config only: token, app ID, DSN, log level, bootstrap admin |
| `internal/settings` | per-guild config in Postgres, cached in memory, invalidated on write |
| `internal/storage` | connection pool, migration runner, SQL migrations |
| `internal/discordguard` | every destructive Discord call goes through here, enforces pause and dry-run |
| `internal/scheduler` | cron core with persisted last-run state, backoff and failure alerts |
| `internal/audit` | audit rows and the `#bird-audit-log` embeds |
| `internal/voice` | what merlin says to members, lines as reviewable YAML, contract in code |
| `internal/plugins/rotation` | channel rotation and the archive sweep |
| `internal/plugins/roles` | jail and timed role grants |
| `internal/plugins/adminconfig` | the `/config` command tree |
| `internal/plugins/ping` | reference plugin, exercises the full lifecycle |

## Contributing

See [`CONTRIBUTING.md`](./CONTRIBUTING.md). Short version: small PRs, `main` is
protected, run vet, tests and the linter before you open one.

Token compromise runbook is in [`SECURITY.md`](./SECURITY.md).
