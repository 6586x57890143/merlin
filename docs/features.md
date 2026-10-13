# What merlin does

**Channel rotation.** Point it at a channel and every so often it swaps that
channel out for a fresh empty one under the same name, in the same place in the
sidebar. The old one gets renamed, moved into a hidden archive category and
deleted after a retention window you set (or kept forever if you'd rather). The
channel gets a heads-up before it happens and a short note afterwards explaining
what just happened. Handy if you run a channel where people would rather their
history not sit around indefinitely.

Archives are hidden from everyone except your mods and anyone with Discord's
Administrator bit. If you want another role to be able to read them, add it under
**Archive readers** on `/rotation`. That grant is read-only, so the role
can view the channel and read the history back and nothing else: no posting, no
reactions, no threads. Permissions live on the archive category and every
archived channel is kept in sync with it, so a role you add or remove applies to
the archives that already exist, not just future ones. The bot also checks
hourly that nothing has been added to that category by hand.

**Jail.** `/roles jail @someone 2h` snapshots their roles, strips them and drops
a shared "Jailed" role on them instead. That role's channel overwrites decide
what a jailed member can still see, so jailing costs one API call no matter how
big the server is. They get their roles back automatically when the timer runs
out, or when a mod runs `/roles release`. Leaving and rejoining doesn't shake it
off. There's a bulk version and a `/roles jail-role` for when a raid shows up.
A jailed member is never given more in an allowlisted channel than an ordinary
member has there (the member role on `/roles settings` says who that is): allow
an announcements room and they can read it, not post in it, and the command
says so.

**Vacation.** The same thing, somewhere nicer. `/roles vacation @someone 2d` is
jail with a different marker role, one the server already has (the Melting
Pot's island; elsewhere set on `/roles settings`), hidden from every
channel except its own allowlist (also on `/roles settings`;
the Melting Pot's beach by default). merlin never creates that role, and when
it syncs a channel it moves only the visibility bits and leaves the rest of
the overwrite as the server set it, so a beach built by hand keeps working.
On both the jail room and the beach, threads, polls, invites and the like are
off; talking and voice are not. Moving
someone between the two is `/roles jail` or `/roles vacation` on them again,
which takes the new duration; a mod swapping the two roles by hand is noticed
and the sentence follows the role. It is a script, so it is off until
turned on under Scripts on `/roles settings`.

**Timed role grants.** `/roles grant @someone @role 24h`, and it comes off by
itself.

**Contests.** `/contest new title:neon cats` and merlin runs the whole thing:
an announce window while the prize pool fills, then a forum channel she creates
for the entries, then voting, then the winner. People enter by posting in the
forum like any other forum, so files are Discord's problem and not the bot's,
and deleting your post is how you withdraw. Voting happens on a web gallery
because forty pieces of art is the one thing Discord is genuinely bad at.
Everyone signs in with Discord there, so it is one vote per member of your
server and no one else, and the page is an unguessable link no search engine
indexes.

Before the first contest, tell merlin who contests are for on
`/contest settings`, by picking a channel like #members-only. She copies that channel's
view permissions onto every contest forum she makes, so a server where only a
gated role sees the chat gets a contest only that role can see, and a server
where everyone sees everything gets one everyone can enter. It is read live
each time, so renaming or replacing the role needs no change here. If no
channel suits, the roles picker on the same panel names them outright, and a
second picker hands attachment rights to a separate role.
`/contest new` refuses until one of the two is set, because the wrong guess
publishes members' work to accounts you deliberately have not let in.

**Prizes.** `/contest prize` opens a form. Steam key, Nitro gift link, crypto,
anything. If it has a code, merlin keeps it encrypted and DMs it to the winner
the moment the contest ends, then wipes it. If it does not, she introduces the
two of you and stays out of it. She never holds money.

**Everything is configured in Discord.** No YAML editing on the host for
day-to-day stuff. `/config` opens one settings panel: admins and mod roles,
channels, plugins, permissions, and pause and dry-run, each a page you change
with pickers rather than a subcommand you type. Its guided setup walks you
through the audit log channel, status channel, mod role and admins. Every command
declares a Public/Mod/Admin tier. You can override tiers per server, allow or
deny specific people or roles per action, or switch a whole plugin off entirely.

**An audit trail and an off switch.** Anything the bot does lands in
`#bird-audit-log` as an embed and in Postgres as a row. Pause, on `/config`,
refuses every destructive action instantly, dry-run makes it describe what it
would have done without doing it, and there's a host-level env var for when
Discord or the database is the thing that's broken.

**AI moderation.** Optional, off until you turn it on, and it needs an
OpenRouter key you provide. It reads messages and checks them against
[Discord's own Community Guidelines](https://discord.com/guidelines), removing
or cleaning up the ones that breach them. The point is narrow: it is there to
stop the server being reported off the platform, not to police how people talk.
Rudeness, hostility, profanity, dark humour and opinions you dislike are not
violations and it is told so explicitly.

Each of the ten policy areas is a file in `internal/plugins/aimod/policy/`
saying both what the rule covers and where it stops, and you choose per area
whether merlin ignores it, flags it, cleans it up or removes it. Only child
safety is fixed on. Hate speech, gore, self-harm and spam are **off** by
default: those are questions about what kind of room you're running, and you
should switch them on deliberately rather than find out later.

It is built to be cheap for whoever is paying. Most messages never reach a
model at all (a skip filter and a small pattern list handle them for free);
what does is batched and read by a cheap model that can only ever *flag*;
nothing is deleted until a better model has re-read it against the full policy
text. You set a daily USD cap per server, and past it merlin falls back to the
free checks rather than spending more. `/aimod models show` prices your current
setup against your server's own measured traffic, and `/aimod models compare`
prices any other model before you switch.

Every action is explained to the member in a DM, recorded in the audit log, and
reversible with `/aimod undo` while the evidence window lasts. Start with
the mode on `/aimod settings` set to flag, and watch it for a week before letting it act.

**Rapsheets.** Every warning, jail, timeout, kick, ban and AI removal a member
collects lands on one per-member record with a case number, whoever or whatever
decided it, including bans your mods place from Discord's own menus.
`/rapsheet view @someone` is the sheet; `/rapsheet me` shows a member their own,
with the moderators anonymised. Entries carry points that fade over a half-life
you set, and the score they add up to drives a ladder: cross a band and merlin
posts the recommended consequence to your mod channel with an Apply button, or,
if you tell it to, applies it (never against staff, never permanently, never on
top of a moderator's own command). `/rapsheet warn`, plus the top-level
`/timeout`, `/kick` and `/ban 7d`, are the commands; temporary bans lift themselves. Each member on
record gets a thread in a mod-only forum so there is somewhere to talk next to
the facts, a moderator can `link` two accounts that are the same person so they
share one score, and merlin flags a joiner who looks like a return (same avatar,
same name, minutes after a ban) for a mod to confirm; it never links anyone by
itself. With AI moderation configured, `/rapsheet summary` writes a paragraph
from the record and a Monday review points out inconsistent sentencing.
