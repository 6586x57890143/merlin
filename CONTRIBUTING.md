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
- Any privileged command handler must call `core.Permissions.Authorize`
  before acting, and register commands via `core.RegisterCommands` (which
  fails closed on missing `DefaultMemberPermissions`) unless the command is
  deliberately public.
- Log IDs, not message content, by default (see `spec.MD` §4).
