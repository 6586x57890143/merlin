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

One PR each, against `main`, each leaving merlin deployable. Where skua
already does a thing, merlin takes skua's version rather than writing a
second one; those pieces are what the shared kit (`aviary`, name not
settled) will be cut from.

1. **Cold gateway events on disgo types.** Guild ready/join/available and
   leave/unavailable (disgo tells removal from an outage apart), member
   add/update/remove, audit log entries, role update/delete, thread create,
   channel delete. Ports `adminconfig.NudgeIfUnconfigured`,
   `rapsheet.HandleMemberJoin`/`HandleAuditLogEntry`, `roles.HandleRoleUpdated`
   and `contest.HandleThreadCreate`, then deletes `cmd/bot/gateway.go` and
   switches raw events off.
2. **Intents from the Developer Portal**, as skua's `internal/intents`. Read
   the application's flags, identify with what is granted, and give each
   plugin a `Want` (aimod requires MESSAGE_CONTENT, roles would like
   GUILD_MEMBERS). A plugin whose required intent is missing is skipped and
   says so on `/help`; a toggle changed in the portal restarts merlin onto it.
   Close code 4014 can then not happen, so `OpenGateway`'s watchdog becomes a
   backstop, and `MERLIN_DISABLE_GUILD_MEMBERS_INTENT` and
   `MERLIN_ENABLE_MESSAGE_CONTENT_INTENT` go: the portal toggle is the switch.
3. **Commands on disgo, and answered in time.** Definitions as
   `discord.ApplicationCommandCreate`; `RegisterGuild`, the global purge and
   `SyncEmoji` through disgo REST, with `Finalize`'s validation moving along.
   A handler's context ends when Discord stops waiting for its first
   response (skua's `deadline`, anchored to the interaction's snowflake),
   not after a flat 30s, so a slow REST call fails in time to tell the
   member why.
4. **Install permissions from the plugins.** Each plugin declares the
   channel permissions it uses as itself (`Perms`, as skua's modules do), and
   the app's install settings ask for the union of what the plugins switched
   on right now need (`core.SyncInstall`): written at boot, and again when a
   plugin is switched on or off in any server. That is least privilege over
   time rather than per link: nobody grants delete, ban or timeout until
   aimod or rapsheet bans are in use somewhere, and the grant goes when they
   stop. cygnet reads the same settings, so `fleet.Perms` and the three
   hand-kept invite links in `docs/install.md` go. A server installed before
   a plugin needed more keeps its old grant until re-invited, so that
   plugin's `/help` page names what is missing and links the current
   install.
5. **discordguard on disgo REST, with skua's rate limiter.** The guard
   already decides in one compare-and-swap; the calls behind it move to
   disgo, wrapped in skua's `ratelimit.Precise` (a 429 waits the body's
   millisecond `retry_after`, not Retry-After's whole second; `userReset` for
   user-scope 429s) and `obs.Limiter`, so `/perf` splits Discord's wait from
   the round trip. Measure `/perf` before and after.
6. **Each plugin's Discord ops on disgo**, one PR per plugin: rotation, roles,
   aimod (webhooks, history), contest (forums, threads), rapsheet (bans,
   case-file forum), statistics (backfill), adminconfig. Their fakes move with
   them, and so do IDs: a plugin keeps `snowflake.ID` from the event to the
   REST call instead of formatting and parsing strings at each step. Columns
   stay text; the conversion happens once, at the store.
7. **Remove the bridges.** `EmbedOf`, `LayoutOf`, `MemberGo`, `ResolvedUserGo`,
   `NewEmbed`, `PaginationRow`, the discordgo panel builders that `ui_test`
   compares against, the `*Go` test adapters (renumber those fixtures to
   snowflakes), `core.NewSession` and the discordgo module.
8. **No allocation on the per-message path.** Statistics keys its counters on
   snowflakes rather than joined strings (137ns and 4 allocations today, to
   none), and aimod asks its gate and config on the guild's snowflake before
   it builds a message view, so a guild with aimod off costs a map read
   (635ns and 12 allocations today). Each claim is a benchmark in
   `test(perf)` before it is a change.
9. **Live benchmarks, skua's loop.** A `tools/` bench for the writes merlin
   makes in bursts: a bulk jail during a raid (member edits) and a rotation
   (channel create, move, delete). Run against a test bot and channel kept
   outside the checkout, as skua's `flightless.skua` is, with strategies
   compared over at least five interleaved trials, inside Discord's invalid
   request limit. Only a strategy that beats the current one by more than
   run-to-run noise replaces it.

Alongside, not blocking: plugin status as a `core.Reporter` on each `/help`
page for admins, in place of the per-plugin `status` subcommands; and the
coverage floor rising toward skua's 85% per `internal/` package, one package
at a time.

After that, as agreed: the shared kit with skua, cut from intents, ratelimit,
the GCRA guard and obs once both bots run the same code, and moving
`/statistics` to skua.

## Watch when it deploys

- READY within 45s of a restart, or the container exits naming the intent
  toggles.
- Commands answer in every server, and panels update on a click.
- `/perf` shows interaction replies, voice time keeps counting, aimod keeps
  scanning.
