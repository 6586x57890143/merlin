# HANDOFF

## Where things stand

merlin is moving from discordgo to disgo, for skua's performance targets.
Merged and deployed (#156 to #173, landed through #166 and #176):

- The aimod rung-1 gate (about 2us, no allocation, from 14.9us and 62) and a
  lock-free write guard (37ns, no allocation).
- Every interaction is disgo end to end: decoded once by the gateway, gated,
  authorized (`AuthorizeIx`) and answered through `core.Ix`.
- disgo holds the gateway: zstd, async listeners, a cache of guilds, roles,
  channels, voice states and only the bot's own member. `Permissions` reads it.
- Messages and voice changes arrive as disgo decoded them. Counting a message
  costs 137ns and 4 allocations.

Still discordgo: the REST calls merlin makes as herself, command registration,
and the cold gateway events, which get Discord's raw JSON decoded into
discordgo's structs by `cmd/bot/gateway.go`.

## Queued, in order

One PR each, against `main`, each leaving merlin deployable.

1. **Cold gateway events on disgo types.** Guild ready/join/available and
   leave/unavailable (disgo tells removal from an outage apart), member
   add/update/remove, audit log entries, role update/delete, thread create,
   channel delete. Ports `adminconfig.NudgeIfUnconfigured`,
   `rapsheet.HandleMemberJoin`/`HandleAuditLogEntry`, `roles.HandleRoleUpdated`
   and `contest.HandleThreadCreate`, then deletes `cmd/bot/gateway.go` and
   raw events.
2. **Command registration on disgo.** Definitions as
   `discord.ApplicationCommandCreate`, `RegisterGuild` and the global purge
   through disgo REST, `SyncEmoji` too. `Finalize`'s validation moves with them.
3. **discordguard on disgo REST.** The guard already decides in a single
   compare-and-swap; the calls behind it move. Take skua's 429 policy
   (`userReset`, measured faster with no more 429s) and re-measure it here with
   `/perf` before and after.
4. **Each plugin's Discord ops on disgo**, one PR per plugin: rotation, roles,
   aimod (webhooks, history), contest (forums, threads), rapsheet (bans,
   case-file forum), statistics (backfill), adminconfig. Their fakes move with
   them.
5. **Remove the bridges.** `EmbedOf`, `LayoutOf`, `MemberGo`, `ResolvedUserGo`,
   `NewEmbed`, `PaginationRow`, the discordgo panel builders that `ui_test`
   compares against, the `*Go` test adapters (renumber those fixtures to
   snowflakes), `core.NewSession` and the discordgo module.

After that, as agreed: the shared kit with skua (`aviary`, name not settled)
and moving `/statistics` to skua.

## Watch when it deploys

- READY within 45s of a restart, or the container exits naming the intent
  toggles.
- Commands answer in every server, and panels update on a click.
- `/perf` shows interaction replies, voice time keeps counting, aimod keeps
  scanning.
