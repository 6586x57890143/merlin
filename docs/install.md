# Add merlin to a server

Invite link (a server admin has to click it):

```
https://discord.com/oauth2/authorize?client_id=1533094679560847460
```

That is Discord's own link, and it asks for what merlin's install settings
say, which merlin writes herself: what the plugins in use need now, across
every server she is in. Never `Administrator`. A plugin asks only where it is
switched on, and aimod only where its mode is on, so nobody is asked to let
merlin delete messages or time people out until something needs it, and the
ask drops again when nothing does. The settings follow within a minute of a
change.

| Plugin | Asks for | Why |
|---|---|---|
| rotation, contest, `/config` setup | `Manage Channels`, `Manage Roles` | create, move and delete channels, set their overwrites |
| roles | the same, and `Move Members` | jail and vacation; overwrites don't apply to a voice call already running, so a jailed member mid-call is moved out |
| statistics | `View Channel`, `Read Message History` | the backfill reads channel history |
| aimod, where its mode is on | `View Channel`, `Read Message History`, `Manage Messages`, `Manage Webhooks`, `Moderate Members` | read a message and the lines before it, delete one, repost a rewrite under its author's name, and the timeout the sanction ladder falls back to |
| rapsheet | `Ban Members`, `Kick Members`, `Moderate Members`, `View Audit Log` | `/ban`, `/kick` and `/timeout`, and hearing about bans placed by hand: without the audit log Discord sends nothing, and `/rapsheet status` says so |

A server keeps what it granted when it was invited. When a plugin needs more
than that, clicking the link again updates merlin's existing role rather than
adding a second one. A server that doesn't want merlin banning switches
rapsheet off on `/config`.

## Intents

`GUILDS`, `GUILD_VOICE_STATES` and `GUILD_MESSAGES` are unprivileged and always
requested. `GUILD_MESSAGES` delivers message events with no text in them (that
needs `MESSAGE_CONTENT`, below); `/statistics` counts who posted where, by the
hour, from those.

`GUILD_MEMBERS` and `MESSAGE_CONTENT` are privileged: each is a toggle under
Bot in the Discord Developer Portal (**Server Members Intent**, **Message
Content Intent**). merlin reads those toggles when she starts and asks for
exactly what they grant, so Discord never refuses her connection over an
intent, and switching one later restarts her onto it within ten minutes.

What `GUILD_MEMBERS` buys you: a jailed member who rejoins gets re-jailed
instantly instead of within a minute, and roles that a server's Onboarding
flow hands back to a jailed member get stripped again right away. Both of
those work without it, just a minute slower.

`MESSAGE_CONTENT` is the far larger ask (every message in every server, and
Discord reviews it above 100 guilds), and nothing but `/aimod` reads it. Leave
it off unless you run AI moderation. Without it that plugin registers its
commands, scans nothing, and says so in `/aimod status` rather than looking
like it's working.

## First run

1. Invite the bot.
2. Run `/config` and press **Guided setup** under Channels, as the server owner, any Administrator or the bootstrap
   admin. It creates `#bird-audit-log`, `#bird-status` and a `merlin Mod` role
   for whatever's missing, and offers a picker for anything you already have. Safe
   to re-run whenever, it doubles as a status screen.
3. Add everyone else on `/config`'s **Admins and mod roles** page.
4. `/rotation`, pick a channel and press **Start rotating** if you want one.
5. `/config`'s overview to check it's all wired up.

Before you trust rotation on a real channel, turn on dry-run
(`/config`, **Pause and dry-run**) and let a full interval pass. It'll write to the audit log
describing exactly what it would have deleted, and touch nothing. Channel
deletion has no undo, so this is worth the wait.
