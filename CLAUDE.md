# CLAUDE.md

## What this is

`merlin` is a production-grade, modular Discord bot (Go 1.27, disgo, pgx v5). Design doc: [`spec.MD`](./spec.MD); read §4 (security), §4a (commands/permissions) and §9 (milestones) before any non-trivial change. Built for a high-free-speech community exposed to weaponized mass reporting, so **fail-safe behaviour and defense in depth are requirements, not polish**.

This file is the rules. **[`docs/design.md`](./docs/design.md) is the why**: the bug behind each rule and the alternatives rejected, under the same headings. Read the matching section before changing how a subsystem behaves, and keep both in step.

## Commands

```sh
go build ./... && go vet ./...
golangci-lint run                             # CI's golangci-lint v2.13.1
go test ./... -race -cover -covermode=atomic  # matches CI (needs CGO_ENABLED=1)
go test ./internal/plugins/rotation/... -run TestRotateFullCycle -v
go test ./... -coverprofile=coverage.out && scripts/coverage-floor.sh coverage.out   # coverage gate
go test ./internal/... -run x -bench . -benchmem   # hot paths
go run ./tools/icons                          # redraw plugin icons
govulncheck ./...
```

Every package under `internal/plugins/` needs 70% statement coverage; older plugins are pinned at their coverage and may only go up.

`internal/settings` and `internal/storage` have Postgres tests (`internal/dbtest`) that skip without `TEST_DATABASE_URL`. Locally: `docker compose up -d postgres`, then `TEST_DATABASE_URL=postgres://merlin:changeme@localhost:5432/merlin?sslmode=disable go test ./internal/settings/... ./internal/storage/...`.

Local run: copy `.env.example` to `.env` and `config.example.yaml` to `config.yaml`, then `docker compose up --build` or `go run ./cmd/bot` (migrations run at startup).

CI (`.github/workflows/ci.yml`): vet, lint, race tests, govulncheck, gitleaks, Docker build, the prose check (no em dashes or curly quotes anywhere), a js/wasm build of `cmd/lab`, `scripts/check-contest.mjs`. Push to `main` builds a multi-arch image to GHCR and deploys via `docker-compose.prod.yml`.

## Architecture

### Plugin model

- Every feature implements `core.Plugin` (`Name/Init/Start/Shutdown`), registered in `cmd/bot/main.go`. **No dynamic loading.** All `Init`s before any `Start`; `Shutdown` in reverse; every lifecycle call under `recover()`.
- **Plugins never import each other.** They talk through `Deps`, the `core.EventBus`, or a narrow interface wired in `main.go` (`aimod.Jailer`, `aimod.History`, `aimod.Traffic`, `roles.SupportersFunc`).
- `EventBus.Publish` runs subscribers synchronously under `recover()`: don't block, don't publish/subscribe reentrantly under a lock.
- **Narrow interface seams**: a consumer defines its own minimal interface (`core.GuildAuthData`, `rotation.SettingsProvider`) instead of importing the concrete store. Prevents cycles (`core` never imports `settings`) and allows tiny fakes.

### Command & Permission Framework (spec.MD §4a)

