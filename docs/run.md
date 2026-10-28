# Run merlin yourself

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

# One-time supporter smoke

`TestLiveSupporterSelfBotLifecycle` is skipped by default. After explicit operator
authorization, build the roles test binary for the deployment host and run only
that test with `MERLIN_LIVE_SUPPORTER_SMOKE=self-bot-one-time`. Supply a JSON object
on private stdin containing `Token`, `Guild`, `Channel`, `Role` and `Bot`; never put
the token in argv, environment variables, logs or a committed file. Preflight the
guild's write controls and confirm the chosen channel belongs to it. Run in an
isolated, resource-limited container with logging disabled and no published ports.

The test targets only the authenticated bot itself, refuses a role it already
holds, and requires a permissionless unmanaged role below the bot. It exercises
the actual supporter sync, event bus, welcome plugin, write guard and Discord REST
calls. It posts one card labelled as an integration test in the supplied channel,
checks renewal sends no second welcome, and verifies expiry removes the role.
Cleanup also attempts to remove an uncertain grant. The grant store and channel
override exist only in memory; production settings, supporter periods and grant
rows are not written. This smoke does not replace funded checkout, fleet-link or
production database validation. Keep the production thank-you channel unchanged.
