# Code layout

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
| `internal/secret` | AES-256-GCM for the few things stored encrypted at rest |
| `internal/plugins/rotation` | channel rotation and the archive sweep |
| `internal/plugins/roles` | jail and timed role grants |
| `internal/plugins/aimod` | the AI moderation ladder, its policy files and the tip jar |
| `internal/plugins/contest` | contests: phases, the entry forum, the prize ledger |
| `internal/plugins/rapsheet` | the per-member moderation ledger, the escalation ladder, bans, case files, alt hints |
| `internal/plugins/statistics` | hourly activity and membership counts, `/statistics` |
| `internal/plugins/adminconfig` | the `/config` command tree |
| `internal/plugins/ping` | reference plugin, exercises the full lifecycle |
| `web/contest` | the contest gallery and ballot, a Cloudflare Worker |
| `web/lab`, `cmd/lab` | merlin's own packages compiled to wasm, so the voice lines and rotation notices can be read in a browser |
