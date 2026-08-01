\# Melting Pot Bot — Project Specification 
CODENAME "merlin", brand it as "merlin" (bird species)



A production-grade, modular Discord bot for "The Melting Pot," built on \*\*discordgo v0.29.0+\*\* (latest stable as of writing — pin the exact tag in `go.mod` and let Dependabot bump it). Designed for a high-free-speech community with a faction system and real exposure to weaponized/mass reporting, so the architecture treats \*\*tamper-resistance and fail-safe behavior\*\* as first-class requirements, not an afterthought.



\---



\## 1. Design Principles



1\. \*\*Modular by plugin, monolithic by binary.\*\* Every feature (scheduler, channel rotation, factions, reporting, etc.) is an isolated Go package implementing a common `Plugin` interface. They are compiled into a single binary and registered at startup — \*\*do not\*\* use Go's native `plugin` package for dynamic/hot-loading. Dynamic loading buys you nothing here (no third parties are shipping plugins) and meaningfully increases attack surface and cross-platform pain. Modularity is for code organization and blast-radius containment, not runtime extensibility.

2\. \*\*Fail safe, not fail silent.\*\* Any plugin error halts \*that operation\* and alerts a mod-only status channel. Nothing destructive (channel delete, role strip, ban) ever happens as a side effect of an unhandled error.

3\. \*\*Defense in depth on every privileged action.\*\* Discord permission checks are necessary but never sufficient — every command re-checks an internal allow-list, role hierarchy, and guild scope before acting.

4\. \*\*Config changes are audited, not silent.\*\* Anything that changes bot behavior (rotation interval, faction lists, mod role list) is logged to an append-only audit trail with who/when/old→new.

5\. \*\*Minimize what you log and retain.\*\* Given the privacy motivation behind the rotation feature, default to logging IDs (user/channel/message) rather than message content, and make any content retention an explicit opt-in per feature.



\---



\## 2. Repository Layout



```

merlin/

├── cmd/bot/main.go                 # entrypoint: load config, wire core, start plugins

├── internal/

│   ├── core/

│   │   ├── session.go              # single shared \*discordgo.Session, intents, sharding-ready

│   │   ├── registry.go             # plugin registration + lifecycle (Init/Start/Shutdown)

│   │   ├── eventbus.go             # internal pub/sub so plugins don't call each other directly

│   │   └── permissions.go          # central authz: Discord perms + internal allow-list + role hierarchy

│   ├── config/

│   │   ├── schema.go               # per-guild config structs (validated with struct tags)

│   │   └── loader.go               # env + YAML, hot-reload on SIGHUP for non-secret values

│   ├── audit/

│   │   └── audit.go                # writes to DB + posts embeds to #bot-audit-log

│   ├── storage/

│   │   ├── postgres.go             # or sqlite.go for small deployments

│   │   └── migrations/

│   └── plugins/

│       ├── scheduler/              # Plugin 1 (this doc's priority)

│       ├── rotation/                # Plugin 2 (this doc's priority)

│       ├── factions/                # Plugin 3

│       └── reporting/               # Plugin 4 (stub for now)

├── .github/

│   ├── workflows/ci.yml

│   └── dependabot.yml

├── Dockerfile

├── docker-compose.yml

├── SECURITY.md

├── CONTRIBUTING.md

├── README.md

└── .env.example

```



\---



\## 3. GitHub \& Docker Setup (do this first, before feature work)



\*\*GitHub:\*\*

\- Private repo initially. Branch protection on `main`: required PR review, required status checks (lint, test, vuln scan, build), no force-push, no direct push.

\- `.github/workflows/ci.yml` running on every push/PR:

&#x20; - `go vet ./...`

&#x20; - `golangci-lint run` (or `staticcheck`)

&#x20; - `go test ./... -race -cover`

&#x20; - `govulncheck ./...` (catches known-vulnerable dependencies)

&#x20; - `gitleaks detect` or `trufflehog` (secret scanning — catches an accidentally committed bot token before it ever reaches a public diff)

&#x20; - `docker build` to confirm the image still builds

\- `.github/dependabot.yml` for Go modules, Docker base image, and GitHub Actions versions.

\- `SECURITY.md` with a private disclosure contact/process.

\- Commit early, commit often, in small reviewable PRs — the milestone breakdown in §9 is designed so each milestone is one or two mergeable PRs.



\*\*Docker:\*\*

