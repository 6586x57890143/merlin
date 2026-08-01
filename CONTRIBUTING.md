# Contributing

## Workflow

- Work in small, reviewable PRs — one milestone (or a slice of one) per PR,
  per the milestone breakdown in `spec.MD` §9.
- `main` is protected: PRs require review and all CI checks green before
  merge. No direct pushes, no force-pushes.

## Before opening a PR

```sh
go vet ./...
go test ./... -race -cover
golangci-lint run   # if installed locally; otherwise CI will run it
```

## Conventions

- Every plugin implements `core.Plugin` (see `internal/core/registry.go`)
  and is registered in `cmd/bot/main.go` — no other wiring.
- Plugins communicate only through `core.EventBus`, never by importing each
  other's packages.
- One top-level slash command per plugin (`/rotation`, `/scheduler`, ...),
  registered via `core.CommandRouter.RegisterCommand`/`Handle` during
  `Init` — never call `session.AddHandler` or `discordgo.ApplicationCommand*`
  directly from a plugin (see `spec.MD` §4a). Every leaf handler requires an
  explicit `core.PermSpec{Tier, Action}`; `Finalize()` fails startup if any
  registered subcommand is missing one, so an ungated command is a build-time
  failure, not a runtime surprise.
- Any option whose valid values come from bot state (job keys, action
  names, ...) gets Discord autocomplete plus a plain `list` subcommand; any
  option that's a channel/role/user ID uses the native
  `Channel`/`Role`/`User` option type, never a raw string (`spec.MD` §4a).
- Guild-scoped config lives in `internal/settings` (Postgres-backed), not
  `config.yaml` — expose new configurable state through your plugin's own
  commands, not a file admins need host access to edit.
- Log IDs, not message content, by default (see `spec.MD` §4).

## Docs: one owner per fact, no append-only sprawl

Three docs, three distinct jobs — don't blur them:

- `spec.MD` is the only place design rationale lives (architecture, security
  model, why a decision was made). When a design changes, **edit the
  relevant section in place** — don't append a new dated/milestone-numbered
  subsection describing the diff (the `§4a`/`§4b`/`§4c` sprawl this
  guardrail replaces). `spec.MD` describes the current state of the design;
  historical narrative belongs in commit messages and PR descriptions.
- `README.md` is orientation and operation only: what this is, how to
  run/test/deploy it, a short per-plugin pointer into `spec.MD` for depth.
  If an explanation needs more than 2-3 sentences, it's a `spec.MD` section
  — link to it, don't duplicate it inline.
- Code comments carry the local WHY for a specific piece of code (per the
  general comment policy — only when genuinely non-obvious). They shouldn't
  re-explain something already covered in `spec.MD`; link there instead if
  the reasoning is more than a line.

Before adding a new doc section, check whether an existing one can just be
updated instead. Prefer trimming over appending.
