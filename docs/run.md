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
