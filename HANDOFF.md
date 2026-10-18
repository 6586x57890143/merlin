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

Steps 1 to 7 (cold events, portal intents, commands and the answer window,
plugins on disgo, discordgo out) are done. Two are left:

### Step 8: no allocation on the per-message path

Every message the bot sees goes through statistics and aimod, so this is the
path that scales with the servers' traffic. Today:

| benchmark | now | target |
|---|---|---|
| `statistics` `BenchmarkHandleMessage` | 137ns, 4 allocs | 0 allocs |
| `aimod` `BenchmarkHandleMessageModeOff` | 635ns, 12 allocs | a map read, 0 allocs |

- Statistics keys its counters on snowflakes rather than joined strings.
- aimod asks its gate and config on the guild's snowflake before it builds a
  message view, so a guild with aimod off never builds one.
- Each claim lands as a benchmark in a `test(perf)` PR before the change, and
  the change PR quotes before and after (`go test ./internal/plugins/<name>
  -bench . -benchmem -run x`, at least `-count 5`).
- Keep the stored IDs as text: convert at the edge (`core.ID`), not in the
  schema.

### Step 9: live benchmarks, skua's loop

A performance claim about Discord is measured live, never assumed. A
`tools/` bench, modelled on skua's `tools/reactbench`, for the writes merlin
makes in bursts:

- a bulk jail during a raid (member edits, one per member);
- a rotation (channel create, move, delete).

Rules, the same as skua's:

- Run against a test bot and channel of its own, never merlin's token, so a run
  never shares merlin's rate limit buckets. Its token and channel live in an
  env file outside every checkout; never print or commit them.
- Strategies are entries in one list, compared over at least five interleaved
  trials. Only one that beats the current first by more than run-to-run noise
  replaces it, and the numbers go in the PR.
- Stay inside Discord's invalid request limit (10,000 401/403/429 per 10
  minutes per IP): honour every wait, never retry a 401 or 403, stop on any
  global or shared 429, cap 429s per run.
- The test bot needs creating in the Developer Portal first; that is the
  owner's click.

Alongside, not blocking: plugin status as a `core.Reporter` on each `/help`
page for admins, in place of the per-plugin `status` subcommands; and the
coverage floor rising toward skua's 85% per `internal/` package, one package
at a time.

After that, as agreed:

- A check that merlin is operational after the port (the owner looks in
  Discord; see "Watch when it deploys").
- A write-up of why the port happened: modules swappable and fully
  interchangeable between all the owner's bots, living in a shared module
  store. The store needs a name that does not overlap cygnet's flock or
  fleet ("aviary" was only a proposal).
- The shared kit with skua, cut from intents, ratelimit, the GCRA guard and
  obs once both bots run the same code.
- Moving `/statistics` to skua.

## Watch when it deploys

- READY within 45s of a restart, or the container exits naming the intent
  toggles.
- Commands answer in every server, and panels update on a click.
- `/perf` shows interaction replies, voice time keeps counting, aimod keeps
  scanning.