- `core.CommandRouter` **alone** registers and dispatches slash commands: `Commands.RegisterCommand(pluginName, cmd)` + `HandleIx(top, path, PermSpec, handler)` in `Init`. Never add interaction handlers or bulk-overwrite yourself.
- **One top-level command per plugin.** Exceptions: `/config`, and rapsheet's `/ban`, `/kick`, `/timeout`. Not a precedent.
- **`PermSpec{Tier, Action}` is mandatory.** Zero tier fails `Finalize()`; above `TierPublic` needs an `Action`.
- `Authorize` order: (0) plugin enabled in guild; (1) deny list, beats everything but bootstrap; (2) tier: guild `RequiredTier` override else the spec's. `TierAdmin` = DB admin, bootstrap, or Administrator bit; `TierMod` = DB mod role or admin, no permission-bit shortcut; (3) allow list, additive.
- Only Mod/Admin overrides are honoured; resolve the effective tier **before** the public shortcut.
- Mutating admins, mod roles, tiers, allow/deny or toggles is `TierAdmin`. `validateTierChange` refuses lowering `config.mutate` below Admin.
- `default_member_permissions` stays **unset** on Mod/Admin commands. Exceptions set to `0`: `/statistics` (`TestCommandIsNotListedToTheServer`) and `/perf`.
- `adminconfig` can never be disabled; it and `help` are never offered as toggles.
- Commands register **per guild** from `GuildCreate`, which must never bail early: on settings refresh failure call `settings.Store.MarkStale` and continue; only rotation's `SyncGuild` is skipped (`RetryStale` reconciles).
- Every dispatch runs under `recover()`.
- State-backed options get autocomplete (max 25) **plus** a `list` subcommand. Channel/role/user options use native types (sole exception: `/rapsheet view`'s `user`, validated by `viewTarget`).
- **Discord limits are correctness constraints.** Over 3s: `core.DeferResponse` then `FollowUpOK/Err`. `core.TruncateEmbedField` (1024), `TruncateEmbedDescription` (4096). Repeated edits replace attachments. `validateCommandShape` checks limits at `Finalize()` because one bad definition silently fails the whole `BulkOverwrite`.
- Read arguments with `core.LeafArgs(i)`.

### On disgo (`internal/core/ix.go`)

- Interactions arrive as `*core.Ix` (string IDs, REST client with mentions off). Register with `HandleIx/AutocompleteIx/HandleComponentIx/HandleModalIx`; refuse with `Ix.Tell`. Gateway: `core.NewGateway`; lookups read `gw.Caches`.
- Every write the bot makes as itself goes through `discordguard` (`snowflake.ID`; `core.ID` parses a stored string, the guard refuses 0).
- Tests decode real interactions (`paneltest.IxCommand/IxClick/IxPick/IxSubmit`).

### Settings panels (`internal/core/panel.go`)

- Plugin settings are a **panel** (one Components V2 container from a bare top-level command, redrawn in place), not `configure` subcommands. Reference: `adminconfig/panel.go`. Moderation actions on a person stay commands.
- Layout: title and one `-#` line beside the mood thumbnail, body full width, `merlin_rule.png` gallery above the footer. Facts via `core.Readout`; code blocks only for things to copy.
- **No session**: re-read everything each render; control state rides in the CustomID.
- `view:` = navigation at read tier, `set:` = mutation at mutate tier, both re-checked by the router; mutating controls drawn `Disabled` for anyone `Authorize` would refuse. Action names match the subcommands they replaced.
- Set pickers diff against **what was drawn** (`core.SelectDiff`), never current settings; each add/remove is its own call and audit line.
- A click redraws with a `Note` (`✓ ...`/`✗ ...`). Slow work: `DeferPanelUpdate` + `FollowUpPanel`.

### Plugin icons (`tools/icons`, `internal/core/emoji.go`)

- Every plugin has `assets/emoji/plugin_<name>.png` (`TestEveryPluginHasAnIcon`), drawn from 11x11 grids in `tools/icons/main.go`. Edit the grid and run `go run ./tools/icons`; never edit PNGs (`TestIconsAreOnTheirGrid`).
- `core.SyncEmoji` uploads missing icons as `<key>_<hash>` app emoji. Use `PluginEmoji/PluginComponentEmoji/PluginIcon`; before sync, headings fall back to text. Panels lead their title with the icon; the thumbnail stays the mood.

### The field guide (`internal/plugins/help`)

- **Every plugin implements `core.Helper`** (`TestEveryPluginHasAHelpPage`). merlin's voice: lowercase, first person, plain. Tiers from `CommandRouter.LeafTier`.
- Stateless; clicks on a posted guide answer privately. Disabled plugins: `off here` to admins, hidden from others.

### Observability (`internal/obs`, `/perf`)

- Always on, allocation-free: interactions per plugin (timed in `commands.go`), jobs as `job <kind>`, REST per route plus rate-limit wait (`obs.Limiter`).
- `/perf` is the bootstrap operator's alone (TierAdmin + identity check, permissions 0).
- `MERLIN_DEBUG_ADDR` (pprof + flight recorder) **refuses non-loopback addresses**; use `ssh -L`.

### Self-serve, DB-backed config

- `config.yaml`/`.env` are bootstrap only: token, app ID, DSN, log level, `MERLIN_BOOTSTRAP_ADMIN_USER_ID` (comma separated; each always `TierAdmin`; `Permissions.IsBootstrapAdmin`).
- Everything guild scoped is in Postgres via `settings.Store`, cached per guild, refreshed on mutation, invalidated by `core.EventConfigChanged`. Admins configure through Discord, never host files.
- `/config setup` creates nothing on its own: pickers for existing things, optional create buttons, state re-derived each render. `/config import` is a one-time explicit YAML migration.
- `config.Loader.OnReload` hooks run outside the write lock, only for valid reloads.

### Scheduler (`internal/scheduler`)

- Recurrence is a `core.Schedule` (`IntervalSchedule`/`CalendarSchedule`), `next_due = Next(last_run)`, persisted per `guildID:name`. Never type-switch on the kind.
- Tunables live in the const block in `scheduler.go`; no inline literals.
- Runs bounded by `jobTimeout`; bookkeeping uses a **detached** context (`context.WithoutCancel`).
- Runtime-varying jobs must `Unregister` (reconcile pattern). A never-seen job is due at once; `Seed(ctx, key, now)` after `Register` to wait a period.
- **A job exists only where it has work** (`reconcileSweepJob`, `reconcileNoticeJob`, rapsheet sweep, statistics backfill, funding poll).

### Channel Rotation (`internal/plugins/rotation`)

- `reconcile(guildID)` diffs DB settings against registered jobs, from `SyncGuild` and `EventConfigChanged`. Don't also call `SyncGuild` after a mutation.
- Each cycle re-derives every step from live Discord/Postgres state so a crash self-heals, including after the archive (`archivedChannelOrigin`). Replacements match name **plus** category.
- **New channel first**: create and populate the replacement before touching the original. The reveal/archive swap is deliberately serial.
- `restorePosition` runs **after** the archive, is non-fatal, skipped on the resumed path.
- Retention deadline is **re-derived every sweep** (`archiveDeadline`: `ArchivedAt` + the slot's current `RetentionHours`, via `rotation_archives.rotation_id`); `delete_after` is a fallback. nil = keep forever; moving an archive out of its category = keep it.
- `rotation-sweep` exists only where there are rotating channels or pending archives; a failed lookup leaves it as is.
- `interval_minutes`, floor `minRotationInterval` (1h). `core.ParseFlexibleDuration` takes m/h/d.
- **Disclosure** (`full|cadence|retention|generic`) maps to six voice keys via `introKey` (key and vars together); `plainRetentionNotice` honours it. Closed set (validation, normalization, CHECK); unreadable = `full`. `archive_visibility: whitelist` accepted, no longer offered.
- **Heads-up**: `notice_lead_minutes` (default 10, 0 off, below the interval), `ColorWarning` embed, countdown via `NextDue`. Claimed in `rotation_notices` (`ON CONFLICT DO NOTHING`) **before** posting; none when overdue or under a minute out; `generic` gets no countdown. `reconcileNoticeJob` assumes `p.mu` is held.
- Member-facing durations are prose (`humanDuration`); admin/audit ones compact (`core.FormatDuration`).

### Audit and status rendering (`internal/audit`, `adminconfig/status.go`)

- Automated actors pass `core.ActorSystem` ("merlin (automatic)" via `core.FormatActor`).
- `core.MentionUser/Channel/Role` at the call site; they return `""` for `""` (load bearing). ID lists as `[%s]` of `core.MentionRoles/MentionUsers`, never `%v`.
- Every entry is a `cygnet/ledger` card (`audit.toEntry`, `ledger.Render`). Call sites write `key=value`; `toEntry` makes fields. New shape = new row in `TestEveryCallSiteShapeIsAValidEntry`.
- `audit_log` is a per-guild sha256 chain (migration 0047); legacy columns still written.
- Runs of one action by one actor batch into one self-editing card (`ledger.Batcher`).
- `audit.actions` maps action to title/colour (`humanize` fallback, `ColorInfo` default); `*.dryrun` wears `MoodIdle`.
- **The audit post uses the raw REST client, not `discordguard`** (posts while paused/dry-run), `AllowedMentions` zeroed.
- `scheduler.alert(jobKey, failures, cause)` posts an embed with a truncated error field.
- `/config status` caps names at `maxMentionsListed`, tracks severity while building (never by scanning for glyphs), truncates its description.
- **An audit failure never fails the operation.** Log and continue.

### Mood icons (`internal/core/embeds.go`)

- Mood thumbnail is **derived from the embed colour** (`moodForColor`); override only with `core.WithMood`.
- Attachments are derived from the finished embed (`core.EmbedFiles`), never hand-listed.
- `NewEmbed`: no footer, no timestamp, `merlin_rule.png` image unless set. `NewLandmarkEmbed` clears the thumbnail.

### merlin's voice (`internal/voice`)

- Every member-facing message comes from here. Lines are data (`lines/*.yaml`); the contract is code (`keys.go`: register, **required placeholders**, length, fallback).
- `loadCatalog` refuses to boot on: missing required placeholder, **any placeholder outside the required set** (keep `spec.optional` empty on rotation keys), unbalanced braces, fewer than 8 lines, duplicates, em dashes/curly quotes.
- Playful keys span calm to blunt by band; `moderation.*` is warm, never witty.
- Depend on `voice.Source`. `Line` never errors; `""` means say nothing. Generators pass output through `Validate`. Pin `voice.WithRand` in tests that read a line.
- `PERSONA.md` is the brief. Operator sticky messages and admin panels are **not** voiced.

### Temporary Role Management (`internal/plugins/roles`)

- Own store (`role_jails`, `role_grants`). Per-guild `roles-sweep` every minute.
- **Release/expiry fire on their own timers** (`release.go`, `time.AfterFunc`), armed wherever `release_at`/`expires_at` is written and by the sweep within `armLookahead`. Timers re-read the row; `releaseJail`/`revokeGrant` `claim` it; a miss returns `errReleaseInProgress`. Drop a row only on Unknown **Member**, never Unknown Guild. `ForgetGuild` disarms.
- **Jail is role-based**: one `GuildMemberEdit` to the shared marker role plus roles the bot can't manage (kept, reported). Visibility from the marker's channel overwrites (`jailchannels.go`); **Sync every channel** recomputes them, also on first marker creation.
- Marker denies only where they do something (`markerOverwrite`): skip rooms whose **channel-level** @everyone already denies View (+Connect); delete merlin-shaped ones there; leave edited ones.
- No member-level jail overwrites. `clearMemberJailOverwrites` checks every channel on release/transfer for `jailMemberShape` only; sync clears leftovers only where the audit log says merlin wrote them.
- **A marker never gets more than an ordinary member on an allowlisted channel** (`memberBaseline` = @everyone + `member_role_id`, `desiredOverwrite`). Withheld bits go in the deny. Merge `(existing.Allow|wantAllow)&^wantDeny`. `restrictedBits` denied on allowlisted rooms. Changing the member role re-syncs both markers.
- **`applyJail` records before it strips**, rolling back on failure. Drop the cached marker ID only on `ErrCodeUnknownRole`.
- **Confused deputy**: sweep and `/roles release`/`revoke` re-fetch the member; marker or granted role missing = already handled, untrack without restoring.
- **Jail survives rejoin**: `JoinedAt` after `JailedAt` (+`rejoinGrace`) re-applies the marker without rewriting snapshot or `ReleaseAt`; `MarkJailApplied` answers each rejoin once. `HandleMemberJoin` closes the window with `GUILD_MEMBERS` (intents follow the portal via `core.Probe`).
- **No `GuildMemberRemove` handler in roles, deliberately**: the row must survive leaving (`evasion_test.go`).
- Grant/revoke `TierAdmin`; jail/release `TierMod`. `CanModerate` gates jail.
- **Bulk jail** (`bulkjail.go`): up to five `User` options or `/roles jail-role` (`TierAdmin`), both via `jailMany`; one `roles.jail_bulk` entry. `partitionByRank` before `resolveJailRole`. Over `maxBulkJailTargets` (raw count) refuses; `membersWithRole` returns `complete=false` on truncation.
- **Absent targets** (`resolveTargets`): a row only if `GET /users/{id}` confirms the account; other failures fail. No DM or announcement; audit `roles.jail_pending`/`vacation_pending`. In-effect is derived live.
- **Supporters** (`supporters.go`): merlin creates her own Supporter role per guild; `roles-supporters` (10 min) mirrors `cygnet.supporters` as timed grants, only adds and re-dates later, never converts a hand-given role, skips jailed members, publishes `core.EventSupportersJoined` for first grants, DMs a reminder a week before expiry (claimed in `supporter_reminders` first).

### Scripts (`internal/scripts`)

- Small per-server behaviours as Go code in the owning plugin, **off by default**, toggled by `/<plugin> scripts set` (`TierAdmin`, warns), stronger than admins while on. Unreadable switch = off. Definitions are code, never commands.
- **vacation**: a `role_jails` row whose `jail_role_id` is the vacation role; the kind is **derived** (`sentenceFor`), never stored; only words and ledger kind branch (`sentence` table). The role is never created. Visibility via `visibilityMarker`/`syncAllVisibility`, synced only on enable, role config, allow/disallow, `sync-channels`. Transfers strip to the new marker **first**, then `TransferJail`; `detectManualTransfer` handles hand swaps; `JailAutomatic` on a vacationer moves them to jail for the later end. The switch gates only the command.
- **eternal-role**: rows in `script_eternal_roles`, added only by owner or operator (`canDefineEternal`). `enforceEternalRoles` re-derives from live state: give back, recreate from the stored copy, or copy above an edited role. The copy is captured once at `eternal-add`; second add refused; disabling keeps rows. Yields to jail. Never compares position.

### AI Moderation (`internal/plugins/aimod`)

- Needs `MESSAGE_CONTENT`; without it commands register, nothing is scanned, `/aimod status` says so.
- **The ladder**: rung 0 free skips; rung 1 regex table (tokens, phishing, SSN, slurs); rung 2 cheap batched model; rung 3 better model, one message, full policy and context. **Rung 2 can only flag. Delete/rewrite needs rung 3 above `actThreshold`. A failed deep pass acts on nothing.**
- Rung 1 slur hits get a second look (`secondlook.go`): the deep model sees only the matched words and can only route *to* the model rungs; failure leaves the hit. Whole-word slurs (`wholeWordSlur`) are never sent. `acrossWords` refuses cross-word matches, retrying inside the first word.
- Rung-1 readers see folded text (`fold.go`), applied inside the readers.
- Rung 1 is gated (`gate.go`); **the gate may never change an answer** (`gate_test.go`). `HandleMessage` reads rung 1 once.
- The hard-slur block runs **ahead of `shouldSkip`**, obeys mode off and hate_speech off, takes the early path only when the slur is the whole hit, logs `aimod: hard slur`.
- `nWordShape`, `hiddenSlur`, `looseSlur`, `nearNWord`: check every new entry against a 370k-word English list. Their hits go to the deep pass regardless (`withSlurHints`).
- **Policy** is `policy/*.yaml`, each with `violations` **and** `not_violations` (validated at boot). `child_safety` cannot be disabled, humour is no defence, `validateCalibration` refuses loosening examples; `calibrationPreamble` carries the same carve-out (`TestCalibrationPromptCarriesTheSameChildSafetyRule`).
- Fast prompt: one `short` line per enforced bucket; full policy only to the deep pass. `temperature: 0` always.
- **Budget**: exact cost from responses (OpenRouter `usage.cost`, OrcaRouter `usage.cost_usd` via header, in `chatOnce`). Book usage even on parse failure. Unreadable spend fails **closed**. Over cap: rungs 0-1 only, one audit entry a day.
- **Every model call goes through `paidChat`**, never `p.client.Chat`: `maxPaidCalls` slots, allowance reserved, cost booked; no call until a failed receipt lands.
- **Cost-drain**: `userMeter` ceilings (`scanCeilings`, `maxUserDeep`); over the deep ceiling = flagged, never dropped. `similarSpam` skips near-copies, yields to `mustScan`; `forgetSimilar` on a confirmed violation.
- **Sanctions use jail** via `aimod.Jailer` (`roles.JailAutomatic`), duration from severity, doubling per prior in `repeatWindow`. Priors from the rapsheet (`aimod.History`), falling back to `CountSanctions`, never zero. Sanction row written whether or not the jail lands; reversed incidents don't count.
- **Never aimable at staff**: `CanModerate` with nil actor; `timeoutMember` refuses owner/Administrator/Moderate Members/Manage Messages, failing closed. `/aimod moderate-me` only waives the rank check; only bootstrap adds others. Bootstrap is never sanctionable.
- **Member opt-out** (`optout.go`): guild switch off by default, flipped only by owner/operator (`ownerOrOperator`); members opt only themselves. Opt-out = exempt role (`scanExempt` atop `scan`), still under the slur block and **`mustScan`**. Switch off keeps the list. The DM mentions opt-out only where offered.
- `/aimod status` is paginated and stateless; every page wears the report's severity; clicks `core.DeferUpdate`.
- **Rewrite** = delete and repost via webhook as the author; resolve the webhook **before** deleting; mentions zeroed; "edited by merlin" marker with a random code for `/aimod why`. Failure after the delete becomes a removal (`rewriteFellShort`, `aimod.rewrite_failed`); too long = removal decided before deleting.
- **Verdicts are re-checked before acting** (`revision.go`): per-message lock, discard on edited text, `stillActs` re-reads gate and config, only moves an action down.
- **Dedupe** keys clears on words+reply; a confirmed violation is reused only for the same author and reply target, else it's a `confirmHint` to a fresh deep pass.
- **Record the incident before touching the message.** Evidence retention re-derived at prune from `aimod_config`.
- **Guild word list** (`wordlist.go`) is house style: `BucketWordList`, outside `AllBuckets`, never sanctions or reaches the rapsheet; run over model rewrites before `redactSlurs`.
- Config is cached (`cachingStore`, `configTTL`); **every new `Store` setter must invalidate**.

### Rung 1.5: the local triage model (`triage.go`)

- Per-guild logistic regression predicting **rung 2's output**. Only skips or passes; never flags or acts.
- Online training from fast-pass verdicts, **nothing retained**; `aimod_triage` holds only weights.
- Fails toward scanning: `triageWarmup` 500, `triageSkipThreshold` 0.02, `neverSkipPattern` and `hiddenSlur` vetoes before the model, `triagePosWeight` 12, `triageSampleRate` 5% of skips still scanned.
- `triage_mode` off/shadow/on, default shadow; counters run in every mode.
- The skip sits ahead of `userMeter.allowScan`, behind the dedupe lookup.
- Weights saved every 500 examples and on `Shutdown`; a wrong-length blob is refused.

### Two gateways (`provider.go`)

- `providerSpec` is a two-row table (OrcaRouter default, OpenRouter fallback), not an interface. nil = OpenRouter on `c.base`.
- **The gateway is derived from which key is stored** (`route`), never a column; key form infers it from the prefix.
- OrcaRouter: singular `model`, cost header required, free-tier 429s not retried (`freeRateLimited`).
- **Only the deep rung falls over** (`escalate`).
- OrcaRouter has no ZDR: `strictPrefs` false, `chatOnce` strips the provider block, `/aimod status` says so (`privacyLine`).
- OrcaRouter "free" means no fees, not free models: never tell a guild it cannot run out of credit.

### The tip jar (`funding.go`, `erc20.go`)

- **The bot holds no key and moves no money.** Automated: balances, burn, runway, a daily `aimod.funding_low`.
- Gauge reads the routed gateway: `KeyInfo` (OpenRouter `GET /key`) or `OrcaBalance` (same struct, cents). Free tier has no balance.
- OpenRouter `limit_remaining` is **key allowance** against `limit`, not account balance. A 402 sets `paymentRefused`, shown separately, never zeroing the allowance; on status it empties the paid meter and suppresses runway. An accepted paid call clears it.
- **An address selects a family, not a chain** (`familyFor`): `0x` = all EVM rails summed, `T...` TRON, 32-byte base58 Solana.
- `fundingRail` (verified chain facts, contracts checked live for `symbol()`/`decimals()`) is separate from `topUpChains` (dated merchant config).
- **A partial read is never booked**: `Balances` is all or nothing.
- Per-rail breakdown in `aimod_funding.balances`; public view shows non-zero rails and the explorer link.
- JSON-RPC errors arrive with HTTP 200; parse `uint256` with `math/big`; no web3 dependency. `MERLIN_RPC_<CHAIN>` and `MERLIN_TOKEN_<CHAIN>_<ASSET>` set together.
- **Only owner or operator sets the address** (`canSetFunding`); any admin may `clear`. Audit names both addresses; public view warns for 24h.
- Donations are balance deltas; first poll is a baseline; a fall is a withdrawal. `aimod_funding` is separate from `aimod_config`. Funding job not `Seed`ed.
- `funding/show` is the one public answer (`core.DeferResponsePublic`); mentions zeroed. Addresses, balances and warnings are code-authored, not voiced.
- Face follows the credit gauge (`tipJarMood`). `/aimod funding` needs a redesign; not a reference.

### The browser lab (`cmd/lab`, `internal/lab`, `web/lab`)

- merlin's real packages compiled to WebAssembly. **The lab reimplements nothing**: it calls the same functions the bot does (`rotation.RetentionNotice`, `HeadsUpNotice`, `ValidateChannel`, `core.IntervalSchedule.Next`, `voice.Validate`).
- Logic in `internal/lab` (native tests), glue in `cmd/lab` (`js && wasm`), driven by `scripts/check-lab.mjs`. No TinyGo.
- `scripts/build-lab.sh` copies `wasm_exec.js` from `GOROOT`; it and `lab.wasm` are gitignored.
- **No authentication, deliberately.**
- `internal/config` reload files are tagged `unix`/`!unix`; no GOOS filename suffixes there.

### Contests (`internal/plugins/contest`)

- Four phases on one per-guild `contest-tick`; entries are posts in a merlin-created forum; the Cloudflare Worker `contest` serves gallery and ballot.
- Discord hosts the files; `media_url` is a cache refreshed from the starter message before its `?ex=` expiry (`soonestExpiry`, `dueForRefresh`, `refreshMargin`, `refreshFallback`, `refreshFloor`) for 30 days after close (`artRefreshWindow`), not for cancelled contests.
- No gateway intent, but needs the portal's Message Content toggle; `ErrNoMessageContent` fails loud.
- Entries re-derived from active **plus archived** threads (`forumThreads`); withdrawal only while submissions are open, only for threads Discord says are gone, **before** the upsert loop.
- `noVotes`: nobody voted, nobody crowned.
- Phase moves claim first (`AdvancePhase`); `finish` claims last and records `tally_error` on failure.
- The Worker is dumb storage: full snapshot pushes, no standings, three outbound calls, nothing inbound.
- Votes via Discord OAuth `identify guilds.members.read`; IDs HMAC-hashed on arrival (`TestTokenGoldenVector`, `check-contest.mjs`). `ballotOpen` checks in the vote's own batch.
- Prize codes sealed with `internal/secret` in Postgres only, entered via modal; record, DM, wipe only on a landed DM; `/contest claim` is guild-scoped (`PrizesAwardedTo`).
- Pledges wait for mod approval; mods see `codeShape`, never the code. Reject wipes ciphertext in the same statement.
- Cancelling deletes nothing. No Worker URL degrades; no secret key refuses codes; Worker URL without link key refuses to boot.
- `web/contest` is one HTML file, no framework or build. Sprite cuts: never chroma key a sheet with alpha; gutters by alpha projection; names by image difference.

### Statistics (`internal/plugins/statistics`)

- Hourly buckets from the gateway (no text read), flushed every `flushEvery` as one `unnest` upsert; failed flushes put cells back (`pendingCap`); `Shutdown` flushes.
- Joins/departures counted with **no user id**. Names recorded as seen (`stats_users`, `stats_channels`), not looked up.
- `/statistics report` is operator-only; `daily`/`channels` are `statistics.view`. `daily`: two charts, never a shared y-axis; zero-fill days, pre-coverage days blank and shaded; partial days dimmed and out of figures.
- **Backfill is idempotent** (`backfill.go`): complete hours only, `SetHour` replaces, cursor-resumed, `backfillSlice` 5m runs, registered only while pending, stops before `live_since` truncated to the hour.
- Voice time from channel changes into `stats_voice_hourly`; mute/deafen ignored; `SyncVoice` seeds from `GuildCreate`.
- Heatmap GitHub-style (hourly up to a week, weekly beyond). `TestRenderSamples` writes to `ACTIVITY_SAMPLE_DIR`.
- Chat and voice listed separately. Partial windows say "counting began ..." in `ColorWarning`.
- Feeds aimod via `aimod.Traffic` only while aimod has no receipts.
- Retention per guild, re-read at prune, default 90 days, max ten years, no "forever".
- Card emoji are Twemoji from a pinned CDN path (`TestTwemojiKeys`).

### Rapsheets (`internal/plugins/rapsheet`)

- `rapsheet_entries` is the record; every write through `record()`. **Points frozen at write time**; automatic consequences and ladder rows score zero.
- Score decays (half-life 30d default); `ValidateBands` refuses a ban band without a duration.
- Ingestion via the bus (`EventModerationAction`/`Reversed`): dedupe on `(source, ref)`, drop own `ladderReasonPrefix` jails, write on a detached goroutine.
- `escalation_mode` off/suggest/auto; **auto never acts on a `command` entry**. `applyLadder` refuses bootstrap and staff. Suggestions are ledger rows; voided tipping entries withdraw them (`withdrawStaleSuggestions`); no mod channel = nothing recorded. Apply re-derives, claims, needs `TierAdmin` for a ban.
- Consequences: entry written, member told (before kick/ban), Discord asked, refusal voids the entry. `rapsheet-sweep` lifts served bans, untracks only on Unknown Ban.
- Client-side bans/kicks/timeouts come from `GUILD_AUDIT_LOG_ENTRY_CREATE` (needs View Audit Log); merlin's own skipped; timeout parser uses the injected clock.
- Case files: forum posts opened lazily on first entry, edits in place, best effort.
- **Alt hints never auto-link** (stored at 2, posted at 3). Linked accounts share one sheet.
- The model reads, never decides: `aimod.Complete` via `rapsheet.Reviewer`; prompts carry **no user ids and no message content** (tests grep).
- `/rapsheet me` hides notes, voids, ladder rows, links, moderator names. Sheets page by eight. `preview_test.go` writes to `RAPSHEET_PREVIEW_DIR`.

### Backing the birds (`internal/plugins/support`)

- Card words from `github.com/6586x57890143/cygnet/support`; layout is `core.Panel`. App emoji as a `Brand`; the owner named by mention, never a handle in the repo.
- `/support` is public, 10 minute per-channel cooldown (`claim`).
- Welcome cards from `core.EventSupportersJoined`, through `discordguard`, at most `maxThanked` names.

### Security model (spec.MD §4)

- **A stale guild is its own state** (`settings.Store.Stale`): writes paused, plugins off except `Store.Recovery`, `ActionPolicy` returns `Unknown` (refused for all but bootstrap). Nothing stale is kept to answer with.
- Layered authorization on every command; `Permissions.CanManageRole` stops the bot touching roles above its own.
- **`CanModerate` on restrictive actions**: refuses admin-equivalent targets unless the actor is one, refuses bootstrap for everyone, fails closed, re-derives the Administrator bit from roles.
- Fail at boot, not silently: unset tier, missing handler, invalid command shape.
- **Only untrack on "gone", never on "failed"** (`core.IsUnknownResource`, a 100xx code).
- **Nothing merlin posts can ping**: guard senders force an empty parse list (`noPings`); a needed ping gets a named method with an explicit allowlist. Raw-client senders zero mentions themselves.
- `everyoneCanRead` answers yes when unsure; `/config status` re-checks every time.
- `GuildRoleDelete` prunes mod roles and allow/deny lists and drops cached marker IDs.
- Log IDs, not content. Self-throttle below Discord's limits (`channelCapHeadroom`).

## Conventions (CONTRIBUTING.md)

- Small PRs: one milestone or slice each (spec.MD §9). `main` is protected: review and all CI checks.
- Before a PR: vet, race tests, lint, the coverage floor, and the prose grep from `ci.yml`.
