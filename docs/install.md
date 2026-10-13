# Add merlin to a server

Invite link (a server admin has to click it):

```
https://discord.com/api/oauth2/authorize?client_id=1533094679560847460&scope=bot%20applications.commands&permissions=285212688
```

That asks for `Manage Channels`, `Manage Roles` and `Move Members`, and nothing
else. Never `Administrator`. If the permission bits ever change, re-clicking the
same link updates the bot's existing role rather than adding a second one.

`Move Members` is only there so `/roles jail` can boot someone out of a voice
call at jail time. Permission overwrites don't apply to a voice session that's
already in progress, so without it a jailed member who was mid-call would stay
in it.

## If you're running AI moderation

`/aimod` needs more than the link above grants, so it has its own:

```
https://discord.com/api/oauth2/authorize?client_id=1533094679560847460&scope=bot%20applications.commands&permissions=1100333786128
```

That's the same three, plus `View Channel` and `Read Message History` (to see
messages at all, and to read the few lines of context before a flagged one),
`Manage Messages` (to delete one), `Manage Webhooks` (to repost a rewritten
message under its author's name), and `Moderate Members` (the timeout that the
sanction ladder falls back to when jail can't be applied).

Two links rather than one on purpose. The plugin is off until an admin turns it
on, and a deployment that never will shouldn't be handing the bot the ability to
delete messages and time people out. Use the narrow link unless you need this.

## If you're running rapsheets with bans

`/rapsheet` keeps the ledger with the link above. To let it also ban, kick and
time people out, and to see the bans your moderators place by hand, it needs
one more:

```
https://discord.com/api/oauth2/authorize?client_id=1533094679560847460&scope=bot%20applications.commands&permissions=1100333786262
```

That's everything in the AI moderation link plus `Ban Members`, `Kick Members`
and `View Audit Log`. The last one is what lets merlin hear about a ban done
from a member's context menu rather than through her, so the sheet doesn't
lie by omission; without it Discord sends nothing and says nothing, and
`/rapsheet status` will tell you.

Three links, same reasoning. Use the narrowest one that covers what you've
turned on.

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
