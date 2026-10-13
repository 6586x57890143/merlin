# HANDOFF

## Where things stand

merlin runs on disgo alone, for skua's performance targets: the port ran
from #156 to #201 and discordgo is out of `go.mod`.

- The aimod rung-1 gate (about 2us, no allocation, from 14.9us and 62) and a
  lock-free write guard (37ns, no allocation).
- Interactions, the gateway, every REST call and command registration are
  disgo. One REST client, on the transport `/perf` and cygnet read.
- Intents come from the Developer Portal, install permissions from the
  plugins switched on, and a handler's context ends when Discord stops
  waiting for its answer.
- Modules keep `snowflake.ID` from the event to the REST call; stored IDs
  stay text and convert once (`core.ID`).
- Counting a message costs 137ns and 4 allocations.

## Queued, in order

One PR each, against `main`, each leaving merlin deployable. Where skua
already does a thing, merlin takes skua's version rather than writing a
second one; those pieces are what the shared kit (name not settled) will be
cut from.

1. **No allocation on the per-message path.** Statistics keys its counters on
   snowflakes rather than joined strings (137ns and 4 allocations today, to
   none), and aimod asks its gate and config on the guild's snowflake before
   it builds a message view, so a guild with aimod off costs a map read
   (635ns and 12 allocations today). Each claim is a benchmark in
   `test(perf)` before it is a change.
2. **Live benchmarks, skua's loop.** A `tools/` bench for the writes merlin
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
