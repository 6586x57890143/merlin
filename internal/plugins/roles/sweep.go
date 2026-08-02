package roles

import (
	"context"
	"fmt"
)

// makeSweepJob returns the Scheduler job function that releases due jails
// and removes due grants for one guild. One sweep job per guild, running
// every sweepInterval (see roles.go).
func (p *Plugin) makeSweepJob(guildID string) func(ctx context.Context) error {
	return func(ctx context.Context) error {
		return p.sweep(ctx, guildID)
	}
}

// sweep processes every due jail and grant for guildID. A single row's
// failure is logged and doesn't abort the rest of the sweep — mirrors
// rotation.sweep's "one bad row doesn't block the others" policy.
func (p *Plugin) sweep(ctx context.Context, guildID string) error {
	var firstErr error

	dueJails, err := p.store.DueJails(ctx, guildID, p.now())
	if err != nil {
		return fmt.Errorf("roles sweep: query due jails: %w", err)
	}
	for _, rec := range dueJails {
		if err := p.releaseJail(ctx, guildID, rec.UserID, rec); err != nil {
			p.log.Error("roles sweep: release jail failed", "guild", guildID, "user", rec.UserID, "err", err)
			if firstErr == nil {
				firstErr = err
			}
		}
	}

	dueGrants, err := p.store.DueGrants(ctx, guildID, p.now())
	if err != nil {
		return fmt.Errorf("roles sweep: query due grants: %w", err)
	}
	for _, rec := range dueGrants {
		if err := p.revokeGrant(ctx, guildID, rec.UserID, rec.RoleID, "system"); err != nil {
			p.log.Error("roles sweep: revoke grant failed", "guild", guildID, "user", rec.UserID, "role", rec.RoleID, "err", err)
			if firstErr == nil {
				firstErr = err
			}
		}
	}

	return firstErr
}