\- Multi-stage build: `golang:1.x-alpine` builder → `gcr.io/distroless/static` (or `scratch` + CA certs) final stage. No shell, no package manager, no build tools in the shipped image.

\- Run as non-root (`USER nonroot` / explicit UID in distroless), read-only root filesystem, `--cap-drop=ALL`, no published ports unless you add an internal metrics endpoint (and even then, bind to a private network only).

\- `docker-compose.yml` for local dev: bot + Postgres (or SQLite volume) + optional Redis if you later need distributed locks.

\- Bot token and DB credentials are injected via environment variables from Docker secrets / your platform's secret manager — \*\*never\*\* baked into an image layer or committed to `.env` (only `.env.example` with placeholder keys is committed).



\---



\## 4. Security Model



\### Secrets \& credentials

\- Bot token, DB creds, and any webhook URLs live only in environment variables sourced from a secret manager (Docker secrets, Vault, or your host's equivalent). Document a token-rotation runbook (revoke in Discord Developer Portal → update secret → redeploy) so a leaked token is a 5-minute fix, not a fire drill.



\### Least privilege

\- Request only the OAuth2 scopes and permission bits the bot actually needs (enumerate them explicitly in `README.md` — e.g., `Manage Channels`, `Manage Roles`, `Manage Messages`, `View Audit Log`). Avoid `Administrator`.

\- Request only the gateway intents you use. `GUILD\_MEMBERS` and `MESSAGE\_CONTENT` are privileged intents requiring Discord approval at scale — only request `MESSAGE\_CONTENT` if a specific plugin genuinely needs message content (the rotation plugin, notably, does \*\*not\*\* need to read message content to do its job).



\### Authorization — layered, not single-point

Every privileged command/action checks, in order, before its handler ever runs:

0\. \*\*Plugin enabled?\*\* (`/config plugins enable\|disable`) — checked first; a disabled plugin's commands are unusable by anyone, including admins, until re-enabled.

1\. \*\*Deny\*\* (`/config permissions block\|unblock`): has this specific person/role been explicitly blocked from this action? Wins over every layer below, including Administrator — except the break-glass admin, which nothing can deny.

2\. \*\*Tier\*\*: Public always passes. Mod/Admin check the action's effective tier — a per-guild override (`/config permissions set-tier\|clear-tier`) if one is set, else the command's own compiled-in default (mandatory at registration; `core.CommandRouter.Finalize()` refuses to start the bot if any command is missing one). Admin passes for a DB-listed admin, the break-glass bootstrap identity, or anyone holding Discord's own Administrator permission (read directly off the interaction, no extra API call) — deliberately \*not\* extended to Mod, so holding an elevated Discord permission never grants bot-reconfiguration rights on its own. Mod passes for a DB-listed mod role or an admin.

3\. \*\*Allow\*\* (`/config permissions grant\|revoke`): an additive per-action whitelist lets a specific role/user past regardless of tier, without becoming a full mod or admin.

4\. Role-hierarchy sanity: the bot can never assign/remove a role positioned above its own top role (Discord enforces this API-side too, but check and fail cleanly rather than surface a raw API error).

5\. Discord's own `default\_member\_permissions` is deliberately \*\*not\*\* used to mirror the model above — left unset on Mod/Admin commands so a whitelisted non-mod user, or a mod role with no matching Discord bit, is never blocked by Discord before the checks above run. Guild admins retain Discord's own separate per-command visibility overrides (Integrations settings) as an orthogonal, optional extra layer.

Mutating any of the above (admin list, mod roles, tier overrides, allow/deny, plugin toggle) is itself Admin-tier only, so a mod can never escalate their own or anyone else's access. See §4a for the mechanics.



\### Anti-tampering / abuse containment

\- \*\*Rate-limited self-throttling independent of Discord's own rate limits\*\*: hard caps like "no more than N channel-create operations per guild per hour" regardless of config, so a bad config value, bug, or malicious internal actor can't spam-create channels and get the bot (or the whole app) rate-limited or banned by Discord.

\- \*\*Panic isolation\*\*: every plugin's event handlers run under `recover()`; a panic in one plugin logs and alerts, it never takes down the process or other plugins.

\- \*\*Config-change audit trail\*\*: every change to rotation intervals, faction definitions, or the mod allow-list is written to an append-only log \*and\* posted as an embed to a locked-down `#bot-audit-log` channel (who, what changed, old value, new value, timestamp) — visible to the bot and, once a guild has any mod roles configured, to those roles too (granted automatically whenever a role becomes a mod role, whether via `/config setup` or `/config mod-roles add`); \*\*not\*\* to `@everyone`.

\- \*\*Confused-deputy checks\*\*: before acting on any channel/role ID pulled from an event or scheduled job, re-verify it still belongs to the configured guild and matches the expected config entry — don't trust a cached ID blindly.

\- \*\*Minimal logging of content\*\*: log IDs, not message bodies, by default. This matters doubly here since the whole point of the rotation feature is \*not\* retaining chat content longer than necessary — your own logs shouldn't quietly become the retention you're trying to avoid.

\- \*\*Dependency hygiene\*\*: `govulncheck` in CI, Dependabot auto-PRs, pinned versions in `go.mod`/`go.sum`.



\---



\## 4a. Command \& Permission Framework



Every plugin registers its own top-level slash command (`/rotation`, `/scheduler`, `/config`, ...) with its own subcommands — no central `/admin` catch-all. The only cross-cutting exception is `/config` (mod roles, admins, permission policy, plugin toggles — concepts that don't belong to any single feature plugin).



\*\*`core.CommandRouter`\*\* (`internal/core/commands.go`) is the sole owner of registration and dispatch: plugins call `Commands.RegisterCommand`/`Handle`/`Autocomplete` during `Init`, never `session.AddHandler` or `discordgo.ApplicationCommand\*` directly, so there's exactly one writer performing the per-guild bulk-overwrite (guild-scoped, not global, so commands appear instantly instead of waiting on Discord's up-to-an-hour global-propagation delay). Every dispatch — command and autocomplete — runs under `recover()` (`discordgo`'s own event dispatch has none of its own), and every leaf's `PermSpec{Tier, Action}` is mandatory and validated at startup (`Finalize`).



\*\*Discoverability\*\*: any option whose valid values come from bot state (job keys, action names, plugin names) uses Discord autocomplete plus a plain `list` subcommand as a no-typing fallback; any channel/role/user-valued option uses Discord's native picker type, never a raw ID string. Every command response is a color-coded, ephemeral embed (`internal/core/embeds.go`'s `RespondOK`/`RespondErr`/`RespondInfo`/`RespondWarn`, from Merlin's brand palette) — the standing convention for every plugin's responses.



\*\*Self-serve configuration\*\*: guild-scoped settings (mod roles, admins, permission policy, rotation config) live in Postgres via `internal/settings`, mutated only through Discord commands. `config.yaml`/`.env` hold nothing but process bootstrap — Discord token/App ID, DB DSN, log level, and one \*\*break-glass admin user ID\*\* that always satisfies Admin tier everywhere, so a wiped or misconfigured guild's settings can never permanently lock the operator out. `/config import` seeds a guild's settings from a legacy `config.yaml` once, for migrating a pre-this-model deployment — never run automatically.



\*\*Permission policy is per-guild-configurable\*\*, not fixed at registration, in three composable layers (coarsest first, matching §4's Authorization order): a whole plugin can be switched off (`/config plugins enable\|disable` — except `/config` itself, which can never be disabled, or a guild could never re-enable anything); any action's effective tier can be overridden (`/config permissions set-tier\|clear-tier`) independent of its compiled-in default — rotation, for instance, splits structural changes (add/remove a rotating channel) from adjustment (interval/retention/visibility/sticky) into two separate actions specifically so one can be loosened without the other; and specific people/roles can be granted or explicitly blocked (`/config permissions grant\|revoke\|block\|unblock`) regardless of tier — a block always wins except against the break-glass identity.



\*\*First-time setup\*\*, concretely:

1\. Before first run, an operator sets `MERLIN\_BREAK\_GLASS\_ADMIN\_USER\_ID` to their own Discord ID.

2\. On joining a guild with nothing configured yet, the bot DMs the guild owner once, pointing at `/config setup` — Discord has no API for "who invited the bot," so the owner (who always implicitly holds Administrator) is the best available proxy. No public fallback if the DM fails; it just retries on the bot's next restart.

3\. The owner (via the Administrator bit) or the break-glass identity runs `/config setup`, which creates `#bot-audit-log`, `#bot-status`, and a mod role for whatever's missing, and is safe to re-run any time as a status check.

4\. `/config admins add`/`/config mod-roles add` bring in others; `/config permissions`/`/config plugins` fine-tune access from there.



\---



\## 5. Plugin 1 — Scheduler (Cron Core)



A generic internal service other plugins register recurring jobs with. This is infrastructure everything else depends on, so build and test it first.



```go

type Scheduler interface {

&#x20;   Register(name string, spec CronSpec, fn func(ctx context.Context) error) error

&#x20;   RunNow(ctx context.Context, name string) error // manual trigger for admin testing

}

```



Key requirements:

\- Wraps `robfig/cron/v3` but \*\*persists last-run timestamps per job key\*\* (guild + job name) in storage. This matters specifically for the rotation plugin: "every N hours" is a per-guild interval, not a fixed daily wall-clock cron expression, so a naive `@every 24h` resets its clock on every process restart. Track `next\_due = last\_run + interval` explicitly so a bot restart doesn't silently reset (or double-fire) a rotation window.

\- Retry with backoff on job failure; after N consecutive failures, alert `#bot-status` rather than retry forever.

\- Jitter across guilds so many guilds with the same interval don't all fire in the same second.

\- Per-job locking (in-process mutex now; a DB/Redis advisory lock later if you ever run multiple replicas) so a job can't double-run.

\- A `/scheduler run-now` command (Mod tier) for testing without waiting for the interval — the `job` option uses Discord autocomplete over the invoking guild's currently-registered job keys, and a companion `/scheduler list` subcommand shows every registered job plus last-run/next-due/failure-count with no typing required, so a mod never has to guess a job's name (see §4a).

\- All timestamps stored and compared in UTC.



\---



\## 6. Plugin 2 — Channel Rotation ("Refresh")



This is the priority feature: rotate `#general-chat` (or any configured channel) immediately or on an interval, giving members a clean history while preserving a moderation trail.



\### Per-channel config

Per-channel rotation config is DB-backed (`internal/settings`), configured entirely through `/rotation configure` (Admin tier by default, per-guild-adjustable per §4a), using Discord's native `Channel`/`Role` option types instead of hand-copied snowflake IDs:



\- `/rotation configure add <channel> <interval-hours> <archive-category> [retention-days] [visibility]` — registers a new rotation, `retention\_days` defaults to 7 (never 0; can be set to "never"), `visibility` defaults to `mod\_only` (`mod\_only` \| `whitelist` \| `export\_then\_delete` (future)).

\- `/rotation configure edit` / `remove` — change or drop an existing rotation; `/rotation list` shows every configured rotation for the guild (channel, interval, next-due, retention, sticky status) with no typing required.

\- `/rotation configure sticky <channel> <on\|off> [text]` — the re-posted sticky message shown in \#6 step 6; off by default.



\### Process (idempotent, fail-safe at every step)

1\. Cron fires → acquire per-guild-per-channel lock.

2\. Fetch the current channel's state: topic, NSFW flag, slowmode, position, category, permission overwrites, active threads.

3\. \*\*Rename\*\* the current channel to an archive pattern, e.g. `general-chat-archive-2026-08-01-1400`.

4\. \*\*Move\*\* it into the hidden Archives category with a permission overwrite granting view access only to the configured mod role(s) — nobody else can see it once rotated.

5\. \*\*Create\*\* a new channel named `general-chat`, replicating position/category/topic/slowmode/permission overwrites captured in step 2, so it's indistinguishable from the original to members. You can clone channels in Discord to achieve this with appropriate perms

6\. Re-post any "sticky" messages (rules, pinned info, memes) and give it a birdlike flavor, can be set to a template instead or turned off completely. Defaults to off.

7\. Post a short system embed in the new channel, e.g.: \*"This channel resets every {N} hours as part of the server's data-retention policy. Anything posted here isn't kept past that window, except where an open moderation report requires it."\* This is genuinely useful transparency for members and also documents your retention policy if it's ever questioned. Make sure this is also communicated in a birdlike way without missing any of the information as Merlin is a bird, like the species of Bird (Merlin)

8\. Write to the audit log and `#bot-audit-log`: old channel ID, new channel ID, timestamp, triggering job.

9\. Schedule a \*\*follow-up\*\* job to permanently delete the archived channel after `retention\_days` — never delete immediately in the same pass. This is the safeguard from the top of this doc: mods retain a window to act on any report that references content now in the archive, and Discord's own report pipeline is unaffected by any of this since it snapshots reported content independently.

10\. On failure at any step, stop, log clearly, leave both channels in whatever state they're in, and alert `#bot-status` for manual mod intervention. Never proceed to "hide" or "delete" a channel if an earlier step (e.g., new-channel creation) failed.



\### Discord API realities to handle explicitly

\- \*\*Guild channel cap\*\*: Discord hard-caps a guild at 500 channels (250 for the older 500-total-object limit context) — check current count before creating and fail gracefully with a clear alert rather than a raw API error.

\- \*\*Rate limits\*\*: channel create/update is rate-limited at the guild level; respect discordgo's rate-limit signals and back off rather than retry-hammering.

\- \*\*The "general-chat" name/ID effectively moves to the new channel\*\*, since you're renaming the original (not deleting it) and creating a fresh one. Anything hardcoded to the \*old\* channel ID — external webhooks, other bots' configs — will keep posting into what is now the hidden archive. Fire an internal event on rotation (`channel.rotated{old\_id, new\_id}`) so other plugins/integrations can react, and document this clearly for anyone setting up external webhooks.

\- \*\*Active threads\*\* on the original channel move with it into the archive when renamed/moved (they don't clone). Decide and document a policy: auto-archive them, or block rotation if a mod has flagged a thread as "keep active" (make this configurable).



\---



\## 7. Plugin 3 — Faction System



\- On member join, present a faction picker (Discord select menu or buttons) in `#welcome` or via DM fallback.

\- Assigns exactly one faction role; mutually exclusive by default (switching factions, if allowed, removes the prior role — configurable cooldown to prevent faction-hopping abuse).

\- Faction membership stored in your DB (not just inferred from Discord roles) so you can rebuild role state if roles are ever wiped, and so faction-specific plugins (leaderboards, faction channels) have a reliable source of truth independent of Discord's role list.



\---



\## 8. Plugin 4 (stub) — Reporting



Given the free-speech context and the explicit worry about weaponized reporting: design so that \*\*reports route to human mod triage, not automated punitive action\*\*. No auto-mute/auto-ban purely from report volume — that's the exact mechanism mass-reporting brigades exploit. Reserve automated action for narrowly-defined, verifiable cases (e.g., a confirmed CSAM hash match via an established provider API), which should always escalate regardless of any other setting. Everything else surfaces to mods with context and lets a human decide.



\---



\## 9. Milestones (small, mergeable PRs)



| # | Milestone | Deliverable |

|---|---|---|

| 0 | Scaffold | Repo, CI green, Docker builds, bot connects and responds to `/ping` |

| 1 | Core | Plugin registry, event bus, config loader, permission layer, unit tests |

| 2 | Scheduler | Cron core with persisted last-run state, `/scheduler run-now`/`list` |

| 3 | Rotation | End-to-end `#general-chat` rotation in a test guild, audit log wired up |

| 4 | Command Framework \& Self-Serve Config | `core.CommandRouter` (single registrar/dispatcher, per-guild registration), tiered+whitelist `PermSpec` authorization, `internal/settings` DB-backed guild config, `/config` (admins/mod-roles/permissions/setup/import), `/rotation configure`, autocomplete + `list` discoverability everywhere (see §4a) |

| 5 | Factions | Join-time faction picker, DB-backed membership |

| 6 | Audit/Admin | Whatever isn't already covered by `/config` (§4a) — e.g. a richer `/config status`/health-check dashboard command, historical audit-log search/export |

| 7 | Reporting | Human-triage report routing |

| 8 | Export (future) | Archived channel export to JSON/HTML before deletion, if still wanted |



\---



\## 10. Open decisions to nail down before coding



\- Storage: Postgres (better for concurrent access, migrations, future scale) vs SQLite (simpler single-instance deploy). Given "production" and "future tasks" plural, Postgres is the safer bet.

\- Exact `retention\_days` default for archived channels (7 suggested — adjust to your comfort level, but avoid 0).

\- Whether faction-switching is allowed and on what cooldown.

\- Which role(s) count as "mod" for both Discord-side permissions and the internal allow-list (keep these lists in sync deliberately, not accidentally).



\---



This document is meant to be handed to whoever (or whatever) scaffolds the repo — it's detailed enough to start Milestone 0 immediately. Happy to start generating the actual Go scaffolding (go.mod, Dockerfile, CI workflow, and the `core` package skeleton) as a next step if you want to move straight into code.

