<img src="internal/core/assets/merlin_avatar.png" width="128" align="right" alt="merlin">

# merlin

A Discord moderation bot in Go, built for **The Melting Pot**, a free-speech
server that draws mass-reporting and brigading. One binary and Postgres, with
features compiled in as plugins. Built on [disgo](https://github.com/disgoorg/disgo)
and [pgx v5](https://github.com/jackc/pgx); the last of its REST calls are moving
over from discordgo.

- Channel rotation, jail and vacation, timed role grants, contests with a voting
  gallery, per-member rapsheets, and optional AI moderation held to Discord's own
  guidelines.
- Configured from inside Discord, on panels. Every command declares who may run
  it, and each server can tighten that per action.
- Least privilege: never Administrator, a separate invite for each level of
  trust, and nothing it posts can ping anyone.
- Anything destructive can be stopped from three places: pause, dry-run and a
  host-level switch.

[`spec.MD`](spec.MD) is the design.

## Docs

- [What it does](docs/features.md)
- [Add it to a server](docs/install.md): the invite links, intents and first run
- [Run it yourself](docs/run.md)
- [Deploy](docs/deploy.md): what a merge to `main` does, and the contest gallery
- [When something breaks](docs/incidents.md): pause, roll back, restore
- [Code layout](docs/layout.md), [contributing](CONTRIBUTING.md) and [security](SECURITY.md)
