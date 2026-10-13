# When something breaks

**Make it stop.** **Pause** on `/config`'s Pause and dry-run page refuses every rotation, archive
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

**Work out what happened.** `/config` in Discord first (its overview): database
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
