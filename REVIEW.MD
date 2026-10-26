# Roles plugin review: fixes implemented

The separate Astra high code and overwrite reviews were completed on 2026-10-26. All 13 recorded findings are addressed in the working tree. These changes have not been deployed to Discord.

| # | Finding | Implemented correction |
|---|---|---|
| 1 | Removing a private channel retained the marker's view allow | Reconcile stale marker visibility before applying the @everyone-deny optimization. |
| 2 | Release could delete moderator-owned member denies | Require the latest matching overwrite audit event to belong to Merlin. Preserve manual, ambiguous and unverifiable restrictions, and re-read the overwrite before deletion. |
| 3 | Cold role caches skipped settings sync | Resolve existing markers from Discord independently of cache warmth; reconcile configured markers before use. |
| 4 | Category propagation undid allowlisted child access | Write categories first, refetch channels, then reconcile children. Serialize guild syncs and refresh mode lists after acquiring the lock. |
| 5 | Failed writes were reported as success or abandoned | Report settings saved but access unapplied, retain pending work for the sweep, and reject marker resolution if initial restrictions fail. Startup reconciliation also recovers interrupted work. |
| 6 | Allowlisted forums could create posts | Forum, media and stage channels are read-only. Conversation selectors accept only text, announcement and voice channels; thread creation and replies remain denied. |
| 7 | Expired/revoked grants returned from jail snapshots | Atomically prune the snapshot when deleting a grant. Release also excludes expired grants that have not yet been swept. Snapshot capture and restoration use fresh state under the member claim. |
| 8 | An in-flight release overrode an extension | Sentence changes share the release claim; automatic release re-reads and validates the current deadline after claiming the member. |
| 9 | Discord received temporary grants before persistence | Defer the interaction, persist and arm expiry before granting the role. Retain tracking after an ambiguous Discord error. Grant changes share the member claim and cannot add roles during confinement. |
| 10 | Eternal-role emoji fallback recreated roles repeatedly | Store the emoji actually returned by Discord after fallback. |
| 11 | Allowed channels omitted message history | Both visible modes explicitly allow View Channel and Read Message History, clearing conflicting managed denies. |
| 12 | Member-baseline capping removed sending | Conversation explicitly grants Send Messages independently of stripped member roles and the ordinary member baseline. Member-specific restrictions still apply. |
| 13 | Settings and migration needed explicit access modes | Separate read-only/conversation selectors for jail and vacation, atomic moves between lists, database disjointness constraints, and read-only defaults for all legacy allowlists. |

## Rollout

Migration `0051_roles_channel_access` retains existing allowed-channel arrays as read-only and starts both conversation arrays empty. It persists the Melting Pot's formerly implicit beach default as read-only; clearing that list now remains cleared. No legacy entry is inferred to permit conversation.

The first roles sweep after startup reconciles Discord overwrites, including old send allows and missing or denied history. Failed syncs stay pending for later sweeps; dry-run and paused writes defer application. Administrators can explicitly move channels to Conversation in `/roles settings`.

The selected mode owns visibility, history, sending and participation restrictions. Unrelated role-overwrite bits are preserved. Member-specific denies are never removed merely to make an allowlisted channel usable. Cleanup of legacy member denies requires available audit provenance; older/unverifiable restrictions remain for manual review.

Operational documentation: [features](docs/features.md). Design summary: [specification](spec.MD). Discord permission semantics: [official permissions documentation](https://docs.discord.com/developers/topics/permissions).

## Validation

## PR review follow-up

The Astra Max review of PR #245 identified two additional grant-path gaps after integration with current main. Both are corrected:

- Supporter grants now use the same member claim and persistence-before-Discord ordering as manual grants. Failed API adds retain tracking and can retry without requiring a new backing deadline.
- Dry-run exits before changing grant state. Definitive guard refusals restore the previous grant record and expiry timer; only ambiguous Discord failures retain the attempted grant for cleanup.

Regression tests cover both paths, including existing untracked, permanent and temporary roles. Full review and current-head CI remain prerequisites for merge.

## Test results

- Full `go test ./... -race -cover` passed in Linux with disposable PostgreSQL 16, including migration and settings round trips and atomic snapshot pruning. Roles coverage: 80.8%.
- The final queued-sync correction passed a separate `go test ./internal/plugins/roles/... -race -count=1` run.
- `go vet ./...` passed; final roles changes also passed a targeted vet run.
- `golangci-lint run ./...` passed with zero issues.
- Regression coverage includes stale marker denies/allows, missing history, @everyone denies with stripped member roles, both mode transitions, removal, cold caches, category propagation, retry reporting, startup migration, manual overwrite ownership, grant persistence/expiry, sentence extensions, queued syncs, and emoji fallback.
- A Windows full-suite run hit `TestInstallerSyncsOnceAfterABurst` (timing assertion in unchanged code). That test passed in the full Linux race run. No unrelated installer code was changed.

The deployed guild's effective permissions have not been inspected. The original send/history report has code-level explanations and regression coverage, rather than a live-server reproduction.
